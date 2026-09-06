package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/golang-jwt/jwt/v5"
)

const (
	issuer      = "https://issuer.example/"
	otherIssuer = "https://other.example/"
	resource    = "https://gateway.example/mcp"
)

// Key generation is slow enough to be worth doing once for the package.
var (
	keyOnce             sync.Once
	signingKey          *rsa.PrivateKey
	otherIssuerKey      *rsa.PrivateKey
	unrelatedSigningKey *rsa.PrivateKey
)

func keys(t *testing.T) (own, other, unrelated *rsa.PrivateKey) {
	t.Helper()
	keyOnce.Do(func() {
		for _, k := range []**rsa.PrivateKey{&signingKey, &otherIssuerKey, &unrelatedSigningKey} {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			*k = key
		}
	})
	return signingKey, otherIssuerKey, unrelatedSigningKey
}

func verifier(t *testing.T) *auth.Verifier {
	t.Helper()
	own, other, _ := keys(t)
	v, err := auth.NewVerifier(auth.NewStaticKeys(map[string]map[string]crypto.PublicKey{
		issuer:      {"": own.Public()},
		otherIssuer: {"": other.Public()},
	}), []string{issuer, otherIssuer}, resource)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// claims fills in the defaults of a valid token, so a test row names only what
// it is bending.
type claims struct {
	issuer   string
	subject  string
	audience string
	expiry   time.Time
	scope    string
	omitAud  bool
	omitExp  bool
}

func (c claims) build() jwt.Claims {
	registered := jwt.RegisteredClaims{
		Issuer:   cmp(c.issuer, issuer),
		Subject:  cmp(c.subject, "alice"),
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}
	if !c.omitAud {
		registered.Audience = jwt.ClaimStrings{cmp(c.audience, resource)}
	}
	if !c.omitExp {
		expiry := c.expiry
		if expiry.IsZero() {
			expiry = time.Now().Add(time.Hour)
		}
		registered.ExpiresAt = jwt.NewNumericDate(expiry)
	}
	return struct {
		jwt.RegisteredClaims
		Scope string `json:"scope,omitempty"`
	}{RegisteredClaims: registered, Scope: c.scope}
}

func cmp(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func mint(t *testing.T, c claims, key *rsa.PrivateKey) auth.Token {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, c.build()).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return auth.Token(signed)
}

// Task 6.3: a token is accepted only when its signature, expiry, issuer and
// audience all hold.
func TestVerifyAcceptsAGoodToken(t *testing.T) {
	t.Parallel()
	own, _, _ := keys(t)
	got, err := verifier(t).Verify(t.Context(), mint(t, claims{scope: "mcp:access mcp:config:define"}, own))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject.Issuer != issuer || got.Subject.Sub != "alice" {
		t.Errorf("subject = %+v, want issuer %q and sub %q", got.Subject, issuer, "alice")
	}
	if !got.HasScope("mcp:config:define") {
		t.Errorf("scopes = %v, want the scope claim split on spaces", got.Scopes)
	}
}

// Task 6.3 and 6.6: every way a token can fail is a refusal.
func TestVerifyRejects(t *testing.T) {
	t.Parallel()
	own, _, unrelated := keys(t)
	tests := []struct {
		name  string
		token func(*testing.T) auth.Token
		want  error
	}{
		{
			name:  "signed with a key the issuer does not use",
			token: func(t *testing.T) auth.Token { return mint(t, claims{}, unrelated) },
			want:  auth.ErrInvalidToken,
		},
		{
			name:  "expired",
			token: func(t *testing.T) auth.Token { return mint(t, claims{expiry: time.Now().Add(-time.Minute)}, own) },
			want:  auth.ErrInvalidToken,
		},
		{
			name:  "no expiry at all",
			token: func(t *testing.T) auth.Token { return mint(t, claims{omitExp: true}, own) },
			want:  auth.ErrInvalidToken,
		},
		{
			name:  "meant for another resource",
			token: func(t *testing.T) auth.Token { return mint(t, claims{audience: "https://elsewhere.example/"}, own) },
			want:  auth.ErrWrongAudience,
		},
		{
			// Without this a token issued for any other service of the same
			// issuer would be accepted here. The library reports it as a
			// required claim that is missing rather than as a mismatch; what
			// matters is that it is refused.
			name:  "naming no audience",
			token: func(t *testing.T) auth.Token { return mint(t, claims{omitAud: true}, own) },
			want:  auth.ErrInvalidToken,
		},
		{
			name:  "from an issuer the gateway does not trust",
			token: func(t *testing.T) auth.Token { return mint(t, claims{issuer: "https://stranger.example/"}, own) },
			want:  auth.ErrUnknownIssuer,
		},
		{
			// Whitespace is not an identity, but it is not the empty string
			// either, so it would otherwise reach the pool as a key.
			name:  "identifying nobody",
			token: func(t *testing.T) auth.Token { return mint(t, claims{subject: " "}, own) },
			want:  auth.ErrNoSubject,
		},
		{
			name:  "empty",
			token: func(*testing.T) auth.Token { return "" },
			want:  auth.ErrNoToken,
		},
		{
			name:  "not a token at all",
			token: func(*testing.T) auth.Token { return "not.a.jwt" },
			want:  auth.ErrInvalidToken,
		},
		{
			// The classic confusion attack: sign with the issuer's published
			// public key as an HMAC secret and hope the algorithm is trusted.
			// What refuses it today is the key type — the HMAC verifier will
			// not take an RSA key — with the algorithm allowlist behind it.
			name: "signed with HMAC over the issuer's public key",
			token: func(t *testing.T) auth.Token {
				pub, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{}.build()).
					SignedString([]byte("public-key-bytes"))
				if err != nil {
					t.Fatalf("sign token: %v", err)
				}
				return auth.Token(pub)
			},
			want: auth.ErrInvalidToken,
		},
		{
			// Refused by the library, which will not verify the none method
			// without an explicit unsafe opt-in. The algorithm allowlist would
			// refuse it too.
			name: "unsigned",
			token: func(t *testing.T) auth.Token {
				unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims{}.build()).
					SignedString(jwt.UnsafeAllowNoneSignatureType)
				if err != nil {
					t.Fatalf("sign token: %v", err)
				}
				return auth.Token(unsigned)
			},
			want: auth.ErrInvalidToken,
		},
	}
	v := verifier(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := v.Verify(t.Context(), tt.token(t))
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want it to wrap %v", err, tt.want)
			}
		})
	}
}

// Task 6.5: `sub` is unique only within an issuer, so the identity is the pair.
func TestSubjectsFromDifferentIssuersAreDistinct(t *testing.T) {
	t.Parallel()
	own, other, _ := keys(t)
	v := verifier(t)

	first, err := v.Verify(t.Context(), mint(t, claims{subject: "1234"}, own))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	second, err := v.Verify(t.Context(), mint(t, claims{issuer: otherIssuer, subject: "1234"}, other))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if first.Subject == second.Subject {
		t.Fatalf("two issuers sharing sub %q produced the same subject %+v", "1234", first.Subject)
	}
	// The pool keys on this string, so it is what must not collide.
	if first.Subject.Key() == second.Subject.Key() {
		t.Errorf("pool keys collide: %q", first.Subject.Key())
	}
}

// A separator that can appear in an issuer or a subject would let two distinct
// subjects share a process, and with it that process's credentials.
func TestSubjectKeysCannotCollide(t *testing.T) {
	t.Parallel()
	a := auth.Subject{Issuer: "https://x.example/a", Sub: "b"}
	b := auth.Subject{Issuer: "https://x.example/a/b", Sub: ""}
	if a.Key() == b.Key() {
		t.Errorf("keys collide: %q", a.Key())
	}
	if b.Valid() {
		t.Error("a subject with no sub reports itself valid")
	}
}

// The token is redacted by its type, wherever it is printed.
func TestTokenIsRedacted(t *testing.T) {
	t.Parallel()
	// G101: a made-up token shape, not a credential.
	const secret = "eyJhbGciOiJSUzI1NiJ9.secret.payload" //nolint:gosec // see above
	token := auth.Token(secret)
	for _, rendered := range []string{
		token.String(),
		token.GoString(),
		token.LogValue().String(),
		errors.New(token.String()).Error(),
	} {
		if strings.Contains(rendered, "secret") {
			t.Errorf("token leaked: %s", rendered)
		}
	}
	if token.Reveal() != secret {
		t.Error("Reveal did not return the token")
	}
}

var _ = context.Background

package auth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// signingMethods are the algorithms the gateway accepts: asymmetric only.
//
// This is defence in depth rather than the only defence. A token signed with
// HMAC over the issuer's published public key — the classic confusion attack —
// is refused today because [KeySource] hands back an asymmetric key and the
// HMAC verifier rejects a key of that type, and an unsigned token is refused by
// the library itself. Both of those depend on choices elsewhere. This list does
// not: it is the check that keeps holding if a key source is ever added that
// returns raw bytes.
var signingMethods = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

type Verifier struct {
	keys     KeySource
	issuers  []string
	audience string
	parser   *jwt.Parser
}

func NewVerifier(keys KeySource, issuers []string, resource string) (*Verifier, error) {
	canonical, err := CanonicalResource(resource)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, errors.New("token verifier: no key source")
	}
	if len(issuers) == 0 {
		return nil, fmt.Errorf("token verifier for %q: no trusted issuers", canonical)
	}
	return &Verifier{
		keys:     keys,
		issuers:  slices.Clone(issuers),
		audience: canonical,
		parser: jwt.NewParser(
			jwt.WithValidMethods(signingMethods),
			// A token that names no audience is not a token for this
			// resource. Without this, one issued for some other service of the
			// same issuer would be accepted here.
			jwt.WithAudience(canonical),
			jwt.WithIssuedAt(),
			jwt.WithExpirationRequired(),
		),
	}, nil
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Scope string `json:"scope,omitempty"`
}

// Verify's error tells the caller why for the sake of the log; the client is told
// only that it was refused, since naming the failed check would help whoever is
// probing more than it helps a legitimate client.
func (v *Verifier) Verify(ctx context.Context, token Token) (Claims, error) {
	if token == "" {
		return Claims{}, ErrNoToken
	}
	var claims tokenClaims
	_, err := v.parser.ParseWithClaims(token.Reveal(), &claims, v.keyFor(ctx))
	if err != nil {
		return Claims{}, classify(err)
	}
	// The issuer decides which key verified the signature, so it is checked
	// while resolving the key; this re-check keeps the two from drifting apart
	// if the key source is ever made permissive.
	if !slices.Contains(v.issuers, claims.Issuer) {
		return Claims{}, fmt.Errorf("issuer %q: %w", claims.Issuer, ErrUnknownIssuer)
	}
	// Trimmed, because a subject of whitespace identifies nobody while still
	// being non-empty — and it would key a pool entry all the same.
	subject := Subject{Issuer: strings.TrimSpace(claims.Issuer), Sub: strings.TrimSpace(claims.Subject)}
	if !subject.Valid() {
		return Claims{}, ErrNoSubject
	}
	return Claims{Subject: subject, Scopes: parseScopes(claims.Scope)}, nil
}

// keyFor refuses an untrusted issuer before any key is looked up.
func (v *Verifier) keyFor(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		issuer, err := token.Claims.GetIssuer()
		if err != nil {
			return nil, fmt.Errorf("read issuer: %w", ErrUnknownIssuer)
		}
		if !slices.Contains(v.issuers, issuer) {
			return nil, fmt.Errorf("issuer %q: %w", issuer, ErrUnknownIssuer)
		}
		kid, _ := token.Header["kid"].(string)
		key, err := v.keys.Key(ctx, issuer, kid)
		if err != nil {
			return nil, fmt.Errorf("key %q of issuer %q: %w", kid, issuer, err)
		}
		return key, nil
	}
}

func classify(err error) error {
	switch {
	case errors.Is(err, ErrUnknownIssuer):
		return fmt.Errorf("%w: %w", ErrUnknownIssuer, err)
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return fmt.Errorf("%w: %w", ErrWrongAudience, err)
	default:
		return fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
}

// StaticKeys holds keys a deployment configured directly. Fetching an issuer's
// JWKS document and following key rotation is the other implementation of
// [KeySource].
type StaticKeys struct {
	// byIssuer is indexed by issuer and then by key id, where the empty key id
	// holds the key for tokens that name none.
	byIssuer map[string]map[string]crypto.PublicKey
}

func NewStaticKeys(byIssuer map[string]map[string]crypto.PublicKey) *StaticKeys {
	return &StaticKeys{byIssuer: byIssuer}
}

var _ KeySource = (*StaticKeys)(nil)

func (s *StaticKeys) Key(_ context.Context, issuer, kid string) (crypto.PublicKey, error) {
	keys, ok := s.byIssuer[issuer]
	if !ok {
		return nil, fmt.Errorf("issuer %q: %w", issuer, ErrUnknownIssuer)
	}
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	// A deployment with one key per issuer need not name it, and a token from
	// such an issuer need not carry a kid.
	if key, ok := keys[""]; ok && len(keys) == 1 {
		return key, nil
	}
	return nil, fmt.Errorf("issuer %q has no key %q: %w", issuer, kid, ErrInvalidToken)
}

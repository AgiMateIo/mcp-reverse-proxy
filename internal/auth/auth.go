// Package auth implements the gateway's side of OAuth 2.1: it publishes
// protected resource metadata, validates the tokens clients present, and
// derives the subject that the process pool is keyed by.
//
// The gateway is a resource server and nothing more. It issues no tokens and
// runs no authorization server; both are somebody else's deployment.
package auth

import (
	"context"
	"crypto"
	"errors"
	"log/slog"
	"strings"
)

// Reasons a request is refused. Callers tell them apart to decide what to log;
// none of them changes what the client is told, because saying which check
// failed would help an attacker more than a client.
var (
	ErrNoToken       = errors.New("no bearer token")
	ErrTokenInQuery  = errors.New("bearer token in the query string")
	ErrUnknownIssuer = errors.New("unknown token issuer")
	ErrInvalidToken  = errors.New("invalid token")
	ErrWrongAudience = errors.New("token audience does not match this resource")
	ErrNoSubject     = errors.New("token carries no subject")
)

// A Token is redacted by its type rather than by the care of whoever handles
// it. The methods below cover every way it would otherwise be printed —
// including %#v, the one verb that ignores [fmt.Stringer] — so the value is
// reachable only through [Token.Reveal], and every call site of that is a place
// where the token leaves the type's protection.
type Token string

const redacted = "[REDACTED]"

func (Token) LogValue() slog.Value { return slog.StringValue(redacted) }
func (Token) String() string       { return redacted }
func (Token) GoString() string     { return `"` + redacted + `"` }

func (t Token) Reveal() string { return string(t) }

var _ slog.LogValuer = Token("")

// A Subject is a token's issuer and subject together, never the subject alone.
//
// `sub` is unique only within one issuer, and the resource metadata may name
// several. On `sub` alone, two people from different authorization servers who
// happened to share an identifier would share a child process, and with it that
// process's environment — which is where a subject's own credentials live.
type Subject struct {
	Issuer string
	Sub    string
}

// Key joins the two parts with a byte that cannot occur in either, so no pair
// of distinct subjects can produce the same pool key. A printable separator
// would not do: an issuer ending in "/" and a subject beginning with one would
// collide.
func (s Subject) Key() string { return s.Issuer + "\x00" + s.Sub }

// String is for logs, where a subject identifier is not a secret but the
// separator of [Subject.Key] has no business appearing.
func (s Subject) String() string { return s.Issuer + " " + s.Sub }

func (s Subject) Valid() bool { return s.Issuer != "" && s.Sub != "" }

type Claims struct {
	Subject Subject
	// Scopes decide what the x-mcp-config header may do for this subject.
	Scopes []string
}

func (c Claims) HasScope(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// A KeySource supplies the public keys of the authorization servers the gateway
// trusts.
//
// It is an interface because where keys come from is a deployment's choice:
// configured directly today, fetched from an issuer's JWKS endpoint and
// refreshed on rotation later. Nothing above this interface changes when that
// happens.
type KeySource interface {
	// Key returns the public key an issuer signs with. kid may be empty when
	// a token names no key.
	Key(ctx context.Context, issuer, kid string) (crypto.PublicKey, error)
}

type subjectKey struct{}

func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, s)
}

// SubjectFromContext reports false for a request that was never authenticated.
func SubjectFromContext(ctx context.Context) (Subject, bool) {
	s, ok := ctx.Value(subjectKey{}).(Subject)
	return s, ok
}

// parseScopes splits the space-delimited form OAuth uses for the claim.
func parseScopes(scope string) []string {
	if scope == "" {
		return nil
	}
	return strings.Fields(scope)
}

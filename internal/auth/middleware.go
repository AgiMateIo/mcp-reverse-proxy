package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// queryTokenParams are the query string parameters a client might try to put a
// token in. Accepting one would leak the token into access logs, browser
// history and Referer headers, so the gateway refuses the request outright
// rather than quietly ignoring the parameter and asking for a header.
var queryTokenParams = []string{"access_token", "token", "bearer_token"}

// Require admits only requests carrying a valid bearer token, and puts the
// subject it identifies in the request context for the layers below.
func Require(v *Verifier, metadataURL string, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := bearer(r)
			if err == nil {
				var claims Claims
				claims, err = v.Verify(r.Context(), token)
				if err == nil {
					next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), claims.Subject)))
					return
				}
			}
			// The reason is recorded; the client is only told to authenticate.
			// Naming the failed check would tell whoever is probing which part
			// of their token to fix.
			logger.Info("refusing an unauthenticated request",
				"reason", err, "path", r.URL.Path, "method", r.Method)
			Unauthorized(w, metadataURL, ScopeAccess)
		})
	}
}

func bearer(r *http.Request) (Token, error) {
	for _, param := range queryTokenParams {
		if r.URL.Query().Has(param) {
			return "", fmt.Errorf("parameter %q: %w", param, ErrTokenInQuery)
		}
	}
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", ErrNoToken
	}
	value, ok := cutBearer(header)
	if !ok {
		return "", fmt.Errorf("the Authorization header is not a Bearer credential: %w", ErrNoToken)
	}
	return Token(value), nil
}

// cutBearer matches the scheme case-insensitively, per RFC 7235.
func cutBearer(header string) (string, bool) {
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

// Unauthorized points the client at the metadata document that says where to
// get a token.
func Unauthorized(w http.ResponseWriter, metadataURL string, scopes ...string) {
	w.Header().Set("WWW-Authenticate", challenge("", metadataURL, scopes))
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// InsufficientScope names every missing scope in one challenge. Naming them one at a time would
// make a client walk through a separate authorization round trip per scope,
// which is both slow and a worse consent experience: the person approving it
// would be asked repeatedly instead of once.
func InsufficientScope(w http.ResponseWriter, metadataURL string, scopes ...string) {
	w.Header().Set("WWW-Authenticate", challenge("insufficient_scope", metadataURL, scopes))
	http.Error(w, "Forbidden", http.StatusForbidden)
}

func challenge(errCode, metadataURL string, scopes []string) string {
	params := []string{}
	if errCode != "" {
		params = append(params, fmt.Sprintf("error=%q", errCode))
	}
	if metadataURL != "" {
		params = append(params, fmt.Sprintf("resource_metadata=%q", metadataURL))
	}
	if len(scopes) > 0 {
		params = append(params, fmt.Sprintf("scope=%q", strings.Join(scopes, " ")))
	}
	return "Bearer " + strings.Join(params, ", ")
}

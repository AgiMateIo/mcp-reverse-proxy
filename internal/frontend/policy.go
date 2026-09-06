package frontend

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/policy"
)

// Resolve returns middleware that turns the base configuration and the
// x-mcp-config header into the server set for one request, refusing the request
// when the header asks for more than the subject may have.
//
// It runs after authentication, because what the header may do depends on the
// token's scopes, and before anything reaches a backend, because the server set
// is what decides which process serves the request.
func Resolve(base *config.File, p *policy.Policy, verify Claims, metadataURL string, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrapped the moment it is read: from here on the value cannot be
			// printed, logged or marshalled by accident.
			raw := config.Secret(r.Header.Get(config.HeaderName))

			var header *config.Header
			if raw != "" {
				parsed, err := config.ParseHeader(raw, base.Limits.MaxHeaderBytes)
				if err != nil {
					// The message names the limit or the expected shape; the
					// value it came from goes nowhere, here or in the log.
					logger.Info("refusing a request over its configuration header",
						"reason", err, config.HeaderName, raw)
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				header = parsed
			}

			// Whether the header may be applied is settled before it is
			// applied: a deployment that refuses headers should say so,
			// rather than report on the merge it never meant to perform.
			err := p.CheckHeader(verify(r), base, header)
			var resolved []config.Server
			if err == nil {
				resolved, err = config.Resolve(base, header)
			}
			if err == nil {
				err = p.CheckResolved(resolved, header)
			}
			if err != nil {
				logger.Info("refusing a request over its configuration header",
					"reason", err, config.HeaderName, raw)
				refuse(w, err, metadataURL)
				return
			}
			next.ServeHTTP(w, r.WithContext(config.WithResolved(r.Context(), resolved)))
		})
	}
}

// Claims reports what the authenticated subject's token carries. It is a
// function so that the middleware need not know how the request was
// authenticated.
type Claims func(*http.Request) auth.Claims

// refuse maps a policy verdict onto an answer.
//
// The distinction that matters is whether the client can do anything about it.
// A missing scope it can go and ask for, so that answer carries a challenge
// naming every scope at once. A deployment that has header configuration off,
// or set below what the request wants, is not something any token can change,
// so those get a plain refusal — a challenge there would send the client round
// an authorization trip that could not possibly help.
func refuse(w http.ResponseWriter, err error, metadataURL string) {
	var scopes *policy.ScopeError
	switch {
	case errors.As(err, &scopes):
		auth.InsufficientScope(w, metadataURL, scopes.Missing...)
	case errors.Is(err, policy.ErrPolicyViolation):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		// A header the deployment would allow but that does not describe a
		// usable server set is the client's mistake to fix.
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

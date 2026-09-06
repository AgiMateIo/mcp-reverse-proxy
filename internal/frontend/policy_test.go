package frontend_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/policy"
)

// G101: a made-up value standing in for a credential, so a test can prove it
// never escapes.
const headerSecret = "sk-live-do-not-log-me" //nolint:gosec // see above

// A recorder collects log output without racing the goroutines writing it.
type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// resolving builds the middleware over a one-server base configuration, and
// returns the handler, the log it writes to, and the set each served request
// resolved to.
func resolving(t *testing.T, mode string, scopes []string) (http.Handler, *recorder, *[]config.Server) {
	t.Helper()
	base := &config.File{
		Servers: []config.Server{{
			ID: "files", Command: "/usr/bin/mcp-files",
			Env: config.Env{"FS_TOKEN": "base-token"}, Era: config.EraAuto,
		}},
		Limits: config.Limits{MaxHeaderBytes: 256, MaxProcesses: 4, MaxProcessesPerSubject: 2},
	}
	p, err := policy.New(config.PolicyConfig{
		Mode:             mode,
		CommandAllowlist: []string{"/usr/bin/mcp-files", "/usr/bin/true"},
		EnvDenylist:      []string{"LD_PRELOAD"},
	})
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}

	logs := &recorder{}
	var served []config.Server
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved, ok := config.ResolvedFromContext(r.Context())
		if !ok {
			t.Error("a request reached the handler with no resolved configuration")
		}
		served = resolved
		w.WriteHeader(http.StatusOK)
	})
	claims := func(*http.Request) auth.Claims { return auth.Claims{Scopes: scopes} }
	handler := frontend.Resolve(base, p, claims, "https://gateway.example/.well-known/oauth-protected-resource",
		slog.New(slog.NewTextHandler(logs, nil)))(next)
	return handler, logs, &served
}

func withHeader(t *testing.T, h http.Handler, headerValue string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://gateway.example/mcp", nil)
	if headerValue != "" {
		req.Header.Set(config.HeaderName, headerValue)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Task 8.1: a header that is not configuration is refused, and the refusal does
// not repeat what it refused.
func TestMalformedHeaderIsRefused(t *testing.T) {
	t.Parallel()
	h, logs, _ := resolving(t, "env-only", []string{policy.ScopeEnv})
	rec := withHeader(t, h, `{"servers":[{"id":"files","env":{"FS_TOKEN":"`+headerSecret)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if strings.Contains(rec.Body.String(), headerSecret) {
		t.Errorf("the response echoed the header: %s", rec.Body)
	}
	// Task 8.9, on the same rejection.
	if strings.Contains(logs.String(), headerSecret) {
		t.Errorf("the header reached the log: %s", logs)
	}
	if !strings.Contains(logs.String(), "REDACTED") {
		t.Errorf("the log does not record that a header was present: %s", logs)
	}
}

// Task 8.2: an oversized header is refused with the limit named, since that is
// the one thing the client can act on.
func TestOversizedHeaderIsRefused(t *testing.T) {
	t.Parallel()
	h, logs, _ := resolving(t, "env-only", []string{policy.ScopeEnv})
	rec := withHeader(t, h, `{"servers":[{"id":"files","env":{"FS_TOKEN":"`+strings.Repeat(headerSecret, 50)+`"}}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "256") {
		t.Errorf("the refusal does not name the limit: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), headerSecret) || strings.Contains(logs.String(), headerSecret) {
		t.Error("the oversized header was repeated back or logged")
	}
}

// Task 8.5: with header configuration off, a header is an error and its absence
// is not.
func TestModeOffOverHTTP(t *testing.T) {
	t.Parallel()
	h, _, served := resolving(t, "off", nil)

	refused := withHeader(t, h, `{"servers":[{"id":"files","env":{"FS_TOKEN":"`+headerSecret+`"}}]}`)
	if refused.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", refused.Code, http.StatusForbidden)
	}
	if got := refused.Header().Get("WWW-Authenticate"); strings.Contains(got, "insufficient_scope") {
		t.Errorf("the refusal invited a step-up no token could satisfy: %s", got)
	}
	if !strings.Contains(refused.Body.String(), "disabled") {
		t.Errorf("the refusal does not say plainly that header configuration is off: %s", refused.Body)
	}

	ordinary := withHeader(t, h, "")
	if ordinary.Code != http.StatusOK {
		t.Fatalf("a request without a header got %d, want %d", ordinary.Code, http.StatusOK)
	}
	if len(*served) != 1 || (*served)[0].ID != "files" {
		t.Errorf("resolved %+v, want the base configuration", *served)
	}
}

// Task 8.6: a scope the deployment would honour is worth asking for, and the
// challenge names every one the request needs at once.
func TestMissingScopeChallenge(t *testing.T) {
	t.Parallel()
	h, _, _ := resolving(t, "define-new", nil)
	rec := withHeader(t, h, `{"servers":[{"id":"scratch","command":"/usr/bin/true"},{"id":"files","env":{"FS_TOKEN":"`+headerSecret+`"}}]}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	got := rec.Header().Get("WWW-Authenticate")
	for _, want := range []string{`error="insufficient_scope"`, policy.ScopeDefine, policy.ScopeEnv, "resource_metadata="} {
		if !strings.Contains(got, want) {
			t.Errorf("challenge does not carry %q: %s", want, got)
		}
	}
	// One challenge, not one per round trip: a client should learn everything
	// it needs from a single answer.
	if strings.Count(got, "scope=") != 1 {
		t.Errorf("challenge names scope more than once: %s", got)
	}
}

// A header the policy permits reaches the handler as a resolved server set.
func TestPermittedHeaderResolves(t *testing.T) {
	t.Parallel()
	h, _, served := resolving(t, "define-new", []string{policy.ScopeEnv, policy.ScopeOverride, policy.ScopeDefine})
	rec := withHeader(t, h, `{"servers":[{"id":"scratch","command":"/usr/bin/true"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(*served) != 2 {
		t.Fatalf("resolved %d servers, want the base one and the declared one", len(*served))
	}
	if (*served)[1].ID != "scratch" {
		t.Errorf("resolved %+v, want the declared server added", *served)
	}
}

// Task 8.7 over HTTP: the allowlist holds under the most permissive mode.
func TestAllowlistHoldsUnderDefineNew(t *testing.T) {
	t.Parallel()
	h, logs, _ := resolving(t, "define-new", []string{policy.ScopeEnv, policy.ScopeOverride, policy.ScopeDefine})
	rec := withHeader(t, h, `{"servers":[{"id":"scratch","command":"/bin/sh"}]}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "/bin/sh") {
		t.Errorf("the refusal does not name the command: %s", rec.Body)
	}
	if strings.Contains(logs.String(), headerSecret) {
		t.Errorf("the header reached the log: %s", logs)
	}
}

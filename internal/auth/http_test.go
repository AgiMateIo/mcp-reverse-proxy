package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
)

// Task 6.1: the metadata document is how a client learns where to get a token.
func TestProtectedResourceMetadata(t *testing.T) {
	t.Parallel()
	// A fragment is never sent to a server and is not part of what identifies
	// the resource, so it must not survive into the canonical form.
	m, err := auth.NewMetadata(resource+"#section", []string{issuer}, nil)
	if err != nil {
		t.Fatalf("NewMetadata: %v", err)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, auth.MetadataPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var doc struct {
		Resource               string   `json:"resource"`
		AuthorizationServers   []string `json:"authorization_servers"`
		ScopesSupported        []string `json:"scopes_supported"`
		BearerMethodsSupported []string `json:"bearer_methods_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode metadata %s: %v", rec.Body, err)
	}
	if doc.Resource != resource {
		t.Errorf("resource = %q, want %q with the fragment dropped", doc.Resource, resource)
	}
	if strings.Contains(doc.Resource, "#") {
		t.Errorf("resource carries a fragment: %q", doc.Resource)
	}
	if !slices.Equal(doc.AuthorizationServers, []string{issuer}) {
		t.Errorf("authorization_servers = %v, want %v", doc.AuthorizationServers, []string{issuer})
	}
	if !slices.Contains(doc.ScopesSupported, auth.ScopeAccess) {
		t.Errorf("scopes_supported = %v, want it to hold the base access scope", doc.ScopesSupported)
	}
	// A refresh token is of no use to a resource server, and inviting a client
	// to ask for one widens the grant for nothing.
	if slices.Contains(doc.ScopesSupported, "offline_access") {
		t.Errorf("scopes_supported offers offline_access: %v", doc.ScopesSupported)
	}
	if !slices.Equal(doc.BearerMethodsSupported, []string{"header"}) {
		t.Errorf("bearer_methods_supported = %v, want only the header", doc.BearerMethodsSupported)
	}
}

func TestMetadataRejectsAnUnusableResource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		resource string
		issuers  []string
		scopes   []string
	}{
		{"relative resource", "/mcp", []string{issuer}, nil},
		{"no authorization servers", resource, nil, nil},
		{"offline_access offered", resource, []string{issuer}, []string{auth.ScopeAccess, "offline_access"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := auth.NewMetadata(tt.resource, tt.issuers, tt.scopes); err == nil {
				t.Error("NewMetadata succeeded, want an error")
			}
		})
	}
}

func TestMetadataURL(t *testing.T) {
	t.Parallel()
	got, err := auth.MetadataURL(resource)
	if err != nil {
		t.Fatalf("MetadataURL: %v", err)
	}
	if want := "https://gateway.example" + auth.MetadataPath + "/mcp"; got != want {
		t.Errorf("MetadataURL = %q, want %q", got, want)
	}
}

// guarded reports the subject the middleware admitted, so a test can tell
// "let through" from "let through as the right person".
func guarded(t *testing.T) http.Handler {
	t.Helper()
	metadataURL, err := auth.MetadataURL(resource)
	if err != nil {
		t.Fatalf("MetadataURL: %v", err)
	}
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, ok := auth.SubjectFromContext(r.Context())
		if !ok {
			t.Error("a request reached the handler without a subject")
		}
		_, _ = w.Write([]byte(subject.Key()))
	})
	return auth.Require(verifier(t), metadataURL, nil)(served)
}

// Task 6.2: a request without a token is refused with everything the client
// needs to go and get one.
func TestUnauthenticatedRequestIsChallenged(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	guarded(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://gateway.example/mcp", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	got := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("challenge = %q, want the Bearer scheme", got)
	}
	for _, want := range []string{`resource_metadata="`, auth.MetadataPath, `scope="`, auth.ScopeAccess} {
		if !strings.Contains(got, want) {
			t.Errorf("challenge does not carry %q: %s", want, got)
		}
	}
}

// Task 6.4: a token in the query string is refused, even when the same token
// would be accepted in the header. Accepting one would put it in access logs,
// browser history and Referer headers.
func TestTokenInTheQueryStringIsRefused(t *testing.T) {
	t.Parallel()
	own, _, _ := keys(t)
	token := mint(t, claims{}, own)

	// The same token in the header is accepted, so the refusal is about where
	// it was presented and not about the token.
	accepted := httptest.NewRecorder()
	authorized := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://gateway.example/mcp", nil)
	authorized.Header.Set("Authorization", "Bearer "+token.Reveal())
	guarded(t).ServeHTTP(accepted, authorized)
	if accepted.Code != http.StatusOK {
		t.Fatalf("the token was not accepted in the header: status %d", accepted.Code)
	}

	for _, param := range []string{"access_token", "token", "bearer_token"} {
		t.Run(param, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"https://gateway.example/mcp?"+param+"="+token.Reveal(), nil)
			guarded(t).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if rec.Body.Len() > 0 && strings.Contains(rec.Body.String(), token.Reveal()) {
				t.Errorf("the response echoed the token: %s", rec.Body)
			}
		})
	}
}

// Task 6.2 and 6.3, over HTTP: every rejection path is a 401, and a good token
// gets through carrying its subject.
func TestMiddleware(t *testing.T) {
	t.Parallel()
	own, _, unrelated := keys(t)
	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"not a bearer credential", "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		{"bearer with no value", "Bearer ", http.StatusUnauthorized},
		{"a token signed by a stranger", "Bearer " + mint(t, claims{}, unrelated).Reveal(), http.StatusUnauthorized},
		{"a good token", "Bearer " + mint(t, claims{}, own).Reveal(), http.StatusOK},
		// The scheme is case-insensitive per RFC 7235.
		{"a good token, lowercase scheme", "bearer " + mint(t, claims{}, own).Reveal(), http.StatusOK},
	}
	handler := guarded(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://gateway.example/mcp", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK && rec.Body.String() == "" {
				t.Error("the handler saw no subject")
			}
		})
	}
}

// Missing scopes are named all at once: challenging for them one at a time
// would make a client walk through a separate authorization round trip per
// scope, and ask the person approving it repeatedly instead of once.
func TestInsufficientScopeNamesEveryMissingScope(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	auth.InsufficientScope(rec, "https://gateway.example"+auth.MetadataPath,
		"mcp:config:override", "mcp:config:define")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	got := rec.Header().Get("WWW-Authenticate")
	for _, want := range []string{`error="insufficient_scope"`, "mcp:config:override", "mcp:config:define", "resource_metadata="} {
		if !strings.Contains(got, want) {
			t.Errorf("challenge does not carry %q: %s", want, got)
		}
	}
	if strings.Count(got, "scope=") != 1 {
		t.Errorf("challenge names scope more than once: %s", got)
	}
}

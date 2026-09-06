package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// MetadataPath is where RFC 9728 says a resource server publishes its metadata.
const MetadataPath = "/.well-known/oauth-protected-resource"

// ScopeAccess is the base scope. Scopes that widen what the x-mcp-config header
// may do are separate, and are checked by the policy layer rather than here.
const ScopeAccess = "mcp:access"

// Metadata is the OAuth 2.0 Protected Resource Metadata document of RFC 9728.
type Metadata struct {
	// Resource is also the audience every accepted token must name.
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`

	body []byte
}

// NewMetadata refuses a resource URI that cannot be a canonical identifier,
// because publishing a wrong one would make every token audience check
// meaningless.
func NewMetadata(resource string, issuers, scopes []string) (*Metadata, error) {
	canonical, err := CanonicalResource(resource)
	if err != nil {
		return nil, err
	}
	if len(issuers) == 0 {
		return nil, fmt.Errorf("protected resource metadata for %q: no authorization servers", canonical)
	}
	if len(scopes) == 0 {
		scopes = []string{ScopeAccess}
	}
	// offline_access asks for a refresh token, which a resource server has no
	// use for and should never invite a client to request.
	if slices.Contains(scopes, "offline_access") {
		return nil, fmt.Errorf("protected resource metadata for %q: offline_access is not a resource scope", canonical)
	}
	m := &Metadata{
		Resource:               canonical,
		AuthorizationServers:   slices.Clone(issuers),
		ScopesSupported:        slices.Clone(scopes),
		BearerMethodsSupported: []string{"header"},
	}
	// Encoded once, here: a document that cannot be encoded is a startup
	// failure, not something to discover on every request that asks for it.
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("protected resource metadata for %q: %w", canonical, err)
	}
	m.body = body
	return m, nil
}

// CanonicalResource strips the fragment from an absolute URI rather than
// rejecting it, because it is never part of what
// identifies a resource to a server — it is not even sent in a request — while
// a token whose audience carried one would fail to match for a reason nobody
// could see.
func CanonicalResource(resource string) (string, error) {
	u, err := url.Parse(resource)
	if err != nil {
		return "", fmt.Errorf("resource identifier %q: %w", resource, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("resource identifier %q: want an absolute URI", resource)
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

func (m *Metadata) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(m.body)
	})
}

// MetadataURL is where a client finds this document. RFC 9728 puts the
// well-known path ahead of the resource's own path rather than after it.
func MetadataURL(resource string) (string, error) {
	canonical, err := CanonicalResource(resource)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(canonical)
	if err != nil {
		return "", fmt.Errorf("resource identifier %q: %w", canonical, err)
	}
	u.Path = MetadataPath + u.Path
	return u.String(), nil
}

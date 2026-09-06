package frontend_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://issuer.example/"
	testResource = "https://gateway.example/mcp"
)

// Task 6.7: the token a client presents is the gateway's business and nobody
// else's. A backend that received it could use it against any other resource
// the subject can reach.
func TestAccessTokenNeverReachesTheBackend(t *testing.T) {
	t.Parallel()
	dump := filepath.Join(t.TempDir(), "environment")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	token := mintToken(t, key)

	url := serveGuarded(t, key, testfixtures.Options{EnvDumpFile: dump})

	// An unauthenticated request is refused, which is what shows the guard is
	// actually in the path rather than merely present.
	if res := get(t, url, ""); res.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", res.status, http.StatusUnauthorized)
	}

	// An authenticated one reaches the backend, which is what causes a process
	// to be started in the first place.
	res := postAuthorized(t, url, "tools/list", token)
	if res.status != http.StatusOK {
		t.Fatalf("authenticated status = %d: %s", res.status, res.body)
	}

	recorded := waitForFile(t, dump)
	if strings.Contains(recorded, token) {
		t.Errorf("the access token reached the backend process:\n%s", recorded)
	}
	// The fixture's own configured environment did arrive, so the absence
	// above is not the absence of an environment.
	if !strings.Contains(recorded, testfixtures.EnvMode) {
		t.Errorf("the backend did not receive its configured environment:\n%s", recorded)
	}
	for _, part := range []string{"Authorization", "Bearer"} {
		if strings.Contains(recorded, part) {
			t.Errorf("the backend received %q from the request:\n%s", part, recorded)
		}
	}
}

func serveGuarded(t *testing.T, key *rsa.PrivateKey, opts testfixtures.Options) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	env := config.Env{}
	for k, v := range testfixtures.Env(testfixtures.ModeModern, opts) {
		env[k] = config.Secret(v)
	}
	server := config.Server{ID: "guarded", Command: self, Env: env, Era: config.EraAuto}
	b := pool.NewBackend(server, backend.NewConnector("test", probeTimeout, nil), pool.DefaultStopPolicy, nil)
	t.Cleanup(func() { _ = b.Close(context.WithoutCancel(t.Context())) })

	verifier, err := auth.NewVerifier(auth.NewStaticKeys(map[string]map[string]crypto.PublicKey{
		testIssuer: {"": key.Public()},
	}), []string{testIssuer}, testResource)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	metadataURL, err := auth.MetadataURL(testResource)
	if err != nil {
		t.Fatalf("MetadataURL: %v", err)
	}
	guard := auth.Require(verifier, metadataURL, nil)
	srv := httptest.NewServer(guard(frontend.NewEndpoint("test", b, nil).Handler()))
	t.Cleanup(srv.Close)
	return srv.URL
}

func mintToken(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   "alice",
		Audience:  jwt.ClaimStrings{testResource},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func get(t *testing.T, url, token string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(t, req)
}

func postAuthorized(t *testing.T, url, method, token string) response {
	t.Helper()
	headers := defaultHeaders()
	headers["Authorization"] = "Bearer " + token
	return post(t, url, method, nil, headers)
}

// waitForFile reads a file the backend writes at startup, which happens on its
// own schedule rather than ours.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	for range 200 {
		// G304: path is this test's own temporary directory.
		data, err := os.ReadFile(path) //nolint:gosec // see above
		if err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the backend never recorded its environment in %s", path)
	return ""
}

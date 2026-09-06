package frontend_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/policy"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A client is one subject talking to the gateway, optionally with a
// configuration header on every request.
type client struct {
	token  string
	header string
}

func (c client) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+c.token)
	if c.header != "" {
		r.Header.Set(config.HeaderName, c.header)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (c client) connect(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: c},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).
		Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect to the gateway: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// Task 11.1: the whole stack over one endpoint — token to subject, header to
// server set, subject and server set to a process — with two subjects and two
// backends of different eras.
func TestEndToEndTwoSubjectsTwoBackendsAndAHeader(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	base := &config.File{
		Servers: []config.Server{
			fixtureServerConfig("gh", self, testfixtures.ModeModern, testfixtures.Options{}),
			fixtureServerConfig("docs", self, testfixtures.ModeLegacy, testfixtures.Options{}),
		},
		Limits: config.Limits{
			MaxProcesses:           8,
			MaxProcessesPerSubject: 4,
			IdleTTL:                config.Duration(time.Minute),
			ProbeTimeout:           config.Duration(probeTimeout),
			MaxHeaderBytes:         4096,
		},
		Policy: config.PolicyConfig{
			Mode:             string(policy.ModeEnvOnly),
			CommandAllowlist: []string{self},
			EnvDenylist:      []string{"LD_PRELOAD"},
		},
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	url, processes := composed(t, base, key)

	alice := client{token: mintSubject(t, key, "alice", policy.ScopeEnv)}
	bob := client{token: mintSubject(t, key, "bob", policy.ScopeEnv)}

	// Both subjects see the same surface: one listing over a modern and a
	// legacy backend, every name under its own namespace.
	want := []string{
		aggregate.Qualify("docs", "echo"),
		aggregate.Qualify("gh", "echo"),
	}
	for name, c := range map[string]client{"alice": alice, "bob": bob} {
		res, err := c.connect(t, url).ListTools(t.Context(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatalf("%s: ListTools: %v", name, err)
		}
		for _, tool := range want {
			if !slices.Contains(toolNames(res), tool) {
				t.Errorf("%s sees tools %v, want it to hold %q", name, toolNames(res), tool)
			}
		}
	}

	// The point of keying the pool by subject: byte-identical configuration,
	// two subjects, and never one process between them.
	for _, server := range base.Servers {
		a, b := processes.PID(t, "alice", server), processes.PID(t, "bob", server)
		if a == 0 || b == 0 {
			t.Fatalf("%q: alice is served by pid %d and bob by pid %d, want a process for each",
				server.ID, a, b)
		}
		if a == b {
			t.Errorf("%q: both subjects are served by pid %d", server.ID, a)
		}
	}

	// A header changes what alice's backend is started with, so it is a
	// different process again — and bob, who sent no header, keeps his.
	dump := filepath.Join(t.TempDir(), "alice.env")
	patched := base.Servers[0]
	patched.Env = config.Env{}
	for k, v := range base.Servers[0].Env {
		patched.Env[k] = v
	}
	patched.Env[testfixtures.EnvEnvDumpFile] = config.Secret(dump)
	withHeaderClient := client{
		token: alice.token,
		header: `{"servers":[{"id":"gh","env":{"` +
			testfixtures.EnvEnvDumpFile + `":"` + dump + `"}}]}`,
	}
	if _, err := withHeaderClient.connect(t, url).ListTools(t.Context(), &mcp.ListToolsParams{}); err != nil {
		t.Fatalf("alice with a configuration header: ListTools: %v", err)
	}

	plain := processes.PID(t, "alice", base.Servers[0])
	overridden := processes.PID(t, "alice", patched)
	if overridden == 0 || overridden == plain {
		t.Errorf("alice's overridden backend runs as pid %d, want one other than %d", overridden, plain)
	}
	if got := processes.PID(t, "bob", base.Servers[0]); got == 0 || got == overridden {
		t.Errorf("bob's backend runs as pid %d after alice's override", got)
	}
	// The override reached the process rather than only the pool's key.
	if recorded := waitForFile(t, dump); !strings.Contains(recorded, dump) {
		t.Errorf("the header's environment did not reach the backend:\n%s", recorded)
	}

	// Five keys, five processes: two subjects times two backends, plus alice's
	// overridden one.
	if got := processes.pool.Stats().Size; got != 5 {
		t.Errorf("the pool holds %d backends, want 5", got)
	}
}

// fixtureServerConfig describes one fixture backend as the gateway configures
// it.
func fixtureServerConfig(id, command string, mode testfixtures.Mode, opts testfixtures.Options) config.Server {
	env := config.Env{}
	for k, v := range testfixtures.Env(mode, opts) {
		env[k] = config.Secret(v)
	}
	return config.Server{ID: id, Command: command, Env: env, Era: config.EraAuto}
}

// A tenants is the test's window into the pool: which process is serving whom.
type tenants struct {
	pool *pool.Pool
}

// PID reports the process serving one subject's server, without starting one.
func (p tenants) PID(t *testing.T, sub string, server config.Server) int {
	t.Helper()
	return p.pool.For(auth.Subject{Issuer: testIssuer, Sub: sub}, server).PID()
}

// composed wires the whole gateway: authentication, then the header policy and
// configuration resolution, then the per-subject surface over the pool.
func composed(t *testing.T, base *config.File, key *rsa.PrivateKey) (string, tenants) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&logs{}, nil))

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
	p, err := policy.New(base.Policy)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	fingerprints, err := config.NewFingerprinter()
	if err != nil {
		t.Fatalf("NewFingerprinter: %v", err)
	}
	// One fingerprinter for the pool, since its key is random per instance and
	// a second one would miss on every request.
	processes := pool.New(
		backend.NewConnector("test", base.Limits.ProbeTimeout.Duration(), logger),
		fingerprints, base.Limits, pool.DefaultStopPolicy, logger)
	t.Cleanup(func() { _ = processes.Close(context.WithoutCancel(t.Context())) })

	// No claims function: authentication runs first and leaves them on the
	// context, which is the whole point of the two middlewares being stacked.
	endpoint := frontend.NewEndpoint("test", frontend.NewTenant(processes, 0, logger), logger)
	handler := auth.Require(verifier, metadataURL, logger)(
		frontend.Resolve(base, p, nil, metadataURL, logger)(endpoint.Handler()))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL, tenants{pool: processes}
}

// mintSubject signs a token for one subject with the scopes it was granted.
func mintSubject(t *testing.T, key *rsa.PrivateKey, sub string, scopes ...string) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   testIssuer,
		"sub":   sub,
		"aud":   testResource,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"scope": strings.Join(append([]string{auth.ScopeAccess}, scopes...), " "),
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// The composed stack must also carry notifications, which is the one thing a
// surface assembled per subject nearly loses: a subscription registered on the
// process serving the request would end with that process, and the pool
// replaces processes routinely.
func TestChangesReachTheSubjectsStreamThroughThePool(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	base := &config.File{
		Servers: []config.Server{
			fixtureServerConfig("gh", self, testfixtures.ModeModern,
				testfixtures.Options{AnnounceChanges: true}),
		},
		Limits: config.Limits{
			MaxProcesses: 4, MaxProcessesPerSubject: 2,
			IdleTTL:      config.Duration(time.Minute),
			ProbeTimeout: config.Duration(probeTimeout),
		},
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	url, _ := composed(t, base, key)
	bearer := map[string]string{"Authorization": "Bearer " + mintSubject(t, key, "alice")}

	s := listenWith(t, url, bearer, "toolsListChanged")
	id := subscriptionID(t, s.next(t))

	changeWith(t, url, bearer, "gh", "tools")
	note := s.next(t)
	if got := method(t, note); got != "notifications/tools/list_changed" {
		t.Fatalf("frame = %q, want the tool list change", got)
	}
	if got := subscriptionID(t, note); got != id {
		t.Errorf("subscriptionId = %v, want the stream's own %v", got, id)
	}
}

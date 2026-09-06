// Package conformance runs the official MCP conformance suite against the
// gateway's front end.
//
// It is a test rather than a command because what it needs is exactly what a
// test already has: a fixture backend, an HTTP server on an ephemeral port, and
// cleanup. It is off by default because it downloads and runs a Node package,
// which no ordinary `go test` should do; `make conformance` turns it on.
package conformance_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
)

// backendID namespaces the backend behind the gateway. Every name the suite
// sees is prefixed with it, which is the gateway's whole point and the reason
// the feature scenarios are expected failures.
const backendID = "everything"

// The suite's own version is pinned so that a run reproduces, and the revision
// is the one the gateway speaks.
const (
	defaultSuiteVersion = "0.2.0-alpha.10"
	specVersion         = "2026-07-28"
)

// Task 11.2: the front end against the conformance suite for revision
// 2026-07-28.
//
// The backend is the SDK's own conformance server, so that what the suite
// exercises reaches a server it was written against and any failure is the
// gateway's rather than the fixture's.
func TestConformance(t *testing.T) {
	if os.Getenv("MCP_CONFORMANCE") == "" {
		t.Skip("set MCP_CONFORMANCE=1, or run `make conformance`, to run the suite")
	}
	url := serveGateway(t)

	suite := os.Getenv("MCP_CONFORMANCE_VERSION")
	if suite == "" {
		suite = defaultSuiteVersion
	}
	expected, err := filepath.Abs("expected-failures.yml")
	if err != nil {
		t.Fatalf("locate the expected failures: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	// G204: every argument is a constant or an environment variable set by
	// whoever runs the suite locally. No request reaches this.
	args := []string{"-y", "@modelcontextprotocol/conformance@" + suite,
		"server",
		"--url", url,
		"--suite", "all",
		"--spec-version", specVersion,
		"--expected-failures", expected,
	}
	// A directory to keep the suite's own per-check output in, which is where
	// the reason for a failure is: the summary only counts them.
	if dir := os.Getenv("MCP_CONFORMANCE_OUTPUT"); dir != "" {
		args = append(args, "--output-dir", dir)
	}
	cmd := exec.CommandContext(ctx, "npx", args...) //nolint:gosec // see above
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("the conformance suite reported failures: %v", err)
	}
	// The suite exits zero when every unexpected failure is absent; the
	// summary is what says how much of it actually ran.
	if !strings.Contains(string(out), "=== SUMMARY ===") {
		t.Error("the suite produced no summary, so nothing was verified")
	}
}

// serveGateway puts the gateway in front of the SDK's conformance server and
// returns the endpoint's URL.
func serveGateway(t *testing.T) string {
	t.Helper()
	server := config.Server{
		ID: backendID, Command: buildEverythingServer(t), Era: config.EraModern,
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := pool.NewBackend(server, backend.NewConnector("conformance", 5*time.Second, logger),
		pool.DefaultStopPolicy, logger)
	t.Cleanup(func() { _ = b.Close(context.WithoutCancel(t.Context())) })

	g := aggregate.New(map[string]aggregate.Source{server.ID: b}, 0, logger)
	srv := httptest.NewServer(frontend.NewEndpoint("conformance", g, logger).Handler())
	t.Cleanup(srv.Close)
	// The suite addresses the endpoint by path; the handler serves every path,
	// and /mcp is the conventional one.
	return srv.URL + "/mcp"
}

// buildEverythingServer builds the conformance server the SDK ships, which is
// a module dependency already.
func buildEverythingServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "everything-server")
	// G204: a constant import path into this test's own temporary directory.
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", path, //nolint:gosec // see above
		"github.com/modelcontextprotocol/go-sdk/conformance/everything-server")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the conformance backend: %v\n%s", err, out)
	}
	return path
}

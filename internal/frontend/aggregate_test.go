package frontend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// logs collects what the gateway wrote while serving, so that a test can show a
// backend was named rather than silently dropped.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// down is the mode of a backend that cannot be started at all. It is not a
// fixture: an executable that does not exist is the cheapest true copy of a
// backend whose process is gone.
const down = testfixtures.Mode("down")

// serveAll puts the gateway in front of several backends, keyed by the
// identifier each is namespaced under.
func serveAll(t *testing.T, backends map[string]testfixtures.Mode) (string, *logs, map[string]*pool.Backend) {
	t.Helper()
	url, captured, started, _ := serveWith(t, backends, testfixtures.Options{})
	return url, captured, started
}

// serveAnnouncing puts the gateway in front of backends that can be made to
// announce a change, and hands back the aggregator so a test can see what it
// still holds.
func serveAnnouncing(t *testing.T, backends map[string]testfixtures.Mode) (string, *logs, *aggregate.Gateway) {
	t.Helper()
	url, captured, _, g := serveWith(t, backends, testfixtures.Options{AnnounceChanges: true})
	return url, captured, g
}

func serveWith(t *testing.T, backends map[string]testfixtures.Mode, opts testfixtures.Options) (string, *logs, map[string]*pool.Backend, *aggregate.Gateway) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	captured := &logs{}
	logger := slog.New(slog.NewTextHandler(captured, nil))

	sources := make(map[string]aggregate.Source, len(backends))
	started := make(map[string]*pool.Backend, len(backends))
	for id, mode := range backends {
		server := config.Server{ID: id, Command: self, Era: config.EraAuto}
		if mode == down {
			server.Command = "/nonexistent/backend"
		} else {
			env := config.Env{}
			for k, v := range testfixtures.Env(mode, opts) {
				env[k] = config.Secret(v)
			}
			server.Env = env
		}
		b := pool.NewBackend(server, backend.NewConnector("test", probeTimeout, logger), pool.DefaultStopPolicy, logger)
		t.Cleanup(func() { _ = b.Close(context.WithoutCancel(t.Context())) })
		sources[id] = b
		started[id] = b
	}

	g := aggregate.New(sources, 0, logger)
	srv := httptest.NewServer(frontend.NewEndpoint("test", g, logger).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, captured, started, g
}

func toolNames(res *mcp.ListToolsResult) []string {
	names := make([]string, 0, len(res.Tools))
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names
}

// Task 9.1: three backends, one listing, every tool under its namespaced name.
func TestMergedListingOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{
		"gh":    testfixtures.ModeModern,
		"docs":  testfixtures.ModeLegacy,
		"files": testfixtures.ModeModern,
	})
	res, err := connect(t, url).ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := []string{
		"docs__add", "docs__echo",
		"files__add", "files__echo",
		"gh__add", "gh__echo",
	}
	if got := toolNames(res); !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// Task 9.4 and 9.5: the call reaches the one backend the prefix names, under
// the name that backend knows, and an unroutable name reaches nobody.
func TestCallRoutingOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, backends := serveAll(t, map[string]testfixtures.Mode{
		"gh":   testfixtures.ModeModern,
		"docs": testfixtures.ModeLegacy,
	})
	session := connect(t, url)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "docs__echo",
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T, want text", res.Content[0])
	}
	// The fixture reports which server it is and what name it was asked for:
	// the legacy one, and the name without the prefix.
	if !strings.Contains(text.Text, "legacy-fixture called echo ") {
		t.Errorf("content = %q, want the legacy backend called under the unprefixed name", text.Text)
	}
	if strings.Contains(text.Text, "modern-fixture") {
		t.Errorf("the call reached the wrong backend: %q", text.Text)
	}

	// Task 9.5: neither an unknown prefix nor a bare name is tried on every
	// backend in the hope that one of them owns it.
	for _, name := range []string{"nosuch__echo", "echo"} {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name})
		if err == nil {
			t.Errorf("CallTool(%q) = %+v, want a not-found error", name, res)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error for %q does not name it: %v", name, err)
		}
	}

	// Nothing above listed anything, so the only reason a process could be
	// running is that a call reached it. The other backend has none: an
	// unroutable name was not tried on it, and neither was the routable one.
	if pid := backends["gh"].PID(); pid != 0 {
		t.Errorf("the backend that owned none of these calls is running as %d", pid)
	}
	if pid := backends["docs"].PID(); pid == 0 {
		t.Error("the backend that owned the call never ran")
	}
}

// Task 9.6: a resource listed under a gateway URI reads back from the backend
// that owns it, under the URI that backend gave.
func TestResourceRoundTripOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{
		"docs": testfixtures.ModeLegacy,
		"gh":   testfixtures.ModeModern,
	})
	session := connect(t, url)

	list, err := session.ListResources(t.Context(), &mcp.ListResourcesParams{})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	var uri string
	for _, r := range list.Resources {
		if strings.HasPrefix(r.URI, "mcp-proxy://docs/") {
			uri = r.URI
		}
	}
	if uri != "mcp-proxy://docs/file%3A%2F%2F%2Freadme.md" {
		t.Fatalf("docs resource URI = %q, want the wrapped original", uri)
	}

	read, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(read.Contents) == 0 {
		t.Fatal("the read returned no contents")
	}
	// The backend was asked for its own URI, and the answer proves which
	// backend served it.
	if !strings.Contains(read.Contents[0].Text, "legacy-fixture served file:///readme.md") {
		t.Errorf("contents = %q, want the owning backend's answer", read.Contents[0].Text)
	}
}

// A resource template keeps its expression, so a client can still expand it.
func TestResourceTemplateOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{"docs": testfixtures.ModeModern})
	res, err := connect(t, url).ListResourceTemplates(t.Context(), &mcp.ListResourceTemplatesParams{})
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(res.ResourceTemplates) != 1 {
		t.Fatalf("templates = %+v, want one", res.ResourceTemplates)
	}
	if got, want := res.ResourceTemplates[0].Name, "docs__file"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := res.ResourceTemplates[0].URITemplate, "mcp-proxy://docs/file%3A%2F%2F%2F{path}"; got != want {
		t.Errorf("uriTemplate = %q, want %q", got, want)
	}
}

// A prompt is namespaced and routed exactly as a tool is.
func TestPromptRoutingOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{
		"docs": testfixtures.ModeLegacy,
		"gh":   testfixtures.ModeModern,
	})
	session := connect(t, url)

	list, err := session.ListPrompts(t.Context(), &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	var names []string
	for _, p := range list.Prompts {
		names = append(names, p.Name)
	}
	if want := []string{"docs__greet", "gh__greet"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("prompts = %v, want %v", names, want)
	}

	got, err := session.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "gh__greet"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	text, ok := got.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T, want text", got.Messages[0].Content)
	}
	if !strings.Contains(text.Text, "modern-fixture served prompt greet") {
		t.Errorf("message = %q, want the modern backend's answer under the unprefixed name", text.Text)
	}
}

// Task 9.7: a URI that is not the gateway's, and one naming a backend that is
// not configured, are both invalid params.
func TestBadResourceURIOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{"docs": testfixtures.ModeModern})
	tests := []struct {
		name string
		uri  string
	}{
		{"a URI of another scheme", "file:///readme.md"},
		{"a gateway URI with no backend", "mcp-proxy:///file%3A%2F%2F%2Freadme.md"},
		{"a backend that is not configured", "mcp-proxy://nosuch/file%3A%2F%2F%2Freadme.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Sent raw rather than through the SDK client: this revision maps
			// an invalid-params answer onto HTTP 400, and the client surfaces
			// that as transport text with the code buried in the body.
			headers := defaultHeaders()
			// This revision carries the target of a request in a header as
			// well as in the body, and they have to agree.
			headers["Mcp-Name"] = tt.uri
			res := post(t, url, "resources/read", map[string]any{"uri": tt.uri}, headers)
			code, message, _ := res.rpcError(t)
			if code != codeInvalidParams {
				t.Errorf("code = %d (%s), want %d", code, message, codeInvalidParams)
			}
		})
	}
}

// Task 9.8: one backend being down costs the listing its entries and nothing
// else, names it in the log, and shortens the lifetime of the answer.
func TestListingSurvivesADownBackendOverHTTP(t *testing.T) {
	t.Parallel()
	url, captured, _ := serveAll(t, map[string]testfixtures.Mode{
		"gh":      testfixtures.ModeModern,
		"missing": down,
	})
	res, err := connect(t, url).ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got, want := toolNames(res), []string{"gh__add", "gh__echo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want only the running backend's %v", got, want)
	}
	if res.TTLMs != aggregate.DegradedTTLMs {
		t.Errorf("ttlMs = %d, want the shortened %d", res.TTLMs, aggregate.DegradedTTLMs)
	}
	if !strings.Contains(captured.String(), "missing") {
		t.Errorf("the log does not name the backend that was left out: %s", captured)
	}
}

// Task 9.9: calling into a backend that is down says so, and says which one.
func TestCallingADownBackendOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{
		"gh":      testfixtures.ModeModern,
		"missing": down,
	})
	_, err := connect(t, url).CallTool(t.Context(), &mcp.CallToolParams{Name: "missing__echo"})
	if err == nil {
		t.Fatal("the call succeeded, want an unavailability error")
	}
	if !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("the error does not identify the backend as unavailable: %v", err)
	}
}

// Task 9.10: with nothing running, the listings are empty rather than failed.
func TestEveryBackendDownOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{"gh": down, "docs": down})
	session := connect(t, url)

	tools, err := session.ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 0 {
		t.Errorf("tools = %v, want none", toolNames(tools))
	}
	prompts, err := session.ListPrompts(t.Context(), &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts.Prompts) != 0 {
		t.Errorf("prompts = %+v, want none", prompts.Prompts)
	}
	resources, err := session.ListResources(t.Context(), &mcp.ListResourcesParams{})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources.Resources) != 0 {
		t.Errorf("resources = %+v, want none", resources.Resources)
	}
}

// Task 9.11: two listings in a row are identical, so a client comparing them
// sees a change only when there was one.
func TestListingOrderIsStableOverHTTP(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAll(t, map[string]testfixtures.Mode{
		"gh":    testfixtures.ModeModern,
		"docs":  testfixtures.ModeLegacy,
		"files": testfixtures.ModeModern,
	})
	// Sent raw rather than through an SDK client: the client caches a listing
	// for the lifetime the gateway attached to it, so a second call through it
	// would compare the answer with itself and pass whatever the server did.
	first := string(post(t, url, "tools/list", nil, defaultHeaders()).json())
	second := string(post(t, url, "tools/list", nil, defaultHeaders()).json())
	if !strings.Contains(first, "gh__echo") {
		t.Fatalf("the listing is not one: %s", first)
	}
	if first != second {
		t.Errorf("two listings differ:\n%s\n%s", first, second)
	}
}

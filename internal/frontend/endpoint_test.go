package frontend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	probeTimeout   = 200 * time.Millisecond
	modernRevision = "2026-07-28"
	metaKeyVersion = "io.modelcontextprotocol/protocolVersion"
)

// serve starts the gateway in front of a fixture backend of the given era, and
// returns the endpoint's URL.
func serve(t *testing.T, mode testfixtures.Mode) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	env := config.Env{}
	for k, v := range testfixtures.Env(mode, testfixtures.Options{}) {
		env[k] = config.Secret(v)
	}
	server := config.Server{ID: string(mode), Command: self, Env: env, Era: config.EraAuto}

	b := pool.NewBackend(server, backend.NewConnector("test", probeTimeout, nil), pool.DefaultStopPolicy, nil)
	t.Cleanup(func() { _ = b.Close(context.WithoutCancel(t.Context())) })

	// Even a single backend is reached through the aggregator: it is what
	// namespaces the names and routes the calls, and a front end that bypassed
	// it for one backend would be a different front end from the one that
	// serves several.
	g := aggregate.New(map[string]aggregate.Source{server.ID: b}, 0, nil)
	http := httptest.NewServer(frontend.NewEndpoint("test", g, nil).Handler())
	t.Cleanup(http.Close)
	return http.URL
}

// Task 5.1: a tools/list over HTTP reaches the backend and comes back.
func TestToolsListOverHTTP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode testfixtures.Mode
	}{
		{"modern backend", testfixtures.ModeModern},
		{"legacy backend", testfixtures.ModeLegacy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			session := connect(t, serve(t, tt.mode))
			res, err := session.ListTools(t.Context(), &mcp.ListToolsParams{})
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			var names []string
			for _, tool := range res.Tools {
				names = append(names, tool.Name)
			}
			if len(names) == 0 {
				t.Fatal("the gateway returned no tools")
			}
			want := aggregate.Qualify(string(tt.mode), "echo")
			if !slices.Contains(names, want) {
				t.Errorf("tools = %v, want it to hold %q", names, want)
			}

			// Task 5.7 and 5.8, on the same result.
			if res.ResultType != "complete" {
				t.Errorf("resultType = %q, want %q", res.ResultType, "complete")
			}
			if res.CacheScope != "private" {
				t.Errorf("cacheScope = %q, want %q", res.CacheScope, "private")
			}
			if res.TTLMs <= 0 {
				t.Errorf("ttlMs = %d, want a positive lifetime", res.TTLMs)
			}
		})
	}
}

// Task 5.1, continued: a call routed through the gateway reaches the backend.
func TestToolCallOverHTTP(t *testing.T) {
	t.Parallel()
	session := connect(t, serve(t, testfixtures.ModeLegacy))
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      aggregate.Qualify(string(testfixtures.ModeLegacy), "echo"),
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("the call returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %T, want text", res.Content[0])
	}
	if !strings.Contains(text.Text, "hello") {
		t.Errorf("content = %q, want the arguments echoed back", text.Text)
	}
}

// Task 5.2: discovery reports what the gateway speaks, and does not offer the
// capabilities this revision deprecated.
func TestDiscover(t *testing.T) {
	t.Parallel()
	res := post(t, serve(t, testfixtures.ModeModern), "server/discover", nil, defaultHeaders())
	result := res.result(t)

	var discover struct {
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		ResultType        string                     `json:"resultType"`
		CacheScope        string                     `json:"cacheScope"`
		TTLMs             int                        `json:"ttlMs"`
		Meta              map[string]struct {
			Name string `json:"name"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(result, &discover); err != nil {
		t.Fatalf("decode discover result %s: %v", result, err)
	}
	if !slices.Equal(discover.SupportedVersions, []string{modernRevision}) {
		t.Errorf("supportedVersions = %v, want only %q — advertising a legacy revision invites the handshake",
			discover.SupportedVersions, modernRevision)
	}
	if len(discover.Capabilities) == 0 {
		t.Error("capabilities missing from the discover result")
	}
	for _, deprecated := range []string{"sampling", "roots", "logging", "elicitation"} {
		if _, found := discover.Capabilities[deprecated]; found {
			t.Errorf("the deprecated %q capability was advertised: %s", deprecated, result)
		}
	}
	if discover.Meta["io.modelcontextprotocol/serverInfo"].Name == "" {
		t.Errorf("serverInfo missing from the discover result: %s", result)
	}
	// The SDK defaults this result's scope to "public"; it must not survive.
	if discover.CacheScope != "private" {
		t.Errorf("cacheScope = %q, want %q", discover.CacheScope, "private")
	}
}

// connect drives the endpoint with the SDK's own client, which proves the
// endpoint is usable by a real one rather than only by this test's idea of one.
func connect(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect to the gateway: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// A response is one JSON-RPC answer, together with its HTTP status.
type response struct {
	status int
	body   []byte
}

// json returns the JSON-RPC envelope, unwrapping the server-sent-event framing
// the endpoint uses for streamed answers.
func (r response) json() []byte {
	for line := range strings.Lines(string(r.body)) {
		if payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			return []byte(strings.TrimSpace(payload))
		}
	}
	return r.body
}

func (r response) result(t *testing.T) json.RawMessage {
	t.Helper()
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.json(), &envelope); err != nil {
		t.Fatalf("decode response %s: %v", r.body, err)
	}
	if envelope.Error != nil {
		t.Fatalf("expected a result, got error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Result
}

func (r response) rpcError(t *testing.T) (int, string, json.RawMessage) {
	t.Helper()
	var envelope struct {
		Error *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.json(), &envelope); err != nil {
		t.Fatalf("decode response %s: %v", r.body, err)
	}
	if envelope.Error == nil {
		t.Fatalf("expected an error, got %s", r.body)
	}
	return envelope.Error.Code, envelope.Error.Message, envelope.Error.Data
}

// postRaw sends a request with no _meta at all, as a client that predates the
// per-request protocol would.
func postRaw(t *testing.T, url, method string, params map[string]any, headers map[string]string) response {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return send(t, url, method, body, headers)
}

// decode unmarshals a JSON payload or fails the test.
func decode(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

func defaultHeaders() map[string]string {
	return map[string]string{
		"Mcp-Protocol-Version": modernRevision,
	}
}

// metaVersionOverride is a pseudo-header a test uses to put a different
// revision in the body's _meta than in the headers.
const metaVersionOverride = "X-Test-Meta-Version"

// post sends one raw JSON-RPC request carrying a modern _meta, so that a test
// can send what an SDK client would refuse to construct.
func post(t *testing.T, url, method string, params map[string]any, headers map[string]string) response {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	version := modernRevision
	if v, ok := headers[metaVersionOverride]; ok {
		version = v
	}
	// A modern request carries per request what the handshake used to carry
	// once.
	params["_meta"] = map[string]any{
		metaKeyVersion:                               version,
		"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "test-client", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return send(t, url, method, body, headers)
}

// send performs one POST and reads the whole answer.
func send(t *testing.T, url, method string, body []byte, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if _, set := headers["Mcp-Method"]; !set {
		req.Header.Set("Mcp-Method", method)
	}
	for k, v := range headers {
		if k == metaVersionOverride {
			continue
		}
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	return do(t, req)
}

// do performs a request and reads the whole answer.
func do(t *testing.T, req *http.Request) response {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer res.Body.Close() //nolint:errcheck // nothing to do with a close error here
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response{status: res.StatusCode, body: raw}
}

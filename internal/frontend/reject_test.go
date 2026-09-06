package frontend_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/frontend"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// JSON-RPC codes revision 2026-07-28 pins to specific failures.
const (
	codeHeaderMismatch  = -32020
	codeUnsupportedVer  = -32022
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	unsupportedRevision = "1999-01-01"
)

// Task 5.3: the mandatory headers are required, and must agree with the body.
func TestMandatoryHeaders(t *testing.T) {
	t.Parallel()
	url := serve(t, testfixtures.ModeModern)
	tests := []struct {
		name    string
		method  string
		params  map[string]any
		headers map[string]string
		// wantCode is the JSON-RPC code; the status is 400 throughout, since
		// these are malformed requests rather than failed ones.
		wantCode int
	}{
		{
			name:     "Mcp-Method absent",
			method:   "tools/list",
			headers:  map[string]string{"Mcp-Method": ""},
			wantCode: codeHeaderMismatch,
		},
		{
			name:     "Mcp-Method contradicts the body",
			method:   "tools/list",
			headers:  map[string]string{"Mcp-Method": "prompts/list"},
			wantCode: codeHeaderMismatch,
		},
		{
			// The name header is required only for the methods that carry
			// one, and tools/call is the main one.
			name:     "Mcp-Name absent on a call that needs it",
			method:   "tools/call",
			params:   map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}},
			wantCode: codeHeaderMismatch,
		},
		{
			name:     "Mcp-Name contradicts the body",
			method:   "tools/call",
			params:   map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}},
			headers:  map[string]string{"Mcp-Name": "add"},
			wantCode: codeHeaderMismatch,
		},
		{
			name:     "Mcp-Protocol-Version absent",
			method:   "tools/list",
			headers:  map[string]string{"Mcp-Protocol-Version": ""},
			wantCode: codeHeaderMismatch,
		},
		{
			// Two revisions named in one request is a contradiction, not a
			// request for the one this endpoint happens not to speak.
			name:     "Mcp-Protocol-Version contradicts the body",
			method:   "tools/list",
			headers:  map[string]string{metaVersionOverride: unsupportedRevision},
			wantCode: codeHeaderMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := defaultHeaders()
			for k, v := range tt.headers {
				headers[k] = v
			}
			res := post(t, url, tt.method, tt.params, headers)
			if res.status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", res.status, http.StatusBadRequest)
			}
			if code, msg, _ := res.rpcError(t); code != tt.wantCode {
				t.Errorf("code = %d (%s), want %d", code, msg, tt.wantCode)
			}
		})
	}
}

// Task 5.4: a client that only knows the handshake must be told what to do
// instead, not merely that the handshake is gone.
func TestInitializeIsRejectedActionably(t *testing.T) {
	t.Parallel()
	url := serve(t, testfixtures.ModeModern)
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{"from a modern client", defaultHeaders()},
		{
			// The realistic case: a client that only knows the handshake
			// announces a legacy revision. It must still reach the message
			// that tells it what to do, rather than a bare version refusal.
			name:    "from a legacy client",
			headers: map[string]string{"Mcp-Protocol-Version": "2025-11-25", metaVersionOverride: "2025-11-25"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := post(t, url, "initialize", map[string]any{"protocolVersion": "2025-11-25"}, tt.headers)
			if res.status != http.StatusNotFound {
				t.Errorf("status = %d, want %d", res.status, http.StatusNotFound)
			}
			code, message, _ := res.rpcError(t)
			if code != codeMethodNotFound {
				t.Errorf("code = %d, want %d", code, codeMethodNotFound)
			}
			for _, want := range frontend.SupportedVersions {
				if !strings.Contains(message, want) {
					t.Errorf("message does not name the supported revision %q: %s", want, message)
				}
			}
			if !strings.Contains(message, "_meta") {
				t.Errorf("message does not say where the version now goes: %s", message)
			}
		})
	}
}

// Task 5.5: every method this revision removed is gone, and says so with the
// standard code rather than as an uninitialized session.
func TestRemovedMethodsAreRejected(t *testing.T) {
	t.Parallel()
	url := serve(t, testfixtures.ModeModern)
	for _, method := range frontend.RemovedMethods {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			res := post(t, url, method, nil, defaultHeaders())
			// A method the endpoint does not have is 404, the same answer any
			// unimplemented method gets: the code says which method, and the
			// status says the client is asking for something that is not there.
			if res.status != http.StatusNotFound {
				t.Errorf("status = %d, want %d", res.status, http.StatusNotFound)
			}
			code, message, _ := res.rpcError(t)
			if code != codeMethodNotFound {
				t.Errorf("code = %d (%s), want %d", code, message, codeMethodNotFound)
			}
			if !strings.Contains(message, method) {
				t.Errorf("message does not name the method: %s", message)
			}
		})
	}
}

// Task 5.5, continued: the same holds for a request that carries no per-request
// version, which the SDK would otherwise treat as a session that never
// initialized.
func TestRemovedMethodsWithoutVersionMeta(t *testing.T) {
	t.Parallel()
	url := serve(t, testfixtures.ModeModern)
	for _, method := range frontend.RemovedMethods {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			res := postRaw(t, url, method, nil, defaultHeaders())
			if code, message, _ := res.rpcError(t); code != codeMethodNotFound {
				t.Errorf("code = %d (%s), want %d", code, message, codeMethodNotFound)
			}
		})
	}
}

// Task 5.6: an unsupported version is refused with the list a client needs to
// pick a supported one.
func TestUnsupportedVersionIsRefused(t *testing.T) {
	t.Parallel()
	res := post(t, serve(t, testfixtures.ModeModern), "tools/list", nil, map[string]string{
		"Mcp-Protocol-Version": unsupportedRevision,
		metaVersionOverride:    unsupportedRevision,
	})
	code, _, data := res.rpcError(t)
	if code != codeUnsupportedVer {
		t.Fatalf("code = %d, want %d", code, codeUnsupportedVer)
	}
	var payload struct {
		Supported []string `json:"supported"`
		Requested string   `json:"requested"`
	}
	decode(t, data, &payload)
	if len(payload.Supported) == 0 {
		t.Errorf("data.supported is empty: %s", data)
	}
	if payload.Requested != unsupportedRevision {
		t.Errorf("data.requested = %q, want %q", payload.Requested, unsupportedRevision)
	}
}

// Task 5.8: no response from the gateway may permit shared caching, whatever
// produced it.
func TestPublicCachingNeverAppears(t *testing.T) {
	t.Parallel()
	for _, mode := range []testfixtures.Mode{testfixtures.ModeModern, testfixtures.ModeLegacy} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			url := serve(t, mode)
			for _, method := range []string{"server/discover", "tools/list"} {
				body := string(post(t, url, method, nil, defaultHeaders()).body)
				if strings.Contains(body, `"public"`) {
					t.Errorf("%s permitted shared caching: %s", method, body)
				}
				if !strings.Contains(body, `"cacheScope":"private"`) {
					t.Errorf("%s did not restrict caching: %s", method, body)
				}
				if !strings.Contains(body, `"resultType":"complete"`) {
					t.Errorf("%s did not mark the result complete: %s", method, body)
				}
				if strings.Contains(body, "input_required") {
					t.Errorf("%s returned an interim result: %s", method, body)
				}
			}
		})
	}
}

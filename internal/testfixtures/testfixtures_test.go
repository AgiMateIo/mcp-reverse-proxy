package testfixtures_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// frame is one decoded JSON-RPC message. The tests assert on the wire shape
// rather than on SDK types, because the shape is what the specs constrain.
type frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// recvTimeout bounds a wait for an expected frame. Under synctest it is virtual
// time, so a fixture that answers nothing fails fast instead of hanging.
const recvTimeout = 5 * time.Second

// A session drives one fixture over a pair of pipes.
type session struct {
	t       *testing.T
	stdin   *io.PipeWriter
	frames  chan frame
	dropped atomic.Bool
	wg      sync.WaitGroup
	runErr  error
	nextID  int
}

// start runs a fixture in this process. The caller closes the session, rather
// than t.Cleanup doing it, so that a synctest bubble can own the goroutines.
func start(t *testing.T, mode testfixtures.Mode) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, stdin: inW, frames: make(chan frame, 64)}

	s.wg.Go(func() {
		s.runErr = testfixtures.Run(context.Background(), mode, inR, outW)
		// Closing stdout unblocks the reader below.
		outW.CloseWithError(s.runErr)
	})
	s.wg.Go(func() {
		scan := bufio.NewScanner(outR)
		for scan.Scan() {
			var f frame
			if err := json.Unmarshal(scan.Bytes(), &f); err != nil {
				t.Errorf("decode fixture output %q: %v", scan.Text(), err)
				continue
			}
			// A non-blocking send keeps the fixture's writes from deadlocking
			// against a test that never reads them; close reports the loss.
			select {
			case s.frames <- f:
			default:
				s.dropped.Store(true)
			}
		}
	})
	return s
}

// close stops the fixture the way the gateway does: by closing its stdin.
func (s *session) close() {
	s.t.Helper()
	if err := s.stdin.Close(); err != nil {
		s.t.Errorf("close fixture stdin: %v", err)
	}
	s.wg.Wait()
	if s.runErr != nil {
		s.t.Errorf("fixture exited with error: %v", s.runErr)
	}
	if s.dropped.Load() {
		s.t.Error("fixture produced more frames than the test read")
	}
}

// request sends a request and returns the id it was sent under.
func (s *session) request(method string, params any) json.RawMessage {
	s.t.Helper()
	s.nextID++
	id, err := json.Marshal(s.nextID)
	if err != nil {
		s.t.Fatalf("encode request id: %v", err)
	}
	s.write(frame{ID: id, Method: method, Params: mustRaw(s.t, params)})
	return id
}

// notify sends a notification, which has no id and expects no reply.
func (s *session) notify(method string, params any) {
	s.t.Helper()
	s.write(frame{Method: method, Params: mustRaw(s.t, params)})
}

// respond answers a server-to-client request with an error, as the gateway does
// for a capability it never advertised.
func (s *session) respondError(id json.RawMessage, code int, message string) {
	s.t.Helper()
	s.write(frame{
		ID: id,
		Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: code, Message: message},
	})
}

func (s *session) write(f frame) {
	s.t.Helper()
	line, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		frame
	}{JSONRPC: "2.0", frame: f})
	if err != nil {
		s.t.Fatalf("encode frame: %v", err)
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("write frame: %v", err)
	}
}

// recv waits for the next frame from the fixture.
func (s *session) recv() frame {
	s.t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-time.After(recvTimeout):
		s.t.Fatal("timed out waiting for a frame from the fixture")
		return frame{}
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	return raw
}

func decodeResult[T any](t *testing.T, f frame) T {
	t.Helper()
	if f.Error != nil {
		t.Fatalf("expected a result, got error %d: %s", f.Error.Code, f.Error.Message)
	}
	var v T
	if err := json.Unmarshal(f.Result, &v); err != nil {
		t.Fatalf("decode result %s: %v", f.Result, err)
	}
	return v
}

// Result shapes the tests assert on.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Tools *struct {
			ListChanged bool `json:"listChanged"`
		} `json:"tools"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type discoverResult struct {
	ResultType        string          `json:"resultType"`
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      json.RawMessage `json:"capabilities"`
	TTLMs             int             `json:"ttlMs"`
	CacheScope        string          `json:"cacheScope"`
	Meta              map[string]struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"_meta"`
}

type listToolsResult struct {
	ResultType string `json:"resultType"`
	CacheScope string `json:"cacheScope"`
	TTLMs      int    `json:"ttlMs"`
	Tools      []struct {
		Name        string          `json:"name"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"tools"`
}

const (
	versionModern = "2026-07-28"
	versionLegacy = "2025-11-25"
)

// Task 2.1: a legacy fixture answers `initialize` and errors on anything else
// before the handshake — the error is what makes an era probe fall back.
func TestLegacyFixture(t *testing.T) {
	t.Parallel()
	s := start(t, testfixtures.ModeLegacy)
	defer s.close()

	t.Run("errors on unknown pre-handshake methods", func(t *testing.T) {
		for _, method := range []string{"server/discover", "tools/list"} {
			s.request(method, nil)
			got := s.recv()
			if got.Error == nil {
				t.Fatalf("%s before initialize: expected an error, got result %s", method, got.Result)
			}
			if got.Error.Code != -32601 {
				t.Errorf("%s before initialize: code = %d, want -32601", method, got.Error.Code)
			}
		}
	})

	t.Run("answers initialize", func(t *testing.T) {
		id := s.request("initialize", map[string]any{
			"protocolVersion": versionLegacy,
			"clientInfo":      map[string]string{"name": "test", "version": "0"},
			"capabilities":    map[string]any{},
		})
		got := s.recv()
		if string(got.ID) != string(id) {
			t.Errorf("response id = %s, want %s", got.ID, id)
		}
		res := decodeResult[initializeResult](t, got)
		if res.ProtocolVersion != versionLegacy {
			t.Errorf("protocolVersion = %q, want %q", res.ProtocolVersion, versionLegacy)
		}
		if res.Capabilities.Tools == nil {
			t.Error("tools capability missing from the handshake result")
		}
		if res.ServerInfo.Name == "" {
			t.Error("serverInfo.name missing from the handshake result")
		}
	})

	t.Run("serves the legacy result shape", func(t *testing.T) {
		s.notify("notifications/initialized", map[string]any{})
		s.request("tools/list", map[string]any{})
		res := decodeResult[listToolsResult](t, s.recv())
		// Normalization has to supply these; a helpful fixture would hide the
		// bug that 4.10 exists to catch.
		if res.ResultType != "" || res.CacheScope != "" || res.TTLMs != 0 {
			t.Errorf("legacy tools/list carries modern envelope fields: %+v", res)
		}
		if len(res.Tools) == 0 {
			t.Error("legacy tools/list returned no tools")
		}
	})
}

// Task 2.2: a modern fixture answers `server/discover` with a DiscoverResult.
func TestModernFixture(t *testing.T) {
	t.Parallel()
	s := start(t, testfixtures.ModeModern)
	defer s.close()

	id := s.request("server/discover", map[string]any{
		"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": versionModern},
	})
	got := s.recv()
	if string(got.ID) != string(id) {
		t.Errorf("response id = %s, want %s", got.ID, id)
	}
	res := decodeResult[discoverResult](t, got)
	if !slices.Contains(res.SupportedVersions, versionModern) {
		// Without this exact revision a client negotiates itself down to the
		// legacy path, and the fixture stops being a modern-era fixture.
		t.Errorf("supportedVersions = %v, want it to contain %q", res.SupportedVersions, versionModern)
	}
	if res.ResultType != "complete" {
		t.Errorf("resultType = %q, want %q", res.ResultType, "complete")
	}
	if len(res.Capabilities) == 0 {
		t.Error("capabilities missing from the discover result")
	}
	if res.CacheScope != "private" {
		t.Errorf("cacheScope = %q, want %q", res.CacheScope, "private")
	}
	if res.TTLMs <= 0 {
		t.Errorf("ttlMs = %d, want a positive cache hint", res.TTLMs)
	}
	if info := res.Meta["io.modelcontextprotocol/serverInfo"]; info.Name == "" {
		t.Errorf("_meta serverInfo missing from the discover result: %+v", res.Meta)
	}

	t.Run("rejects the removed handshake", func(t *testing.T) {
		s.request("initialize", map[string]any{"protocolVersion": versionLegacy})
		if got := s.recv(); got.Error == nil {
			t.Errorf("initialize on a modern backend: expected an error, got %s", got.Result)
		}
	})
}

// Task 2.3: a silent fixture never answers `server/discover`. Virtual time makes
// the absence of a reply provable: once every goroutine is durably blocked, no
// answer is coming.
func TestSilentFixture(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := start(t, testfixtures.ModeSilent)
		defer s.close()

		// Walk a probe deadline the way era detection will: ask, wait out the
		// timeout, and find nothing. Under virtual time the wait is free.
		const probeTimeout = 2 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), probeTimeout)
		defer cancel()
		s.request("server/discover", map[string]any{})
		<-ctx.Done()
		synctest.Wait()
		select {
		case f := <-s.frames:
			t.Fatalf("silent fixture answered server/discover: %+v", f)
		default:
		}

		// The probe timeout is a delay, not a dead end: the fallback handshake
		// still has to succeed against the same process.
		s.request("initialize", map[string]any{"protocolVersion": versionLegacy})
		res := decodeResult[initializeResult](t, s.recv())
		if res.ProtocolVersion != versionLegacy {
			t.Errorf("protocolVersion = %q, want %q", res.ProtocolVersion, versionLegacy)
		}
	})
}

// Task 2.4: a misbehaving fixture issues a server-to-client request although
// the capability was never advertised, and keeps serving after being refused.
func TestMisbehavingFixture(t *testing.T) {
	t.Parallel()
	s := start(t, testfixtures.ModeMisbehaving)
	defer s.close()

	s.request("initialize", map[string]any{
		// Exactly what the gateway sends: no roots, sampling, or elicitation.
		"protocolVersion": versionLegacy,
		"capabilities":    map[string]any{},
	})
	decodeResult[initializeResult](t, s.recv())

	s.notify("notifications/initialized", map[string]any{})
	unsolicited := s.recv()
	if unsolicited.Method != "sampling/createMessage" {
		t.Fatalf("expected an unsolicited sampling/createMessage, got %+v", unsolicited)
	}
	if len(unsolicited.ID) == 0 {
		t.Error("unsolicited sampling/createMessage arrived without an id, so it cannot be refused")
	}

	// Refuse it the way the gateway does, then show the connection survives.
	s.respondError(unsolicited.ID, -32601, "sampling is not advertised")
	s.request("tools/list", map[string]any{})
	if res := decodeResult[listToolsResult](t, s.recv()); len(res.Tools) == 0 {
		t.Error("misbehaving fixture stopped serving after its request was refused")
	}
}

// Every fixture serves the same tool set once its era's handshake is done, so
// that a test can tell backends apart by server id rather than by tool names.
func TestFixturesServeTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mode      testfixtures.Mode
		handshake func(*session)
	}{
		{"legacy", testfixtures.ModeLegacy, legacyHandshake},
		{"silent", testfixtures.ModeSilent, legacyHandshake},
		{"misbehaving", testfixtures.ModeMisbehaving, func(s *session) {
			legacyHandshake(s)
			s.respondError(s.recv().ID, -32601, "sampling is not advertised")
		}},
		{"modern", testfixtures.ModeModern, func(s *session) {
			s.request("server/discover", map[string]any{})
			s.recv()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := start(t, tt.mode)
			defer s.close()

			tt.handshake(s)
			s.request("tools/list", map[string]any{})
			res := decodeResult[listToolsResult](t, s.recv())
			var names []string
			for _, tool := range res.Tools {
				names = append(names, tool.Name)
				if len(tool.InputSchema) == 0 {
					t.Errorf("tool %q has no input schema", tool.Name)
				}
			}
			if want := []string{"echo", "add"}; !slices.Equal(names, want) {
				t.Errorf("tools = %v, want %v", names, want)
			}
		})
	}
}

func legacyHandshake(s *session) {
	s.t.Helper()
	s.request("initialize", map[string]any{"protocolVersion": versionLegacy})
	s.recv()
	s.notify("notifications/initialized", map[string]any{})
}

func TestRunRejectsUnknownMode(t *testing.T) {
	t.Parallel()
	err := testfixtures.Run(context.Background(), "nonsense", strings.NewReader(""), io.Discard)
	if !errors.Is(err, testfixtures.ErrUnknownMode) {
		t.Errorf("Run with an unknown mode: err = %v, want %v", err, testfixtures.ErrUnknownMode)
	}
}

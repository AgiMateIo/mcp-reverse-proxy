// Package testfixtures provides stdio MCP servers with deliberately chosen
// behavior: a legacy-era server, a modern-era one, one that never answers the
// era probe, and one that violates capability masking.
//
// The failure modes the specs describe — a silent backend, an unsolicited
// server-to-client request, a legacy result missing the modern envelope —
// cannot be reproduced on demand against real MCP servers, so backend behavior
// is exercised against these instead.
//
// A fixture is a plain [Run] call over a pair of streams, not a process. Tests
// that need only the wire behavior drive it in-process, where synctest can
// observe it; tests that need a real child process run it through
// cmd/mcpfixture. The package speaks newline-delimited JSON-RPC directly rather
// than through the SDK, both because the SDK stays inside internal/backend and
// internal/frontend and because a well-behaved server cannot express these
// modes.
package testfixtures

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// A Mode selects the behavior of a fixture server.
type Mode string

const (
	// ModeLegacy speaks revision 2025-11-25: it answers `initialize` and
	// rejects every other method until the handshake completes, which is what
	// makes an era probe fall back.
	ModeLegacy Mode = "legacy"
	// ModeModern speaks revision 2026-07-28: it answers `server/discover` and
	// has no handshake at all.
	ModeModern Mode = "modern"
	// ModeSilent is a legacy server that drops `server/discover` without a
	// reply instead of erroring on it. It exercises the probe timeout: the
	// fallback handshake still succeeds once the gateway stops waiting.
	ModeSilent Mode = "silent"
	// ModeMisbehaving is a legacy server that issues a server-to-client
	// `sampling/createMessage` request although the capability was never
	// advertised to it. It keeps serving afterwards, so a test can show the
	// violation does not take the connection down.
	ModeMisbehaving Mode = "misbehaving"
)

// Modes lists every supported fixture mode.
var Modes = []Mode{ModeLegacy, ModeModern, ModeSilent, ModeMisbehaving}

// ErrUnknownMode reports a [Mode] this package does not implement.
var ErrUnknownMode = errors.New("testfixtures: unknown mode")

// Protocol versions the fixtures speak. Modern-era backends must offer exactly
// the revision the gateway front-end speaks, or a client negotiates itself down
// to the legacy path.
const (
	versionModern = "2026-07-28"
	versionLegacy = "2025-11-25"
)

// JSON-RPC error codes the fixtures produce.
const (
	codeMethodNotFound = -32601
	// codeResourceNotFound is the legacy "resource not found" code that
	// revision 2026-07-28 folds into -32602.
	codeResourceNotFound = -32002
)

// Meta keys of revision 2026-07-28, which carries on every message what the
// legacy handshake carried once.
const (
	metaKeyServerInfo = "io.modelcontextprotocol/serverInfo"
)

// maxLine bounds a single JSON-RPC frame. Fixture traffic is small; the limit
// exists so a runaway peer cannot grow the read buffer without bound.
const maxLine = 1 << 20

// Run serves one fixture session, reading newline-delimited JSON-RPC from stdin
// and writing replies to stdout. It returns when stdin reaches EOF: closing
// stdin is the gateway's stop signal, and the only one a fixture blocked on a
// silent peer can observe. ctx is consulted between frames, so cancelling it
// stops a fixture that is being talked to but not one that is idle.
func Run(ctx context.Context, mode Mode, stdin io.Reader, stdout io.Writer) error {
	f, err := newFixture(mode, stdout)
	if err != nil {
		return err
	}
	return f.serve(ctx, stdin)
}

// A fixture is one running fixture session.
type fixture struct {
	mode Mode
	out  *writer
	// initialized reports whether the legacy handshake has completed. Modes
	// with no handshake leave it false and never consult it.
	initialized bool
	// nextID numbers the server-to-client requests that ModeMisbehaving sends.
	nextID int
}

func newFixture(mode Mode, stdout io.Writer) (*fixture, error) {
	if _, ok := serverNames[mode]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, mode)
	}
	return &fixture{mode: mode, out: &writer{w: stdout}}, nil
}

// serverNames doubles as the set of implemented modes.
var serverNames = map[Mode]string{
	ModeLegacy:      "legacy-fixture",
	ModeModern:      "modern-fixture",
	ModeSilent:      "silent-fixture",
	ModeMisbehaving: "misbehaving-fixture",
}

func (f *fixture) serve(ctx context.Context, stdin io.Reader) error {
	scan := bufio.NewScanner(stdin)
	scan.Buffer(make([]byte, 0, 4096), maxLine)
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		var msg message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return fmt.Errorf("testfixtures: decode frame: %w", err)
		}
		if msg.Method == "" {
			// A response to one of our own requests. ModeMisbehaving expects
			// an error here and carries on regardless.
			continue
		}
		if err := f.dispatch(&msg); err != nil {
			return err
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("testfixtures: read stdin: %w", err)
	}
	return ctx.Err()
}

// dispatch answers one inbound request or notification.
func (f *fixture) dispatch(msg *message) error {
	switch f.mode {
	case ModeModern:
		return f.dispatchModern(msg)
	case ModeSilent:
		if msg.Method == methodDiscover {
			// The point of this fixture: no reply at all, not even an error.
			return nil
		}
		return f.dispatchLegacy(msg)
	default:
		return f.dispatchLegacy(msg)
	}
}

// dispatchModern serves revision 2026-07-28. There is no handshake: every
// request carries its own context in _meta, so any method is answerable at once.
func (f *fixture) dispatchModern(msg *message) error {
	switch msg.Method {
	case methodDiscover:
		return f.out.result(msg.ID, discoverResult{
			ResultType:        "complete",
			SupportedVersions: []string{versionModern},
			Capabilities:      serverCapabilities(),
			TTLMs:             ttlMs,
			CacheScope:        "private",
			Meta: map[string]any{
				metaKeyServerInfo: f.implementation(),
			},
		})
	case methodToolsList:
		return f.out.result(msg.ID, modernListToolsResult{
			ResultType: "complete",
			Tools:      fixtureTools,
			TTLMs:      ttlMs,
			CacheScope: "private",
		})
	case methodToolsCall:
		return f.callTool(msg, true)
	case methodInitialize:
		// Revision 2026-07-28 removed the handshake.
		return f.out.fail(msg.ID, codeMethodNotFound, "initialize was removed in "+versionModern)
	default:
		return f.unknownMethod(msg)
	}
}

// dispatchLegacy serves revision 2025-11-25, where nothing but `initialize` is
// answerable before the handshake completes.
func (f *fixture) dispatchLegacy(msg *message) error {
	if msg.Method == methodInitialize {
		f.initialized = true
		return f.out.result(msg.ID, initializeResult{
			ProtocolVersion: versionLegacy,
			Capabilities:    serverCapabilities(),
			ServerInfo:      f.implementation(),
		})
	}
	if !f.initialized {
		return f.unknownMethod(msg)
	}
	switch msg.Method {
	case notificationInitialized:
		if f.mode == ModeMisbehaving {
			return f.sample()
		}
		return nil
	case methodToolsList:
		// Deliberately the legacy shape: no resultType, no ttlMs, no
		// cacheScope. Normalization has to supply them.
		return f.out.result(msg.ID, legacyListToolsResult{Tools: fixtureTools})
	case methodToolsCall:
		return f.callTool(msg, false)
	case methodResourcesRead:
		// The obsolete code that normalization remaps to -32602.
		return f.out.fail(msg.ID, codeResourceNotFound, "resource not found")
	default:
		return f.unknownMethod(msg)
	}
}

// callTool answers tools/call, echoing the arguments back so a test can tell
// which backend served the call.
func (f *fixture) callTool(msg *message, modern bool) error {
	var params callToolParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("testfixtures: decode tools/call params: %w", err)
		}
	}
	res := callToolResult{
		Content: []content{{
			Type: "text",
			Text: fmt.Sprintf("%s called %s with %s", serverNames[f.mode], params.Name, string(params.Arguments)),
		}},
	}
	if modern {
		res.ResultType = "complete"
	}
	return f.out.result(msg.ID, res)
}

// sample sends the unsolicited server-to-client request that capability masking
// is supposed to prevent.
func (f *fixture) sample() error {
	f.nextID++
	id, err := json.Marshal(fmt.Sprintf("%s-%d", f.mode, f.nextID))
	if err != nil {
		return fmt.Errorf("testfixtures: encode request id: %w", err)
	}
	return f.out.request(id, methodSamplingCreateMessage, createMessageParams{
		MaxTokens: 16,
		Messages: []samplingMessage{{
			Role:    "user",
			Content: content{Type: "text", Text: "unsolicited"},
		}},
	})
}

// unknownMethod answers a request the fixture does not implement. Notifications
// get no reply: they have no id to answer.
func (f *fixture) unknownMethod(msg *message) error {
	if len(msg.ID) == 0 {
		return nil
	}
	return f.out.fail(msg.ID, codeMethodNotFound, "method not found: "+msg.Method)
}

func (f *fixture) implementation() implementation {
	return implementation{Name: serverNames[f.mode], Version: "1.0.0"}
}

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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
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

// Options tune a fixture beyond its mode. The zero value is what [Run] uses.
type Options struct {
	// Revision is the protocol revision a legacy handshake answers with.
	// Empty means [versionLegacy]. "Legacy" is not one revision: a backend may
	// name any published revision older than 2026-07-28, and the gateway is
	// expected to adopt the one it names.
	Revision string
	// NegotiateOnce makes the first server/discover fail with -32022 naming a
	// mutually supported version, and the retry succeed. It is the only way to
	// exercise that retry path against an SDK that knows one modern revision.
	NegotiateOnce bool
	// Stderr, when non-empty, is written to the fixture's standard error
	// before it serves anything. Writing there is not a failure, and the
	// fixture goes on serving.
	Stderr string
	// ChildPIDFile, when non-empty, makes the fixture spawn a long-lived
	// descendant and write its process id to that path. Terminating the
	// fixture must terminate the descendant too.
	ChildPIDFile string
	// IgnoreStdinClose makes the fixture keep running after its standard input
	// is closed, which is what forces the escalation to signals.
	IgnoreStdinClose bool
	// EnvDumpFile, when non-empty, makes the fixture record its whole
	// environment and argument list at startup. It is how a test can see
	// exactly what the gateway handed the process — and what it did not.
	EnvDumpFile string
	// ExitOnCall makes a call to the "exit" tool terminate the fixture without
	// answering, which is how a request in flight when its backend dies is
	// staged. It calls os.Exit, so it is only meaningful — and only safe — in
	// a fixture running as its own process.
	ExitOnCall bool
}

// Run serves one fixture session with default options, reading
// newline-delimited JSON-RPC from stdin and writing replies to stdout. It returns when stdin reaches EOF: closing
// stdin is the gateway's stop signal, and the only one a fixture blocked on a
// silent peer can observe. ctx is consulted between frames, so cancelling it
// stops a fixture that is being talked to but not one that is idle.
func Run(ctx context.Context, mode Mode, stdin io.Reader, stdout io.Writer) error {
	return RunWith(ctx, mode, Options{}, stdin, stdout)
}

// RunWith serves one fixture session with the given options. stderr may be nil
// when [Options.Stderr] is empty.
func RunWith(ctx context.Context, mode Mode, opts Options, stdin io.Reader, stdout io.Writer) error {
	f, err := newFixture(mode, stdout)
	if err != nil {
		return err
	}
	f.opts = opts
	if opts.Revision == "" {
		f.opts.Revision = versionLegacy
	}
	if opts.EnvDumpFile != "" {
		if err := f.dumpEnvironment(); err != nil {
			return err
		}
	}
	if opts.ChildPIDFile != "" {
		if err := f.spawnChild(); err != nil {
			return err
		}
	}
	return f.serve(ctx, stdin)
}

// A fixture is one running fixture session.
type fixture struct {
	mode Mode
	opts Options
	out  *writer
	// initialized reports whether the legacy handshake has completed. Modes
	// with no handshake leave it false and never consult it.
	initialized bool
	// nextID numbers the server-to-client requests that ModeMisbehaving sends.
	nextID int
	// negotiated records that the version retry of Options.NegotiateOnce has
	// already been demanded once.
	negotiated bool
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
	if f.opts.Stderr != "" {
		// Diagnostics on stderr are ordinary behavior, not a failure.
		fmt.Fprintln(os.Stderr, f.opts.Stderr)
	}
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
	if f.opts.IgnoreStdinClose {
		// Ignoring the stop signal is the point: the gateway must escalate.
		<-ctx.Done()
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
		if f.opts.NegotiateOnce && !f.negotiated {
			f.negotiated = true
			return f.out.failData(msg.ID, codeUnsupportedProtocolVersion,
				"unsupported protocol version",
				unsupportedVersionData{Supported: []string{versionModern}})
		}
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
			Tools:      f.tools(),
			TTLMs:      ttlMs,
			CacheScope: "private",
		})
	case methodToolsCall:
		return f.callTool(msg, true)
	case methodPromptsList:
		return f.out.result(msg.ID, listPromptsResult{envelope: modernEnvelope(), Prompts: fixturePrompts})
	case methodPromptsGet:
		return f.getPrompt(msg, true)
	case methodResourcesList:
		return f.out.result(msg.ID, listResourcesResult{envelope: modernEnvelope(), Resources: fixtureResources})
	case methodResourcesTemplates:
		return f.out.result(msg.ID, listResourceTemplatesResult{
			envelope:          modernEnvelope(),
			ResourceTemplates: fixtureResourceTemplates,
		})
	case methodResourcesRead:
		return f.readResource(msg, true)
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
			ProtocolVersion: f.opts.Revision,
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
		return f.out.result(msg.ID, legacyListToolsResult{Tools: f.tools()})
	case methodToolsCall:
		return f.callTool(msg, false)
	case methodPromptsList:
		// Deliberately the legacy shape, like tools/list above: the envelope
		// is missing and normalization has to supply it.
		return f.out.result(msg.ID, listPromptsResult{Prompts: fixturePrompts})
	case methodPromptsGet:
		return f.getPrompt(msg, false)
	case methodResourcesList:
		return f.out.result(msg.ID, listResourcesResult{Resources: fixtureResources})
	case methodResourcesTemplates:
		return f.out.result(msg.ID, listResourceTemplatesResult{ResourceTemplates: fixtureResourceTemplates})
	case methodResourcesRead:
		return f.readResource(msg, false)
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
	if f.opts.ExitOnCall && params.Name == exitToolName {
		os.Exit(1)
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

// getPrompt answers prompts/get, naming the fixture that served it so a test
// can tell one backend's answer from another's.
func (f *fixture) getPrompt(msg *message, modern bool) error {
	var params getPromptParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("testfixtures: decode prompts/get params: %w", err)
		}
	}
	res := getPromptResult{
		Description: params.Name,
		Messages: []promptMessage{{
			Role: "user",
			Content: content{
				Type: "text",
				Text: fmt.Sprintf("%s served prompt %s", serverNames[f.mode], params.Name),
			},
		}},
	}
	if modern {
		res.ResultType = "complete"
	}
	return f.out.result(msg.ID, res)
}

// readResource answers resources/read for the one resource a fixture has, and
// answers anything else with the obsolete not-found code that normalization
// remaps to -32602.
func (f *fixture) readResource(msg *message, modern bool) error {
	var params readResourceParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("testfixtures: decode resources/read params: %w", err)
		}
	}
	if params.URI != fixtureResourceURI {
		return f.out.fail(msg.ID, codeResourceNotFound, "resource not found")
	}
	res := readResourceResult{
		Contents: []resourceContents{{
			URI:      params.URI,
			MIMEType: "text/markdown",
			Text:     fmt.Sprintf("%s served %s", serverNames[f.mode], params.URI),
		}},
	}
	if modern {
		res.envelope = modernEnvelope()
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

// tools is the tool set this fixture serves.
func (f *fixture) tools() []tool {
	if !f.opts.ExitOnCall {
		return fixtureTools
	}
	return append(append([]tool{}, fixtureTools...), exitTool)
}

func (f *fixture) implementation() implementation {
	return implementation{Name: serverNames[f.mode], Version: "1.0.0"}
}

// spawnChild starts a long-lived descendant, so that a shutdown test can show
// the gateway's escalation reaches the whole process tree and not only the
// process it started.
//
// This is the one place outside the gateway's own process constructor that
// calls exec.Command, and it exists because the failure it reproduces — an
// orphaned grandchild — cannot be staged any other way.
func (f *fixture) spawnChild() error {
	// noctx: deliberately not CommandContext. If the fixture's own context
	// killed the child, a shutdown test would pass without the gateway ever
	// signalling the process group, which is the thing under test.
	child := exec.Command("/bin/sh", "-c", "sleep 600") //nolint:noctx // see above
	// The child joins the fixture's process group, which is the gateway's
	// process group for the backend. Nothing here creates a new one.
	child.SysProcAttr = &syscall.SysProcAttr{}
	if err := child.Start(); err != nil {
		return fmt.Errorf("testfixtures: spawn child: %w", err)
	}
	pid := strconv.Itoa(child.Process.Pid)
	if err := os.WriteFile(f.opts.ChildPIDFile, []byte(pid), 0o600); err != nil {
		return fmt.Errorf("testfixtures: record child pid: %w", err)
	}
	return nil
}

// dumpEnvironment lets a test assert on what reached the process, rather than
// on what the gateway believes it passed.
func (f *fixture) dumpEnvironment() error {
	record := strings.Join(os.Environ(), "\n") + "\n--args--\n" + strings.Join(os.Args, "\n") + "\n"
	if err := os.WriteFile(f.opts.EnvDumpFile, []byte(record), 0o600); err != nil {
		return fmt.Errorf("testfixtures: record environment: %w", err)
	}
	return nil
}

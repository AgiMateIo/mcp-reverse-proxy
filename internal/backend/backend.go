// Package backend connects the gateway to local stdio MCP servers.
//
// Every call into github.com/modelcontextprotocol/go-sdk on the client side
// happens here, behind the interfaces declared in this file. No SDK type
// appears in an exported signature, so a version bump of that young library
// cannot ripple through the rest of the gateway.
package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

// ErrNotConnected reports use of a [Connection] whose session is gone.
var ErrNotConnected = errors.New("backend: not connected")

// Pipes are the standard streams of an already-running backend process.
//
// The process itself is spawned elsewhere: a single constructor sets Setpgid so
// that signals reach the whole descendant tree, which is why this package never
// calls exec.Command.
type Pipes struct {
	// Stdout carries the backend's JSON-RPC output.
	Stdout io.ReadCloser
	// Stdin carries JSON-RPC input to the backend. Closing it is the primary
	// stop signal for the process, which is why it belongs to whoever spawned
	// the process and not to the session over it.
	Stdin io.WriteCloser
}

// A Tool is one tool as a backend describes it, before namespacing.
type Tool struct {
	Name        string
	Title       string
	Description string
	// InputSchema is the raw JSON Schema. It is carried opaquely so that
	// callers need not name an SDK type to pass it on.
	InputSchema json.RawMessage
}

// A ToolList is the outcome of a tools/list against a backend, in the shape of
// revision 2026-07-28 whatever era produced it.
type ToolList struct {
	Tools      []Tool
	ResultType string
	Cache      Cache
}

// A ToolResult is the outcome of a tools/call against a backend.
type ToolResult struct {
	// Content is the raw JSON content array of the result.
	Content    json.RawMessage
	IsError    bool
	ResultType string
}

// A Connection is a live MCP session with one stdio backend.
type Connection interface {
	// Era reports the protocol era this session settled on. It is fixed for
	// the life of the session, so no request repeats the probe.
	Era() config.Era
	// Revision reports the protocol revision the session settled on. Legacy is
	// not one revision: a backend names its own in the handshake, and the
	// gateway adopts it.
	Revision() string
	ListTools(ctx context.Context) (ToolList, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error)
	// Close ends the session. It does not stop the backend process: the pipes
	// belong to whoever spawned it.
	Close() error
}

// A Connector establishes sessions over the pipes of running backends.
type Connector interface {
	Connect(ctx context.Context, server config.Server, p Pipes) (Connection, error)
}

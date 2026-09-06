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

// A Prompt is one prompt as a backend describes it, before namespacing.
type Prompt struct {
	Name        string
	Title       string
	Description string
	// Arguments is the raw JSON array of argument descriptors, carried
	// opaquely for the same reason a tool's schema is.
	Arguments json.RawMessage
}

// A PromptList is the outcome of a prompts/list against a backend.
type PromptList struct {
	Prompts    []Prompt
	ResultType string
	Cache      Cache
}

// A PromptResult is the outcome of a prompts/get against a backend.
type PromptResult struct {
	Description string
	// Messages is the raw JSON message array.
	Messages   json.RawMessage
	ResultType string
}

// A Resource is one resource as a backend describes it, under the backend's own
// URI rather than the gateway's.
type Resource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// A ResourceList is the outcome of a resources/list against a backend.
type ResourceList struct {
	Resources  []Resource
	ResultType string
	Cache      Cache
}

// A ResourceTemplate is one resource template as a backend describes it.
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// A ResourceTemplateList is the outcome of a resources/templates/list.
type ResourceTemplateList struct {
	Templates  []ResourceTemplate
	ResultType string
	Cache      Cache
}

// A ResourceContents is the outcome of a resources/read against a backend.
type ResourceContents struct {
	// Contents is the raw JSON contents array. The URIs inside it are the
	// backend's own; wrapping them into gateway URIs is aggregation's job.
	Contents   json.RawMessage
	ResultType string
	Cache      Cache
}

// A ChangeKind names what changed on a backend.
//
// The revision has one notification per kind and no payload beyond that, so a
// change is a signal to list again rather than a description of a difference.
type ChangeKind string

const (
	// ChangeTools reports that the backend's tool list changed.
	ChangeTools ChangeKind = "tools"
	// ChangePrompts reports that its prompt list changed.
	ChangePrompts ChangeKind = "prompts"
	// ChangeResources reports that its resource list changed.
	ChangeResources ChangeKind = "resources"
)

// A Change is one backend saying that part of its surface is no longer what it
// listed.
type Change struct {
	// ServerID names the backend the change came from.
	ServerID string
	Kind     ChangeKind
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
	ListPrompts(ctx context.Context) (PromptList, error)
	GetPrompt(ctx context.Context, name string, arguments map[string]string) (PromptResult, error)
	ListResources(ctx context.Context) (ResourceList, error)
	ListResourceTemplates(ctx context.Context) (ResourceTemplateList, error)
	// ReadResource takes the backend's own URI, never a gateway one: the
	// wrapping is undone before a request reaches this far.
	ReadResource(ctx context.Context, uri string) (ResourceContents, error)
	// Close ends the session. It does not stop the backend process: the pipes
	// belong to whoever spawned it.
	Close() error
}

// A Connector establishes sessions over the pipes of running backends.
type Connector interface {
	// Connect opens a session. Changes the backend reports are handed to
	// notify, which may be nil when nobody is listening.
	//
	// The callback is per session rather than per connector because a backend
	// is one subject's process: a connector-wide callback could not tell one
	// subject's copy of a server from another's, and would report one
	// subject's change to everybody.
	Connect(ctx context.Context, server config.Server, p Pipes, notify func(Change)) (Connection, error)
}

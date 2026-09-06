// Package frontend serves the modern HTTP Streamable MCP endpoint.
//
// Every call into github.com/modelcontextprotocol/go-sdk on the server side
// happens here, behind the interfaces declared in this file. Callers receive a
// plain http.Handler and never name an SDK type.
package frontend

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
)

// SupportedVersions are the protocol revisions the endpoint speaks. The
// gateway's whole purpose is to present one modern surface, so it advertises
// exactly one: telling a client that a legacy revision is available would
// invite the handshake this revision removed.
var SupportedVersions = []string{"2026-07-28"}

// RemovedMethods are the methods revision 2026-07-28 deleted. `ping` and
// `logging/setLevel` are gone outright; the two subscription methods are
// replaced by subscriptions/listen.
var RemovedMethods = []string{
	"ping",
	"logging/setLevel",
	"resources/subscribe",
	"resources/unsubscribe",
}

// A Backend is one stdio server as the endpoint uses it. The endpoint depends
// on this rather than on the pool, so that what serves a request can be a live
// child process or a stub.
type Backend interface {
	ListTools(ctx context.Context) (backend.ToolList, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (backend.ToolResult, error)
}

// An Endpoint is the gateway's public MCP surface.
type Endpoint interface {
	// Handler serves the MCP endpoint.
	Handler() http.Handler
}

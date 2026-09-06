package frontend

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// implName identifies the gateway to clients.
const implName = "mcp-reverse-proxy"

// An SDKEndpoint is the only [Endpoint] implementation. It is the seam that
// keeps the SDK server contained in this package.
type SDKEndpoint struct {
	version string
	backend Backend
	logger  *slog.Logger
}

// NewEndpoint returns an [Endpoint] serving one backend.
func NewEndpoint(version string, b Backend, logger *slog.Logger) *SDKEndpoint {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SDKEndpoint{version: version, backend: b, logger: logger}
}

var _ Endpoint = (*SDKEndpoint)(nil)

// Handler serves the endpoint over HTTP Streamable.
func (e *SDKEndpoint) Handler() http.Handler {
	return gate(mcp.NewStreamableHTTPHandler(e.server, &mcp.StreamableHTTPOptions{
		// Revision 2026-07-28 has no sessions, and the SDK serves it only in
		// stateless mode. It is also what lets the gateway scale out without
		// sticky routing.
		Stateless: true,
		Logger:    e.logger,
	}))
}

// server builds the MCP surface for one request.
//
// The surface is assembled per request rather than registered once, because it
// depends on the request: which subject is asking, and what its x-mcp-config
// header resolved to. A registry built at startup could not vary with either,
// and would make the front end stateful for no gain.
//
// Nothing is registered with the SDK server at all. The aggregated methods are
// answered by middleware, so that a call routes by name whether or not the
// backend that owns it answered the last listing.
func (e *SDKEndpoint) server(*http.Request) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: implName, Version: e.version}, &mcp.ServerOptions{
		// The gateway advertises none of the deprecated capabilities, so no
		// client asks for a feature it would have to refuse.
		Capabilities: &mcp.ServerCapabilities{
			Tools:     &mcp.ToolCapabilities{},
			Prompts:   &mcp.PromptCapabilities{},
			Resources: &mcp.ResourceCapabilities{},
		},
		Logger: e.logger,
	})
	// Order matters: shaping is added second so that it wraps the serving
	// middleware, and every result it produces carries the envelope this
	// revision requires.
	s.AddReceivingMiddleware(serve(e.backend))
	s.AddReceivingMiddleware(shapeResults)
	return s
}

// schemaOf carries a backend's input schema through untouched. The SDK refuses
// a tool without one, so a backend that offered none gets the empty object
// schema, which accepts anything.
func schemaOf(raw json.RawMessage) any {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return raw
}

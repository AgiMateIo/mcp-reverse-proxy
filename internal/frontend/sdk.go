package frontend

import (
	"context"
	"encoding/json"
	"fmt"
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
func (e *SDKEndpoint) server(r *http.Request) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: implName, Version: e.version}, &mcp.ServerOptions{
		// The gateway advertises none of the deprecated capabilities, so no
		// client asks for a feature it would have to refuse.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		Logger:       e.logger,
	})
	s.AddReceivingMiddleware(shapeResults)

	list, err := e.backend.ListTools(r.Context())
	if err != nil {
		// A backend that cannot be listed yields an empty surface rather than
		// a failed request: the client learns what is available, and the
		// shortened cache lifetime brings it back soon.
		e.logger.Warn("listing backend tools", "error", err)
		return s
	}
	for _, tool := range list.Tools {
		s.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			InputSchema: schemaOf(tool.InputSchema),
		}, e.callTool(tool.Name))
	}
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

// callTool forwards one tool call to the backend.
func (e *SDKEndpoint) callTool(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments json.RawMessage
		if req.Params != nil {
			arguments = req.Params.Arguments
		}
		res, err := e.backend.CallTool(ctx, name, arguments)
		if err != nil {
			return nil, fmt.Errorf("call tool %q: %w", name, err)
		}
		// The backend's content is carried opaquely; the SDK's own decoder is
		// what turns it back into typed content blocks.
		out := &mcp.CallToolResult{IsError: res.IsError}
		if len(res.Content) > 0 {
			wire, err := json.Marshal(map[string]json.RawMessage{"content": res.Content})
			if err != nil {
				return nil, fmt.Errorf("re-encode content of tool %q: %w", name, err)
			}
			if err := json.Unmarshal(wire, out); err != nil {
				return nil, fmt.Errorf("decode content of tool %q: %w", name, err)
			}
		}
		return out, nil
	}
}

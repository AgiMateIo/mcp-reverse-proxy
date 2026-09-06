package frontend

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// implName identifies the gateway to clients.
const implName = "mcp-reverse-proxy"

// An SDKEndpoint is the only [Endpoint] implementation. It is the seam that
// keeps the SDK server contained in this package.
type SDKEndpoint struct {
	server *mcp.Server
}

// NewEndpoint returns an [Endpoint] announcing the given gateway version.
func NewEndpoint(version string) *SDKEndpoint {
	return &SDKEndpoint{
		server: mcp.NewServer(&mcp.Implementation{Name: implName, Version: version}, nil),
	}
}

var _ Endpoint = (*SDKEndpoint)(nil)

// Handler serves the endpoint over HTTP Streamable.
//
// Header validation, per-request version negotiation and result shaping are
// added by the frontend work; this is the bare SDK handler.
func (e *SDKEndpoint) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return e.server }, nil)
}

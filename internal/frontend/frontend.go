// Package frontend serves the modern HTTP Streamable MCP endpoint.
//
// Every call into github.com/modelcontextprotocol/go-sdk on the server side
// happens here, behind the interfaces declared in this file. Callers receive a
// plain http.Handler and never name an SDK type.
package frontend

import "net/http"

// An Endpoint is the gateway's public MCP surface.
type Endpoint interface {
	// Handler serves the MCP endpoint.
	Handler() http.Handler
}

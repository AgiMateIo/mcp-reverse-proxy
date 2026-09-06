package backend

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// implName identifies the gateway to backends.
const implName = "mcp-reverse-proxy"

// An SDKConnector is the only [Connector] implementation. It is the seam that
// keeps the SDK client contained in this package.
type SDKConnector struct {
	version string
}

// NewConnector returns a [Connector] announcing the given gateway version.
func NewConnector(version string) *SDKConnector {
	return &SDKConnector{version: version}
}

var _ Connector = (*SDKConnector)(nil)

// Connect opens an MCP session over p.
//
// Era detection, capability masking and legacy response normalization are added
// by the backend connection work; this is the plain session handshake.
func (c *SDKConnector) Connect(ctx context.Context, serverID string, p Pipes) (Connection, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: implName, Version: c.version}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: p.Stdout, Writer: p.Stdin}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to backend %q: %w", serverID, err)
	}
	return &sdkConnection{serverID: serverID, session: session}, nil
}

// An sdkConnection adapts an SDK client session to [Connection].
type sdkConnection struct {
	serverID string
	session  *mcp.ClientSession
}

var _ Connection = (*sdkConnection)(nil)

func (c *sdkConnection) ListTools(ctx context.Context) ([]Tool, error) {
	if c.session == nil {
		return nil, fmt.Errorf("list tools on %q: %w", c.serverID, ErrNotConnected)
	}
	res, err := c.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("list tools on %q: %w", c.serverID, err)
	}
	tools := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		tool := Tool{Name: t.Name, Title: t.Title, Description: t.Description}
		if t.InputSchema != nil {
			schema, err := json.Marshal(t.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("input schema of tool %q on %q: %w", t.Name, c.serverID, err)
			}
			tool.InputSchema = schema
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (c *sdkConnection) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	if c.session == nil {
		return ToolResult{}, fmt.Errorf("call tool %q on %q: %w", name, c.serverID, ErrNotConnected)
	}
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return ToolResult{}, fmt.Errorf("call tool %q on %q: %w", name, c.serverID, err)
	}
	content, err := json.Marshal(res.Content)
	if err != nil {
		return ToolResult{}, fmt.Errorf("content of tool %q on %q: %w", name, c.serverID, err)
	}
	return ToolResult{Content: content, IsError: res.IsError}, nil
}

func (c *sdkConnection) Close() error {
	if c.session == nil {
		return nil
	}
	if err := c.session.Close(); err != nil {
		return fmt.Errorf("close session with %q: %w", c.serverID, err)
	}
	return nil
}

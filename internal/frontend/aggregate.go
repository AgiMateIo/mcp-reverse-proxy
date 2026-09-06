package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// codeInvalidParams is what this revision answers both for a name no backend
// owns and for a URI that is not the gateway's. Revision 2026-07-28 folded the
// separate "resource not found" code into it.
const codeInvalidParams int64 = -32602

// serve answers the aggregated methods from the backends directly, instead of
// registering the surface with the SDK server and letting it dispatch.
//
// Routing must not depend on listing. A registry is built from what the
// backends answered, so a backend that is down would have contributed nothing
// and a call into it would come back as "no such tool" — telling the client
// that something it saw a moment ago never existed, when the truth is that one
// process is not running. Routing by prefix against the configured backends
// says the true thing instead, and says it without asking any other backend.
func serve(b Backend) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch r := req.(type) {
			case *mcp.ListToolsRequest:
				return listTools(ctx, b)
			case *mcp.CallToolRequest:
				return callTool(ctx, b, r)
			case *mcp.ListPromptsRequest:
				return listPrompts(ctx, b)
			case *mcp.GetPromptRequest:
				return getPrompt(ctx, b, r)
			case *mcp.ListResourcesRequest:
				return listResources(ctx, b)
			case *mcp.ListResourceTemplatesRequest:
				return listResourceTemplates(ctx, b)
			case *mcp.ReadResourceRequest:
				return readResource(ctx, b, r)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

func listTools(ctx context.Context, b Backend) (mcp.Result, error) {
	s, err := b.Surface(ctx)
	if err != nil {
		return nil, err
	}
	tools := make([]*mcp.Tool, 0, len(s.Tools))
	for _, t := range s.Tools {
		tools = append(tools, &mcp.Tool{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: schemaOf(t.InputSchema),
		})
	}
	res := &mcp.ListToolsResult{Tools: tools}
	res.TTLMs = s.TTLMs
	return res, nil
}

func callTool(ctx context.Context, b Backend, req *mcp.CallToolRequest) (mcp.Result, error) {
	var name string
	var arguments json.RawMessage
	if req.Params != nil {
		name, arguments = req.Params.Name, req.Params.Arguments
	}
	res, err := b.CallTool(ctx, name, arguments)
	if err != nil {
		return nil, dispatchError(err)
	}
	out := &mcp.CallToolResult{IsError: res.IsError}
	if err := decodeInto(res.Content, "content", out); err != nil {
		return nil, fmt.Errorf("result of tool %q: %w", name, err)
	}
	return out, nil
}

func listPrompts(ctx context.Context, b Backend) (mcp.Result, error) {
	s, err := b.Surface(ctx)
	if err != nil {
		return nil, err
	}
	prompts := make([]*mcp.Prompt, 0, len(s.Prompts))
	for _, p := range s.Prompts {
		prompt := &mcp.Prompt{Name: p.Name, Title: p.Title, Description: p.Description}
		if len(p.Arguments) > 0 {
			if err := json.Unmarshal(p.Arguments, &prompt.Arguments); err != nil {
				return nil, fmt.Errorf("arguments of prompt %q: %w", p.Name, err)
			}
		}
		prompts = append(prompts, prompt)
	}
	res := &mcp.ListPromptsResult{Prompts: prompts}
	res.TTLMs = s.TTLMs
	return res, nil
}

func getPrompt(ctx context.Context, b Backend, req *mcp.GetPromptRequest) (mcp.Result, error) {
	var name string
	var arguments map[string]string
	if req.Params != nil {
		name, arguments = req.Params.Name, req.Params.Arguments
	}
	res, err := b.GetPrompt(ctx, name, arguments)
	if err != nil {
		return nil, dispatchError(err)
	}
	out := &mcp.GetPromptResult{Description: res.Description}
	if len(res.Messages) > 0 {
		if err := json.Unmarshal(res.Messages, &out.Messages); err != nil {
			return nil, fmt.Errorf("messages of prompt %q: %w", name, err)
		}
	}
	return out, nil
}

func listResources(ctx context.Context, b Backend) (mcp.Result, error) {
	s, err := b.Surface(ctx)
	if err != nil {
		return nil, err
	}
	resources := make([]*mcp.Resource, 0, len(s.Resources))
	for _, r := range s.Resources {
		resources = append(resources, &mcp.Resource{
			URI:         r.URI,
			Name:        r.Name,
			Title:       r.Title,
			Description: r.Description,
			MIMEType:    r.MIMEType,
		})
	}
	res := &mcp.ListResourcesResult{Resources: resources}
	res.TTLMs = s.TTLMs
	return res, nil
}

func listResourceTemplates(ctx context.Context, b Backend) (mcp.Result, error) {
	s, err := b.Surface(ctx)
	if err != nil {
		return nil, err
	}
	templates := make([]*mcp.ResourceTemplate, 0, len(s.Templates))
	for _, t := range s.Templates {
		templates = append(templates, &mcp.ResourceTemplate{
			URITemplate: t.URITemplate,
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			MIMEType:    t.MIMEType,
		})
	}
	res := &mcp.ListResourceTemplatesResult{ResourceTemplates: templates}
	res.TTLMs = s.TTLMs
	return res, nil
}

func readResource(ctx context.Context, b Backend, req *mcp.ReadResourceRequest) (mcp.Result, error) {
	var uri string
	if req.Params != nil {
		uri = req.Params.URI
	}
	res, err := b.ReadResource(ctx, uri)
	if err != nil {
		return nil, dispatchError(err)
	}
	out := &mcp.ReadResourceResult{}
	if len(res.Contents) > 0 {
		if err := json.Unmarshal(res.Contents, &out.Contents); err != nil {
			return nil, fmt.Errorf("contents of resource %q: %w", uri, err)
		}
	}
	out.TTLMs = res.Cache.TTLMs
	return out, nil
}

// dispatchError gives a routing failure the code this revision defines for it.
// A backend that is merely down keeps its own message, which names the backend:
// that is what tells the client the tool exists and the process does not.
func dispatchError(err error) error {
	if errors.Is(err, aggregate.ErrNotFound) ||
		errors.Is(err, aggregate.ErrBadURI) ||
		errors.Is(err, aggregate.ErrUnknownBackend) {
		return &jsonrpc.Error{Code: codeInvalidParams, Message: err.Error()}
	}
	return err
}

// decodeInto puts a raw JSON array back into the SDK's typed field. The content
// of a result is carried opaquely from the backend, and the SDK's own decoder
// is what turns it into typed blocks.
func decodeInto(raw json.RawMessage, field string, out any) error {
	if len(raw) == 0 {
		return nil
	}
	wire, err := json.Marshal(map[string]json.RawMessage{field: raw})
	if err != nil {
		return fmt.Errorf("re-encode %s: %w", field, err)
	}
	if err := json.Unmarshal(wire, out); err != nil {
		return fmt.Errorf("decode %s: %w", field, err)
	}
	return nil
}

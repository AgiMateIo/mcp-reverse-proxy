package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// implName identifies the gateway to backends.
const implName = "mcp-reverse-proxy"

// An SDKConnector is the only [Connector] implementation. It is the seam that
// keeps the SDK client contained in this package.
type SDKConnector struct {
	version      string
	probeTimeout time.Duration
	logger       *slog.Logger
}

// NewConnector returns a [Connector] announcing the given gateway version and
// bounding every era probe by probeTimeout.
func NewConnector(version string, probeTimeout time.Duration, logger *slog.Logger) *SDKConnector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &SDKConnector{version: version, probeTimeout: probeTimeout, logger: logger}
}

var _ Connector = (*SDKConnector)(nil)

// Connect opens an MCP session over p, applying the server's era policy.
func (c *SDKConnector) Connect(ctx context.Context, server config.Server, p Pipes, notify func(Change)) (Connection, error) {
	transport := &eraTransport{
		// Stdin belongs to whoever spawned the process: closing it is the stop
		// signal, and a session ending must not stop a process the pool still
		// owns.
		inner: &mcp.IOTransport{Reader: p.Stdout, Writer: nopCloser{p.Stdin}},
		opts: interposeOptions{
			serverID:     server.ID,
			era:          server.Era,
			probeTimeout: c.probeTimeout,
			logger:       c.logger,
		},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: implName, Version: c.version}, &mcp.ClientOptions{
		// The gateway advertises none of roots, sampling, elicitation or
		// logging. A correctly written backend then never sends the
		// server-to-client requests that a stateless front end could not
		// carry, and one that sends them anyway is refused by the transport.
		Capabilities: &mcp.ClientCapabilities{},
		Logger:       c.logger,
		// One handler per kind, and the same handler whichever era the backend
		// speaks: a modern backend delivers these over the subscriptions/listen
		// stream the SDK opens during Connect, a legacy one over the ordinary
		// notification channel, and the client folds both into this call. That
		// fold is what normalizing a legacy notification amounts to.
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			report(notify, server.ID, ChangeTools)
		},
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
			report(notify, server.ID, ChangePrompts)
		},
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
			report(notify, server.ID, ChangeResources)
		},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		// An error raised inside the connection reaches here flattened, so the
		// era policy's own verdict is read from the transport instead.
		if conn := transport.result(); conn != nil {
			if _, mismatch := conn.outcome(); mismatch != nil {
				_ = conn.Close()
				return nil, mismatch
			}
			// The SDK does not always close the connection it failed to
			// establish, and this one owns a goroutine.
			_ = conn.Close()
		}
		return nil, fmt.Errorf("connect to backend %q: %w", server.ID, err)
	}
	era, _ := transport.result().outcome()
	revision := ""
	if res := session.InitializeResult(); res != nil {
		revision = res.ProtocolVersion
	}
	return &sdkConnection{serverID: server.ID, era: era, revision: revision, session: session}, nil
}

// report hands one change on, if anybody is listening.
func report(notify func(Change), serverID string, kind ChangeKind) {
	if notify == nil {
		return
	}
	notify(Change{ServerID: serverID, Kind: kind})
}

// nopCloser keeps the SDK's connection from closing a pipe it does not own.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// An sdkConnection adapts an SDK client session to [Connection].
type sdkConnection struct {
	serverID string
	era      config.Era
	revision string
	session  *mcp.ClientSession
}

var _ Connection = (*sdkConnection)(nil)

// Era implements [Connection]. The era was settled once, when the session was
// established, and is not probed again.
func (c *sdkConnection) Era() config.Era { return c.era }

// Revision implements [Connection], reporting the revision the backend named
// rather than the one the gateway offered.
func (c *sdkConnection) Revision() string { return c.revision }

func (c *sdkConnection) ListTools(ctx context.Context) (ToolList, error) {
	if c.session == nil {
		return ToolList{}, fmt.Errorf("list tools on %q: %w", c.serverID, ErrNotConnected)
	}
	res, err := c.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return ToolList{}, fmt.Errorf("list tools on %q: %w", c.serverID, normalizeError(c.serverID, err))
	}
	tools := make([]Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		tool := Tool{Name: t.Name, Title: t.Title, Description: t.Description}
		if t.InputSchema != nil {
			schema, err := json.Marshal(t.InputSchema)
			if err != nil {
				return ToolList{}, fmt.Errorf("input schema of tool %q on %q: %w", t.Name, c.serverID, err)
			}
			tool.InputSchema = schema
		}
		tools = append(tools, tool)
	}
	return ToolList{
		Tools:      tools,
		ResultType: normalizeResultType(string(res.ResultType)),
		Cache:      normalizeCache(Cache{TTLMs: res.TTLMs, CacheScope: res.CacheScope}),
	}, nil
}

func (c *sdkConnection) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	if c.session == nil {
		return ToolResult{}, fmt.Errorf("call tool %q on %q: %w", name, c.serverID, ErrNotConnected)
	}
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return ToolResult{}, fmt.Errorf("call tool %q on %q: %w", name, c.serverID, normalizeError(c.serverID, err))
	}
	content, err := json.Marshal(res.Content)
	if err != nil {
		return ToolResult{}, fmt.Errorf("content of tool %q on %q: %w", name, c.serverID, err)
	}
	return ToolResult{
		Content: content,
		IsError: res.IsError,
		// The SDK keeps a call result's own type unexported, so there is
		// nothing to preserve: the gateway emits "complete" on every result
		// regardless, since the input_required flow is not implemented.
		ResultType: ResultTypeComplete,
	}, nil
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

func (c *sdkConnection) ListPrompts(ctx context.Context) (PromptList, error) {
	if c.session == nil {
		return PromptList{}, fmt.Errorf("list prompts on %q: %w", c.serverID, ErrNotConnected)
	}
	res, err := c.session.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return PromptList{}, fmt.Errorf("list prompts on %q: %w", c.serverID, normalizeError(c.serverID, err))
	}
	prompts := make([]Prompt, 0, len(res.Prompts))
	for _, p := range res.Prompts {
		prompt := Prompt{Name: p.Name, Title: p.Title, Description: p.Description}
		if len(p.Arguments) > 0 {
			args, err := json.Marshal(p.Arguments)
			if err != nil {
				return PromptList{}, fmt.Errorf("arguments of prompt %q on %q: %w", p.Name, c.serverID, err)
			}
			prompt.Arguments = args
		}
		prompts = append(prompts, prompt)
	}
	return PromptList{
		Prompts:    prompts,
		ResultType: normalizeResultType(string(res.ResultType)),
		Cache:      normalizeCache(Cache{TTLMs: res.TTLMs, CacheScope: res.CacheScope}),
	}, nil
}

func (c *sdkConnection) GetPrompt(ctx context.Context, name string, arguments map[string]string) (PromptResult, error) {
	if c.session == nil {
		return PromptResult{}, fmt.Errorf("get prompt %q on %q: %w", name, c.serverID, ErrNotConnected)
	}
	res, err := c.session.GetPrompt(ctx, &mcp.GetPromptParams{Name: name, Arguments: arguments})
	if err != nil {
		return PromptResult{}, fmt.Errorf("get prompt %q on %q: %w", name, c.serverID, normalizeError(c.serverID, err))
	}
	messages, err := json.Marshal(res.Messages)
	if err != nil {
		return PromptResult{}, fmt.Errorf("messages of prompt %q on %q: %w", name, c.serverID, err)
	}
	return PromptResult{
		Description: res.Description,
		Messages:    messages,
		// As with a tool call, the SDK keeps this result's own type unexported;
		// the gateway emits "complete" because it implements no other flow.
		ResultType: ResultTypeComplete,
	}, nil
}

func (c *sdkConnection) ListResources(ctx context.Context) (ResourceList, error) {
	if c.session == nil {
		return ResourceList{}, fmt.Errorf("list resources on %q: %w", c.serverID, ErrNotConnected)
	}
	res, err := c.session.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		return ResourceList{}, fmt.Errorf("list resources on %q: %w", c.serverID, normalizeError(c.serverID, err))
	}
	resources := make([]Resource, 0, len(res.Resources))
	for _, r := range res.Resources {
		resources = append(resources, Resource{
			URI:         r.URI,
			Name:        r.Name,
			Title:       r.Title,
			Description: r.Description,
			MIMEType:    r.MIMEType,
		})
	}
	return ResourceList{
		Resources:  resources,
		ResultType: normalizeResultType(string(res.ResultType)),
		Cache:      normalizeCache(Cache{TTLMs: res.TTLMs, CacheScope: res.CacheScope}),
	}, nil
}

func (c *sdkConnection) ListResourceTemplates(ctx context.Context) (ResourceTemplateList, error) {
	if c.session == nil {
		return ResourceTemplateList{}, fmt.Errorf("list resource templates on %q: %w", c.serverID, ErrNotConnected)
	}
	res, err := c.session.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
	if err != nil {
		return ResourceTemplateList{}, fmt.Errorf("list resource templates on %q: %w", c.serverID, normalizeError(c.serverID, err))
	}
	templates := make([]ResourceTemplate, 0, len(res.ResourceTemplates))
	for _, t := range res.ResourceTemplates {
		templates = append(templates, ResourceTemplate{
			URITemplate: t.URITemplate,
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			MIMEType:    t.MIMEType,
		})
	}
	return ResourceTemplateList{
		Templates:  templates,
		ResultType: normalizeResultType(string(res.ResultType)),
		Cache:      normalizeCache(Cache{TTLMs: res.TTLMs, CacheScope: res.CacheScope}),
	}, nil
}

func (c *sdkConnection) ReadResource(ctx context.Context, uri string) (ResourceContents, error) {
	if c.session == nil {
		return ResourceContents{}, fmt.Errorf("read resource %q on %q: %w", uri, c.serverID, ErrNotConnected)
	}
	res, err := c.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		// A legacy backend answers an unknown resource with -32002, which this
		// revision folds into invalid params; the normalizer does that.
		return ResourceContents{}, fmt.Errorf("read resource %q on %q: %w", uri, c.serverID, normalizeError(c.serverID, err))
	}
	contents, err := json.Marshal(res.Contents)
	if err != nil {
		return ResourceContents{}, fmt.Errorf("contents of resource %q on %q: %w", uri, c.serverID, err)
	}
	return ResourceContents{
		Contents:   contents,
		ResultType: ResultTypeComplete,
		Cache:      normalizeCache(Cache{TTLMs: res.TTLMs, CacheScope: res.CacheScope}),
	}, nil
}

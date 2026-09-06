package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
)

// ErrBackendUnavailable reports a backend that did not serve a request. It is
// distinct from a backend answering with an error: a client can retry the
// first and cannot do anything about the second.
var ErrBackendUnavailable = errors.New("backend unavailable")

// codeMethodNotFound is how a backend says it has no prompts or resources at
// all. That is an answer, not a failure, so it costs the surface nothing.
const codeMethodNotFound int64 = -32601

// DefaultListTimeout bounds one backend's contribution to a listing. Without
// it the slowest backend decides how long every client waits, which is the
// opposite of what resilience to an unavailable backend is supposed to mean.
const DefaultListTimeout = 5 * time.Second

// A Source is one backend as aggregation uses it.
type Source interface {
	ListTools(ctx context.Context) (backend.ToolList, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (backend.ToolResult, error)
	ListPrompts(ctx context.Context) (backend.PromptList, error)
	GetPrompt(ctx context.Context, name string, arguments map[string]string) (backend.PromptResult, error)
	ListResources(ctx context.Context) (backend.ResourceList, error)
	ListResourceTemplates(ctx context.Context) (backend.ResourceTemplateList, error)
	ReadResource(ctx context.Context, uri string) (backend.ResourceContents, error)
}

// A Gateway presents several backends as one surface.
type Gateway struct {
	sources map[string]Source
	router  *Router
	timeout time.Duration
	logger  *slog.Logger
}

// New returns a gateway over the backends of one resolved configuration,
// keyed by their identifiers.
func New(sources map[string]Source, timeout time.Duration, logger *slog.Logger) *Gateway {
	if timeout <= 0 {
		timeout = DefaultListTimeout
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	return &Gateway{sources: sources, router: NewRouter(ids), timeout: timeout, logger: logger}
}

// Router exposes the name routing of this configuration, so that a caller can
// resolve a name without assembling the whole surface.
func (g *Gateway) Router() *Router { return g.router }

// Surface lists every backend and folds the answers into one.
//
// A backend that fails is left out and named in the log rather than failing the
// listing: the client learns what is available, and the shortened lifetime of
// the result brings it back for the rest.
func (g *Gateway) Surface(ctx context.Context) (Surface, error) {
	ids := slices.Sorted(maps.Keys(g.sources))
	listings := make([]Listing, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			// Per backend, so that one that never answers cannot hold the
			// whole listing past the client's patience.
			ctx, cancel := context.WithTimeout(ctx, g.timeout)
			defer cancel()
			listings[i] = g.list(ctx, id, g.sources[id])
		})
	}
	wg.Wait()

	for _, l := range listings {
		if l.Err != nil {
			g.logger.Warn("backend left out of the listing", "backend", l.Backend, "error", l.Err)
		}
	}
	return Assemble(listings)
}

// list gathers one backend's entries.
func (g *Gateway) list(ctx context.Context, id string, s Source) Listing {
	l := Listing{Backend: id}

	tools, err := s.ListTools(ctx)
	if err != nil {
		if !absent(err) {
			l.Err = err
			return l
		}
	}
	l.Tools = tools.Tools
	l.TTLMs = tools.Cache.TTLMs

	prompts, err := s.ListPrompts(ctx)
	if err != nil && !absent(err) {
		l.Err = err
		return l
	}
	l.Prompts = prompts.Prompts

	resources, err := s.ListResources(ctx)
	if err != nil && !absent(err) {
		l.Err = err
		return l
	}
	l.Resources = resources.Resources

	templates, err := s.ListResourceTemplates(ctx)
	if err != nil && !absent(err) {
		l.Err = err
		return l
	}
	l.Templates = templates.Templates
	return l
}

// absent reports an error that means the backend has none of this kind of
// entry. Most backends implement tools and nothing else, so answering
// "method not found" must cost them neither their place in the listing nor the
// listing's cache lifetime.
func absent(err error) bool {
	var protocol *backend.ProtocolError
	return errors.As(err, &protocol) && protocol.Code == codeMethodNotFound
}

// CallTool routes a namespaced tool name to its backend and calls it under the
// name that backend knows.
func (g *Gateway) CallTool(ctx context.Context, qualified string, arguments json.RawMessage) (backend.ToolResult, error) {
	id, name, err := g.router.Route(qualified)
	if err != nil {
		return backend.ToolResult{}, err
	}
	res, err := g.sources[id].CallTool(ctx, name, arguments)
	if err != nil {
		return backend.ToolResult{}, unavailable(id, err)
	}
	return res, nil
}

// GetPrompt routes a namespaced prompt name to its backend.
func (g *Gateway) GetPrompt(ctx context.Context, qualified string, arguments map[string]string) (backend.PromptResult, error) {
	id, name, err := g.router.Route(qualified)
	if err != nil {
		return backend.PromptResult{}, err
	}
	res, err := g.sources[id].GetPrompt(ctx, name, arguments)
	if err != nil {
		return backend.PromptResult{}, unavailable(id, err)
	}
	return res, nil
}

// ReadResource unwraps a gateway URI and reads the resource from the backend
// that owns it, under the backend's own URI.
func (g *Gateway) ReadResource(ctx context.Context, gatewayURI string) (backend.ResourceContents, error) {
	id, uri, err := UnwrapURI(gatewayURI)
	if err != nil {
		return backend.ResourceContents{}, err
	}
	source, known := g.sources[id]
	if !known {
		return backend.ResourceContents{}, fmt.Errorf("%q: %w: %q", gatewayURI, ErrUnknownBackend, id)
	}
	res, err := source.ReadResource(ctx, uri)
	if err != nil {
		return backend.ResourceContents{}, unavailable(id, err)
	}
	return res, nil
}

// unavailable names the backend in a failure that was not the backend's own
// answer. A JSON-RPC error is an answer and passes through: reporting a tool's
// own refusal as an unavailable backend would send a client retrying something
// that will never succeed.
func unavailable(id string, err error) error {
	var protocol *backend.ProtocolError
	if errors.As(err, &protocol) {
		return err
	}
	return fmt.Errorf("backend %q is unavailable: %w: %w", id, ErrBackendUnavailable, err)
}

package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
)

// Failures of the layers a request passes before reaching a backend. They are
// not client errors and not backend errors: they say the request arrived here
// without going through a middleware it was supposed to.
var (
	ErrUnauthenticated = errors.New("the request carries no authenticated subject")
	ErrUnresolved      = errors.New("the request carries no resolved configuration")
)

// A Tenant is the aggregated surface assembled for whoever is asking.
//
// It is where the layers meet: authentication put a subject on the request,
// resolution put a server set there, and the pool keys its processes by the
// pair. Nothing is held between requests — the surface is built per request —
// which is what keeps two subjects from ever being served by one another's
// processes, however identical their configurations.
type Tenant struct {
	pool    *pool.Pool
	timeout time.Duration
	logger  *slog.Logger
}

// NewTenant returns the gateway's surface over a process pool. timeout bounds
// one backend's contribution to a listing; zero means the aggregator's own
// default.
func NewTenant(p *pool.Pool, timeout time.Duration, logger *slog.Logger) *Tenant {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Tenant{pool: p, timeout: timeout, logger: logger}
}

var _ Backend = (*Tenant)(nil)

// gateway builds the surface this request is entitled to.
func (t *Tenant) gateway(ctx context.Context) (*aggregate.Gateway, error) {
	subject, ok := auth.SubjectFromContext(ctx)
	if !ok {
		return nil, ErrUnauthenticated
	}
	resolved, ok := config.ResolvedFromContext(ctx)
	if !ok {
		return nil, ErrUnresolved
	}
	sources := make(map[string]aggregate.Source, len(resolved))
	for _, server := range resolved {
		sources[server.ID] = t.pool.For(subject, server)
	}
	return aggregate.New(sources, t.timeout, t.logger), nil
}

func (t *Tenant) Surface(ctx context.Context) (aggregate.Surface, error) {
	g, err := t.gateway(ctx)
	if err != nil {
		return aggregate.Surface{}, err
	}
	return g.Surface(ctx)
}

func (t *Tenant) CallTool(ctx context.Context, name string, arguments json.RawMessage) (backend.ToolResult, error) {
	g, err := t.gateway(ctx)
	if err != nil {
		return backend.ToolResult{}, err
	}
	return g.CallTool(ctx, name, arguments)
}

func (t *Tenant) GetPrompt(ctx context.Context, name string, arguments map[string]string) (backend.PromptResult, error) {
	g, err := t.gateway(ctx)
	if err != nil {
		return backend.PromptResult{}, err
	}
	return g.GetPrompt(ctx, name, arguments)
}

func (t *Tenant) ReadResource(ctx context.Context, uri string) (backend.ResourceContents, error) {
	g, err := t.gateway(ctx)
	if err != nil {
		return backend.ResourceContents{}, err
	}
	return g.ReadResource(ctx, uri)
}

// Listen opens this subject's stream of backend changes.
//
// The subscription is registered with the pool rather than with a process, so
// it survives the backend being evicted, expiring or restarting underneath it:
// the client asked about a server, not about the process serving it at the
// moment it asked.
func (t *Tenant) Listen(ctx context.Context) (*aggregate.Listener, error) {
	g, err := t.gateway(ctx)
	if err != nil {
		return nil, err
	}
	return g.Listen(ctx)
}

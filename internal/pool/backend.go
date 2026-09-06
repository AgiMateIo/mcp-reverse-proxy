package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

// ErrRetryable marks a failure caused by a backend process going away rather
// than by the request itself. The protocol is stateless, so repeating the
// request is enough: the next one starts a fresh process.
var ErrRetryable = errors.New("backend unavailable")

// A Backend is one configured server together with the process currently
// serving it. It starts that process on first use and replaces it if it exits.
type Backend struct {
	server    config.Server
	connector backend.Connector
	policy    StopPolicy
	logger    *slog.Logger

	mu     sync.Mutex
	proc   *Process
	conn   backend.Connection
	closed bool
}

// NewBackend returns a Backend for server. No process is started until one is
// needed.
func NewBackend(server config.Server, connector backend.Connector, policy StopPolicy, logger *slog.Logger) *Backend {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Backend{server: server, connector: connector, policy: policy, logger: logger}
}

// ListTools lists the backend's tools, starting or restarting it as needed.
func (b *Backend) ListTools(ctx context.Context) (backend.ToolList, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.ToolList{}, err
	}
	res, err := conn.ListTools(ctx)
	if err != nil {
		return backend.ToolList{}, b.classify(proc, err)
	}
	return res, nil
}

// CallTool calls a tool on the backend, starting or restarting it as needed.
func (b *Backend) CallTool(ctx context.Context, name string, arguments json.RawMessage) (backend.ToolResult, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.ToolResult{}, err
	}
	res, err := conn.CallTool(ctx, name, arguments)
	if err != nil {
		return backend.ToolResult{}, b.classify(proc, err)
	}
	return res, nil
}

// PID reports the process currently serving this backend, or zero if none is
// running. It exists so that a caller can tell one process from its
// replacement.
func (b *Backend) PID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proc == nil || b.proc.Exited() {
		return 0
	}
	return b.proc.PID()
}

// session returns a live session, starting a process if there is none and
// replacing one that has exited.
func (b *Backend) session(ctx context.Context) (backend.Connection, *Process, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, fmt.Errorf("backend %q is closed: %w", b.server.ID, backend.ErrNotConnected)
	}
	if b.proc != nil && !b.proc.Exited() {
		return b.conn, b.proc, nil
	}
	if b.proc != nil {
		b.logger.Info("backend exited; starting a replacement", "server", b.server.ID, "pid", b.proc.PID())
		if err := b.discardLocked(ctx); err != nil {
			// Tidying up after a process that is already gone can fail; it
			// must not stop the replacement from starting.
			b.logger.Warn("cleaning up after an exited backend", "server", b.server.ID, "error", err)
		}
	}
	proc, err := Spawn(ctx, b.server, b.policy, b.logger)
	if err != nil {
		return nil, nil, err
	}
	conn, err := b.connector.Connect(ctx, b.server, proc.Pipes())
	if err != nil {
		// The process is of no use without a session over it.
		_ = proc.Stop(ctx)
		return nil, nil, err
	}
	b.proc, b.conn = proc, conn
	return conn, proc, nil
}

// classify turns a call failure into a retryable one when the cause was the
// process going away, and forgets the dead process so the next call starts a
// fresh one.
func (b *Backend) classify(proc *Process, err error) error {
	// The exit is observed by a separate goroutine, so a request that raced it
	// may see the lost connection before the process is known to be gone.
	if !proc.Exited() && !errors.Is(err, backend.ErrConnectionLost) {
		return err
	}
	b.mu.Lock()
	if b.proc == proc {
		// Leave the process in place but unusable; session replaces it, and
		// doing that here would need a context this path does not have.
		b.conn = nil
	}
	b.mu.Unlock()
	return fmt.Errorf("backend %q exited during the request: %w",
		b.server.ID, errors.Join(err, ErrRetryable))
}

// Close stops the backend for good.
func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return b.discardLocked(ctx)
}

func (b *Backend) discardLocked(ctx context.Context) error {
	var errs []error
	if b.conn != nil {
		errs = append(errs, b.conn.Close())
		b.conn = nil
	}
	if b.proc != nil {
		errs = append(errs, b.proc.Stop(ctx))
		b.proc = nil
	}
	return errors.Join(errs...)
}

// ListPrompts lists the backend's prompts, starting or restarting it as needed.
func (b *Backend) ListPrompts(ctx context.Context) (backend.PromptList, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.PromptList{}, err
	}
	res, err := conn.ListPrompts(ctx)
	if err != nil {
		return backend.PromptList{}, b.classify(proc, err)
	}
	return res, nil
}

// GetPrompt fetches a prompt from the backend.
func (b *Backend) GetPrompt(ctx context.Context, name string, arguments map[string]string) (backend.PromptResult, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.PromptResult{}, err
	}
	res, err := conn.GetPrompt(ctx, name, arguments)
	if err != nil {
		return backend.PromptResult{}, b.classify(proc, err)
	}
	return res, nil
}

// ListResources lists the backend's resources.
func (b *Backend) ListResources(ctx context.Context) (backend.ResourceList, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.ResourceList{}, err
	}
	res, err := conn.ListResources(ctx)
	if err != nil {
		return backend.ResourceList{}, b.classify(proc, err)
	}
	return res, nil
}

// ListResourceTemplates lists the backend's resource templates.
func (b *Backend) ListResourceTemplates(ctx context.Context) (backend.ResourceTemplateList, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.ResourceTemplateList{}, err
	}
	res, err := conn.ListResourceTemplates(ctx)
	if err != nil {
		return backend.ResourceTemplateList{}, b.classify(proc, err)
	}
	return res, nil
}

// ReadResource reads one of the backend's resources, under the backend's own
// URI.
func (b *Backend) ReadResource(ctx context.Context, uri string) (backend.ResourceContents, error) {
	conn, proc, err := b.session(ctx)
	if err != nil {
		return backend.ResourceContents{}, err
	}
	res, err := conn.ReadResource(ctx, uri)
	if err != nil {
		return backend.ResourceContents{}, b.classify(proc, err)
	}
	return res, nil
}

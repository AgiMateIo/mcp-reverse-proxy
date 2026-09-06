package pool

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

// A Process is one running stdio backend.
type Process struct {
	server config.Server
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	policy StopPolicy
	logger *slog.Logger

	wg   sync.WaitGroup
	done chan struct{}

	stopOnce sync.Once
	stopErr  error

	mu      sync.Mutex
	waitErr error
}

// Spawn starts a backend as a child process.
//
// It is the only place in the gateway that starts a process. Every child is put
// in its own process group here, so that stopping one reaches the helpers it
// goes on to spawn; a second spawn site would be a second way to leak a process
// tree.
//
// ctx bounds the start, not the life of the process: the process outlives the
// request that needed it, and is stopped through [Process.Stop].
func Spawn(ctx context.Context, server config.Server, policy StopPolicy, logger *slog.Logger) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("spawn backend %q: %w", server.ID, err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// G204: the command and arguments come from the deployment's own
	// configuration, resolved under the header policy — an allowlist of
	// commands and a denylist of environment keys — before reaching here. The
	// define-new policy mode exists to permit exactly this, so the guard is
	// that policy, not the absence of variables.
	//
	// noctx: exec.CommandContext would bind the process to the context of
	// whichever request first needed it, and kill the backend when that
	// request ends. The process outlives the request by design; its lifetime
	// belongs to Stop.
	cmd := exec.Command(server.Command, server.Args...) //nolint:gosec,noctx // see above
	isolate(cmd)
	cmd.Env = environ(server.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin of backend %q: %w", server.ID, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout of backend %q: %w", server.ID, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr of backend %q: %w", server.ID, err)
	}

	p := &Process{
		server: server, cmd: cmd, stdin: stdin, stdout: stdout,
		policy: policy, logger: logger, done: make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start backend %q: %w", server.ID, err)
	}
	p.wg.Go(func() { p.drainStderr(stderr) })
	p.wg.Go(p.reap)
	return p, nil
}

// environ renders a backend's configured environment. Only what the
// configuration names is passed: the gateway's own environment belongs to the
// gateway, and in a multi-tenant deployment it is not the subject's to read.
func environ(env config.Env) []string {
	out := make([]string, 0, len(env))
	for _, k := range env.Keys() {
		out = append(out, k+"="+env[k].Reveal())
	}
	return out
}

// Pipes returns the streams an MCP session runs over.
func (p *Process) Pipes() backend.Pipes {
	return backend.Pipes{Stdout: p.stdout, Stdin: p.stdin}
}

// PID is the process identifier, which is also its process group.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done is closed when the process has exited, however it exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Exited reports whether the process is already gone.
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// drainStderr copies the backend's diagnostics into the gateway's log. Writing
// to standard error is how most MCP servers report progress; it is not a
// failure and must not be treated as one.
func (p *Process) drainStderr(stderr io.ReadCloser) {
	scan := bufio.NewScanner(stderr)
	for scan.Scan() {
		p.logger.Info("backend stderr", "server", p.server.ID, "line", scan.Text())
	}
}

// reap waits for the process and records how it ended.
func (p *Process) reap() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	close(p.done)
}

// Stop ends the process, escalating from closing standard input to signalling
// its process group.
func (p *Process) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.stopErr = stop(ctx, p, p.policy)
		// The reaping goroutine and the stderr drain both end with the
		// process; waiting for them here is what keeps a stopped backend from
		// leaving a goroutine behind.
		p.wg.Wait()
	})
	return p.stopErr
}

func (p *Process) closeStdin() error {
	if err := p.stdin.Close(); err != nil {
		return fmt.Errorf("close stdin of backend %q: %w", p.server.ID, err)
	}
	return nil
}

func (p *Process) signal(sig syscall.Signal) error {
	if p.Exited() {
		return nil
	}
	p.logger.Info("escalating backend shutdown", "server", p.server.ID, "signal", sig.String())
	return signalGroup(p.PID(), sig)
}

func (p *Process) exited() <-chan struct{} { return p.done }

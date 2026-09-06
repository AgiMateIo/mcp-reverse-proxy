package pool_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

const probeTimeout = 200 * time.Millisecond

// testPolicy escalates quickly, so that a test that must reach a signal does
// not wait out a production-sized grace period.
var testPolicy = pool.StopPolicy{AfterStdin: 250 * time.Millisecond, AfterTerm: 2 * time.Second}

// fixtureServer describes this test binary running as a fixture backend.
func fixtureServer(t *testing.T, id string, mode testfixtures.Mode, opts testfixtures.Options) config.Server {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	env := config.Env{}
	for k, v := range testfixtures.Env(mode, opts) {
		env[k] = config.Secret(v)
	}
	return config.Server{ID: id, Command: self, Env: env, Era: config.EraAuto}
}

// A recordingLogger collects log output for assertions without racing the
// goroutines that write it.
type recordingLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *recordingLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *recordingLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *recordingLogger) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, nil))
}

// Task 4.1: a backend is started in its own process group and speaks MCP over
// its pipes.
func TestSpawnAndRoundTrip(t *testing.T) {
	t.Parallel()
	server := fixtureServer(t, "modern", testfixtures.ModeModern, testfixtures.Options{})
	proc, err := pool.Spawn(t.Context(), server, testPolicy, nil)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(context.WithoutCancel(t.Context())) })

	// Its own process group is what lets termination reach descendants.
	pgid, err := syscall.Getpgid(proc.PID())
	if err != nil {
		t.Fatalf("read the process group: %v", err)
	}
	if pgid != proc.PID() {
		t.Errorf("process group = %d, want %d — the backend leads its own group", pgid, proc.PID())
	}

	conn, err := backend.NewConnector("test", probeTimeout, nil).
		Connect(t.Context(), server, proc.Pipes(), nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	list, err := conn.ListTools(t.Context())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) == 0 {
		t.Error("the backend returned no tools")
	}
	if conn.Era() != config.EraModern {
		t.Errorf("Era = %q, want %q", conn.Era(), config.EraModern)
	}
}

// Task 4.13: stopping a backend must leave no descendant behind, which is why
// the signal goes to the process group rather than the process.
func TestStopLeavesNoDescendant(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	server := fixtureServer(t, "spawner", testfixtures.ModeModern, testfixtures.Options{
		ChildPIDFile: pidFile,
		// Ignoring the polite signal is what forces the escalation.
		IgnoreStdinClose: true,
	})
	proc, err := pool.Spawn(t.Context(), server, testPolicy, nil)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	childPID := waitForPIDFile(t, pidFile)
	if !processAlive(childPID) {
		t.Fatalf("the fixture's child %d was not running to begin with", childPID)
	}

	if err := proc.Stop(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !proc.Exited() {
		t.Error("the backend is still running after Stop")
	}
	// The descendant is reaped by init, so give it a moment to actually go.
	for range 100 {
		if !processAlive(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the backend's descendant %d survived the shutdown", childPID)
}

// Task 4.14: writing to standard error is how most MCP servers report progress.
// It is recorded, and it is not a failure.
func TestStderrIsLoggedAndNotFatal(t *testing.T) {
	t.Parallel()
	const line = "loading index, please wait"
	logs := &recordingLogger{}
	server := fixtureServer(t, "noisy", testfixtures.ModeModern, testfixtures.Options{Stderr: line})

	b := pool.NewBackend(server, backend.NewConnector("test", probeTimeout, nil), testPolicy, logs.logger())
	t.Cleanup(func() { _ = b.Close(context.WithoutCancel(t.Context())) })

	list, err := b.ListTools(t.Context())
	if err != nil {
		t.Fatalf("a backend that wrote to stderr failed to serve: %v", err)
	}
	if len(list.Tools) == 0 {
		t.Error("the backend returned no tools")
	}
	for range 100 {
		if strings.Contains(logs.String(), line) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the backend's stderr was not recorded in the log: %s", logs.String())
}

// Task 4.15: a backend that exits on its own fails the request in flight as
// retryable, and the next request reaches a fresh process.
func TestRestartAfterUnexpectedExit(t *testing.T) {
	t.Parallel()
	logs := &recordingLogger{}
	// The fixture dies instead of answering this one tool, which puts the exit
	// squarely inside a request rather than between two of them.
	server := fixtureServer(t, "modern", testfixtures.ModeModern, testfixtures.Options{ExitOnCall: true})
	b := pool.NewBackend(server, backend.NewConnector("test", probeTimeout, nil), testPolicy, logs.logger())
	t.Cleanup(func() { _ = b.Close(context.WithoutCancel(t.Context())) })

	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	first := b.PID()
	if first == 0 {
		t.Fatal("no process is serving the backend")
	}

	_, err := b.CallTool(t.Context(), "exit", nil)
	if err == nil {
		t.Fatal("the call outlived the backend, want a failure")
	}
	if !errors.Is(err, pool.ErrRetryable) {
		t.Errorf("errors.Is(err, ErrRetryable) = false for %v", err)
	}
	// The request must be recognized as retryable because the session was
	// lost, not only because the exit happened to be observed in time: the
	// two race, and only one of them is deterministic.
	if !errors.Is(err, backend.ErrConnectionLost) {
		t.Errorf("errors.Is(err, ErrConnectionLost) = false for %v", err)
	}
	if !strings.Contains(err.Error(), `"modern"`) {
		t.Errorf("error does not name the backend: %v", err)
	}

	// Repeating the request is all a client has to do: the protocol is
	// stateless, so a fresh process serves it.
	if _, err := b.ListTools(t.Context()); err != nil {
		t.Fatalf("the retry failed too: %v", err)
	}
	second := b.PID()
	if second == 0 || second == first {
		t.Errorf("second request served by pid %d, want a process other than %d", second, first)
	}
	if !strings.Contains(logs.String(), "starting a replacement") {
		t.Errorf("the restart was not recorded in the log: %s", logs.String())
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	for range 200 {
		// G304: path is this test's own temporary directory.
		data, err := os.ReadFile(path) //nolint:gosec // see above
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			pid, err := strconv.Atoi(string(bytes.TrimSpace(data)))
			if err != nil {
				t.Fatalf("read child pid from %s: %v", path, err)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fixture never recorded its child's pid in %s", path)
	return 0
}

// processAlive reports whether a process still exists. Signal zero performs the
// error checks without sending anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

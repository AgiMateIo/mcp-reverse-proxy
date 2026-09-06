package pool_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// livePool returns a pool that starts real child processes.
func livePool(t *testing.T) *pool.Pool {
	t.Helper()
	fingerprints, err := config.NewFingerprinter()
	if err != nil {
		t.Fatalf("NewFingerprinter: %v", err)
	}
	limits := config.Limits{
		MaxProcesses:           8,
		MaxProcessesPerSubject: 4,
		IdleTTL:                config.Duration(time.Hour),
		ProbeTimeout:           config.Duration(probeTimeout),
	}
	p := pool.New(backend.NewConnector("test", probeTimeout, nil), fingerprints, limits, testPolicy, nil)
	t.Cleanup(func() { _ = p.Close(context.WithoutCancel(t.Context())) })
	return p
}

// fixtureBackend describes this test binary running as a backend, with env
// carrying both the fixture's mode and whatever the test wants to vary.
func fixtureBackend(t *testing.T, id string, extra map[string]string) config.Server {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}
	env := config.Env{}
	for k, v := range testfixtures.Env(testfixtures.ModeModern, testfixtures.Options{}) {
		env[k] = config.Secret(v)
	}
	for k, v := range extra {
		env[k] = config.Secret(v)
	}
	return config.Server{ID: id, Command: self, Env: env, Era: config.EraAuto}
}

// pidFor drives one request and reports which process served it.
func pidFor(t *testing.T, p *pool.Pool, s auth.Subject, server config.Server) int {
	t.Helper()
	h := p.For(s, server)
	if _, err := h.ListTools(t.Context()); err != nil {
		t.Fatalf("ListTools for %s: %v", s, err)
	}
	pid := h.PID()
	if pid == 0 {
		t.Fatalf("no process is serving %s", s)
	}
	return pid
}

// Tasks 7.1 to 7.3: what decides whether two requests share a process.
//
// The environment of a backend generally holds the subject's own credentials,
// so sharing one across subjects would hand those credentials over silently.
func TestProcessIsolation(t *testing.T) {
	t.Parallel()
	alice := auth.Subject{Issuer: "https://issuer.example/", Sub: "alice"}
	bob := auth.Subject{Issuer: "https://issuer.example/", Sub: "bob"}

	tests := []struct {
		name string
		// second describes the request made after the first one.
		secondSubject auth.Subject
		secondEnv     map[string]string
		wantSame      bool
	}{
		{
			name:          "the same subject and configuration",
			secondSubject: alice,
			wantSame:      true,
		},
		{
			name:          "two subjects with byte-identical configuration",
			secondSubject: bob,
			wantSame:      false,
		},
		{
			name:          "the same subject after an environment change",
			secondSubject: alice,
			secondEnv:     map[string]string{"API_TOKEN": "second"},
			wantSame:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := livePool(t)
			first := fixtureBackend(t, "files", map[string]string{"API_TOKEN": "first"})
			second := fixtureBackend(t, "files", map[string]string{"API_TOKEN": "first"})
			for k, v := range tt.secondEnv {
				second.Env[k] = config.Secret(v)
			}

			firstPID := pidFor(t, p, alice, first)
			secondPID := pidFor(t, p, tt.secondSubject, second)

			if tt.wantSame && firstPID != secondPID {
				t.Errorf("pids %d and %d differ, want the running process reused", firstPID, secondPID)
			}
			if !tt.wantSame && firstPID == secondPID {
				t.Errorf("both requests were served by pid %d, want separate processes", firstPID)
			}
		})
	}
}

// A subject that reaches two different servers gets a process for each, which
// is what makes the per-subject limit a limit on servers and not on requests.
func TestOneProcessPerServer(t *testing.T) {
	t.Parallel()
	p := livePool(t)
	alice := auth.Subject{Issuer: "https://issuer.example/", Sub: "alice"}

	files := pidFor(t, p, alice, fixtureBackend(t, "files", nil))
	search := pidFor(t, p, alice, fixtureBackend(t, "search", nil))
	if files == search {
		t.Errorf("both servers were served by pid %d, want one process each", files)
	}
	if got := p.Stats(); got.Size != 2 {
		t.Errorf("size = %d, want 2", got.Size)
	}
}

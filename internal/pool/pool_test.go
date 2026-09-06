package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

// A fakeBackend stands in for a running backend. Eviction and idle expiry are
// timing behavior, and a real child process cannot be driven by a clock.
type fakeBackend struct {
	pid int
	mu  sync.Mutex
	// closed records that the pool stopped this backend, which is what
	// "the process is terminated" means from the pool's side.
	closed bool
	// block, when non-nil, holds a call open so a test can make an entry busy.
	block chan struct{}
	// closeBlock, when non-nil, holds Close open, which is the window a
	// concurrent request would otherwise slip through.
	closeBlock chan struct{}
}

func (f *fakeBackend) ListTools(context.Context) (backend.ToolList, error) {
	if f.block != nil {
		<-f.block
	}
	return backend.ToolList{}, nil
}

func (f *fakeBackend) CallTool(context.Context, string, json.RawMessage) (backend.ToolResult, error) {
	return backend.ToolResult{}, nil
}

// The pool forwards the rest of the surface without inspecting it, so these
// answer emptily: what the pool is tested for is process lifetime.
func (f *fakeBackend) ListPrompts(context.Context) (backend.PromptList, error) {
	return backend.PromptList{}, nil
}

func (f *fakeBackend) GetPrompt(context.Context, string, map[string]string) (backend.PromptResult, error) {
	return backend.PromptResult{}, nil
}

func (f *fakeBackend) ListResources(context.Context) (backend.ResourceList, error) {
	return backend.ResourceList{}, nil
}

func (f *fakeBackend) ListResourceTemplates(context.Context) (backend.ResourceTemplateList, error) {
	return backend.ResourceTemplateList{}, nil
}

func (f *fakeBackend) ReadResource(context.Context, string) (backend.ResourceContents, error) {
	return backend.ResourceContents{}, nil
}

// Subscribe is part of the surface the pool forwards; the pool's own tests are
// about process lifetime and never publish anything.
func (f *fakeBackend) Subscribe(func(backend.Change)) func() { return func() {} }

func (f *fakeBackend) PID() int { return f.pid }

func (f *fakeBackend) Close(context.Context) error {
	if f.closeBlock != nil {
		<-f.closeBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeBackend) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakePool returns a pool whose backends are fakes, and a lookup of every fake
// it has handed out.
func fakePool(t *testing.T, limits config.Limits) (*Pool, func(config.Server) *fakeBackend) {
	t.Helper()
	fingerprints, err := config.NewFingerprinter()
	if err != nil {
		t.Fatalf("NewFingerprinter: %v", err)
	}
	p := New(nil, fingerprints, limits, DefaultStopPolicy, nil)

	var mu sync.Mutex
	made := map[string]*fakeBackend{}
	next := 1000
	p.newBackend = func(server config.Server) poolBackend {
		mu.Lock()
		defer mu.Unlock()
		next++
		f := &fakeBackend{pid: next}
		made[fingerprintOf(t, fingerprints, server)] = f
		return f
	}
	return p, func(server config.Server) *fakeBackend {
		mu.Lock()
		defer mu.Unlock()
		return made[fingerprintOf(t, fingerprints, server)]
	}
}

func fingerprintOf(t *testing.T, f *config.Fingerprinter, server config.Server) string {
	t.Helper()
	return f.Server(server)
}

func subject(issuer, sub string) auth.Subject {
	return auth.Subject{Issuer: issuer, Sub: sub}
}

func srv(id string, env config.Env) config.Server {
	return config.Server{ID: id, Command: "/bin/true", Env: env, Era: config.EraAuto}
}

func use(t *testing.T, p *Pool, s auth.Subject, server config.Server) error {
	t.Helper()
	_, err := p.For(s, server).ListTools(t.Context())
	return err
}

func limits(global, perSubject int, idle time.Duration) config.Limits {
	return config.Limits{
		MaxProcesses:           global,
		MaxProcessesPerSubject: perSubject,
		IdleTTL:                config.Duration(idle),
		ProbeTimeout:           config.Duration(time.Second),
		MaxHeaderBytes:         8 << 10,
	}
}

// Task 7.4: a backend that has served nothing for the configured period is
// stopped and its slot released.
func TestIdleBackendsExpire(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Minute
		p, fake := fakePool(t, limits(8, 4, idle))
		defer func() { _ = p.Close(t.Context()) }()

		server := srv("files", nil)
		if err := use(t, p, subject("https://a/", "alice"), server); err != nil {
			t.Fatalf("first request: %v", err)
		}
		if got := p.Stats().Size; got != 1 {
			t.Fatalf("size = %d, want 1", got)
		}

		// Just short of the period, the backend is still there: expiring early
		// would throw away a process a client is about to use again.
		time.Sleep(idle - time.Second)
		synctest.Wait()
		if fake(server).isClosed() {
			t.Error("the backend was stopped before its idle period elapsed")
		}

		// The reaper ticks, so expiry lands within a tick of the period.
		time.Sleep(idle)
		synctest.Wait()
		if !fake(server).isClosed() {
			t.Error("the backend outlived its idle period")
		}
		stats := p.Stats()
		if stats.Size != 0 {
			t.Errorf("size = %d, want the slot released", stats.Size)
		}
		if stats.IdleExpired != 1 {
			t.Errorf("IdleExpired = %d, want 1", stats.IdleExpired)
		}
	})
}

// Task 7.5: when the global limit is reached, the backend unused for longest
// makes way.
func TestGlobalLimitEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, fake := fakePool(t, limits(2, 8, time.Hour))
		defer func() { _ = p.Close(t.Context()) }()

		alice := subject("https://a/", "alice")
		first, second, third := srv("first", nil), srv("second", nil), srv("third", nil)

		if err := use(t, p, alice, first); err != nil {
			t.Fatalf("first: %v", err)
		}
		time.Sleep(time.Second)
		if err := use(t, p, alice, second); err != nil {
			t.Fatalf("second: %v", err)
		}
		// Touching the older one makes it the newer one, which is the whole
		// point of ordering by use rather than by age. The sleeps are what
		// separate the timestamps: virtual time only advances when something
		// sleeps, so without them every entry would be used at the same
		// instant and "least recently" would mean nothing.
		time.Sleep(time.Second)
		if err := use(t, p, alice, first); err != nil {
			t.Fatalf("first again: %v", err)
		}

		time.Sleep(time.Second)
		if err := use(t, p, alice, third); err != nil {
			t.Fatalf("third: %v", err)
		}
		if !fake(second).isClosed() {
			t.Error("the least recently used backend survived the limit")
		}
		if fake(first).isClosed() {
			t.Error("a recently used backend was evicted")
		}
		if got := p.Stats(); got.Size != 2 || got.Evicted != 1 {
			t.Errorf("stats = %+v, want size 2 and one eviction", got)
		}
	})
}

// Making room by killing a backend in the middle of somebody's request would
// turn one subject's load into another subject's failure.
func TestEvictionSkipsBusyBackends(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fingerprints, err := config.NewFingerprinter()
		if err != nil {
			t.Fatalf("NewFingerprinter: %v", err)
		}
		p := New(nil, fingerprints, limits(2, 8, time.Hour), DefaultStopPolicy, nil)
		defer func() { _ = p.Close(t.Context()) }()

		// Only the first backend holds its request open; the rest answer at
		// once, so the busy one is also the least recently used.
		release := make(chan struct{})
		var mu sync.Mutex
		made := map[string]*fakeBackend{}
		p.newBackend = func(server config.Server) poolBackend {
			mu.Lock()
			defer mu.Unlock()
			f := &fakeBackend{pid: len(made) + 1}
			if server.ID == "busy" {
				f.block = release
			}
			made[server.ID] = f
			return f
		}
		fake := func(id string) *fakeBackend {
			mu.Lock()
			defer mu.Unlock()
			return made[id]
		}

		alice := subject("https://a/", "alice")
		var wg sync.WaitGroup
		wg.Go(func() { _ = use(t, p, alice, srv("busy", nil)) })
		synctest.Wait()

		time.Sleep(time.Second)
		if err := use(t, p, alice, srv("idle", nil)); err != nil {
			t.Fatalf("idle: %v", err)
		}
		time.Sleep(time.Second)
		if err := use(t, p, alice, srv("wanted", nil)); err != nil {
			t.Fatalf("wanted: %v", err)
		}

		if fake("busy").isClosed() {
			t.Error("a backend serving a request was evicted, although it was the least recently used")
		}
		if !fake("idle").isClosed() {
			t.Error("the idle backend was not the one evicted")
		}
		close(release)
		wg.Wait()
	})
}

// Task 7.6: a subject beyond its own limit is refused, and told why.
func TestPerSubjectQuota(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p, _ := fakePool(t, limits(16, 2, time.Hour))
		defer func() { _ = p.Close(t.Context()) }()

		alice := subject("https://a/", "alice")
		for i := range 2 {
			if err := use(t, p, alice, srv(fmt.Sprintf("server-%d", i), nil)); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
		err := use(t, p, alice, srv("one-too-many", nil))
		if !errors.Is(err, ErrSubjectQuotaExhausted) {
			t.Fatalf("err = %v, want it to wrap %v", err, ErrSubjectQuotaExhausted)
		}
		if !strings.Contains(err.Error(), "quota") {
			t.Errorf("error does not name the exhausted quota: %v", err)
		}

		// One subject's exhaustion is not another's: the limit exists so that
		// one subject cannot consume the host on everyone else's behalf.
		if err := use(t, p, subject("https://a/", "bob"), srv("server-0", nil)); err != nil {
			t.Errorf("a second subject was refused for the first one's usage: %v", err)
		}
		if got := p.Stats().Rejected; got != 1 {
			t.Errorf("Rejected = %d, want 1", got)
		}
	})
}

// A global limit reached with every backend busy is a refusal, not a reason to
// kill somebody's running request.
func TestPoolExhaustedWhenEverythingIsBusy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fingerprints, err := config.NewFingerprinter()
		if err != nil {
			t.Fatalf("NewFingerprinter: %v", err)
		}
		p := New(nil, fingerprints, limits(1, 8, time.Hour), DefaultStopPolicy, nil)
		defer func() { _ = p.Close(t.Context()) }()

		release := make(chan struct{})
		p.newBackend = func(config.Server) poolBackend { return &fakeBackend{pid: 1, block: release} }

		alice := subject("https://a/", "alice")
		var wg sync.WaitGroup
		wg.Go(func() { _ = use(t, p, alice, srv("busy", nil)) })
		synctest.Wait()

		err = use(t, p, alice, srv("wanted", nil))
		if !errors.Is(err, ErrPoolExhausted) {
			t.Errorf("err = %v, want it to wrap %v", err, ErrPoolExhausted)
		}
		close(release)
		wg.Wait()
	})
}

// Task 7.7: the pool reports its size and what has happened to it, and those
// numbers move under load rather than sitting where they were initialized.
//
// The limit is deliberately smaller than the number of distinct backends the
// load asks for, so eviction is forced rather than hoped for.
func TestStatsUnderLoad(t *testing.T) {
	t.Parallel()
	const (
		subjects = 4
		servers  = 5
		rounds   = 20
		global   = 6
	)
	p, _ := fakePool(t, limits(global, servers, time.Hour))
	defer func() { _ = p.Close(t.Context()) }()

	before := p.Stats()
	if before.Size != 0 || before.Created != 0 {
		t.Fatalf("a fresh pool reports %+v", before)
	}

	var wg sync.WaitGroup
	for s := range subjects {
		wg.Go(func() {
			who := subject("https://a/", fmt.Sprintf("subject-%d", s))
			for round := range rounds {
				server := srv(fmt.Sprintf("server-%d", round%servers), nil)
				if err := use(t, p, who, server); err != nil &&
					!errors.Is(err, ErrPoolExhausted) && !errors.Is(err, ErrSubjectQuotaExhausted) {
					t.Errorf("request: %v", err)
					return
				}
				// Sampled mid-load: the limit is what the pool promises, so it
				// has to hold while requests are in flight and not only after.
				if got := p.Stats().Size; got > global {
					t.Errorf("size = %d, want no more than the limit of %d", got, global)
					return
				}
			}
		})
	}
	wg.Wait()

	after := p.Stats()
	if after.Created == 0 {
		t.Error("the pool reports having created nothing")
	}
	if after.Evicted == 0 {
		t.Errorf("the pool reports no evictions after %d requests over %d slots", subjects*rounds, global)
	}
	if after.Size > global {
		t.Errorf("size = %d, want no more than the limit of %d", after.Size, global)
	}
	if after.Size != len(p.entries) {
		t.Errorf("reported size %d does not match the %d live entries", after.Size, len(p.entries))
	}
}

// Two requests arriving at a full pool must each get a slot after evicting one
// backend apiece.
//
// Publishing an eviction before taking the slot it frees would let the other
// request take it, sending this one round to evict somebody else — one busy
// moment turning into a cascade of terminations.
func TestConcurrentEvictionsDoNotCascade(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fingerprints, err := config.NewFingerprinter()
		if err != nil {
			t.Fatalf("NewFingerprinter: %v", err)
		}
		p := New(nil, fingerprints, limits(2, 8, time.Hour), DefaultStopPolicy, nil)
		defer func() { _ = p.Close(t.Context()) }()

		// Stopping a backend takes as long as the escalation allows, which is
		// the window a second request would slip through.
		slowClose := make(chan struct{})
		p.newBackend = func(config.Server) poolBackend {
			return &fakeBackend{pid: 1, closeBlock: slowClose}
		}

		alice := subject("https://a/", "alice")
		for i := range 2 {
			if err := use(t, p, alice, srv(fmt.Sprintf("resident-%d", i), nil)); err != nil {
				t.Fatalf("filling the pool: %v", err)
			}
			time.Sleep(time.Second)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range 2 {
			wg.Go(func() { errs[i] = use(t, p, alice, srv(fmt.Sprintf("newcomer-%d", i), nil)) })
		}
		// Both are now blocked stopping the backends they evicted.
		synctest.Wait()
		close(slowClose)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("newcomer %d was refused: %v", i, err)
			}
		}
		stats := p.Stats()
		if stats.Size != 2 {
			t.Errorf("size = %d, want the limit of 2", stats.Size)
		}
		if stats.Evicted != 2 {
			t.Errorf("Evicted = %d, want one eviction per newcomer", stats.Evicted)
		}
		if stats.Created != 4 {
			t.Errorf("Created = %d, want two residents and two newcomers", stats.Created)
		}
	})
}

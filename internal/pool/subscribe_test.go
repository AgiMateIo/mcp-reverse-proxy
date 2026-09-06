package pool_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/pool"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// changes collects what a subscription delivered, from the reading goroutine
// of whichever backend is current.
type changes struct {
	mu   sync.Mutex
	seen []backend.ChangeKind
}

func (c *changes) deliver(ch backend.Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, ch.Kind)
}

// await waits for one more change than the count it was given, and reports how
// many there are now.
func (c *changes) await(t *testing.T, more int) int {
	t.Helper()
	for range 200 {
		c.mu.Lock()
		n := len(c.seen)
		c.mu.Unlock()
		if n >= more {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("only %d changes arrived, wanted %d", len(c.seen), more)
	return 0
}

// A subscription is to a server, not to the process serving it.
//
// The pool replaces processes as a matter of course — eviction, idle expiry, a
// backend that exited — and a client's stream must not end because one of them
// did. Registering the subscription on the backend would make it end: nothing
// would fail, no error would be returned, and the client would simply stop
// hearing about changes it had asked for.
func TestASubscriptionOutlivesTheProcess(t *testing.T) {
	t.Parallel()
	p := livePool(t)
	subject := auth.Subject{Issuer: "https://issuer.example/", Sub: "alice"}
	server := announcingBackend(t)
	h := p.For(subject, server)

	var seen changes
	release := h.Subscribe(seen.deliver)
	defer release()

	// The subscription was registered before any process existed, and asking
	// for it started none.
	if pid := h.PID(); pid != 0 {
		t.Fatalf("subscribing started a process (pid %d)", pid)
	}

	announce(t, h)
	before := seen.await(t, 1)
	first := h.PID()
	if first == 0 {
		t.Fatal("no process is serving the backend")
	}

	// Evicting is the honest way to lose the process: it is what the pool does
	// under load, and it leaves the key untouched.
	evict(t, p, subject)
	if pid := h.PID(); pid != 0 {
		t.Fatalf("the backend survived eviction as pid %d", pid)
	}

	announce(t, h)
	seen.await(t, before+1)
	if second := h.PID(); second == 0 || second == first {
		t.Errorf("the replacement runs as pid %d, want a process other than %d", second, first)
	}

	// And releasing still works against the replacement, which is a different
	// backend from the one the subscription was made against.
	release()
	announce(t, h)
	time.Sleep(200 * time.Millisecond)
	if got := seen.await(t, before+1); got != before+1 {
		t.Errorf("%d changes arrived after the release, want %d", got, before+1)
	}
}

// announce makes the backend report a change, by calling the fixture's tool
// for it.
func announce(t *testing.T, h *pool.Handle) {
	t.Helper()
	if _, err := h.CallTool(t.Context(), "change", json.RawMessage(`{"kind":"tools"}`)); err != nil {
		t.Fatalf("triggering a change: %v", err)
	}
}

// evict fills the pool until the least recently used entry — the only one
// there — is dropped to make room.
func evict(t *testing.T, p *pool.Pool, subject auth.Subject) {
	t.Helper()
	// One filler per subject: a single subject would hit its own quota long
	// before the global limit, and it is the global limit that evicts.
	for i := range 8 {
		filler := auth.Subject{Issuer: subject.Issuer, Sub: "filler-" + string(rune('a'+i))}
		h := p.For(filler, fixtureBackend(t, "filler", nil))
		if _, err := h.ListTools(t.Context()); err != nil {
			t.Fatalf("filling the pool: %v", err)
		}
	}
}

// announcingBackend is this test binary as a backend that can be told to
// announce a change.
func announcingBackend(t *testing.T) config.Server {
	t.Helper()
	server := fixtureBackend(t, "announcing", nil)
	for k, v := range testfixtures.Env(testfixtures.ModeModern, testfixtures.Options{AnnounceChanges: true}) {
		server.Env[k] = config.Secret(v)
	}
	return server
}

var _ = context.Background

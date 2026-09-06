package aggregate

import (
	"context"
	"sync"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
)

// A Subscriber is a source that reports its own changes. Not every source is
// one: something that can only be listed still belongs on the surface.
type Subscriber interface {
	Subscribe(f func(backend.Change)) func()
}

// kinds are the change kinds a listener can carry.
var kinds = []backend.ChangeKind{backend.ChangeTools, backend.ChangePrompts, backend.ChangeResources}

// A Listener is one client's view of the backends' changes: every backend's
// notifications on one stream, whichever era they came from.
type Listener struct {
	changes chan backend.ChangeKind

	// mu guards pending, which holds the kinds already queued and not yet
	// taken. It is what keeps a burst of one kind from filling the buffer and
	// crowding out another.
	mu      sync.Mutex
	pending map[backend.ChangeKind]bool

	release func()
	once    sync.Once
}

// Next returns the next change, or false when the context ends.
//
// It is the receive rather than a channel field so that a delivered kind is
// marked taken here: the sender has no way to observe a receive, and without
// that the coalescing below could not tell "already queued" from "already
// read".
func (l *Listener) Next(ctx context.Context) (backend.ChangeKind, bool) {
	select {
	case <-ctx.Done():
		return "", false
	case kind := <-l.changes:
		l.mu.Lock()
		delete(l.pending, kind)
		l.mu.Unlock()
		return kind, true
	}
}

// Close releases what the listener holds. It is safe to call more than once:
// a stream ends either by the client going away or by its request being
// cancelled, and both paths run it.
func (l *Listener) Close() { l.once.Do(l.release) }

// deliver queues one change.
//
// At most one of each kind is ever queued, and the buffer has a slot per kind,
// so this can neither block nor drop. Coalescing loses nothing: a change
// notification carries only its kind — it says the list is no longer what the
// client was told, and two of them say exactly what one says. What would be
// lost without it is a change of another kind, crowded out of the buffer by a
// backend announcing the same one repeatedly.
func (l *Listener) deliver(c backend.Change) {
	l.mu.Lock()
	if l.pending[c.Kind] {
		l.mu.Unlock()
		return
	}
	l.pending[c.Kind] = true
	l.mu.Unlock()
	l.changes <- c.Kind
}

// Listen merges the changes of every backend into one stream for one client.
//
// The context is the request the stream belongs to. This gateway is built for
// one request already, so it has nothing to resolve and never fails; the
// signature is the surface's, and a surface assembled per subject needs both.
func (g *Gateway) Listen(context.Context) (*Listener, error) {
	l := &Listener{
		changes: make(chan backend.ChangeKind, len(kinds)),
		pending: make(map[backend.ChangeKind]bool, len(kinds)),
	}
	var releases []func()
	for _, s := range g.sources {
		if sub, ok := s.(Subscriber); ok {
			releases = append(releases, sub.Subscribe(l.deliver))
		}
	}
	g.listeners.Add(1)
	l.release = func() {
		for _, release := range releases {
			release()
		}
		g.listeners.Add(-1)
	}
	return l, nil
}

// Listeners reports how many streams are open. It exists so that a test can
// show a broken stream released what it held, rather than infer it.
func (g *Gateway) Listeners() int { return int(g.listeners.Load()) }

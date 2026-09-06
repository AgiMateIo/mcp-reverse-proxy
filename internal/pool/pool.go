package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

var (
	// ErrSubjectQuotaExhausted reports a subject that already holds as many
	// backends as it is allowed, so that one subject cannot consume the whole
	// host on everyone else's behalf.
	ErrSubjectQuotaExhausted = errors.New("subject quota exhausted")
	// ErrPoolExhausted reports a global limit reached with nothing idle to
	// evict. Killing a backend in the middle of somebody's request to make room
	// would turn one subject's load into another subject's failure.
	ErrPoolExhausted = errors.New("process pool exhausted")
	// ErrPoolClosed reports use of a pool that has been shut down.
	ErrPoolClosed = errors.New("process pool is closed")
)

// A poolBackend is the part of [Backend] the pool depends on. The pool owns
// process lifetime and nothing else, so a test can drive its bookkeeping
// without a child process on the other end.
type poolBackend interface {
	ListTools(ctx context.Context) (backend.ToolList, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (backend.ToolResult, error)
	ListPrompts(ctx context.Context) (backend.PromptList, error)
	GetPrompt(ctx context.Context, name string, arguments map[string]string) (backend.PromptResult, error)
	ListResources(ctx context.Context) (backend.ResourceList, error)
	ListResourceTemplates(ctx context.Context) (backend.ResourceTemplateList, error)
	ReadResource(ctx context.Context, uri string) (backend.ResourceContents, error)
	Subscribe(f func(backend.Change)) func()
	PID() int
	Close(ctx context.Context) error
}

// A Pool keeps one backend per subject and resolved configuration, and bounds
// how many of them may live at once.
//
// The key is the pair, never the configuration alone. A backend's environment
// generally holds the subject's own credentials, so a process started for one
// subject and reused for another would hand over those credentials silently —
// a leak invisible to anyone testing with a single user.
type Pool struct {
	connector    backend.Connector
	fingerprints *config.Fingerprinter
	limits       config.Limits
	policy       StopPolicy
	logger       *slog.Logger
	// newBackend builds the thing an entry owns. It is a field so that tests
	// of eviction and idle expiry need not spawn processes to observe them.
	newBackend func(config.Server) poolBackend

	mu         sync.Mutex
	entries    map[key]*entry
	perSubject map[string]int
	// listeners are the change subscriptions of each key, kept by the pool
	// rather than by the backend because a backend is the shorter-lived of the
	// two: it is evicted, expires and is restarted, and a client's stream must
	// outlive all three. Every backend started for a key is subscribed to on
	// the subscribers' behalf.
	listeners  map[key]map[int]func(backend.Change)
	nextListen int
	stats      Stats
	closed     bool

	wg       sync.WaitGroup
	stopReap context.CancelFunc
}

// A key identifies one backend: who it belongs to, and what it was started
// with.
type key struct {
	subject     string
	fingerprint string
}

// An entry is one live backend and what the pool knows about its use.
type entry struct {
	key     key
	server  config.Server
	backend poolBackend
	// lastUsed orders eviction and decides idle expiry.
	lastUsed time.Time
	// inflight counts requests currently being served. A backend serving one
	// is never evicted: making room by killing somebody's running request
	// turns load into failure.
	inflight int
	// releases undoes this backend's subscriptions, by the identifier the pool
	// gave each subscriber.
	releases map[int]func()
}

// Stats is what the pool reports about itself.
type Stats struct {
	// Size is how many backends are live now.
	Size int
	// Created counts backends the pool has started, each of which owns at most
	// one child process at a time.
	Created uint64
	// Evicted counts backends stopped to make room for another.
	Evicted uint64
	// IdleExpired counts backends stopped for going unused.
	IdleExpired uint64
	// Rejected counts requests refused for want of a slot.
	Rejected uint64
}

// New returns a pool bounded by limits.
//
// The fingerprinter is taken rather than made, because it must be the process's
// only one: its key is random per instance, so a second fingerprinter would
// give the same configuration a different fingerprint and the pool would miss
// on every request.
func New(connector backend.Connector, fingerprints *config.Fingerprinter, limits config.Limits, policy StopPolicy, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		connector:    connector,
		fingerprints: fingerprints,
		limits:       limits,
		policy:       policy,
		logger:       logger,
		entries:      make(map[key]*entry),
		perSubject:   make(map[string]int),
		listeners:    make(map[key]map[int]func(backend.Change)),
		stopReap:     cancel,
	}
	p.newBackend = func(server config.Server) poolBackend {
		return NewBackend(server, p.connector, p.policy, p.logger)
	}
	p.wg.Go(func() { p.reap(ctx) })
	return p
}

// For returns the handle through which a subject reaches one server. No process
// starts until the handle is used.
func (p *Pool) For(subject auth.Subject, server config.Server) *Handle {
	return &Handle{
		pool:   p,
		server: server,
		key:    key{subject: subject.Key(), fingerprint: p.fingerprints.Server(server)},
	}
}

// A Handle is one subject's access to one server.
//
// The pool hands out handles rather than backends: an entry may be evicted or
// expire between two requests, and a caller holding the backend itself would be
// holding something the pool had already stopped.
type Handle struct {
	pool   *Pool
	server config.Server
	// key is computed once: it is the same for every request this handle
	// serves, and hashing the configuration again per request buys nothing.
	key key
}

// ListTools lists the server's tools for this subject.
func (h *Handle) ListTools(ctx context.Context) (backend.ToolList, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.ToolList{}, err
	}
	defer h.pool.release(e)
	return e.backend.ListTools(ctx)
}

// CallTool calls a tool on the server for this subject.
func (h *Handle) CallTool(ctx context.Context, name string, arguments json.RawMessage) (backend.ToolResult, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.ToolResult{}, err
	}
	defer h.pool.release(e)
	return e.backend.CallTool(ctx, name, arguments)
}

// Subscribe registers f for the changes of this subject's server, across every
// process that serves it: a backend evicted, expired or restarted is replaced
// under the same subscription, and the caller is not told, because from the
// client's side nothing happened.
//
// The callback runs on the backend connection's reading goroutine and must not
// block.
func (h *Handle) Subscribe(f func(backend.Change)) func() {
	return h.pool.subscribe(h.key, f)
}

// PID reports the process currently serving this subject's requests, or zero if
// none is. It exists so that a caller can tell one process from another, and so
// it looks rather than starts one: asking which process is running must not be
// what causes one to run.
func (h *Handle) PID() int {
	h.pool.mu.Lock()
	e, ok := h.pool.entries[h.key]
	h.pool.mu.Unlock()
	if !ok {
		return 0
	}
	return e.backend.PID()
}

// acquire returns the entry serving this key, starting one if there is none,
// and marks it busy for the duration of the caller's request.
func (p *Pool) acquire(ctx context.Context, k key, server config.Server) (*entry, error) {
	acquired, victim, err := p.tryAcquire(k, server)
	if err != nil {
		return nil, err
	}
	if victim != nil {
		// Stopping a backend runs the shutdown escalation, which takes as long
		// as the policy allows. Doing it outside the lock keeps one slow
		// backend from stalling every other subject's request — and the slot
		// it freed is already taken, so the wait costs this caller nothing.
		p.closeEntry(ctx, victim, "evicted to make room")
	}
	return acquired, nil
}

// tryAcquire does the whole bookkeeping under one lock: it returns the entry to
// serve from, together with a backend the caller must stop on its way out.
//
// Freeing a slot and taking it are one step deliberately. Were the eviction
// published before the new entry existed, a concurrent request could take the
// slot while this one was still stopping the backend it evicted, and this
// caller would go round to evict somebody else — turning one busy moment into a
// cascade of terminations.
func (p *Pool) tryAcquire(k key, server config.Server) (acquired, victim *entry, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, fmt.Errorf("serve %q: %w", server.ID, ErrPoolClosed)
	}
	if e, ok := p.entries[k]; ok {
		e.lastUsed = time.Now()
		e.inflight++
		return e, nil, nil
	}
	if p.perSubject[k.subject] >= p.limits.MaxProcessesPerSubject {
		p.stats.Rejected++
		return nil, nil, fmt.Errorf("serve %q: %d of %d backends already running for this subject: %w",
			server.ID, p.perSubject[k.subject], p.limits.MaxProcessesPerSubject, ErrSubjectQuotaExhausted)
	}
	if len(p.entries) >= p.limits.MaxProcesses {
		evict := p.leastRecentlyUsedLocked()
		if evict == nil {
			p.stats.Rejected++
			return nil, nil, fmt.Errorf("serve %q: %d backends are running and every one is busy: %w",
				server.ID, len(p.entries), ErrPoolExhausted)
		}
		p.detachLocked(evict)
		p.stats.Evicted++
		return p.insertLocked(k, server), evict, nil
	}
	return p.insertLocked(k, server), nil, nil
}

// insertLocked starts a new entry, already marked busy so that nothing evicts
// it before its first request is served.
func (p *Pool) insertLocked(k key, server config.Server) *entry {
	e := &entry{
		key: k, server: server, backend: p.newBackend(server),
		lastUsed: time.Now(), inflight: 1, releases: map[int]func(){},
	}
	// Whoever was listening for this key before this backend existed is
	// listening to it now: a subscription is to the server, not to the process
	// that happens to be serving it.
	for id, f := range p.listeners[k] {
		e.releases[id] = e.backend.Subscribe(f)
	}
	p.entries[k] = e
	p.perSubject[k.subject]++
	p.stats.Created++
	return e
}

// subscribe registers f for one key's changes, for as long as the caller keeps
// the returned release. It starts no process: a client may listen for changes
// before it has asked for anything, and asking who is listening must not be
// what causes a backend to run.
func (p *Pool) subscribe(k key, f func(backend.Change)) func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := p.nextListen
	p.nextListen++
	if p.listeners[k] == nil {
		p.listeners[k] = map[int]func(backend.Change){}
	}
	p.listeners[k][id] = f
	if e, ok := p.entries[k]; ok {
		e.releases[id] = e.backend.Subscribe(f)
	}
	return func() { p.unsubscribe(k, id) }
}

// unsubscribe forgets one subscription, and detaches it from the backend
// currently serving the key if there is one.
func (p *Pool) unsubscribe(k key, id int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.listeners[k], id)
	if len(p.listeners[k]) == 0 {
		delete(p.listeners, k)
	}
	if e, ok := p.entries[k]; ok {
		if release, ok := e.releases[id]; ok {
			release()
			delete(e.releases, id)
		}
	}
}

// release marks a request finished.
func (p *Pool) release(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.inflight--
	e.lastUsed = time.Now()
}

// leastRecentlyUsedLocked returns the idle entry unused for longest, or nil if
// every entry is serving a request.
//
// The scan is linear in the pool's size, which is bounded by MaxProcesses and
// therefore small. A deployment that raised that limit into the thousands would
// want an ordered structure here instead.
func (p *Pool) leastRecentlyUsedLocked() *entry {
	var oldest *entry
	for _, e := range p.entries {
		if e.inflight > 0 {
			continue
		}
		if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
			oldest = e
		}
	}
	return oldest
}

// detachLocked removes an entry from the pool's bookkeeping. The backend it
// owned is still running and is the caller's to stop.
func (p *Pool) detachLocked(e *entry) {
	delete(p.entries, e.key)
	p.perSubject[e.key.subject]--
	if p.perSubject[e.key.subject] <= 0 {
		delete(p.perSubject, e.key.subject)
	}
}

// closeEntry stops a backend the pool has already let go of.
func (p *Pool) closeEntry(ctx context.Context, e *entry, why string) {
	p.logger.Info("stopping a backend", "server", e.server.ID, "reason", why)
	// The reason the backend is going away has nothing to do with the request
	// that happened to trigger it, so a cancelled request must not cut the
	// shutdown short and leave a process behind.
	if err := e.backend.Close(context.WithoutCancel(ctx)); err != nil {
		p.logger.Warn("stopping a backend", "server", e.server.ID, "error", err)
	}
}

// reap stops backends that have gone unused.
//
// It ticks rather than arming a timer per entry: a backend may therefore
// outlive its idle period by up to one tick, which costs a little memory and
// saves a great deal of bookkeeping.
func (p *Pool) reap(ctx context.Context) {
	interval := p.limits.IdleTTL.Duration() / 2
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, e := range p.expired() {
				p.closeEntry(ctx, e, "idle for longer than the configured period")
			}
		}
	}
}

// expired detaches every entry that has gone unused, and returns them for the
// caller to stop outside the lock.
func (p *Pool) expired() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	var stale []*entry
	now := time.Now()
	for _, e := range p.entries {
		if e.inflight == 0 && now.Sub(e.lastUsed) >= p.limits.IdleTTL.Duration() {
			p.detachLocked(e)
			p.stats.IdleExpired++
			stale = append(stale, e)
		}
	}
	return stale
}

// Stats reports the pool's current size and what has happened to it.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stats
	s.Size = len(p.entries)
	return s
}

// Close stops every backend and the pool with them.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	live := make([]*entry, 0, len(p.entries))
	for _, e := range p.entries {
		live = append(live, e)
	}
	p.entries = make(map[key]*entry)
	p.perSubject = make(map[string]int)
	p.mu.Unlock()

	p.stopReap()
	p.wg.Wait()

	var errs []error
	for _, e := range live {
		if err := e.backend.Close(context.WithoutCancel(ctx)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ListPrompts lists the server's prompts for this subject.
func (h *Handle) ListPrompts(ctx context.Context) (backend.PromptList, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.PromptList{}, err
	}
	defer h.pool.release(e)
	return e.backend.ListPrompts(ctx)
}

// GetPrompt fetches a prompt from the server for this subject.
func (h *Handle) GetPrompt(ctx context.Context, name string, arguments map[string]string) (backend.PromptResult, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.PromptResult{}, err
	}
	defer h.pool.release(e)
	return e.backend.GetPrompt(ctx, name, arguments)
}

// ListResources lists the server's resources for this subject.
func (h *Handle) ListResources(ctx context.Context) (backend.ResourceList, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.ResourceList{}, err
	}
	defer h.pool.release(e)
	return e.backend.ListResources(ctx)
}

// ListResourceTemplates lists the server's resource templates for this subject.
func (h *Handle) ListResourceTemplates(ctx context.Context) (backend.ResourceTemplateList, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.ResourceTemplateList{}, err
	}
	defer h.pool.release(e)
	return e.backend.ListResourceTemplates(ctx)
}

// ReadResource reads one of the server's resources for this subject.
func (h *Handle) ReadResource(ctx context.Context, uri string) (backend.ResourceContents, error) {
	e, err := h.pool.acquire(ctx, h.key, h.server)
	if err != nil {
		return backend.ResourceContents{}, err
	}
	defer h.pool.release(e)
	return e.backend.ReadResource(ctx, uri)
}

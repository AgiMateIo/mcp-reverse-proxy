package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP methods the era policy turns on.
const (
	methodDiscover   = "server/discover"
	methodInitialize = "initialize"
	// methodSubscriptionsListen is the stream a modern backend reports its own
	// changes on. The SDK client opens it during Connect, because the gateway
	// registers handlers for the list-changed notifications.
	methodSubscriptionsListen = "subscriptions/listen"
	methodSampling            = "sampling/createMessage"
	methodRootsList           = "roots/list"
	methodElicitationCreate   = "elicitation/create"
)

// maskedRequests are the server-to-client requests the gateway never invites.
// The capabilities behind them are not advertised, so a backend that sends one
// anyway is violating the contract rather than using a feature.
var maskedRequests = []string{methodSampling, methodRootsList, methodElicitationCreate}

// An eraTransport interposes the gateway's era policy between the SDK client
// and a backend's pipes.
//
// The SDK client implements the specified probe-and-fall-back sequence, but it
// offers no way to bound the probe on its own, to skip it, or to forbid the
// fallback: it runs both halves under one context and keeps the requested
// protocol version private. Since the policy cannot be expressed through the
// client's options, it is expressed one layer down, on the messages themselves.
//
// The transport also holds the outcome of that policy, because an error raised
// inside a JSON-RPC connection reaches the caller flattened; [SDKConnector]
// reads [eraTransport.result] rather than trying to recover meaning from it.
type eraTransport struct {
	inner mcp.Transport
	opts  interposeOptions
	conn  *interposedConn
}

type interposeOptions struct {
	serverID     string
	era          config.Era
	probeTimeout time.Duration
	logger       *slog.Logger
}

var _ mcp.Transport = (*eraTransport)(nil)

// Connect implements [mcp.Transport].
func (t *eraTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	inner, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to backend %q: %w", t.opts.serverID, err)
	}
	t.conn = newInterposedConn(inner, t.opts)
	return t.conn, nil
}

// result reports what the era policy concluded, or nil before a connection was
// made.
func (t *eraTransport) result() *interposedConn { return t.conn }

// An interposedConn applies the era policy to one connection's traffic.
type interposedConn struct {
	inner mcp.Connection
	opts  interposeOptions

	// inbound carries what the backend said; injected carries what the policy
	// says on the backend's behalf. Both are read by [interposedConn.Read].
	inbound  chan jsonrpc.Message
	injected chan jsonrpc.Message

	// armed carries each probe deadline to the goroutine that watches it. A
	// deadline cannot be watched from Read's select: Read is already blocked
	// there when the probe goes out, and a select does not re-evaluate its
	// channels once it is waiting.
	armed chan probeArm

	mu       sync.Mutex
	probeID  jsonrpc.ID
	probeGen uint64
	probing  bool
	expired  bool
	detected config.Era
	mismatch error

	readCtx    context.Context
	stopReader context.CancelFunc
	wg         sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

var _ mcp.Connection = (*interposedConn)(nil)

func newInterposedConn(inner mcp.Connection, opts interposeOptions) *interposedConn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &interposedConn{
		inner:      inner,
		opts:       opts,
		inbound:    make(chan jsonrpc.Message),
		injected:   make(chan jsonrpc.Message, 1),
		armed:      make(chan probeArm, 1),
		readCtx:    ctx,
		stopReader: cancel,
	}
	// The backend is read by one owned goroutine, and the probe deadline is
	// watched by another. Both are waited for by Close.
	c.wg.Go(c.readLoop)
	c.wg.Go(c.watchProbe)
	return c
}

// outcome reports the era the policy settled on, and the mismatch that stopped
// the connection if it did not settle.
func (c *interposedConn) outcome() (config.Era, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.detected, c.mismatch
}

func (c *interposedConn) readLoop() {
	defer close(c.inbound)
	for {
		msg, err := c.inner.Read(c.readCtx)
		if err != nil {
			return
		}
		select {
		case c.inbound <- msg:
		case <-c.readCtx.Done():
			return
		}
	}
}

// Read implements [mcp.Connection]. It delivers what the backend said, what the
// policy said in its place, and the expiry of the probe deadline.
func (c *interposedConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		select {
		case msg := <-c.injected:
			return msg, nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg := <-c.injected:
			return msg, nil
		case msg, ok := <-c.inbound:
			if !ok {
				return nil, fmt.Errorf("backend %q closed the connection: %w", c.opts.serverID, mcp.ErrConnectionClosed)
			}
			deliver, err := c.inspect(ctx, msg)
			if err != nil {
				return nil, err
			}
			if deliver {
				return msg, nil
			}
		}
	}
}

// A probeArm is one probe's deadline. The generation distinguishes it from an
// earlier probe on the same connection: version negotiation can send a second
// one, and the first one's timer must not expire it.
type probeArm struct {
	gen uint64
	d   time.Duration
}

// watchProbe answers a probe the backend never answered. A silent backend is
// the case the SDK's own fall-back cannot reach: it triggers on an error, and
// silence produces none.
func (c *interposedConn) watchProbe() {
	var (
		timer  *time.Timer
		expiry <-chan time.Time
		gen    uint64
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-c.readCtx.Done():
			return
		case arm := <-c.armed:
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(arm.d)
			expiry, gen = timer.C, arm.gen
		case <-expiry:
			expiry = nil
			msg := c.onProbeExpired(gen)
			if msg == nil {
				// The backend answered in time, or moved on to a later probe.
				continue
			}
			select {
			case c.injected <- msg:
			case <-c.readCtx.Done():
				return
			}
		}
	}
}

// inspect reports whether msg should reach the SDK client.
func (c *interposedConn) inspect(ctx context.Context, msg jsonrpc.Message) (bool, error) {
	switch m := msg.(type) {
	case *jsonrpc.Response:
		return c.onProbeAnswer(m), nil
	case *jsonrpc.Request:
		if m.ID.IsValid() && isMasked(m.Method) {
			return false, c.refuse(ctx, m)
		}
	}
	return true, nil
}

// onProbeAnswer records what a probe response means, and reports whether it is
// still wanted: a response that arrives after the deadline has already been
// answered on the backend's behalf and would be an orphan.
func (c *interposedConn) onProbeAnswer(res *jsonrpc.Response) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.probing || res.ID != c.probeID {
		return true
	}
	if c.expired {
		return false
	}
	c.probing = false
	if res.Error != nil {
		// The client falls back to the legacy handshake from here, which
		// era: modern forbids. Recording the reason now makes the refusal
		// legible later, when the fallback is stopped.
		c.failLocked(fmt.Sprintf("answered %s with an error: %v", methodDiscover, res.Error))
		return true
	}
	c.detected = config.EraModern
	return true
}

// onProbeExpired records the deadline having passed, and reports the answer to
// give the client in the backend's place, or nil if the backend answered first.
func (c *interposedConn) onProbeExpired(gen uint64) jsonrpc.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.probing || c.probeGen != gen {
		return nil
	}
	c.probing = false
	c.expired = true
	c.failLocked(fmt.Sprintf("did not answer %s within %s", methodDiscover, c.opts.probeTimeout))
	c.opts.logger.Info("backend did not answer the era probe; falling back to the legacy handshake",
		"server", c.opts.serverID, "probeTimeout", c.opts.probeTimeout)
	return probeUnsupported(c.probeID)
}

// failLocked records why the backend cannot be modern. Under era: modern the
// reason becomes the connection error; otherwise it only explains the fallback.
func (c *interposedConn) failLocked(reason string) {
	if c.opts.era != config.EraModern {
		return
	}
	c.mismatch = fmt.Errorf("backend %q is pinned to era %q but %s: %w",
		c.opts.serverID, config.EraModern, reason, ErrEraMismatch)
}

// refuse answers a server-to-client request the gateway never invited. The
// backend gets an error, the event is recorded, and nothing about it reaches
// the rest of the gateway — least of all the other backends.
func (c *interposedConn) refuse(ctx context.Context, req *jsonrpc.Request) error {
	c.opts.logger.Warn("backend sent a request for a capability that was not advertised",
		"server", c.opts.serverID, "method", req.Method)
	res := &jsonrpc.Response{ID: req.ID, Error: &jsonrpc.Error{
		Code:    jsonrpc.CodeMethodNotFound,
		Message: fmt.Sprintf("%s is not available: the gateway does not advertise that capability", req.Method),
	}}
	if err := c.inner.Write(ctx, res); err != nil {
		return fmt.Errorf("refuse %s from backend %q: %w", req.Method, c.opts.serverID, err)
	}
	return nil
}

// Write implements [mcp.Connection], applying the era policy to what the client
// is about to say.
func (c *interposedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	req, ok := msg.(*jsonrpc.Request)
	if !ok {
		return c.write(ctx, msg)
	}
	switch req.Method {
	case methodDiscover:
		if c.opts.era == config.EraLegacy {
			// Pinned legacy: the probe is not sent at all, so a backend that
			// ignores it costs nothing. The client is answered as though the
			// backend had refused, which starts the handshake immediately.
			c.mu.Lock()
			c.detected = config.EraLegacy
			c.mu.Unlock()
			return c.inject(ctx, probeUnsupported(req.ID))
		}
		c.armProbe(req.ID)
		return c.write(ctx, req)
	case methodInitialize:
		if err := c.refuseFallback(); err != nil {
			return err
		}
		c.mu.Lock()
		c.detected = config.EraLegacy
		c.mu.Unlock()
		return c.write(ctx, req)
	default:
		return c.write(ctx, msg)
	}
}

// refuseFallback stops the legacy handshake under era: modern. Returning an
// error here — rather than after the handshake — is what keeps `initialize`
// off the backend's stdin.
func (c *interposedConn) refuseFallback() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opts.era != config.EraModern {
		return nil
	}
	if c.mismatch == nil {
		c.mismatch = fmt.Errorf("backend %q is pinned to era %q but did not complete %s: %w",
			c.opts.serverID, config.EraModern, methodDiscover, ErrEraMismatch)
	}
	return c.mismatch
}

func (c *interposedConn) armProbe(id jsonrpc.ID) {
	c.mu.Lock()
	c.probeID = id
	c.probeGen++
	c.probing = true
	c.expired = false
	gen := c.probeGen
	c.mu.Unlock()
	c.armed <- probeArm{gen: gen, d: c.opts.probeTimeout}
}

func (c *interposedConn) inject(ctx context.Context, msg jsonrpc.Message) error {
	select {
	case c.injected <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *interposedConn) write(ctx context.Context, msg jsonrpc.Message) error {
	if err := c.inner.Write(ctx, msg); err != nil {
		return fmt.Errorf("write to backend %q: %w", c.opts.serverID, err)
	}
	return nil
}

// Close implements [mcp.Connection].
func (c *interposedConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.inner.Close()
		c.stopReader()
		c.wg.Wait()
	})
	if c.closeErr != nil {
		return fmt.Errorf("close connection to backend %q: %w", c.opts.serverID, c.closeErr)
	}
	return nil
}

// SessionID implements [mcp.Connection].
func (c *interposedConn) SessionID() string { return c.inner.SessionID() }

// probeUnsupported is the answer that tells the SDK client this backend does
// not speak the modern era, which is what starts the legacy handshake.
func probeUnsupported(id jsonrpc.ID) *jsonrpc.Response {
	return &jsonrpc.Response{ID: id, Error: &jsonrpc.Error{
		Code:    jsonrpc.CodeMethodNotFound,
		Message: methodDiscover + " is not available on this backend",
	}}
}

func isMasked(method string) bool {
	for _, m := range maskedRequests {
		if m == method {
			return true
		}
	}
	return false
}

// ErrEraMismatch reports a backend whose behavior contradicts its pinned era.
var ErrEraMismatch = errors.New("backend era mismatch")

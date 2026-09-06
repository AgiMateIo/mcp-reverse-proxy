package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/agimate/mcp-reverse-proxy/internal/aggregate"
	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fanIn carries the backends' changes onto one client's subscriptions/listen
// stream.
//
// The protocol of the stream is the SDK's: it agrees the subscription against
// the advertised capabilities, acknowledges it, stamps every notification with
// the stream's subscriptionId, delivers only the kinds this client asked for,
// and holds the request open until it is cancelled. What the SDK cannot know is
// when to send anything, because the surface it would notify about is not its
// registry — it is several backend processes. So this middleware wraps the
// stream: for as long as the SDK holds it open, a pump turns each backend
// change into a notification on it.
func fanIn(s *mcp.Server, b Backend, logger *slog.Logger) mcp.Middleware {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if _, listening := req.(*mcp.SubscriptionsListenRequest); !listening {
				return next(ctx, method, req)
			}
			listener, err := b.Listen(ctx)
			if err != nil {
				return nil, err
			}
			pumpCtx, stop := context.WithCancel(ctx)
			var wg sync.WaitGroup
			wg.Go(func() { pump(pumpCtx, s, listener, logger) })
			// Ordered by hand rather than by three defers: the pump ends when
			// the context is cancelled, so waiting for it before cancelling
			// would wait forever.
			defer func() {
				stop()
				wg.Wait()
				listener.Close()
			}()
			// The SDK's handler blocks here until the client's request is
			// cancelled, which is what a stream breaking looks like from
			// inside. Everything above is released on the way out.
			return next(ctx, method, req)
		}
	}
}

// pump turns backend changes into notifications on one open stream.
func pump(ctx context.Context, s *mcp.Server, l *aggregate.Listener, logger *slog.Logger) {
	for {
		kind, ok := l.Next(ctx)
		if !ok {
			return
		}
		logger.Debug("forwarding a backend change", "kind", kind)
		announce(s, kind)
	}
}

// announce makes the SDK send one list-changed notification.
//
// The SDK sends these only as a consequence of its own registry changing, and
// exposes no way to say "tell the subscribers" directly. Adding an entry is
// therefore the only seam there is. It costs nothing here because this gateway
// does not serve its surface from that registry: every listing method is
// answered by [serve] from the backends, so the registry is written to and
// never read, and what a client sees is unchanged. The name is fixed, so the
// registry holds one entry per kind however long a stream lives.
//
// A test pins the invisibility, because it is the assumption this depends on.
func announce(s *mcp.Server, kind backend.ChangeKind) {
	switch kind {
	case backend.ChangeTools:
		s.AddTool(sentinelTool, sentinelCall)
	case backend.ChangePrompts:
		s.AddPrompt(sentinelPrompt, sentinelGet)
	case backend.ChangeResources:
		s.AddResource(sentinelResource, sentinelRead)
	}
}

// The entries whose addition stands in for "the list changed". They are never
// listed and never called: [serve] answers every listing and every call from
// the backends without consulting the registry these live in.
const sentinelName = "mcp-reverse-proxy/change"

var (
	sentinelTool = &mcp.Tool{
		Name:        sentinelName,
		Description: "Internal to the gateway; never listed.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	sentinelPrompt   = &mcp.Prompt{Name: sentinelName}
	sentinelResource = &mcp.Resource{URI: "mcp-reverse-proxy:///change", Name: sentinelName}
)

// errSentinel is what a sentinel entry would answer if it were ever reachable.
// Nothing routes to it, so this exists to make that explicit rather than to be
// returned.
var errSentinel = errors.New("the gateway's internal placeholder is not callable")

func sentinelCall(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return nil, errSentinel
}

func sentinelGet(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	return nil, errSentinel
}

func sentinelRead(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return nil, errSentinel
}

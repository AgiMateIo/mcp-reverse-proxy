package frontend_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// metaKeySubscriptionID ties a notification to the stream that asked for it.
const metaKeySubscriptionID = "io.modelcontextprotocol/subscriptionId"

// quiet is how long a test waits before concluding that nothing is coming. It
// bounds only the negative assertions; every positive one waits far longer for
// something that should arrive promptly.
const quiet = 300 * time.Millisecond

// A stream is an open subscriptions/listen request, read as it arrives.
//
// The existing helpers read a whole response body, which never returns here:
// the request stays open for as long as the client wants notifications, and
// that is the behavior under test.
type stream struct {
	events chan json.RawMessage
	cancel context.CancelFunc
}

// next returns the next frame on the stream, or fails.
func (s *stream) next(t *testing.T) json.RawMessage {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("the stream closed while waiting for a frame")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived on the stream")
		return nil
	}
}

// silent fails if anything arrives within the quiet window.
func (s *stream) silent(t *testing.T) {
	t.Helper()
	select {
	case ev := <-s.events:
		t.Fatalf("the stream carried something it should not have: %s", ev)
	case <-time.After(quiet):
	}
}

// method reads the method of a frame.
func method(t *testing.T, frame json.RawMessage) string {
	t.Helper()
	var m struct {
		Method string `json:"method"`
	}
	decode(t, frame, &m)
	return m.Method
}

// subscriptionID reads the subscription id a frame is tagged with, and fails if
// it carries none.
func subscriptionID(t *testing.T, frame json.RawMessage) any {
	t.Helper()
	var m struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
		Result struct {
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	decode(t, frame, &m)
	meta := m.Params.Meta
	if meta == nil {
		meta = m.Result.Meta
	}
	id, tagged := meta[metaKeySubscriptionID]
	if !tagged {
		t.Fatalf("the frame carries no %s: %s", metaKeySubscriptionID, frame)
	}
	return id
}

// listen opens a subscriptions/listen stream for the named notification kinds.
func listen(t *testing.T, url string, kinds ...string) *stream {
	t.Helper()
	return listenWith(t, url, nil, kinds...)
}

// listenWith opens the same stream with extra headers, which is how a test
// reaches an endpoint that authenticates its requests.
func listenWith(t *testing.T, url string, extra map[string]string, kinds ...string) *stream {
	t.Helper()
	notifications := map[string]any{}
	for _, k := range kinds {
		notifications[k] = true
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "subscriptions/listen",
		"params": map[string]any{
			"notifications": notifications,
			"_meta": map[string]any{
				metaKeyVersion:                               modernRevision,
				"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "test-client", "version": "1"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	})
	if err != nil {
		t.Fatalf("encode listen request: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		t.Fatalf("build listen request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	req.Header.Set("Mcp-Protocol-Version", modernRevision)
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	res, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader goroutine below, which owns the stream for its lifetime
	if err != nil {
		cancel()
		t.Fatalf("open the stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		_ = res.Body.Close()
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	s := &stream{events: make(chan json.RawMessage, 8), cancel: cancel}
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(s.events)
		defer func() { _ = res.Body.Close() }()
		scan := bufio.NewScanner(res.Body)
		for scan.Scan() {
			payload, ok := strings.CutPrefix(strings.TrimSpace(scan.Text()), "data:")
			if !ok {
				continue
			}
			select {
			case s.events <- json.RawMessage(strings.TrimSpace(payload)):
			case <-ctx.Done():
				return
			}
		}
	})
	// Cancelling closes the body, which ends the scan and the goroutine with
	// it, so the wait always terminates.
	t.Cleanup(func() { cancel(); wg.Wait() })
	return s
}

// change makes one backend announce that a list of its changed.
func change(t *testing.T, url, backendID, kind string) {
	t.Helper()
	changeWith(t, url, nil, backendID, kind)
}

// changeWith does the same on an endpoint that wants extra headers.
func changeWith(t *testing.T, url string, extra map[string]string, backendID, kind string) {
	t.Helper()
	headers := defaultHeaders()
	headers["Mcp-Name"] = backendID + "__change"
	for k, v := range extra {
		headers[k] = v
	}
	res := post(t, url, "tools/call", map[string]any{
		"name":      backendID + "__change",
		"arguments": map[string]any{"kind": kind},
	}, headers)
	if res.status != http.StatusOK {
		t.Fatalf("triggering a %s change on %q: status %d: %s", kind, backendID, res.status, res.body)
	}
}

// Task 10.1: the stream is acknowledged, and what arrives on it is tagged with
// the subscription it belongs to.
func TestListenIsAcknowledgedAndTagged(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAnnouncing(t, map[string]testfixtures.Mode{"gh": testfixtures.ModeModern})
	s := listen(t, url, "toolsListChanged")

	ack := s.next(t)
	if got := method(t, ack); got != "notifications/subscriptions/acknowledged" {
		t.Fatalf("first frame = %q, want the acknowledgement", got)
	}
	id := subscriptionID(t, ack)

	change(t, url, "gh", "tools")
	note := s.next(t)
	if got := method(t, note); got != "notifications/tools/list_changed" {
		t.Fatalf("frame = %q, want the tool list change", got)
	}
	if got := subscriptionID(t, note); got != id {
		t.Errorf("subscriptionId = %v, want the stream's own %v", got, id)
	}
}

// Task 10.2 and 10.3: a change reaches the client's stream from a backend of
// either era, in the same modern shape.
func TestChangesFromBothErasReachTheStream(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		mode testfixtures.Mode
	}{
		{"modern backend", testfixtures.ModeModern},
		{"legacy backend", testfixtures.ModeLegacy},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url, _, _ := serveAnnouncing(t, map[string]testfixtures.Mode{"srv": tt.mode})
			s := listen(t, url, "toolsListChanged")
			id := subscriptionID(t, s.next(t))

			change(t, url, "srv", "tools")
			note := s.next(t)
			if got := method(t, note); got != "notifications/tools/list_changed" {
				t.Fatalf("frame = %q, want the tool list change", got)
			}
			// A legacy backend sends this bare, with no stream and no id of
			// its own. What the client sees is the modern shape regardless.
			if got := subscriptionID(t, note); got != id {
				t.Errorf("subscriptionId = %v, want the stream's own %v", got, id)
			}
		})
	}
}

// Task 10.4: a client is told about what it asked for and nothing else.
func TestNotificationsAreFilteredToTheSubscription(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAnnouncing(t, map[string]testfixtures.Mode{"gh": testfixtures.ModeModern})
	s := listen(t, url, "toolsListChanged")
	s.next(t) // the acknowledgement

	change(t, url, "gh", "prompts")
	s.silent(t)

	// The same stream then carries a change it did ask for, which is what
	// shows the silence above was a filter and not a dead stream.
	change(t, url, "gh", "tools")
	if got := method(t, s.next(t)); got != "notifications/tools/list_changed" {
		t.Errorf("frame = %q, want the tool list change", got)
	}
}

// Task 10.5: one stream breaking releases what it held and leaves the others
// serving.
func TestABrokenStreamLeavesTheOthersServing(t *testing.T) {
	t.Parallel()
	url, _, g := serveAnnouncing(t, map[string]testfixtures.Mode{"gh": testfixtures.ModeModern})
	first := listen(t, url, "toolsListChanged")
	second := listen(t, url, "toolsListChanged")
	first.next(t)
	second.next(t)
	if got := g.Listeners(); got != 2 {
		t.Fatalf("listeners = %d, want 2", got)
	}

	first.cancel()
	// The release happens as the server's handler unwinds, which is after the
	// client's side of the connection is gone.
	waitFor(t, func() bool { return g.Listeners() == 1 })

	change(t, url, "gh", "tools")
	if got := method(t, second.next(t)); got != "notifications/tools/list_changed" {
		t.Errorf("the surviving stream carried %q, want the tool list change", got)
	}
}

// The notifications are driven by adding entries to the SDK's own registry,
// which works only because the gateway answers every listing from its backends
// instead. This pins that: it is the assumption the mechanism rests on.
func TestTheChangeMechanismIsInvisibleToClients(t *testing.T) {
	t.Parallel()
	url, _, _ := serveAnnouncing(t, map[string]testfixtures.Mode{"gh": testfixtures.ModeModern})
	s := listen(t, url, "toolsListChanged", "promptsListChanged", "resourcesListChanged")
	s.next(t)
	for _, kind := range []string{"tools", "prompts", "resources"} {
		change(t, url, "gh", kind)
		s.next(t)
	}

	for _, m := range []string{"tools/list", "prompts/list", "resources/list"} {
		body := string(post(t, url, m, nil, defaultHeaders()).json())
		if strings.Contains(body, "mcp-reverse-proxy/change") {
			t.Errorf("%s exposes the gateway's internal entry: %s", m, body)
		}
	}
}

// waitFor polls a condition to a deadline. It exists for the one thing a test
// cannot observe directly: a server-side handler unwinding after its client
// went away.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the condition was not reached before the deadline")
}

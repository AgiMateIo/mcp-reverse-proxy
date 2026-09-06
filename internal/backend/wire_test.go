package backend_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// probeTimeout is short enough to keep the suite quick and long enough that a
// fixture answering promptly is never mistaken for a silent one.
const probeTimeout = 200 * time.Millisecond

// A frame is one JSON-RPC message observed on the wire. The tests assert on the
// wire rather than on SDK types, because the wire is what the specs constrain.
type frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// A session is a connection to a fixture together with everything the gateway
// wrote to that fixture's stdin. It is returned even when the connection
// failed, because what the gateway did not write is often the point.
type session struct {
	conn backend.Connection
	tee  *tee
	logs *bytes.Buffer
	// changes records what the backend reported, in order, so a test can show
	// a notification arrived rather than infer it.
	changes *changeLog
}

// A changeLog collects the changes a backend reported. The callback runs on the
// connection's reading goroutine, so it is guarded.
type changeLog struct {
	mu   sync.Mutex
	seen []backend.Change
	// signal is closed-on-append, so a test can wait for one without sleeping.
	signal chan struct{}
}

func newChangeLog() *changeLog { return &changeLog{signal: make(chan struct{}, 8)} }

func (l *changeLog) add(c backend.Change) {
	l.mu.Lock()
	l.seen = append(l.seen, c)
	l.mu.Unlock()
	select {
	case l.signal <- struct{}{}:
	default:
	}
}

// await waits for one more change than it has already seen, or fails.
func (l *changeLog) await(t *testing.T) backend.Change {
	t.Helper()
	select {
	case <-l.signal:
	case <-time.After(5 * time.Second):
		t.Fatal("no change arrived from the backend")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen[len(l.seen)-1]
}

// dial starts a fixture with default options and connects the gateway to it.
func dial(t *testing.T, mode testfixtures.Mode, server config.Server) (*session, error) {
	t.Helper()
	return dialWith(t, mode, testfixtures.Options{}, server)
}

// dialWith starts a fixture with the given options and connects the gateway.
func dialWith(t *testing.T, mode testfixtures.Mode, opts testfixtures.Options, server config.Server) (*session, error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stdin := &tee{WriteCloser: inW}

	var wg sync.WaitGroup
	wg.Go(func() {
		err := testfixtures.RunWith(context.Background(), mode, opts, inR, outW)
		outW.CloseWithError(err)
	})

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	changes := newChangeLog()
	conn, err := backend.NewConnector("test", probeTimeout, logger).
		Connect(t.Context(), server, backend.Pipes{Stdout: outR, Stdin: stdin}, changes.add)

	t.Cleanup(func() {
		if conn != nil {
			_ = conn.Close()
		}
		// Closing stdin is how the gateway stops a backend, and it is what
		// ends the fixture here too.
		_ = stdin.Close()
		wg.Wait()
	})
	return &session{conn: conn, tee: stdin, logs: logs, changes: changes}, err
}

// mustDial fails the test if the connection could not be established.
func mustDial(t *testing.T, mode testfixtures.Mode, server config.Server) *session {
	t.Helper()
	s, err := dial(t, mode, server)
	if err != nil {
		t.Fatalf("connect to the %s fixture: %v", mode, err)
	}
	return s
}

// A tee records everything written through it, so a test can see exactly what
// reached the backend's stdin — including what never did.
type tee struct {
	io.WriteCloser
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tee) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf.Write(p)
	t.mu.Unlock()
	return t.WriteCloser.Write(p)
}

// frames returns every message written to the backend so far.
func (t *tee) frames(tb testing.TB) []frame {
	tb.Helper()
	t.mu.Lock()
	raw := t.buf.String()
	t.mu.Unlock()

	var frames []frame
	scan := bufio.NewScanner(strings.NewReader(raw))
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			tb.Fatalf("decode frame %q: %v", line, err)
		}
		frames = append(frames, f)
	}
	return frames
}

// methods returns the methods written to the backend, in order.
func (t *tee) methods(tb testing.TB) []string {
	tb.Helper()
	var methods []string
	for _, f := range t.frames(tb) {
		if f.Method != "" {
			methods = append(methods, f.Method)
		}
	}
	return methods
}

// count returns how many times a method was written to the backend.
func (t *tee) count(tb testing.TB, method string) int {
	tb.Helper()
	n := 0
	for _, m := range t.methods(tb) {
		if m == method {
			n++
		}
	}
	return n
}

// params returns the params of the first request for a method.
func (t *tee) params(tb testing.TB, method string) json.RawMessage {
	tb.Helper()
	for _, f := range t.frames(tb) {
		if f.Method == method {
			return f.Params
		}
	}
	tb.Fatalf("no %s was written to the backend; saw %v", method, t.methods(tb))
	return nil
}

package backend_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/backend"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

const (
	methodDiscover            = "server/discover"
	methodInitialize          = "initialize"
	methodSubscriptionsListen = "subscriptions/listen"
)

// Task 4.2: era detection under `auto`, against a backend of each era.
func TestEraDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode testfixtures.Mode
		want config.Era
		// wantMethods is what the gateway should have written to the backend
		// while establishing the session.
		wantMethods []string
	}{
		{
			name: "modern backend answers the probe",
			mode: testfixtures.ModeModern,
			want: config.EraModern,
			// No handshake at all: revision 2026-07-28 removed it. The
			// listen stream follows the probe because the gateway asks a
			// modern backend to report its own changes, which is the only
			// way it hears about one.
			wantMethods: []string{methodDiscover, methodSubscriptionsListen},
		},
		{
			name: "legacy backend refuses the probe",
			mode: testfixtures.ModeLegacy,
			want: config.EraLegacy,
			wantMethods: []string{
				methodDiscover, methodInitialize, "notifications/initialized",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := mustDial(t, tt.mode, config.Server{ID: "srv", Era: config.EraAuto})
			if got := s.conn.Era(); got != tt.want {
				t.Errorf("Era = %q, want %q", got, tt.want)
			}
			if got := s.tee.methods(t); !equal(got, tt.wantMethods) {
				t.Errorf("wrote %v to the backend, want %v", got, tt.wantMethods)
			}
		})
	}
}

// Task 4.6: a silent backend must cost the probe timeout and no more. The SDK's
// own fall-back triggers on an error, and silence produces none.
func TestProbeTimeoutFallsBack(t *testing.T) {
	t.Parallel()
	s := mustDial(t, testfixtures.ModeSilent, config.Server{ID: "srv", Era: config.EraAuto})
	if got := s.conn.Era(); got != config.EraLegacy {
		t.Errorf("Era = %q, want %q", got, config.EraLegacy)
	}
	// The probe went out and was never answered; the handshake still happened.
	if n := s.tee.count(t, methodDiscover); n != 1 {
		t.Errorf("wrote %d probes, want 1", n)
	}
	if n := s.tee.count(t, methodInitialize); n != 1 {
		t.Errorf("wrote %d handshakes, want 1", n)
	}
	if !strings.Contains(s.logs.String(), "did not answer the era probe") {
		t.Errorf("the fallback was not recorded in the log: %s", s.logs)
	}
}

// Task 4.7: `era: legacy` skips the probe outright, which is what keeps a
// backend that ignores server/discover from costing a timeout on every cold
// start.
func TestPinnedLegacySkipsTheProbe(t *testing.T) {
	t.Parallel()
	s := mustDial(t, testfixtures.ModeLegacy, config.Server{ID: "srv", Era: config.EraLegacy})
	if got := s.conn.Era(); got != config.EraLegacy {
		t.Errorf("Era = %q, want %q", got, config.EraLegacy)
	}
	if n := s.tee.count(t, methodDiscover); n != 0 {
		t.Errorf("%s reached a backend pinned to legacy %d times, want 0: %v",
			methodDiscover, n, s.tee.methods(t))
	}
	if n := s.tee.count(t, methodInitialize); n != 1 {
		t.Errorf("wrote %d handshakes, want 1", n)
	}
}

// Task 4.8: `era: modern` must not fall back, and must say why. Both a refusing
// backend and a silent one are mismatches.
func TestPinnedModernRefusesToFallBack(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode testfixtures.Mode
		// wantReason distinguishes the two ways a modern pin can fail.
		wantReason string
	}{
		{"backend refuses the probe", testfixtures.ModeLegacy, "with an error"},
		{"backend ignores the probe", testfixtures.ModeSilent, "did not answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := dial(t, tt.mode, config.Server{ID: "srv", Era: config.EraModern})
			if err == nil {
				t.Fatal("connect succeeded, want a mismatch error")
			}
			if !errors.Is(err, backend.ErrEraMismatch) {
				t.Errorf("errors.Is(err, ErrEraMismatch) = false for %v", err)
			}
			for _, want := range []string{`"srv"`, "modern", tt.wantReason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
			// The point of the pin: no handshake is attempted, so a mismatch
			// is never disguised as a working legacy session.
			if n := s.tee.count(t, methodInitialize); n != 0 {
				t.Errorf("%s reached a backend pinned to modern %d times, want 0: %v",
					methodInitialize, n, s.tee.methods(t))
			}
		})
	}
}

// Task 4.9: the era is settled once, when the session is established. Later
// requests must not probe again.
func TestEraIsNotProbedAgain(t *testing.T) {
	t.Parallel()
	s := mustDial(t, testfixtures.ModeModern, config.Server{ID: "srv", Era: config.EraAuto})
	for range 3 {
		if _, err := s.conn.ListTools(t.Context()); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if got := s.conn.Era(); got != config.EraModern {
			t.Fatalf("Era = %q, want %q", got, config.EraModern)
		}
	}
	if n := s.tee.count(t, methodDiscover); n != 1 {
		t.Errorf("wrote %d probes across four requests, want 1: %v", n, s.tee.methods(t))
	}
}

// Task 4.10: the gateway advertises none of roots, sampling, elicitation or
// logging, so a correctly written backend never sends the server-to-client
// requests a stateless front end could not carry.
func TestCapabilitiesAreMasked(t *testing.T) {
	t.Parallel()
	s := mustDial(t, testfixtures.ModeLegacy, config.Server{ID: "srv", Era: config.EraAuto})

	var params struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	raw := s.tee.params(t, methodInitialize)
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("decode %s params %s: %v", methodInitialize, raw, err)
	}
	for _, masked := range []string{"roots", "sampling", "elicitation", "logging"} {
		if _, found := params.Capabilities[masked]; found {
			t.Errorf("%q was advertised to the backend: %s", masked, raw)
		}
	}
}

// Task 4.11: a backend that sends a request anyway is refused and recorded, and
// must not take down a second backend running beside it.
func TestUnsolicitedRequestIsRefused(t *testing.T) {
	t.Parallel()
	misbehaving := mustDial(t, testfixtures.ModeMisbehaving, config.Server{ID: "bad", Era: config.EraAuto})
	wellBehaved := mustDial(t, testfixtures.ModeLegacy, config.Server{ID: "good", Era: config.EraAuto})

	// The unsolicited request arrives after the handshake; the session has to
	// keep working through it.
	if _, err := misbehaving.conn.ListTools(t.Context()); err != nil {
		t.Errorf("the misbehaving backend stopped serving after its request was refused: %v", err)
	}
	if _, err := wellBehaved.conn.ListTools(t.Context()); err != nil {
		t.Errorf("a well-behaved backend was disrupted by its neighbour: %v", err)
	}
	if logs := misbehaving.logs.String(); !strings.Contains(logs, "sampling/createMessage") ||
		!strings.Contains(logs, "not advertised") {
		t.Errorf("the violation was not recorded in the log: %s", logs)
	}
	if logs := wellBehaved.logs.String(); strings.Contains(logs, "sampling/createMessage") {
		t.Errorf("a neighbour's violation was recorded against the wrong backend: %s", logs)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

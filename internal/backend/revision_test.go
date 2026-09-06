package backend_test

import (
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/testfixtures"
)

// Task 4.3: legacy is not one revision. The handshake offers 2025-11-25, the
// backend answers with its own, and the gateway adopts what it was told.
func TestLegacyRevisionIsAdopted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		revision string
	}{
		{"the revision the gateway offers", "2025-11-25"},
		{"an older published revision", "2025-06-18"},
		{"the earliest published revision", "2024-11-05"},
	}

	// Every revision must produce the same surface; a difference would mean
	// the era translation leaks into what the client sees.
	var reference []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := dialWith(t, testfixtures.ModeLegacy,
				testfixtures.Options{Revision: tt.revision},
				config.Server{ID: "srv", Era: config.EraAuto})
			if err != nil {
				t.Fatalf("connect to a %s backend: %v", tt.revision, err)
			}
			if got := s.conn.Era(); got != config.EraLegacy {
				t.Errorf("Era = %q, want %q", got, config.EraLegacy)
			}
			if got := s.conn.Revision(); got != tt.revision {
				t.Errorf("Revision = %q, want %q — the backend's revision, not the offered one", got, tt.revision)
			}
			list, err := s.conn.ListTools(t.Context())
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			var names []string
			for _, tool := range list.Tools {
				names = append(names, tool.Name)
			}
			if reference == nil {
				reference = names
			} else if !equal(names, reference) {
				t.Errorf("tools = %v, want %v — the same as at %s", names, reference, tests[0].revision)
			}
			if list.ResultType != "complete" || list.Cache.CacheScope != "private" {
				t.Errorf("result was not normalized: %+v", list)
			}
		})
	}
}

// Task 4.4: a revision the gateway does not support is a connection failure
// that says which server and which revision.
func TestUnsupportedRevisionFailsConnection(t *testing.T) {
	t.Parallel()
	const revision = "2019-01-01"
	_, err := dialWith(t, testfixtures.ModeLegacy,
		testfixtures.Options{Revision: revision},
		config.Server{ID: "srv", Era: config.EraAuto})
	if err == nil {
		t.Fatal("connect succeeded against an unsupported revision, want an error")
	}
	for _, want := range []string{`"srv"`, revision} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// Task 4.5: a modern backend that refuses the offered revision names what it
// speaks, and the gateway retries the probe rather than giving up on the modern
// era — the -32022 answer is not the "arbitrary error" that triggers fallback.
func TestVersionNegotiationRetriesTheProbe(t *testing.T) {
	t.Parallel()
	s, err := dialWith(t, testfixtures.ModeModern,
		testfixtures.Options{NegotiateOnce: true},
		config.Server{ID: "srv", Era: config.EraAuto})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := s.conn.Era(); got != config.EraModern {
		t.Errorf("Era = %q, want %q", got, config.EraModern)
	}
	if n := s.tee.count(t, methodDiscover); n != 2 {
		t.Errorf("wrote %d probes, want 2 — one refused, one retried: %v", n, s.tee.methods(t))
	}
	if n := s.tee.count(t, methodInitialize); n != 0 {
		t.Errorf("fell back to %s after a version error, want no fallback: %v",
			methodInitialize, s.tee.methods(t))
	}
}

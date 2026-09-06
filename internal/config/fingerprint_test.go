package config_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

func base() config.Server {
	return config.Server{
		ID:      "filesystem",
		Command: "mcp-server-filesystem",
		Args:    []string{"--root", "/srv/data"},
		Env:     config.Env{"FS_TOKEN": config.Secret(token), "FS_MODE": "ro"},
		Era:     config.EraAuto,
	}
}

func newFingerprinter(t *testing.T) *config.Fingerprinter {
	t.Helper()
	f, err := config.NewFingerprinter()
	if err != nil {
		t.Fatalf("NewFingerprinter: %v", err)
	}
	return f
}

// The same configuration fingerprints equally, however its map happens to be
// laid out — otherwise the pool would miss and spawn a duplicate process.
func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()
	f := newFingerprinter(t)
	reordered := base()
	reordered.Env = config.Env{"FS_MODE": "ro", "FS_TOKEN": config.Secret(token)}
	if a, b := f.Server(base()), f.Server(reordered); a != b {
		t.Errorf("fingerprints differ for equal configurations: %s != %s", a, b)
	}
}

// Anything that changes which process would be started changes the
// fingerprint. An env value in particular: two subjects whose configurations
// differ only by a token must not share a process.
func TestFingerprintDistinguishes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*config.Server)
	}{
		{"env value", func(s *config.Server) { s.Env["FS_TOKEN"] = "another-token" }},
		{"env key", func(s *config.Server) { s.Env["FS_EXTRA"] = "" }},
		{"env key removed", func(s *config.Server) { delete(s.Env, "FS_MODE") }},
		{"id", func(s *config.Server) { s.ID = "files" }},
		{"command", func(s *config.Server) { s.Command = "mcp-server-fs" }},
		{"era", func(s *config.Server) { s.Era = config.EraLegacy }},
		{"argument value", func(s *config.Server) { s.Args = []string{"--root", "/srv/other"} }},
		{"argument count", func(s *config.Server) { s.Args = []string{"--root"} }},
		// The same bytes regrouped across arguments must not collide.
		{"argument regrouping", func(s *config.Server) { s.Args = []string{"--roo", "t/srv/data"} }},
	}
	f := newFingerprinter(t)
	unchanged := f.Server(base())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := base()
			tt.mutate(&s)
			if got := f.Server(s); got == unchanged {
				t.Errorf("fingerprint unchanged after altering the %s", tt.name)
			}
		})
	}
}

// The digest is keyed per process, so a fingerprint that reaches a log line or
// a metric label cannot be replayed against a guessed environment value.
func TestFingerprintIsKeyedPerProcess(t *testing.T) {
	t.Parallel()
	a, b := newFingerprinter(t), newFingerprinter(t)
	if a.Server(base()) == b.Server(base()) {
		t.Error("two fingerprinters produced the same digest, so the key is not random")
	}
}

// The fingerprint is a digest and nothing else: no part of the configuration
// survives in it.
func TestFingerprintRetainsNothing(t *testing.T) {
	t.Parallel()
	got := newFingerprinter(t).Server(base())
	raw, err := hex.DecodeString(got)
	if err != nil {
		t.Fatalf("fingerprint %q is not hex: %v", got, err)
	}
	if len(raw) != 32 {
		t.Errorf("fingerprint is %d bytes, want a 32-byte digest", len(raw))
	}
	for _, secret := range []string{token, "mcp-server-filesystem", "/srv/data", "FS_TOKEN"} {
		if strings.Contains(got, secret) {
			t.Errorf("fingerprint contains %q: %s", secret, got)
		}
	}
}

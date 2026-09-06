package config_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

func load(t *testing.T, name string) (*config.File, error) {
	t.Helper()
	return config.Load(t.Context(), filepath.Join("testdata", name))
}

// A valid file loads into the expected struct, values and all.
func TestLoadValid(t *testing.T) {
	t.Parallel()
	got, err := load(t, "valid.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := &config.File{
		Servers: []config.Server{
			{
				ID:      "filesystem",
				Command: "mcp-server-filesystem",
				Args:    []string{"--root", "/srv/data"},
				Env:     config.Env{"FS_TOKEN": "s3cret", "FS_MODE": "ro"},
				Era:     config.EraLegacy,
			},
			{
				ID:      "search",
				Command: "mcp-server-search",
				Era:     config.EraModern,
			},
		},
		Limits: config.Limits{
			MaxProcesses:           32,
			MaxProcessesPerSubject: 4,
			IdleTTL:                config.Duration(90 * time.Second),
			ProbeTimeout:           config.Duration(1500 * time.Millisecond),
			MaxHeaderBytes:         16384,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", got, want)
	}
	// The struct holds the real value even though every printed form hides it.
	if secret := got.Servers[0].Env["FS_TOKEN"].Reveal(); secret != "s3cret" {
		t.Errorf("FS_TOKEN = %q, want %q", secret, "s3cret")
	}
}

// A file that omits optional fields gets the documented defaults.
func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	got, err := load(t, "defaults.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if era := got.Servers[0].Era; era != config.EraAuto {
		t.Errorf("era = %q, want %q when the field is omitted", era, config.EraAuto)
	}
	if got.Limits != config.DefaultLimits {
		t.Errorf("limits = %+v, want %+v", got.Limits, config.DefaultLimits)
	}
}

// Defaulting must not manufacture a conflict with a limit the operator did set.
func TestLoadDefaultsRespectLoweredGlobalLimit(t *testing.T) {
	t.Parallel()
	got, err := load(t, "low-global-limit.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Limits.MaxProcessesPerSubject > got.Limits.MaxProcesses {
		t.Errorf("per-subject limit %d exceeds global limit %d",
			got.Limits.MaxProcessesPerSubject, got.Limits.MaxProcesses)
	}
}

// An empty server list is valid: under the define-new policy mode every server
// arrives in the request header.
func TestLoadAcceptsNoServers(t *testing.T) {
	t.Parallel()
	got, err := load(t, "no-servers.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Servers) != 0 {
		t.Errorf("servers = %+v, want none", got.Servers)
	}
}

// Every rejection names the file and says what specifically is wrong.
func TestLoadRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		file string
		// want are fragments the message must contain, beyond the file name.
		want []string
	}{
		{"malformed json", "malformed.json", []string{"invalid character"}},
		{"missing id", "missing-id.json", []string{"servers[0]", "missing id"}},
		{"missing command", "missing-command.json", []string{"servers[0]", `"search"`, "missing command"}},
		{"duplicate id", "duplicate-id.json", []string{"servers[1]", `"search"`, "servers[0]"}},
		{"unknown era", "unknown-era.json", []string{"servers[0]", `era "ancient"`}},
		{"unknown field", "unknown-field.json", []string{"commnd"}},
		// encoding/json drops the field path when a custom unmarshaler
		// fails, so the message locates the problem by quoting the value.
		{"bad duration", "bad-duration.json", []string{"duration", `"5 fortnights"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := load(t, tt.file)
			if err == nil {
				t.Fatalf("Load(%s) succeeded, want an error", tt.file)
			}
			if !errors.Is(err, config.ErrInvalid) {
				t.Errorf("errors.Is(err, ErrInvalid) = false for %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.file) {
				t.Errorf("error does not name the file: %v", msg)
			}
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not mention %q: %v", want, msg)
				}
			}
		})
	}
}

// A missing file is reported as such, not as a parse failure.
func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "absent.json")
	_, err := config.Load(t.Context(), path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

// A cancelled startup does not wait on the filesystem.
func TestLoadHonorsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := config.Load(ctx, filepath.Join("testdata", "valid.json"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// A rejection must not carry an environment value into the message.
func TestLoadErrorsWithholdSecrets(t *testing.T) {
	t.Parallel()
	// Both files carry a secret and a defect, so a message that echoed its
	// input would fail here. The second defect is in the env value itself,
	// which is the input most likely to be quoted back.
	for _, file := range []string{"secret-and-unknown-era.json", "secret-wrong-type.json"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			_, err := load(t, file)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "31337") {
				t.Errorf("error leaked an environment value: %v", err)
			}
		})
	}
}

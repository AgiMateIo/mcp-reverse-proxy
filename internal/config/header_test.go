package config_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

// G101: a made-up value standing in for a credential, so a test can prove it
// never escapes.
const headerSecret = "sk-live-do-not-log-me" //nolint:gosec // see above

func baseFile() *config.File {
	return &config.File{
		Servers: []config.Server{{
			ID:      "files",
			Command: "mcp-server-files",
			Args:    []string{"--root", "/srv"},
			Env:     config.Env{"FS_TOKEN": "base-token", "FS_MODE": "ro", "FS_REGION": "eu"},
			Era:     config.EraLegacy,
		}},
		Limits: config.DefaultLimits,
	}
}

func parse(t *testing.T, value string) *config.Header {
	t.Helper()
	h, err := config.ParseHeader(config.Secret(value), 8<<10)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	return h
}

// Task 8.3: the environment merges key by key; everything else replaces.
func TestResolveMergeRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header string
		want   func(*testing.T, config.Server)
	}{
		{
			name:   "one environment variable of three",
			header: `{"servers":[{"id":"files","env":{"FS_TOKEN":"` + headerSecret + `"}}]}`,
			want: func(t *testing.T, s config.Server) {
				if got := slices.Sorted(maps.Keys(s.Env)); !slices.Equal(got, []string{"FS_MODE", "FS_REGION", "FS_TOKEN"}) {
					t.Errorf("env keys = %v, want all three kept", got)
				}
				if got := s.Env["FS_TOKEN"].Reveal(); got != headerSecret {
					t.Errorf("FS_TOKEN = %q, want the header's value", got)
				}
				if got := s.Env["FS_MODE"].Reveal(); got != "ro" {
					t.Errorf("FS_MODE = %q, want the base value untouched", got)
				}
			},
		},
		{
			name:   "arguments replace rather than extend",
			header: `{"servers":[{"id":"files","args":["--root","/tmp"]}]}`,
			want: func(t *testing.T, s config.Server) {
				if want := []string{"--root", "/tmp"}; !slices.Equal(s.Args, want) {
					t.Errorf("args = %v, want %v", s.Args, want)
				}
			},
		},
		{
			// Not the same as leaving args alone: a server that takes no
			// arguments is a thing a header may ask for.
			name:   "an empty argument list is a value, not an omission",
			header: `{"servers":[{"id":"files","args":[]}]}`,
			want: func(t *testing.T, s config.Server) {
				if len(s.Args) != 0 {
					t.Errorf("args = %v, want none", s.Args)
				}
			},
		},
		{
			name:   "an untouched server keeps everything",
			header: `{"servers":[]}`,
			want: func(t *testing.T, s config.Server) {
				if want := []string{"--root", "/srv"}; !slices.Equal(s.Args, want) {
					t.Errorf("args = %v, want %v", s.Args, want)
				}
				if s.Era != config.EraLegacy {
					t.Errorf("era = %q, want it untouched", s.Era)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := baseFile()
			resolved, err := config.Resolve(base, parse(t, tt.header))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(resolved) != 1 {
				t.Fatalf("resolved %d servers, want 1", len(resolved))
			}
			tt.want(t, resolved[0])

			// The base is shared by every subject: a request that edited it
			// would change what everybody else sees.
			if got := base.Servers[0].Env["FS_TOKEN"].Reveal(); got != "base-token" {
				t.Errorf("the base configuration was modified: FS_TOKEN = %q", got)
			}
			if want := []string{"--root", "/srv"}; !slices.Equal(base.Servers[0].Args, want) {
				t.Errorf("the base configuration was modified: args = %v", base.Servers[0].Args)
			}
		})
	}
}

// Task 8.3: a server the base does not describe is added to the set.
func TestResolveDeclaresNewServer(t *testing.T) {
	t.Parallel()
	header := parse(t, `{"servers":[{"id":"scratch","command":"/usr/bin/true","args":["-x"]}]}`)
	resolved, err := config.Resolve(baseFile(), header)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved %d servers, want the base one and the declared one", len(resolved))
	}
	added := resolved[1]
	if added.ID != "scratch" || added.Command != "/usr/bin/true" {
		t.Errorf("declared server = %+v, want the header's own", added)
	}
	// A server the header did not date gets the same default a file's would.
	if added.Era != config.EraAuto {
		t.Errorf("era = %q, want %q", added.Era, config.EraAuto)
	}
}

// A header cannot resolve to a server set the gateway would refuse from a file.
func TestResolveValidatesTheResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header string
	}{
		{"a declared server with no command", `{"servers":[{"id":"scratch","env":{"A":"b"}}]}`},
		{"a known server emptied of its command", `{"servers":[{"id":"files","command":""}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Resolve(baseFile(), parse(t, tt.header)); !errors.Is(err, config.ErrInvalid) {
				t.Errorf("err = %v, want it to wrap %v", err, config.ErrInvalid)
			}
		})
	}
}

// Tasks 8.1 and 8.2: a header the gateway will not read is refused, and the
// refusal says why without repeating what it refused.
func TestParseHeaderRejects(t *testing.T) {
	t.Parallel()
	oversized := `{"servers":[{"id":"files","env":{"FS_TOKEN":"` + strings.Repeat(headerSecret, 100) + `"}}]}`
	tests := []struct {
		name  string
		value string
		limit int
		want  error
		// says is what the message must contain, beyond not containing the
		// value.
		says string
	}{
		{
			name:  "not JSON at all",
			value: `{"servers":[{"id":"files","env":{"FS_TOKEN":"` + headerSecret + `"`,
			limit: 8 << 10,
			want:  config.ErrHeaderMalformed,
			says:  "servers",
		},
		{
			name:  "an unknown field",
			value: `{"serverz":[]}`,
			limit: 8 << 10,
			want:  config.ErrHeaderMalformed,
			says:  "servers",
		},
		{
			name:  "a patch naming no server",
			value: `{"servers":[{"env":{"A":"b"}}]}`,
			limit: 8 << 10,
			want:  config.ErrHeaderMalformed,
			says:  "id",
		},
		{
			// A File built anywhere but Load carries no limit, and that must
			// not read as permission to send anything.
			name:  "larger than the default limit, with none configured",
			value: strings.Repeat(oversized, 200),
			limit: 0,
			want:  config.ErrHeaderTooLarge,
			says:  "8192",
		},
		{
			name:  "larger than the limit",
			value: oversized,
			limit: 64,
			want:  config.ErrHeaderTooLarge,
			says:  "64",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseHeader(config.Secret(tt.value), tt.limit)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.says) {
				t.Errorf("message does not mention %q: %v", tt.says, err)
			}
			// The header is where a subject's credentials arrive; an error
			// message is the easiest place for them to escape.
			if strings.Contains(err.Error(), headerSecret) {
				t.Errorf("the refusal repeated the header value: %v", err)
			}
		})
	}
}

func TestParseHeaderAcceptsTheLimitExactly(t *testing.T) {
	t.Parallel()
	value := `{"servers":[]}`
	if _, err := config.ParseHeader(config.Secret(value), len(value)); err != nil {
		t.Errorf("a header exactly at the limit was refused: %v", err)
	}
}

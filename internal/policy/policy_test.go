package policy_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
	"github.com/agimate/mcp-reverse-proxy/internal/policy"
)

// G101: a made-up value standing in for a credential, so a test can prove it
// never escapes.
const secretValue = "sk-live-do-not-log-me" //nolint:gosec // see above

func base() *config.File {
	return &config.File{
		Servers: []config.Server{{
			ID:      "files",
			Command: "/usr/bin/mcp-files",
			Env:     config.Env{"FS_TOKEN": "base-token"},
			Era:     config.EraAuto,
		}},
		Limits: config.DefaultLimits,
	}
}

func header(t *testing.T, value string) *config.Header {
	t.Helper()
	if value == "" {
		return nil
	}
	h, err := config.ParseHeader(config.Secret(value), 8<<10)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	return h
}

func mustPolicy(t *testing.T, c config.PolicyConfig) *policy.Policy {
	t.Helper()
	p, err := policy.New(c)
	if err != nil {
		t.Fatalf("policy.New: %v", err)
	}
	return p
}

// check runs the whole decision the way the request path does: resolve, then
// judge what the header asked for.
func check(t *testing.T, p *policy.Policy, scopes []string, h *config.Header) error {
	t.Helper()
	b := base()
	claims := auth.Claims{Scopes: scopes}
	if err := p.CheckHeader(claims, b, h); err != nil {
		return err
	}
	resolved, err := config.Resolve(b, h)
	if err != nil {
		return err
	}
	return p.CheckResolved(resolved, h)
}

const (
	overrideEnvOfKnown = `{"servers":[{"id":"files","env":{"FS_TOKEN":"` + secretValue + `"}}]}`
	overrideCommand    = `{"servers":[{"id":"files","command":"/usr/bin/other"}]}`
	overrideArgs       = `{"servers":[{"id":"files","args":["--x"]}]}`
	declareNewServer   = `{"servers":[{"id":"scratch","command":"/usr/bin/true"}]}`
	// G101: a header body, not a credential.
	declareAndRecredit = `{"servers":[{"id":"scratch","command":"/usr/bin/true"},{"id":"files","env":{"FS_TOKEN":"x"}}]}` //nolint:gosec // see above
	everyScope         = "mcp:config:env mcp:config:override mcp:config:define"
)

// Task 8.4 and 8.5: each mode permits and refuses exactly what the spec says.
func TestModes(t *testing.T) {
	t.Parallel()
	allScopes := strings.Fields(everyScope)
	tests := []struct {
		name   string
		mode   string
		header string
		want   error
	}{
		{"off refuses a header", "off", overrideEnvOfKnown, policy.ErrHeaderDisabled},
		{"off does not obstruct a request without one", "off", "", nil},
		{"env-only permits an environment override", "env-only", overrideEnvOfKnown, nil},
		{"env-only refuses a command substitution", "env-only", overrideCommand, policy.ErrInsufficientPrivileges},
		{"env-only refuses an argument substitution", "env-only", overrideArgs, policy.ErrInsufficientPrivileges},
		{"env-only refuses a new server", "env-only", declareNewServer, policy.ErrInsufficientPrivileges},
		{"override-known permits a command substitution", "override-known", overrideCommand, nil},
		{"override-known refuses a new server", "override-known", declareNewServer, policy.ErrInsufficientPrivileges},
		{"define-new permits a new server", "define-new", declareNewServer, nil},
		{"define-new permits everything below it", "define-new", overrideEnvOfKnown, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := mustPolicy(t, config.PolicyConfig{
				Mode:             tt.mode,
				CommandAllowlist: []string{"/usr/bin/mcp-files", "/usr/bin/other", "/usr/bin/true"},
			})
			err := check(t, p, allScopes, header(t, tt.header))
			if tt.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want the header applied", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
			if !errors.Is(err, policy.ErrPolicyViolation) {
				t.Errorf("err = %v, want it to be recognizable as a policy violation", err)
			}
		})
	}
}

// Task 8.5: a deployment's own decision is not a privilege the client can
// acquire, so refusing over it must not send the client after a scope.
func TestModeOffOffersNoEscalation(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, config.PolicyConfig{Mode: "off"})
	err := check(t, p, nil, header(t, overrideEnvOfKnown))
	if !errors.Is(err, policy.ErrHeaderDisabled) {
		t.Fatalf("err = %v, want it to wrap %v", err, policy.ErrHeaderDisabled)
	}
	var scopes *policy.ScopeError
	if errors.As(err, &scopes) {
		t.Errorf("the refusal asked for scopes %v, which no token could turn into permission", scopes.Missing)
	}
}

// The same holds when the deployment's mode is merely too low: widening the
// token cannot raise a ceiling the deployment set.
func TestInsufficientPrivilegesOffersNoEscalation(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, config.PolicyConfig{Mode: "env-only"})
	err := check(t, p, strings.Fields(everyScope), header(t, overrideCommand))
	var scopes *policy.ScopeError
	if errors.As(err, &scopes) {
		t.Errorf("the refusal asked for scopes %v, although the token already held every one", scopes.Missing)
	}
}

// Task 8.6: a scope the deployment would honour is worth asking for, and every
// one the request needs is named at once.
func TestMissingScopes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header string
		held   []string
		want   []string
	}{
		{
			name:   "one privilege, one scope",
			header: overrideEnvOfKnown,
			want:   []string{policy.ScopeEnv},
		},
		{
			name:   "two privileges at once, both named",
			header: declareAndRecredit,
			want:   []string{policy.ScopeDefine, policy.ScopeEnv},
		},
		{
			name:   "only what is actually missing",
			header: declareAndRecredit,
			held:   []string{policy.ScopeEnv},
			want:   []string{policy.ScopeDefine},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := mustPolicy(t, config.PolicyConfig{
				Mode:             "define-new",
				CommandAllowlist: []string{"/usr/bin/mcp-files", "/usr/bin/true"},
			})
			err := check(t, p, tt.held, header(t, tt.header))
			var scopes *policy.ScopeError
			if !errors.As(err, &scopes) {
				t.Fatalf("err = %v, want a scope error", err)
			}
			if !slices.Equal(scopes.Missing, tt.want) {
				t.Errorf("missing = %v, want %v in one answer", scopes.Missing, tt.want)
			}
			if !errors.Is(err, policy.ErrMissingScope) {
				t.Errorf("err = %v, want it to wrap %v", err, policy.ErrMissingScope)
			}
			if !errors.Is(err, policy.ErrPolicyViolation) {
				t.Errorf("err = %v, want it to be recognizable as a policy violation", err)
			}
		})
	}
}

// Task 8.7: the allowlist and denylist hold whatever the mode is.
func TestCommandAllowlistAndEnvDenylist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		allowlist []string
		denylist  []string
		header    string
		want      error
		// names is what the refusal must identify.
		names string
	}{
		{
			name:      "a declared command outside the allowlist",
			allowlist: []string{"/usr/bin/mcp-files"},
			header:    `{"servers":[{"id":"scratch","command":"/bin/sh"}]}`,
			want:      policy.ErrCommandNotAllowed,
			names:     "/bin/sh",
		},
		{
			// The command is the base one, untouched, but it was never
			// permitted; a header that only re-credits it must not slip past.
			name:      "a known server whose own command is not allowed",
			allowlist: []string{"/usr/bin/something-else"},
			header:    overrideEnvOfKnown,
			want:      policy.ErrCommandNotAllowed,
			names:     "/usr/bin/mcp-files",
		},
		{
			name:      "a denied environment key",
			allowlist: []string{"/usr/bin/mcp-files"},
			denylist:  []string{"LD_PRELOAD"},
			header:    `{"servers":[{"id":"files","env":{"LD_PRELOAD":"` + secretValue + `"}}]}`,
			want:      policy.ErrEnvKeyDenied,
			names:     "LD_PRELOAD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := mustPolicy(t, config.PolicyConfig{
				Mode:             "define-new",
				CommandAllowlist: tt.allowlist,
				EnvDenylist:      tt.denylist,
			})
			err := check(t, p, strings.Fields(everyScope), header(t, tt.header))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.want)
			}
			// Keys are named because the spec requires it; values never are.
			if !strings.Contains(err.Error(), tt.names) {
				t.Errorf("refusal does not name %q: %v", tt.names, err)
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("refusal carried an environment value: %v", err)
			}
		})
	}
}

// Task 8.8: define-new without an allowlist would be an open invitation to run
// anything, so the gateway refuses to start that way.
func TestDefineNewRequiresAnAllowlist(t *testing.T) {
	t.Parallel()
	_, err := policy.New(config.PolicyConfig{Mode: "define-new"})
	if !errors.Is(err, policy.ErrNoAllowlist) {
		t.Fatalf("err = %v, want it to wrap %v", err, policy.ErrNoAllowlist)
	}
	// Startup failures are told apart by this sentinel throughout.
	if !errors.Is(err, config.ErrInvalid) {
		t.Errorf("err = %v, want it to wrap %v", err, config.ErrInvalid)
	}
	if _, err := policy.New(config.PolicyConfig{Mode: "define-new", CommandAllowlist: []string{"/usr/bin/true"}}); err != nil {
		t.Errorf("define-new with an allowlist was refused: %v", err)
	}
}

// Task 8.4: a deployment that says nothing gets the closed mode, rather than
// discovering header configuration is on.
func TestDefaultModeIsOff(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, config.PolicyConfig{})
	if p.Mode() != policy.ModeOff {
		t.Errorf("mode = %q, want %q", p.Mode(), policy.ModeOff)
	}
	if err := check(t, p, nil, header(t, overrideEnvOfKnown)); !errors.Is(err, policy.ErrHeaderDisabled) {
		t.Errorf("err = %v, want the header refused", err)
	}
}

// A header the deployment refuses is answered as refused, not as a merge that
// happened to fail: a subject told "your servers are malformed" would go on
// trying to fix a header that would never be applied.
func TestModeIsDecidedBeforeTheMerge(t *testing.T) {
	t.Parallel()
	// This header would fail resolution too: a declared server with no command.
	const wouldNotResolve = `{"servers":[{"id":"scratch","env":{"A":"b"}}]}`
	p := mustPolicy(t, config.PolicyConfig{Mode: "off"})
	err := check(t, p, nil, header(t, wouldNotResolve))
	if !errors.Is(err, policy.ErrHeaderDisabled) {
		t.Errorf("err = %v, want the closed deployment reported first", err)
	}
}

func TestUnknownModeIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := policy.New(config.PolicyConfig{Mode: "permissive"}); !errors.Is(err, config.ErrInvalid) {
		t.Errorf("err = %v, want it to wrap %v", err, config.ErrInvalid)
	}
}

// Task 8.7: the denylist judges what the header supplied, not what it was
// merged into. A deployment that sets a denied key for itself has made that
// choice; a request that never mentions the key has not asked for it.
func TestEnvDenylistJudgesOnlyWhatTheHeaderSupplied(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, config.PolicyConfig{
		Mode:             "define-new",
		CommandAllowlist: []string{"/usr/bin/mcp-files"},
		EnvDenylist:      []string{"FS_TOKEN"},
	})
	if err := check(t, p, strings.Fields(everyScope), header(t, overrideArgs)); err != nil {
		t.Errorf("a header touching only args was refused over a base env key: %v", err)
	}
	if err := check(t, p, strings.Fields(everyScope), header(t, overrideEnvOfKnown)); !errors.Is(err, policy.ErrEnvKeyDenied) {
		t.Errorf("err = %v, want it to wrap %v", err, policy.ErrEnvKeyDenied)
	}
}

// Task 8.5: a header describing nothing is still a header, and a deployment
// with header configuration off refuses it rather than quietly serving it.
func TestModeOffRefusesAnEmptyHeader(t *testing.T) {
	t.Parallel()
	p := mustPolicy(t, config.PolicyConfig{})
	if err := check(t, p, nil, header(t, `{"servers":[]}`)); !errors.Is(err, policy.ErrHeaderDisabled) {
		t.Errorf("err = %v, want it to wrap %v", err, policy.ErrHeaderDisabled)
	}
}

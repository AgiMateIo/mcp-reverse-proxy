// Package policy decides what a subject may express through the x-mcp-config
// header.
//
// Authentication answers who is asking; this answers whether they may start a
// process of their choosing on the host. Keeping the two apart is the point:
// were they one layer, every authenticated subject would hold that privilege.
package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/agimate/mcp-reverse-proxy/internal/auth"
	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

// A Mode bounds what the header may do. The modes are ordered: each permits
// everything the one before it does.
type Mode string

const (
	// ModeOff refuses the header outright.
	ModeOff Mode = "off"
	// ModeEnvOnly permits overriding the environment of a server the
	// deployment already describes — handing a known program a different
	// credential.
	ModeEnvOnly Mode = "env-only"
	// ModeOverrideKnown additionally permits changing what a known server runs.
	ModeOverrideKnown Mode = "override-known"
	// ModeDefineNew additionally permits declaring servers the deployment
	// never described, which is running a command of the subject's choosing on
	// the host.
	ModeDefineNew Mode = "define-new"
)

// modes is the order of privilege, least first.
var modes = []Mode{ModeOff, ModeEnvOnly, ModeOverrideKnown, ModeDefineNew}

// Scopes a token needs for each mode beyond the closed one. A deployment's mode
// is the ceiling; these decide how much of it a particular subject reaches.
const (
	ScopeEnv      = "mcp:config:env"
	ScopeOverride = "mcp:config:override"
	ScopeDefine   = "mcp:config:define"
)

var scopeFor = map[Mode]string{
	ModeEnvOnly:       ScopeEnv,
	ModeOverrideKnown: ScopeOverride,
	ModeDefineNew:     ScopeDefine,
}

var (
	// ErrPolicyViolation is the category: a request asking for something the
	// header policy does not allow.
	ErrPolicyViolation = errors.New("header policy violation")
	// ErrHeaderDisabled reports a header sent to a deployment that does not
	// accept one. It is deliberately not a scope failure: no token can change
	// a decision the deployment made.
	ErrHeaderDisabled = errors.New("header configuration is disabled")
	// ErrInsufficientPrivileges reports a header asking for more than the
	// deployment's mode permits anybody. Widening the token would not help.
	ErrInsufficientPrivileges = errors.New("insufficient privileges for this header")
	// ErrMissingScope reports a header the deployment would allow from a
	// subject whose token does not carry the scope for it. This one a client
	// can act on, by asking its authorization server for more.
	ErrMissingScope = errors.New("token lacks the scope for this header")
	// ErrCommandNotAllowed reports a command outside the allowlist.
	ErrCommandNotAllowed = errors.New("command is not on the allowlist")
	// ErrEnvKeyDenied reports an environment key the deployment refuses.
	ErrEnvKeyDenied = errors.New("environment key is not permitted")
	// ErrNoAllowlist reports define-new configured without an allowlist, which
	// would be an open invitation to run anything.
	ErrNoAllowlist = errors.New("define-new requires a command allowlist")
)

// A ScopeError names every scope the token is missing.
//
// All of them at once: challenging for one, then the next, would send a client
// through a separate authorization round trip per scope and ask whoever
// approves it repeatedly instead of once.
type ScopeError struct {
	Missing []string
}

func (e *ScopeError) Error() string {
	return fmt.Sprintf("%s: %s", ErrMissingScope, strings.Join(e.Missing, " "))
}

func (e *ScopeError) Is(target error) bool {
	return target == ErrMissingScope || target == ErrPolicyViolation
}

// A Policy is a deployment's stance on header configuration.
type Policy struct {
	mode      Mode
	allowlist []string
	denylist  []string
}

// New builds a policy from the deployment's configuration.
func New(c config.PolicyConfig) (*Policy, error) {
	mode := ModeOff
	if c.Mode != "" {
		mode = Mode(c.Mode)
		if !slices.Contains(modes, mode) {
			return nil, fmt.Errorf("policy mode %q is not one of %v: %w", mode, modes, config.ErrInvalid)
		}
	}
	if mode == ModeDefineNew && len(c.CommandAllowlist) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrNoAllowlist, config.ErrInvalid)
	}
	return &Policy{
		mode:      mode,
		allowlist: slices.Clone(c.CommandAllowlist),
		denylist:  slices.Clone(c.EnvDenylist),
	}, nil
}

// Mode reports the deployment's ceiling.
func (p *Policy) Mode() Mode { return p.mode }

// CheckHeader decides whether the subject may have this header applied at all.
//
// It runs before the header is merged, deliberately. A header the deployment
// refuses outright must be answered as refused, not as whatever the merge would
// have made of it: a subject sent one under a closed deployment should be told
// that header configuration is off, rather than that the servers it described
// were malformed.
func (p *Policy) CheckHeader(claims auth.Claims, base *config.File, header *config.Header) error {
	if header == nil {
		// No header is not a policy question, whatever the mode. A deployment
		// with header configuration off still serves ordinary requests. A
		// header that happens to describe nothing is still a header, though,
		// and a deployment that refuses them says so.
		return nil
	}
	known := make(map[string]bool, len(base.Servers))
	for _, s := range base.Servers {
		known[s.ID] = true
	}
	return p.decide(claims, known, header)
}

// CheckResolved judges the server set the header produced.
//
// The allowlist and denylist are checked here rather than against the patch,
// and hold regardless of mode: a header touching only the environment of a
// server whose command is not permitted must still be refused.
func (p *Policy) CheckResolved(resolved []config.Server, header *config.Header) error {
	return p.checkResolved(resolved, header)
}

func (p *Policy) decide(claims auth.Claims, known map[string]bool, header *config.Header) error {
	if p.mode == ModeOff {
		return fmt.Errorf("%w: %w", ErrHeaderDisabled, ErrPolicyViolation)
	}

	wanted := demands(known, header)
	highest := ModeEnvOnly
	for _, m := range wanted {
		if rank(m) > rank(highest) {
			highest = m
		}
	}
	if rank(highest) > rank(p.mode) {
		return fmt.Errorf("%w: it asks for %q while this deployment permits %q: %w",
			ErrInsufficientPrivileges, highest, p.mode, ErrPolicyViolation)
	}

	var missing []string
	for _, m := range wanted {
		if scope := scopeFor[m]; scope != "" && !claims.HasScope(scope) {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return &ScopeError{Missing: slices.Compact(missing)}
	}
	return nil
}

// demands reports every privilege the header exercises. A header may exercise
// several at once — declaring one server and re-crediting another — and each is
// named so the client learns all of them from one answer.
func demands(known map[string]bool, header *config.Header) []Mode {
	var wanted []Mode
	for _, patch := range header.Servers {
		if !known[patch.ID] {
			// Naming a server the deployment never described is declaring one,
			// whichever field the patch happens to set.
			wanted = append(wanted, ModeDefineNew)
			continue
		}
		if patch.TouchesProcess() {
			wanted = append(wanted, ModeOverrideKnown)
		}
		if patch.TouchesEnv() {
			wanted = append(wanted, ModeEnvOnly)
		}
	}
	return wanted
}

func (p *Policy) checkResolved(resolved []config.Server, header *config.Header) error {
	for _, s := range resolved {
		if len(p.allowlist) > 0 && !slices.Contains(p.allowlist, s.Command) {
			return fmt.Errorf("server %q: %w: %q: %w", s.ID, ErrCommandNotAllowed, s.Command, ErrPolicyViolation)
		}
	}
	if header == nil {
		return nil
	}
	// The denylist bounds what a request may set, not what the deployment chose
	// for itself, so the keys judged are the ones the header supplied rather
	// than the merged result: a header touching only the arguments of a server
	// whose base environment holds a denied key has asked for nothing.
	for _, patch := range header.Servers {
		for _, k := range patch.Env.Keys() {
			if slices.Contains(p.denylist, k) {
				return fmt.Errorf("server %q: %w: %q: %w", patch.ID, ErrEnvKeyDenied, k, ErrPolicyViolation)
			}
		}
	}
	return nil
}

func rank(m Mode) int { return slices.Index(modes, m) }

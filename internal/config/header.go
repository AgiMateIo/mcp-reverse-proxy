package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// HeaderName carries per-request configuration.
const HeaderName = "x-mcp-config"

var (
	// ErrHeaderTooLarge reports a header value past the configured limit.
	// Headers are commonly capped at 8 to 16 KiB by intermediaries, so a
	// larger one would be truncated somewhere further out anyway.
	ErrHeaderTooLarge = errors.New("x-mcp-config exceeds the size limit")
	// ErrHeaderMalformed reports a header value that is not the expected JSON.
	ErrHeaderMalformed = errors.New("x-mcp-config is not valid configuration")
)

// A ServerPatch is what the header says about one server.
//
// Every field is a pointer or a nil-able map so that "left alone" is
// distinguishable from "set to nothing": a header supplying `args: []` means
// the server takes no arguments, which is not the same as not mentioning args.
type ServerPatch struct {
	ID      string    `json:"id"`
	Command *string   `json:"command,omitempty"`
	Args    *[]string `json:"args,omitempty"`
	Env     Env       `json:"env,omitempty"`
	Era     *Era      `json:"era,omitempty"`
}

// TouchesEnv reports whether the patch changes the server's environment.
func (p ServerPatch) TouchesEnv() bool { return p.Env != nil }

// TouchesProcess reports whether the patch changes what would be executed.
// That is a different privilege from changing the environment: it is the
// difference between handing a known program a different token and running a
// different program.
func (p ServerPatch) TouchesProcess() bool {
	return p.Command != nil || p.Args != nil || p.Era != nil
}

// A Header is the parsed x-mcp-config document.
type Header struct {
	Servers []ServerPatch `json:"servers"`
}

// ParseHeader reads the header value, refusing one too large to be meant
// seriously before trying to parse it.
//
// No error it returns contains the value. The header is where a subject's
// credentials arrive, and an error message is the easiest place for them to
// escape into a log that nobody meant to be sensitive.
func ParseHeader(value Secret, maxBytes int) (*Header, error) {
	if maxBytes <= 0 {
		// A File built anywhere but Load has no limit set, and "no limit
		// configured" must not read as "no limit".
		maxBytes = DefaultLimits.MaxHeaderBytes
	}
	raw := value.Reveal()
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("%w of %d bytes (received %d)", ErrHeaderTooLarge, maxBytes, len(raw))
	}
	var h Header
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		// Deliberately not wrapping the decoder's error: it quotes the input
		// it choked on, which is the one thing that must not travel.
		return nil, fmt.Errorf("%w: the value is a JSON object with a \"servers\" array", ErrHeaderMalformed)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing content after the top-level object", ErrHeaderMalformed)
	}
	for i, p := range h.Servers {
		if p.ID == "" {
			return nil, fmt.Errorf("%w: servers[%d] has no id", ErrHeaderMalformed, i)
		}
	}
	return &h, nil
}

// Resolve layers the header over the base configuration and returns the server
// set for one request.
//
// The base is not modified: it is shared by every subject, and a request that
// edited it would change what everybody else sees.
func Resolve(base *File, h *Header) ([]Server, error) {
	servers := make([]Server, 0, len(base.Servers))
	index := make(map[string]int, len(base.Servers))
	for _, s := range base.Servers {
		index[s.ID] = len(servers)
		servers = append(servers, cloneServer(s))
	}
	if h != nil {
		for _, p := range h.Servers {
			if at, known := index[p.ID]; known {
				apply(&servers[at], p)
				continue
			}
			s := Server{ID: p.ID, Era: EraAuto}
			apply(&s, p)
			index[p.ID] = len(servers)
			servers = append(servers, s)
		}
	}
	if err := validateServers(servers); err != nil {
		return nil, fmt.Errorf("resolved configuration: %w", err)
	}
	return servers, nil
}

// apply layers one patch over one server. The environment merges key by key,
// because a subject overriding one credential should not have to restate the
// rest; everything else replaces wholesale, because half an argument list is
// not a meaningful thing to run.
func apply(s *Server, p ServerPatch) {
	if p.Command != nil {
		s.Command = *p.Command
	}
	if p.Args != nil {
		s.Args = slices.Clone(*p.Args)
	}
	if p.Era != nil {
		s.Era = *p.Era
	}
	if p.Env != nil {
		if s.Env == nil {
			s.Env = Env{}
		}
		maps.Copy(s.Env, p.Env)
	}
}

func cloneServer(s Server) Server {
	s.Args = slices.Clone(s.Args)
	s.Env = maps.Clone(s.Env)
	return s
}

// A resolvedKey is the context key under which one request's server set
// travels.
type resolvedKey struct{}

// WithResolved returns a context carrying the servers resolved for a request.
func WithResolved(ctx context.Context, servers []Server) context.Context {
	return context.WithValue(ctx, resolvedKey{}, servers)
}

// ResolvedFromContext returns the servers resolved for the request being
// served, and whether resolution ran at all.
func ResolvedFromContext(ctx context.Context) ([]Server, bool) {
	servers, ok := ctx.Value(resolvedKey{}).([]Server)
	return servers, ok
}

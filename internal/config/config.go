package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"
)

// ErrInvalid reports a configuration the gateway refuses to start with. Startup
// failures are told apart by this sentinel rather than by matching messages.
var ErrInvalid = errors.New("invalid configuration")

// An Era fixes how the gateway decides which protocol revision a backend
// speaks: by probing, or by taking the configuration's word for it.
type Era string

const (
	// EraAuto probes the backend with server/discover and falls back to the
	// legacy handshake. It is the default.
	EraAuto Era = "auto"
	// EraModern sends the probe like EraAuto but forbids the fallback, so a
	// backend that starts answering as legacy fails loudly instead of quietly
	// losing the modern result shape.
	EraModern Era = "modern"
	// EraLegacy skips the probe entirely, which is what keeps a backend that
	// ignores server/discover from costing a probe timeout on every cold
	// start. Cold starts are frequent: the pool is keyed by subject.
	EraLegacy Era = "legacy"
)

var eras = []Era{EraAuto, EraModern, EraLegacy}

// A Server describes one local stdio MCP server.
type Server struct {
	// ID namespaces the server's tools, prompts and resource URIs, and so must
	// be unique across the configuration.
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Env     Env      `json:"env,omitempty"`
	Era     Era      `json:"era,omitempty"`
}

// Limits bound what the gateway will spend on backends. Because the pool is
// keyed by subject, process count grows as subjects times servers, so these are
// operational necessities rather than tuning knobs.
type Limits struct {
	// MaxProcesses caps live child processes across all subjects.
	MaxProcesses int `json:"maxProcesses,omitempty"`
	// MaxProcessesPerSubject caps them for one subject, so that a single
	// subject cannot consume the global budget.
	MaxProcessesPerSubject int `json:"maxProcessesPerSubject,omitempty"`
	// IdleTTL is how long a process may serve no request before it is stopped.
	IdleTTL Duration `json:"idleTtl,omitempty"`
	// ProbeTimeout bounds the server/discover probe. Without it a legacy
	// backend that answers nothing hangs the connection until its context
	// expires.
	ProbeTimeout Duration `json:"probeTimeout,omitempty"`
	// MaxHeaderBytes caps the x-mcp-config header value.
	MaxHeaderBytes int `json:"maxHeaderBytes,omitempty"`
}

// DefaultLimits applies to any limit the configuration file omits.
//
// The specs fix only the default era; these values are deployment judgment, and
// a deployment is expected to set its own.
var DefaultLimits = Limits{
	MaxProcesses:           64,
	MaxProcessesPerSubject: 8,
	IdleTTL:                Duration(5 * time.Minute),
	// Long enough for a slow interpreter start, short enough that a cold
	// start against a silent backend is not felt as a hang.
	ProbeTimeout: Duration(2 * time.Second),
	// Reverse proxies commonly cap a header at 8 KiB; a larger limit here
	// would only be rejected further out.
	MaxHeaderBytes: 8 << 10,
}

// A File is a parsed and validated base configuration.
type File struct {
	// Servers may be empty: under the define-new policy mode every server
	// arrives in the x-mcp-config header.
	Servers []Server `json:"servers"`
	Limits  Limits   `json:"limits,omitempty"`
}

// Load reads and validates the base configuration at path.
//
// ctx is honored before the read so that a startup cancelled while its
// configuration sits on an unresponsive filesystem does not wait for it.
func Load(ctx context.Context, path string) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	// G304: the path is the deployment's own configuration file, named on the
	// command line by whoever starts the gateway. No request can influence it;
	// the request-supplied configuration never names a file at all.
	data, err := os.ReadFile(path) //nolint:gosec // see above
	if err != nil {
		// The os error already names the file.
		return nil, fmt.Errorf("config: %w", err)
	}
	return parse(path, data)
}

func parse(path string, data []byte) (*File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	// An unknown field is almost always a typo in a field the gateway then
	// silently ignores; naming it beats starting with the wrong behavior.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("config %s: %w: %w", path, ErrInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("config %s: %w: trailing content after the top-level object", path, ErrInvalid)
	}
	f.applyDefaults()
	if err := f.validate(path); err != nil {
		return nil, err
	}
	return &f, nil
}

// applyDefaults fills in what the file left out.
func (f *File) applyDefaults() {
	for i := range f.Servers {
		if f.Servers[i].Era == "" {
			f.Servers[i].Era = EraAuto
		}
	}
	if f.Limits.MaxProcesses == 0 {
		f.Limits.MaxProcesses = DefaultLimits.MaxProcesses
	}
	if f.Limits.MaxProcessesPerSubject == 0 {
		// A deployment that lowers only the global limit must not then be
		// rejected over a per-subject limit it never wrote.
		f.Limits.MaxProcessesPerSubject = min(DefaultLimits.MaxProcessesPerSubject, f.Limits.MaxProcesses)
	}
	if f.Limits.IdleTTL == 0 {
		f.Limits.IdleTTL = DefaultLimits.IdleTTL
	}
	if f.Limits.ProbeTimeout == 0 {
		f.Limits.ProbeTimeout = DefaultLimits.ProbeTimeout
	}
	if f.Limits.MaxHeaderBytes == 0 {
		f.Limits.MaxHeaderBytes = DefaultLimits.MaxHeaderBytes
	}
}

// validate reports the first problem, naming the file and where in it the
// problem sits.
func (f *File) validate(path string) error {
	seen := make(map[string]int, len(f.Servers))
	for i, s := range f.Servers {
		where := fmt.Sprintf("servers[%d]", i)
		switch {
		case s.ID == "":
			return f.problem(path, where, "missing id")
		case s.Command == "":
			return f.problem(path, fmt.Sprintf("servers[%d] %q", i, s.ID), "missing command")
		case !slices.Contains(eras, s.Era):
			return f.problem(path, fmt.Sprintf("servers[%d] %q", i, s.ID),
				fmt.Sprintf("era %q is not one of %v", s.Era, eras))
		}
		if first, dup := seen[s.ID]; dup {
			return f.problem(path, fmt.Sprintf("servers[%d]", i),
				fmt.Sprintf("id %q duplicates servers[%d]", s.ID, first))
		}
		seen[s.ID] = i
		for _, k := range s.Env.Keys() {
			if k == "" {
				return f.problem(path, fmt.Sprintf("servers[%d] %q", i, s.ID), "env has an empty key")
			}
		}
	}
	for _, l := range []struct {
		field string
		value int
	}{
		{"maxProcesses", f.Limits.MaxProcesses},
		{"maxProcessesPerSubject", f.Limits.MaxProcessesPerSubject},
		{"maxHeaderBytes", f.Limits.MaxHeaderBytes},
		{"idleTtl", int(f.Limits.IdleTTL)},
		{"probeTimeout", int(f.Limits.ProbeTimeout)},
	} {
		if l.value < 0 {
			return f.problem(path, "limits", fmt.Sprintf("%s must not be negative", l.field))
		}
	}
	if f.Limits.MaxProcessesPerSubject > f.Limits.MaxProcesses {
		return f.problem(path, "limits",
			fmt.Sprintf("maxProcessesPerSubject (%d) exceeds maxProcesses (%d)",
				f.Limits.MaxProcessesPerSubject, f.Limits.MaxProcesses))
	}
	return nil
}

func (f *File) problem(path, where, problem string) error {
	return fmt.Errorf("config %s: %s: %s: %w", path, where, problem, ErrInvalid)
}

// A Duration is a time span written as a Go duration string, since JSON has no
// duration of its own.
type Duration time.Duration

// Duration returns the span as a [time.Duration].
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalJSON accepts a duration string such as "5m".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"5m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration: %w", err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON writes the duration string form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

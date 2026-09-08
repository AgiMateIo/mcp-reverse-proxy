package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
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
	// Policy bounds what the x-mcp-config header may do. Its zero value is
	// the closed one: a deployment opts into header configuration rather than
	// discovering it is on.
	Policy PolicyConfig `json:"policy,omitempty"`
}

// PolicyConfig is the deployment's stance on header configuration, carried as
// plain strings because the package that interprets them depends on this one.
type PolicyConfig struct {
	Mode             string   `json:"mode,omitempty"`
	CommandAllowlist []string `json:"commandAllowlist,omitempty"`
	EnvDenylist      []string `json:"envDenylist,omitempty"`
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
	document, err := toJSON(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w: %w", path, ErrInvalid, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(document))
	// An unknown field is almost always a typo in a field the gateway then
	// silently ignores; naming it beats starting with the wrong behavior.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("config %s: %w: %w", path, ErrInvalid, err)
	}
	f.applyDefaults()
	if err := f.validate(path); err != nil {
		return nil, err
	}
	return &f, nil
}

// toJSON reads the file as YAML and re-encodes it as JSON.
//
// The schema is described once, by the json tags on the types above: they are
// what the x-mcp-config header is parsed against, what [Duration] and [Secret]
// hook into, and what the deployment document is checked against. Decoding
// YAML directly would need a second set of tags to keep in step with the first,
// so the file is converted instead and the strict JSON decoder stays the one
// place a configuration is interpreted.
//
// JSON files keep loading, YAML being a superset of JSON, with one difference:
// a duplicate key is refused rather than resolved to the last one.
func toJSON(data []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// KnownFields is deliberately not set here: unknown fields are the strict
	// JSON decoder's to report, which names them the same way whichever syntax
	// the file was written in.
	var document any
	if err := dec.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, err
	}
	// A YAML stream may hold several documents. Only the first would ever be
	// read, so a second one is a configuration the operator believes is in
	// effect and is not.
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing content after the top-level object")
	}
	// An explicitly null document — an empty file with a `---` in it, or one
	// truncated to nothing — is not an empty configuration; it is a file that
	// says nothing, which is never what an operator meant to deploy.
	if document == nil {
		return nil, errors.New("the file is empty")
	}
	if err := refuseTimestamps(document, ""); err != nil {
		return nil, err
	}
	// Fails on a mapping key YAML allows and JSON does not, such as a number.
	// The error names the offending type rather than the value, which keeps an
	// env value out of it.
	return json.Marshal(document)
}

// refuseTimestamps rejects an unquoted scalar YAML read as a date or a time.
//
// Every other mistyped scalar is already loud: a number or a boolean where a
// string belongs fails in the decoder, naming the field. A timestamp does not,
// because it re-encodes as a string and lands in the field as though it had
// been written that way — `TOKEN: 2026-09-08` reaching the backend as
// "2026-09-08T00:00:00Z". A configuration value that arrives altered is worse
// than one that is refused, and env values are precisely where an operator
// cannot check the result: they are redacted everywhere the gateway could show
// them back.
//
// The error names the path and never the value, so a secret written without
// quotes stays out of the message.
func refuseTimestamps(node any, path string) error {
	switch n := node.(type) {
	case time.Time:
		return fmt.Errorf("%s: an unquoted value read as a date or a time; quote it", at(path))
	case map[string]any:
		// Sorted so that a file with two such values fails the same way twice
		// rather than naming whichever one the map happened to yield first.
		for _, key := range slices.Sorted(maps.Keys(n)) {
			if err := refuseTimestamps(n[key], path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range n {
			if err := refuseTimestamps(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// at names a position in the document for an error message.
func at(path string) string {
	if path == "" {
		return "the document"
	}
	return strings.TrimPrefix(path, ".")
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
	if err := validateServers(f.Servers); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
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

// validateServers checks a server set from either source. The header can
// produce a duplicate id or an empty command exactly as a file can, so both go
// through this.
func validateServers(servers []Server) error {
	seen := make(map[string]int, len(servers))
	for i, s := range servers {
		named := fmt.Sprintf("servers[%d] %q", i, s.ID)
		switch {
		case s.ID == "":
			return fmt.Errorf("servers[%d]: missing id: %w", i, ErrInvalid)
		case s.Command == "":
			return fmt.Errorf("%s: missing command: %w", named, ErrInvalid)
		case !slices.Contains(eras, s.Era):
			return fmt.Errorf("%s: era %q is not one of %v: %w", named, s.Era, eras, ErrInvalid)
		}
		if first, dup := seen[s.ID]; dup {
			return fmt.Errorf("servers[%d]: id %q duplicates servers[%d]: %w", i, s.ID, first, ErrInvalid)
		}
		seen[s.ID] = i
		for _, k := range s.Env.Keys() {
			if k == "" {
				return fmt.Errorf("%s: env has an empty key: %w", named, ErrInvalid)
			}
		}
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

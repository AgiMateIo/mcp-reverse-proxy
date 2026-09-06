package config

import (
	"fmt"
	"log/slog"
	"slices"
)

// redacted stands in for any value that must not reach a log, an error, or
// telemetry.
const redacted = "[REDACTED]"

// A Secret is a configuration value that must never be observable.
//
// Redaction is a property of the type, not of the caller's discipline: the
// value is a placeholder under slog, under fmt, and under encoding/json. The
// real value is reachable only through [Secret.Reveal], so the few places that
// legitimately need it — spawning a backend, fingerprinting a configuration —
// are greppable.
type Secret string

var (
	_ fmt.GoStringer = Secret("")
	_ slog.LogValuer = Secret("")
	_ slog.LogValuer = Env(nil)
)

// LogValue implements [slog.LogValuer].
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// String implements [fmt.Stringer], covering the formatting verbs that do not
// consult [slog.LogValuer].
func (Secret) String() string { return redacted }

// GoString implements [fmt.GoStringer]. Without it %#v would print the value as
// Go syntax, which is the one verb that ignores [fmt.Stringer].
func (Secret) GoString() string { return `"` + redacted + `"` }

// MarshalJSON redacts the value. Configuration is parsed, never re-emitted, so
// a lossy round trip is the safer default.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Reveal returns the underlying value. Every call site is a place where a
// secret leaves the type's protection.
func (s Secret) Reveal() string { return string(s) }

// An Env is the environment of a backend process. Keys are not secret — the
// specs require rejected keys to be named in errors — but values always are.
type Env map[string]Secret

// LogValue implements [slog.LogValuer], reporting which variables are set
// without reporting what they hold.
func (e Env) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, len(e))
	for _, k := range e.Keys() {
		attrs = append(attrs, slog.String(k, redacted))
	}
	return slog.GroupValue(attrs...)
}

// Keys returns the variable names in sorted order, which is what makes a
// fingerprint of an Env reproducible.
func (e Env) Keys() []string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

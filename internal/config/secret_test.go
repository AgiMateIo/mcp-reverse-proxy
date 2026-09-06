package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/agimate/mcp-reverse-proxy/internal/config"
)

const token = "super-secret-token"

// Redaction holds for every way a value ordinarily escapes: a log record, a
// format verb, a JSON encoding.
func TestSecretIsRedactedEverywhere(t *testing.T) {
	t.Parallel()
	secret := config.Secret(token)
	tests := []struct {
		name string
		out  func() string
	}{
		{"slog attribute", func() string { return logLine(t, slog.Any("token", secret)) }},
		{"slog inside a group", func() string {
			return logLine(t, slog.Any("server", slog.GroupValue(slog.Any("token", secret))))
		}},
		{"fmt %v", func() string { return fmt.Sprintf("%v", secret) }},
		{"fmt %s", func() string { return fmt.Sprintf("token=%s", secret) }},
		{"fmt %q", func() string { return fmt.Sprintf("%q", secret) }},
		{"fmt %#v", func() string { return fmt.Sprintf("%#v", secret) }},
		{"fmt %#v of a server", func() string {
			return fmt.Sprintf("%#v", config.Server{ID: "s", Env: config.Env{"TOKEN": secret}})
		}},
		{"error message", func() string { return fmt.Errorf("spawn failed: %v", secret).Error() }},
		{"json.Marshal", func() string { return mustMarshal(t, secret) }},
		{"json.Marshal of the env map", func() string {
			return mustMarshal(t, config.Env{"TOKEN": secret})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.out()
			if strings.Contains(got, token) {
				t.Errorf("secret leaked: %s", got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Errorf("no placeholder in %q", got)
			}
		})
	}
}

// Reveal is the one way through, and it is the real value.
func TestSecretReveal(t *testing.T) {
	t.Parallel()
	if got := config.Secret(token).Reveal(); got != token {
		t.Errorf("Reveal = %q, want %q", got, token)
	}
}

// An Env logs which variables are set without logging what they hold: the specs
// require rejected keys to be named, so keys are not secret.
func TestEnvLogValueKeepsKeys(t *testing.T) {
	t.Parallel()
	got := logLine(t, slog.Any("env", config.Env{"TOKEN": config.Secret(token), "MODE": "ro"}))
	for _, key := range []string{"TOKEN", "MODE"} {
		if !strings.Contains(got, key) {
			t.Errorf("key %q missing from %q", key, got)
		}
	}
	for _, value := range []string{token, "ro"} {
		if strings.Contains(got, value) {
			t.Errorf("value %q leaked into %q", value, got)
		}
	}
}

// Keys are sorted, which is what makes an Env fingerprint reproducible.
func TestEnvKeysSorted(t *testing.T) {
	t.Parallel()
	got := config.Env{"c": "3", "a": "1", "b": "2"}.Keys()
	want := []string{"a", "b", "c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
}

func logLine(t *testing.T, attr slog.Attr) string {
	t.Helper()
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).LogAttrs(t.Context(), slog.LevelInfo, "record", attr)
	return buf.String()
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

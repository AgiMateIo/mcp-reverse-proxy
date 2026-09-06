package backend

import (
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// Task 4.12: each transformation that brings a legacy response into the shape
// of revision 2026-07-28.

func TestNormalizeResultType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"legacy result carries none", "", ResultTypeComplete},
		{"modern result keeps its own", ResultTypeComplete, ResultTypeComplete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeResultType(tt.in); got != tt.want {
				t.Errorf("normalizeResultType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Cache
		want Cache
	}{
		{
			name: "legacy listing carries neither field",
			in:   Cache{},
			want: Cache{TTLMs: DefaultTTLMs, CacheScope: CacheScopePrivate},
		},
		{
			name: "a backend's own lifetime is kept",
			in:   Cache{TTLMs: 5_000, CacheScope: CacheScopePrivate},
			want: Cache{TTLMs: 5_000, CacheScope: CacheScopePrivate},
		},
		{
			// The gateway's surface depends on the subject, so no backend may
			// widen the scope: "public" would let an intermediary serve one
			// subject's tools to another.
			name: "a backend may not widen the scope",
			in:   Cache{TTLMs: 5_000, CacheScope: "public"},
			want: Cache{TTLMs: 5_000, CacheScope: CacheScopePrivate},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeCache(tt.in); got != tt.want {
				t.Errorf("normalizeCache(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeErrorCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"retired resource-not-found code", codeResourceNotFound, jsonrpc.CodeInvalidParams},
		{"any other code is untouched", jsonrpc.CodeMethodNotFound, jsonrpc.CodeMethodNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeErrorCode(tt.in); got != tt.want {
				t.Errorf("normalizeErrorCode(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// A wrapped JSON-RPC error is remapped and stays inspectable.
func TestNormalizeError(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("read resource: %w",
		&jsonrpc.Error{Code: codeResourceNotFound, Message: "no such resource"})

	err := normalizeError("files", wrapped)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) {
		t.Fatalf("errors.As(..., *ProtocolError) = false for %v", err)
	}
	if protocol.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want %d", protocol.Code, jsonrpc.CodeInvalidParams)
	}
	if protocol.ServerID != "files" {
		t.Errorf("server = %q, want %q", protocol.ServerID, "files")
	}
	if protocol.Message != "no such resource" {
		t.Errorf("message = %q, want it preserved", protocol.Message)
	}
}

// An error about the connection is not an error about the protocol, and must
// pass through untouched.
func TestNormalizeErrorPassesThroughNonProtocolErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("broken pipe")
	if got := normalizeError("files", sentinel); !errors.Is(got, sentinel) {
		t.Errorf("normalizeError rewrote a non-protocol error: %v", got)
	}
}

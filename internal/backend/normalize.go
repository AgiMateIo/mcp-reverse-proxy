package backend

import (
	"errors"
	"fmt"
	"io"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Revision 2026-07-28 shapes every result the same way, whichever era produced
// it. A legacy backend predates all of it, so the gateway supplies what is
// missing rather than letting the omission reach the client.
const (
	// ResultTypeComplete is the only result type the gateway emits: the
	// input_required flow belongs to MRTR, which is not in this gateway.
	ResultTypeComplete = "complete"
	// CacheScopePrivate is the only cache scope the gateway emits. The tool
	// surface depends on the subject, so "public" would let an intermediary
	// serve one subject's tools to another.
	CacheScopePrivate = "private"
	// DefaultTTLMs is the cache lifetime attached to a listing whose backend
	// offered none.
	DefaultTTLMs = 60_000
)

// codeResourceNotFound is the "resource not found" code of revision
// 2025-11-25. Revision 2026-07-28 folds it into the standard invalid-params
// code.
const codeResourceNotFound int64 = -32002

// A Cache carries the client-side caching hints revision 2026-07-28 requires on
// listing results and on resources/read.
type Cache struct {
	TTLMs      int
	CacheScope string
}

// normalizeCache supplies hints a legacy backend did not send, and forces the
// scope even when it did: no backend may widen the gateway's own scope.
func normalizeCache(c Cache) Cache {
	if c.TTLMs <= 0 {
		c.TTLMs = DefaultTTLMs
	}
	c.CacheScope = CacheScopePrivate
	return c
}

// normalizeResultType supplies the result type a legacy backend omits.
func normalizeResultType(resultType string) string {
	if resultType == "" {
		return ResultTypeComplete
	}
	return resultType
}

// normalizeErrorCode maps codes that revision 2026-07-28 retired.
func normalizeErrorCode(code int64) int64 {
	if code == codeResourceNotFound {
		return jsonrpc.CodeInvalidParams
	}
	return code
}

// A ProtocolError is a JSON-RPC error from a backend, with its code normalized
// to revision 2026-07-28.
type ProtocolError struct {
	ServerID string
	Code     int64
	Message  string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("backend %q returned error %d: %s", e.ServerID, e.Code, e.Message)
}

// ErrConnectionLost reports a call that failed because the session with the
// backend is gone, rather than because the backend refused the request. It lets
// a caller distinguish the two without naming an SDK error.
var ErrConnectionLost = errors.New("backend connection lost")

// connectionLost reports whether err means the session is gone.
func connectionLost(err error) bool {
	return errors.Is(err, mcp.ErrConnectionClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// normalizeError rewrites a JSON-RPC error from a backend. Errors that are not
// JSON-RPC errors — a broken pipe, a cancelled context — pass through, because
// they are about the connection and not about the protocol.
func normalizeError(serverID string, err error) error {
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		if connectionLost(err) {
			return fmt.Errorf("%w: %w", ErrConnectionLost, err)
		}
		return err
	}
	return fmt.Errorf("%w", &ProtocolError{
		ServerID: serverID,
		Code:     normalizeErrorCode(wire.Code),
		Message:  wire.Message,
	})
}

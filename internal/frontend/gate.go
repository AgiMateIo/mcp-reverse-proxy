package frontend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// JSON-RPC error codes revision 2026-07-28 defines beyond the standard set.
const (
	// codeHeaderMismatch reports a request whose headers contradict its body.
	codeHeaderMismatch = -32020
	// codeMethodNotFound is the standard code for a method the server does not
	// implement — including the ones this revision removed.
	codeMethodNotFound = -32601
	// codeUnsupportedVersion reports a revision this endpoint does not speak.
	codeUnsupportedVersion = -32022
)

// protocolVersionHeader carries the revision a request follows, alongside the
// copy in the body's _meta.
const protocolVersionHeader = "Mcp-Protocol-Version"

// gate answers the requests the SDK handler cannot answer correctly on its own,
// and passes everything else through.
//
// The SDK already rejects a header that contradicts the body, an unsupported
// version in _meta, and a removed method on a request that took the per-request
// protocol path. Three cases it does not cover are the gateway's to answer:
//
//   - `initialize`. The SDK refuses it, but its message does not name the
//     revisions on offer, and a client that only knows the handshake has
//     nothing else to go on.
//   - A removed method on a request with no `_meta` version. The SDK treats
//     that as a session that never initialized rather than as a method that no
//     longer exists.
//   - A POST with no version header at all, which skips the SDK's header
//     validation entirely.
//   - A request whose version header and body name different revisions. The
//     SDK compares the method and the name headers against the body but not
//     the version, and answering that with a version error would refuse a
//     revision the client never asked for.
//   - A version this gateway does not speak. The SDK accepts every revision it
//     knows, including the legacy ones, and refuses anything else as plain
//     text before the negotiation error is ever reached. The gateway speaks
//     one revision, so this is its own to answer — and answering it wrongly
//     would mean serving a legacy request through a front end that has no
//     session to serve it with.
func gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Bad Request: cannot read the request body", http.StatusBadRequest)
			return
		}
		// The handler behind us reads the body too.
		r.Body = io.NopCloser(bytes.NewReader(body))

		call, ok := peek(body)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// The order of these cases is load-bearing. A client that only knows
		// the handshake announces a legacy revision, so answering the version
		// first would refuse it with a bare version error instead of the
		// message that tells it what to do.
		inHeader, inBody := r.Header.Get(protocolVersionHeader), declared(call)
		switch {
		case call.Method == "initialize":
			// Naming the revisions is the whole value of this answer: it tells
			// a legacy client what to do instead of handshaking.
			writeError(w, http.StatusNotFound, call.ID, codeMethodNotFound, fmt.Sprintf(
				"the initialize handshake was removed in revision %s; this endpoint speaks %s, "+
					"carried per request in the %q field of _meta",
				SupportedVersions[0], strings.Join(SupportedVersions, ", "), metaKeyProtocolVersion))
		case slices.Contains(RemovedMethods, call.Method):
			writeError(w, http.StatusNotFound, call.ID, codeMethodNotFound, fmt.Sprintf(
				"%q was removed in revision %s", call.Method, SupportedVersions[0]))
		case inHeader == "":
			// Without it the SDK skips its own header checks, so a request
			// missing Mcp-Method would be served rather than refused.
			writeError(w, http.StatusBadRequest, call.ID, codeHeaderMismatch, fmt.Sprintf(
				"the %s header is required on every request", protocolVersionHeader))
		case inBody != "" && inBody != inHeader:
			// A request that names two revisions has contradicted itself, and
			// that is a different failure from naming one revision this
			// endpoint does not speak: neither of the two is what the client
			// asked for, so refusing one of them would be an answer to a
			// request nobody made.
			writeError(w, http.StatusBadRequest, call.ID, codeHeaderMismatch, fmt.Sprintf(
				"the %s header says %q and the request body says %q",
				protocolVersionHeader, inHeader, inBody))
		case !slices.Contains(SupportedVersions, requested(call, r)):
			writeVersionError(w, call.ID, requested(call, r))
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// metaKeyProtocolVersion is where a request carries its revision.
const metaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"

// A call is as much of a JSON-RPC request as the gate needs to see.
type call struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	} `json:"params"`
}

// requested is the revision a request claims to follow. The body is
// authoritative — that is where this revision carries it — and the header is
// the fallback for a request that has no _meta at all.
func requested(c call, r *http.Request) string {
	if version := declared(c); version != "" {
		return version
	}
	return r.Header.Get(protocolVersionHeader)
}

// declared is the revision the request body names, or the empty string if it
// names none.
func declared(c call) string {
	raw, ok := c.Params.Meta[metaKeyProtocolVersion]
	if !ok {
		return ""
	}
	var version string
	if err := json.Unmarshal(raw, &version); err != nil {
		return ""
	}
	return version
}

// writeVersionError refuses a revision, naming what is on offer so that the
// client can pick one rather than guess.
func writeVersionError(w http.ResponseWriter, id json.RawMessage, version string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    codeUnsupportedVersion,
			"message": fmt.Sprintf("unsupported protocol version %q", version),
			"data": map[string]any{
				"supported": SupportedVersions,
				"requested": version,
			},
		},
	})
}

// peek reads the method and id of a single request, and reports whether the
// body was one. Anything else — a batch, a notification, malformed JSON — is
// the SDK handler's to answer.
func peek(body []byte) (call, bool) {
	var c call
	if err := json.Unmarshal(body, &c); err != nil || c.Method == "" || len(c.ID) == 0 {
		return call{}, false
	}
	return c, true
}

// writeError answers with a JSON-RPC error.
//
// The status carries the same distinction as the code: a method this revision
// no longer has is 404, matching what a client is told about any method the
// server does not implement, while a malformed request is 400.
func writeError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

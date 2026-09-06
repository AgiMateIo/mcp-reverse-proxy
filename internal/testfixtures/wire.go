package testfixtures

import (
	"encoding/json"
	"fmt"
	"io"
)

// MCP methods the fixtures recognize.
const (
	methodInitialize            = "initialize"
	notificationInitialized     = "notifications/initialized"
	methodDiscover              = "server/discover"
	methodToolsList             = "tools/list"
	methodToolsCall             = "tools/call"
	methodResourcesRead         = "resources/read"
	methodSamplingCreateMessage = "sampling/createMessage"
)

// ttlMs is the cache hint modern-era fixture results carry.
const ttlMs = 60_000

// A message is one JSON-RPC frame. Requests, responses and notifications share
// the shape so the read loop can decode any of them without looking first.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

// A wireError is the JSON-RPC error object.
type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// A writer serializes frames onto a fixture's stdout.
//
// It is not safe for concurrent use, and does not need to be: a fixture writes
// only from the goroutine running its read loop, including the unsolicited
// request ModeMisbehaving sends.
type writer struct {
	w io.Writer
}

// send writes one frame, newline-delimited as the stdio transport requires.
func (w *writer) send(msg message) error {
	msg.JSONRPC = "2.0"
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("testfixtures: encode frame: %w", err)
	}
	if _, err := w.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("testfixtures: write frame: %w", err)
	}
	return nil
}

// result answers a request with a result value.
func (w *writer) result(id json.RawMessage, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("testfixtures: encode result: %w", err)
	}
	return w.send(message{ID: id, Result: raw})
}

// fail answers a request with an error.
func (w *writer) fail(id json.RawMessage, code int, msg string) error {
	return w.send(message{ID: id, Error: &wireError{Code: code, Message: msg}})
}

// request issues a server-to-client request.
func (w *writer) request(id json.RawMessage, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("testfixtures: encode params of %s: %w", method, err)
	}
	return w.send(message{ID: id, Method: method, Params: raw})
}

// An implementation names a peer, as `serverInfo` and `clientInfo` do.
type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// capabilities is the server capability set. Only tools are advertised: the
// fixtures exist to exercise era handling, not the whole feature surface.
type capabilities struct {
	Tools *listChangedCapability `json:"tools,omitempty"`
}

type listChangedCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

func serverCapabilities() capabilities {
	return capabilities{Tools: &listChangedCapability{ListChanged: true}}
}

// An initializeResult is the legacy handshake response.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    capabilities   `json:"capabilities"`
	ServerInfo      implementation `json:"serverInfo"`
}

// A discoverResult is the modern-era answer to `server/discover`. It carries in
// `_meta` what the legacy handshake carried in its result body.
type discoverResult struct {
	ResultType        string         `json:"resultType"`
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      capabilities   `json:"capabilities"`
	TTLMs             int            `json:"ttlMs"`
	CacheScope        string         `json:"cacheScope"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// A tool is one entry of a `tools/list` result.
type tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// legacyListToolsResult is the pre-2026-07-28 shape: no result type and no
// cache hints, so that normalization has something to normalize.
type legacyListToolsResult struct {
	Tools []tool `json:"tools"`
}

// modernListToolsResult carries the envelope revision 2026-07-28 requires.
type modernListToolsResult struct {
	ResultType string `json:"resultType"`
	Tools      []tool `json:"tools"`
	TTLMs      int    `json:"ttlMs"`
	CacheScope string `json:"cacheScope"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type callToolResult struct {
	ResultType string    `json:"resultType,omitempty"`
	Content    []content `json:"content"`
	IsError    bool      `json:"isError,omitempty"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type createMessageParams struct {
	Messages  []samplingMessage `json:"messages"`
	MaxTokens int               `json:"maxTokens"`
}

type samplingMessage struct {
	Role    string  `json:"role"`
	Content content `json:"content"`
}

// fixtureTools is the tool set every fixture serves. Two entries are enough to
// show namespacing and ordering without making assertions unreadable.
var fixtureTools = []tool{
	{
		Name:        "echo",
		Title:       "Echo",
		Description: "Returns its argument unchanged.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	},
	{
		Name:        "add",
		Title:       "Add",
		Description: "Adds two numbers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
	},
}

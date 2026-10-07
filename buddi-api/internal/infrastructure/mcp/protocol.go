// Package mcp is a minimal Model Context Protocol implementation covering the
// subset Buddi needs: listing a server's tools and calling one.
//
// The protocol is JSON-RPC 2.0 over a line-delimited stream. This package owns the
// wire format and nothing else — no session state, no prompts, no resources,
// because the orchestrator uses tools and only tools. Adding those later does not
// change the transport.
//
// Two transports are provided. An in-memory pipe is used in tests and for
// embedding a server in the same process; stdio is used for a server that runs as
// its own program, which is how a third-party connector is expected to arrive.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the revision this implementation speaks.
//
// It is checked rather than assumed. A server on a different revision still works,
// because the fields used here have been stable, but the check is what makes a
// mismatch visible in a log instead of as a mysteriously empty tool list.
const ProtocolVersion = "2024-11-05"

// JSON-RPC 2.0 reserved values.
const (
	// Version is the only protocol version JSON-RPC defines.
	Version = "2.0"

	// MethodInitialize is the handshake every session begins with.
	MethodInitialize = "initialize"
	// MethodInitialized tells the server the handshake is accepted.
	MethodInitialized = "notifications/initialized"
	// MethodToolsList asks a server what it can do.
	MethodToolsList = "tools/list"
	// MethodToolsCall invokes one tool.
	MethodToolsCall = "tools/call"

	// CodeParseError means the payload was not valid JSON.
	CodeParseError = -32700
	// CodeInvalidRequest means valid JSON that is not a valid request.
	CodeInvalidRequest = -32600
	// CodeMethodNotFound means the server does not implement the method.
	CodeMethodNotFound = -32601
	// CodeInvalidParams means the method's parameters were wrong.
	CodeInvalidParams = -32602
	// CodeInternalError means the server failed while handling a valid request.
	CodeInternalError = -32603
)

// Request is an outbound JSON-RPC call. The ID is a pointer so a notification,
// which carries none, is distinguishable from a call that happens to use zero.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// Response is an inbound JSON-RPC reply. Exactly one of Result and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error object. The message is written for a log or a user, not
// for a machine, so Code is what callers branch on.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "mcp: <nil error>"
	}

	return fmt.Sprintf("mcp: %s (code %d)", e.Message, e.Code)
}

// Tool is what a server offers. The schema is kept as raw JSON rather than
// compiled here: the client does not need to understand it, and inventing a
// partial typed form would be a second thing to keep in step with the spec.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolListResult is the result of tools/list.
type ToolListResult struct {
	Tools []Tool `json:"tools"`
	// NextCursor paginates. An empty cursor means the list is complete; it is not
	// the absence of a field, so the field is not a pointer.
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams is the parameters of tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult is the result of tools/call.
//
// Content is a list of typed parts rather than a bare result value, because that is
// what a server may return for a tool whose output is text, JSON or an error. An
// error is reported here with IsError set, not as an RPC error: the call reached
// the tool and the tool refused, which is a different thing from the call not
// arriving.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is one part of a tool result.
type Content struct {
	Type string `json:"type"`
	// Text is set when Type is "text".
	Text string `json:"text,omitempty"`
	// MimeType is set when Type is "image" or "audio".
	MimeType string `json:"mimeType,omitempty"`
	// Data is base64 for binary content types.
	Data string `json:"data,omitempty"`
}

// initializeParams is the handshake payload. Only the fields that matter for
// agreeing a version are sent; the rest of the spec's options are omitted rather
// than filled with defaults the client would then have to honour.
type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      implementation `json:"clientInfo"`
}

// implementation identifies one end of the connection.
type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeResult is what the server answers the handshake with.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ServerInfo      implementation `json:"serverInfo,omitempty"`
	Instructions    string         `json:"instructions,omitempty"`
}

// Text builds a text content part.
func Text(s string) Content {
	return Content{Type: "text", Text: s}
}

// JSONText builds a text content part holding encoded JSON, which is what Buddi's
// own tools return so the agent sees one shape regardless of the server.
func JSONText(v any) (Content, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return Content{}, fmt.Errorf("mcp: could not encode tool result: %w", err)
	}

	return Text(string(encoded)), nil
}

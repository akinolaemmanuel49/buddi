package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Server answers JSON-RPC calls for a set of tools.
//
// One server may be reached over several connections, so the handlers must not
// hold per-session state. Credentials live behind the token store rather than on
// the connection, which is what makes that possible and is also why a token is
// keyed by user rather than by session.
type Server struct {
	name    string
	version string
	tools   map[string]ToolHandler

	// instructions is optional prose shown to the client on connect. It is
	// returned by the handshake and never logged.
	instructions string
}

// ToolHandler is one tool's implementation.
//
// Implementations receive the arguments as raw JSON rather than a decoded type so
// that each tool decides how strict to be. A tool that must reject unknown fields
// decodes with DisallowUnknownFields; one that tolerates extra keys does not.
type ToolHandler interface {
	// Descriptor is the tool's name, description and input schema.
	Descriptor() Tool
	// Handle runs the tool. Returning a CallToolError marks the result as an
	// error rather than as a JSON-RPC failure: the call arrived and the tool
	// refused, which the protocol distinguishes from the call not arriving.
	Handle(ctx context.Context, arguments json.RawMessage) (any, error)
}

// ToolHandlerFunc adapts a function to ToolHandler.
type ToolHandlerFunc struct {
	Tool       Tool
	HandleFunc func(ctx context.Context, arguments json.RawMessage) (any, error)
}

// Descriptor implements ToolHandler.
func (f ToolHandlerFunc) Descriptor() Tool { return f.Tool }

// Handle implements ToolHandler.
func (f ToolHandlerFunc) Handle(ctx context.Context, arguments json.RawMessage) (any, error) {
	return f.HandleFunc(ctx, arguments)
}

// CallToolError marks a result as a tool-level failure.
//
// It is an error value rather than a struct callers construct by hand so the
// distinction between "refused" and "unreachable" is in the type, which is the
// only thing that keeps the two from being confused at a call site.
type CallToolError struct {
	Reason string
}

func (e *CallToolError) Error() string { return e.Reason }

// Refused marks arguments a tool will not act on.
func Refused(format string, args ...any) error {
	return &CallToolError{Reason: fmt.Sprintf(format, args...)}
}

// NewServer builds a server serving the given tools.
//
// A duplicate tool name is rejected rather than silently overwritten: two handlers
// answering to one name means the model picks between them unpredictably, and the
// user cannot tell which one produced an approved action.
func NewServer(name, version string, handlers ...ToolHandler) (*Server, error) {
	if name == "" {
		return nil, fmt.Errorf("mcp: server name is required")
	}

	server := &Server{
		name:    name,
		version: version,
		tools:   make(map[string]ToolHandler, len(handlers)),
	}

	for _, handler := range handlers {
		tool := handler.Descriptor()

		if tool.Name == "" {
			return nil, fmt.Errorf("mcp: a tool on %s has no name", name)
		}

		if _, exists := server.tools[tool.Name]; exists {
			return nil, fmt.Errorf("mcp: %s registers %q twice", name, tool.Name)
		}

		server.tools[tool.Name] = handler
	}

	return server, nil
}

// WithInstructions attaches prose returned on connect.
func (s *Server) WithInstructions(instructions string) *Server {
	s.instructions = instructions
	return s
}

// Serve handles messages from one connection until it closes.
//
// It returns when the transport reaches EOF, which is how a stdio server is told
// to exit: the parent closed the pipe.
func (s *Server) Serve(ctx context.Context, transport Transport) error {
	scanner := newLineScanner(transport)

	for {
		line, err := scanner.next()
		if err != nil {
			if errors.Is(err, errReadDone) {
				return nil
			}

			return err
		}

		response := s.dispatch(ctx, line)

		// A notification expects no reply. Replying to one makes a strict client
		// treat a well-behaved message as a protocol violation.
		if response == nil {
			continue
		}

		if err := s.write(transport, response); err != nil {
			return err
		}
	}
}

func (s *Server) write(transport Transport, response *Response) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("mcp: could not encode response: %w", err)
	}

	if _, err := transport.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("mcp: could not write response: %w", err)
	}

	return nil
}

// dispatch answers one message, returning nil for a notification.
func (s *Server) dispatch(ctx context.Context, line []byte) *Response {
	var request Request
	if err := json.Unmarshal(line, &request); err != nil {
		return errorResponse(nil, CodeParseError, "request is not valid JSON")
	}

	if request.JSONRPC != Version {
		return errorResponse(request.ID, CodeInvalidRequest, "jsonrpc version must be 2.0")
	}

	if request.ID == nil {
		// A notification. Handled for its effect (there is none yet) and
		// deliberately not answered.
		return nil
	}

	switch request.Method {
	case MethodInitialize:
		return s.initialize(request)
	case MethodInitialized:
		return nil
	case MethodToolsList:
		return s.listTools(request)
	case MethodToolsCall:
		return s.callTool(ctx, request)
	default:
		return errorResponse(request.ID, CodeMethodNotFound, "unknown method "+request.Method)
	}
}

func (s *Server) initialize(request Request) *Response {
	// The client's requested version is echoed back only when it is one this
	// implementation speaks. Reporting a version we do not implement would make a
	// mismatch invisible until a field behaves unexpectedly.
	var params initializeParams
	if len(request.Params) > 0 {
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return errorResponse(request.ID, CodeInvalidParams, "initialize params are not valid")
		}
	}

	agreed := ProtocolVersion
	if params.ProtocolVersion != "" && params.ProtocolVersion != ProtocolVersion {
		agreed = params.ProtocolVersion
	}

	result, err := json.Marshal(initializeResult{
		ProtocolVersion: agreed,
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo:      implementation{Name: s.name, Version: s.version},
		Instructions:    s.instructions,
	})
	if err != nil {
		return errorResponse(request.ID, CodeInternalError, "could not encode initialize result")
	}

	return &Response{JSONRPC: Version, ID: requestID(request), Result: result}
}

func (s *Server) listTools(request Request) *Response {
	// Sorted so two connections to the same server list the same tools in the same
	// order. A model choosing between tools sees a stable list.
	tools := make([]Tool, 0, len(s.tools))
	for _, handler := range s.tools {
		tools = append(tools, handler.Descriptor())
	}

	sortTools(tools)

	result, err := json.Marshal(ToolListResult{Tools: tools})
	if err != nil {
		return errorResponse(request.ID, CodeInternalError, "could not encode tool list")
	}

	return &Response{JSONRPC: Version, ID: requestID(request), Result: result}
}

func (s *Server) callTool(ctx context.Context, request Request) *Response {
	var params CallToolParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return errorResponse(request.ID, CodeInvalidParams, "tools/call params are not valid")
	}

	handler, ok := s.tools[params.Name]
	if !ok {
		// Unknown tool is a tool-level refusal, not a protocol error: the method
		// existed and the call was well formed, the name just was not one we serve.
		return toolResult(request.ID, fmt.Sprintf("no tool named %q", params.Name), true)
	}

	value, err := handler.Handle(ctx, params.Arguments)
	if err != nil {
		var refused *CallToolError
		if errors.As(err, &refused) {
			return toolResult(request.ID, refused.Reason, true)
		}

		// An unexpected failure is a protocol-level error: the tool could not
		// complete, as opposed to declining to.
		return errorResponse(request.ID, CodeInternalError, err.Error())
	}

	encoded, err := JSONText(value)
	if err != nil {
		return errorResponse(request.ID, CodeInternalError, "could not encode tool result")
	}

	result, err := json.Marshal(CallToolResult{Content: []Content{encoded}})
	if err != nil {
		return errorResponse(request.ID, CodeInternalError, "could not encode tool result")
	}

	return &Response{JSONRPC: Version, ID: requestID(request), Result: result}
}

// errorResponse builds a JSON-RPC failure. A nil id is accepted because a request
// whose id could not be parsed still deserves an error, even though the reply
// cannot be correlated to it.
func errorResponse(id *json.RawMessage, code int, message string) *Response {
	response := &Response{JSONRPC: Version, Error: &RPCError{Code: code, Message: message}}
	if id != nil {
		response.ID = *id
	}

	return response
}

// toolResult reports a refusal inside a successful JSON-RPC response, which is
// what lets the client tell "the tool said no" from "the call failed".
func toolResult(id *json.RawMessage, message string, isError bool) *Response {
	result, err := json.Marshal(CallToolResult{
		Content: []Content{Text(message)},
		IsError: isError,
	})
	if err != nil {
		return errorResponse(id, CodeInternalError, "could not encode tool result")
	}

	response := &Response{JSONRPC: Version, Result: result}
	if id != nil {
		response.ID = *id
	}

	return response
}

// requestID returns the id to reply with.
//
// dispatch has already established that a request reaching a handler is not a
// notification, so the id is present. The helper exists so that invariant is
// stated once rather than dereferenced at four call sites.
func requestID(request Request) json.RawMessage {
	if request.ID == nil {
		return json.RawMessage("null")
	}

	return *request.ID
}

// sortTools orders a tool list by name so two connections to the same server see
// the same list in the same order.
func sortTools(tools []Tool) {
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
}

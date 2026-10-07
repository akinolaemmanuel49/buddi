package calendar

import (
	"context"
	"encoding/json"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// MCPHandlers adapts the connector's tools into MCP handlers for one user.
//
// The user is bound here rather than read per call because the token is resolved per
// call and a session is one user: binding it once makes it structurally impossible
// for a session to answer with somebody else's credential.
func MCPHandlers(userID string, tools *Tools) []mcp.ToolHandler {
	handlers := tools.Handlers()
	wrapped := make([]mcp.ToolHandler, 0, len(handlers))

	for _, handler := range handlers {
		bound := handler

		wrapped = append(wrapped, mcp.ToolHandlerFunc{
			Tool: mcp.Tool{
				Name:        bound.Tool.Name,
				Description: bound.Tool.Description,
				InputSchema: bound.Tool.InputSchema,
			},
			HandleFunc: func(ctx context.Context, arguments json.RawMessage) (any, error) {
				return bound.HandleFunc(ctx, userID, arguments)
			},
		})
	}

	return wrapped
}

// Handle executes a tool by name for a user.
//
// Exposed separately from the MCP adapters because the in-process path calls a tool
// without a protocol round trip, and a second dispatch implementation would be a
// second place for the tool list to drift.
func (t *Tools) Handle(ctx context.Context, userID, name string, arguments json.RawMessage) (any, error) {
	for _, handler := range t.Handlers() {
		if handler.Tool.Name != name {
			continue
		}

		return handler.HandleFunc(ctx, userID, arguments)
	}

	return nil, Refused("no tool named %q", name)
}

// ToolNames lists the connector's tools.
func (t *Tools) ToolNames() []string {
	handlers := t.Handlers()
	names := make([]string, 0, len(handlers))

	for _, handler := range handlers {
		names = append(names, handler.Tool.Name)
	}

	return names
}

// ReadOnly reports whether a named tool changes nothing.
//
// An unknown name is reported as mutating: a tool that cannot be resolved must be
// refused, not assumed harmless.
func (t *Tools) ReadOnly(name string) bool {
	for _, handler := range t.Handlers() {
		if handler.Tool.Name == name {
			return handler.Tool.ReadOnly
		}
	}

	return false
}

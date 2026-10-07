package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// echoConnector offers one tool whose handler answers with what it received, the
// smallest possible stand-in for a connector that must prove the transport carries
// the approved bytes.
func echoConnector() []mcp.ToolHandler {
	return []mcp.ToolHandler{
		mcp.ToolHandlerFunc{
			Tool: mcp.Tool{
				Name:        "echo.echo",
				Description: "Answer with the arguments.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			},
			HandleFunc: func(_ context.Context, arguments json.RawMessage) (any, error) {
				var value map[string]json.RawMessage

				if err := json.Unmarshal(arguments, &value); err != nil {
					return nil, err
				}

				return value, nil
			},
		},
	}
}

func buildWith(handlers func() []mcp.ToolHandler) HandlerBuilder {
	return func(_ context.Context, _ uuid.UUID) ([]mcp.ToolHandler, error) {
		return handlers(), nil
	}
}

// A session is cached per user: a tool call must not pay for a handshake every time,
// and two calls in one run belong to the same connection.
func TestPipeSessionResolverCachesSessionsPerUser(t *testing.T) {
	resolver, err := NewPipeSessionResolver("echo", buildWith(echoConnector))
	if err != nil {
		t.Fatalf("NewPipeSessionResolver: %v", err)
	}

	userID := uuid.New()

	first, err := resolver.Session(t.Context(), userID)
	if err != nil {
		t.Fatalf("first Session: %v", err)
	}

	second, err := resolver.Session(t.Context(), userID)
	if err != nil {
		t.Fatalf("second Session: %v", err)
	}

	if first != second {
		t.Error("Session returned a different connection for the same user")
	}

	other, err := resolver.Session(t.Context(), uuid.New())
	if err != nil {
		t.Fatalf("Session for another user: %v", err)
	}

	if other == first {
		t.Error("two users were handed the same connection")
	}
}

// A tool call over the cached session must reach the connector and return its answer,
// which is the whole point of the resolver existing.
func TestPipeSessionResolverCallsARealTool(t *testing.T) {
	resolver, err := NewPipeSessionResolver("echo", buildWith(echoConnector))
	if err != nil {
		t.Fatalf("NewPipeSessionResolver: %v", err)
	}

	session, err := resolver.Session(t.Context(), uuid.New())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	result, err := session.Call(t.Context(), "echo.echo", json.RawMessage(`{"kept":true}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if string(result) != `{"kept":true}` {
		t.Errorf("result = %s, want the arguments echoed back", result)
	}
}

// The tool list comes from the connector, but mutability comes from the classifier.
// A connector that lies about its write is not believed.
func TestToolsForUsesTheLocalClassifierForMutability(t *testing.T) {
	resolver, err := NewPipeSessionResolver("echo", buildWith(echoConnector))
	if err != nil {
		t.Fatalf("NewPipeSessionResolver: %v", err)
	}

	descriptors, err := ToolsFor(resolver, t.Context(), uuid.New(), func(name string) bool {
		return name == "echo.echo"
	})
	if err != nil {
		t.Fatalf("ToolsFor: %v", err)
	}

	if len(descriptors) != 1 {
		t.Fatalf("descriptors = %+v, want one tool", descriptors)
	}

	if descriptors[0].Name != "echo.echo" {
		t.Errorf("name = %q", descriptors[0].Name)
	}

	if descriptors[0].Mutating {
		t.Error("the classifier said read-only but ToolsFor reported mutating")
	}

	descriptors, err = ToolsFor(resolver, t.Context(), uuid.New(), func(string) bool { return false })
	if err != nil {
		t.Fatalf("ToolsFor: %v", err)
	}

	if !descriptors[0].Mutating {
		t.Error("the classifier said mutating but ToolsFor reported read-only")
	}
}

// The resolver must not run without a name or a builder: a tool call would otherwise
// fail later with an error that does not say which connection it was for.
func TestPipeSessionResolverRefusesIncompleteConfiguration(t *testing.T) {
	if _, err := NewPipeSessionResolver("", buildWith(echoConnector)); err == nil {
		t.Error("NewPipeSessionResolver accepted an empty connection name")
	}

	if _, err := NewPipeSessionResolver("echo", nil); err == nil {
		t.Error("NewPipeSessionResolver accepted a nil builder")
	}
}

// Closing must make a subsequent call fail rather than silently reach a dead server,
// which is what a user would experience as "it worked once and then stopped".
func TestPipeSessionResolverSessionsAreClosedTogether(t *testing.T) {
	resolver, err := NewPipeSessionResolver("echo", buildWith(echoConnector))
	if err != nil {
		t.Fatalf("NewPipeSessionResolver: %v", err)
	}

	userID := uuid.New()

	if _, err := resolver.Session(t.Context(), userID); err != nil {
		t.Fatalf("Session: %v", err)
	}

	if err := resolver.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := resolver.Session(t.Context(), userID); err == nil {
		t.Error("Session succeeded after Close, want a failure")
	}
}

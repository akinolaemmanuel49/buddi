package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// connectInMemory wires a client and server over a pipe and serves the server on
// its own goroutine.
func connectInMemory(t *testing.T, server *mcp.Server) *mcp.Client {
	t.Helper()

	clientTransport, serverTransport := mcp.NewPipeTransport()

	done := make(chan error, 1)

	go func() { done <- server.Serve(t.Context(), serverTransport) }()

	client, err := mcp.Connect(t.Context(), mcp.ClientOptions{
		Name:      "test",
		Transport: clientTransport,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	t.Cleanup(func() {
		_ = client.Close()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after the client closed")
		}
	})

	return client
}

// echoTool returns its arguments, so a test can assert on exactly what arrived.
func echoTool(name string) mcp.ToolHandler {
	return mcp.ToolHandlerFunc{
		Tool: mcp.Tool{
			Name:        name,
			Description: "returns its arguments",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		HandleFunc: func(_ context.Context, arguments json.RawMessage) (any, error) {
			return map[string]any{"received": json.RawMessage(arguments)}, nil
		},
	}
}

func TestConnectCompletesTheHandshakeAndDiscoversTools(t *testing.T) {
	server, err := mcp.NewServer("calendar", "0.1.0", echoTool("calendar.list_events"), echoTool("calendar.create_event"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := connectInMemory(t, server)

	tools := client.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}

	// Sorted, so the order the agent sees does not depend on map iteration.
	if tools[0].Name != "calendar.create_event" || tools[1].Name != "calendar.list_events" {
		t.Errorf("tools = %q, %q, want calendar.create_event, calendar.list_events", tools[0].Name, tools[1].Name)
	}

	if !client.Has("calendar.create_event") {
		t.Error("Has(calendar.create_event) = false, want true")
	}
}

func TestCallToolReturnsTheServersResult(t *testing.T) {
	server, err := mcp.NewServer("calendar", "0.1.0", echoTool("calendar.create_event"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := connectInMemory(t, server)

	result, err := client.CallTool(t.Context(), "calendar.create_event",
		json.RawMessage(`{"summary":"Deploy freeze"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	var decoded struct {
		Received json.RawMessage `json:"received"`
	}

	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("decode result %q: %v", result, err)
	}

	var received map[string]any
	if err := json.Unmarshal(decoded.Received, &received); err != nil {
		t.Fatalf("decode received %q: %v", decoded.Received, err)
	}

	if received["summary"] != "Deploy freeze" {
		t.Errorf("summary = %v, want Deploy freeze", received["summary"])
	}
}

// A refusal has to be distinguishable from a broken connection: a connector saying
// "not authorised" is an ordinary outcome that the agent should surface, not a
// transport error that reads as the server being down.
func TestRefusedToolIsDistinctFromAFailedCall(t *testing.T) {
	refusing := mcp.ToolHandlerFunc{
		Tool: mcp.Tool{Name: "calendar.create_event"},
		HandleFunc: func(context.Context, json.RawMessage) (any, error) {
			return nil, mcp.Refused("calendar is not connected")
		},
	}

	failing := mcp.ToolHandlerFunc{
		Tool: mcp.Tool{Name: "calendar.list_events"},
		HandleFunc: func(context.Context, json.RawMessage) (any, error) {
			return nil, errors.New("database connection refused")
		},
	}

	server, err := mcp.NewServer("calendar", "0.1.0", refusing, failing)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := connectInMemory(t, server)

	_, err = client.CallTool(t.Context(), "calendar.create_event", json.RawMessage(`{}`))

	var refused *mcp.ToolError
	if !errors.As(err, &refused) {
		t.Fatalf("refusal error = %v, want a *mcp.ToolError", err)
	}

	if !strings.Contains(refused.Reason, "not connected") {
		t.Errorf("reason = %q, want it to mention the disconnection", refused.Reason)
	}

	_, err = client.CallTool(t.Context(), "calendar.list_events", json.RawMessage(`{}`))

	var toolErr *mcp.ToolError
	if errors.As(err, &toolErr) {
		t.Errorf("an unexpected failure was reported as a refusal: %v", err)
	}

	if err == nil {
		t.Fatal("CallTool succeeded, want an error")
	}
}

func TestUnknownToolFailsBeforeSpendingARoundTrip(t *testing.T) {
	server, err := mcp.NewServer("calendar", "0.1.0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := connectInMemory(t, server)

	if _, err := client.CallTool(t.Context(), "calendar.nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("CallTool succeeded for an unknown tool, want an error")
	}
}

// An unknown method must be reported as such rather than as a malformed request,
// so a protocol mismatch is visible as itself.
func TestUnknownMethodIsReportedAsMethodNotFound(t *testing.T) {
	server, err := mcp.NewServer("calendar", "0.1.0", echoTool("calendar.list_events"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	clientTransport, serverTransport := mcp.NewPipeTransport()

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(t.Context(), serverTransport)
	}()

	t.Cleanup(func() {
		_ = clientTransport.Close()
		<-served
	})

	// Issued straight at the server, with no client connected. A connected client
	// would have its own reader on this transport and would consume the reply.
	err = rawCall(clientTransport, "resources/list")

	var callErr *mcp.CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("error = %v, want a *mcp.CallError", err)
	}

	if callErr.Err.Code != mcp.CodeMethodNotFound {
		t.Errorf("code = %d, want %d", callErr.Err.Code, mcp.CodeMethodNotFound)
	}
}

// Concurrent calls must not corrupt each other's replies. The agent holds one
// connection per server and may propose steps in parallel, so this is a real shape
// rather than a hypothetical one.
func TestConcurrentCallsGetTheirOwnReplies(t *testing.T) {
	server, err := mcp.NewServer("calendar", "0.1.0", mcp.ToolHandlerFunc{
		Tool: mcp.Tool{Name: "calendar.create_event"},
		HandleFunc: func(_ context.Context, arguments json.RawMessage) (any, error) {
			var decoded struct {
				Summary string `json:"summary"`
			}

			_ = json.Unmarshal(arguments, &decoded)

			// Uneven so an early reply cannot be mistaken for a late one.
			if decoded.Summary == "slow" {
				time.Sleep(20 * time.Millisecond)
			}

			return map[string]any{"summary": decoded.Summary}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := connectInMemory(t, server)

	var wg sync.WaitGroup

	errs := make(chan error, 5)

	for _, summary := range []string{"slow", "fast", "slow", "fast", "slow"} {
		wg.Add(1)

		go func(summary string) {
			defer wg.Done()

			result, err := client.CallTool(t.Context(), "calendar.create_event",
				json.RawMessage(fmt.Sprintf(`{"summary":%q}`, summary)))
			if err != nil {
				errs <- err

				return
			}

			var decoded map[string]any
			if err := json.Unmarshal(result, &decoded); err != nil {
				errs <- err

				return
			}

			if decoded["summary"] != summary {
				errs <- fmt.Errorf("got summary %v, want %s", decoded["summary"], summary)
			}
		}(summary)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// A server that dies must not leave the caller blocked forever.
func TestCallUnblocksWhenTheServerGoesAway(t *testing.T) {
	server, err := mcp.NewServer("calendar", "0.1.0", echoTool("calendar.list_events"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	clientTransport, serverTransport := mcp.NewPipeTransport()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(t.Context(), serverTransport)
	}()

	client, err := mcp.Connect(t.Context(), mcp.ClientOptions{Name: "test", Transport: clientTransport})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	_ = serverTransport.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the transport closed")
	}

	if _, err := client.CallTool(t.Context(), "calendar.list_events", json.RawMessage(`{}`)); err == nil {
		t.Fatal("CallTool succeeded after the server closed, want an error")
	}
}

// A cancelled context must abandon the call rather than leaving it pending
// forever, and must not disturb calls that are still running.
func TestCancelledCallReturnsWithoutAffectingOthers(t *testing.T) {
	release := make(chan struct{})

	server, err := mcp.NewServer("calendar", "0.1.0", mcp.ToolHandlerFunc{
		Tool: mcp.Tool{Name: "calendar.create_event"},
		HandleFunc: func(_ context.Context, arguments json.RawMessage) (any, error) {
			var decoded struct {
				Summary string `json:"summary"`
			}

			_ = json.Unmarshal(arguments, &decoded)

			if decoded.Summary == "wait" {
				<-release
			}

			return map[string]any{"summary": decoded.Summary}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	client := connectInMemory(t, server)

	ctx, cancel := context.WithCancel(t.Context())

	cancelled := make(chan error, 1)

	go func() {
		_, err := client.CallTool(ctx, "calendar.create_event", json.RawMessage(`{"summary":"wait"}`))
		cancelled <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled call did not return")
	}

	close(release)

	if _, err := client.CallTool(t.Context(), "calendar.create_event", json.RawMessage(`{"summary":"quick"}`)); err != nil {
		t.Errorf("a later call failed after an earlier one was cancelled: %v", err)
	}
}

func TestNewServerRejectsADuplicateToolName(t *testing.T) {
	_, err := mcp.NewServer("calendar", "0.1.0", echoTool("calendar.create_event"), echoTool("calendar.create_event"))
	if err == nil {
		t.Fatal("NewServer accepted the same tool name twice")
	}
}

func TestNewServerRejectsAnUnnamedTool(t *testing.T) {
	_, err := mcp.NewServer("calendar", "0.1.0", echoTool(""))
	if err == nil {
		t.Fatal("NewServer accepted a tool with no name")
	}
}

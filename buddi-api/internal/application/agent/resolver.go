package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// HandlerBuilder supplies a connector's tools for one user.
//
// The user is an argument rather than something captured, because a connector's tools
// differ per user only in which credential they resolve, and building them per session
// is what keeps one session from ever holding another's.
type HandlerBuilder func(ctx context.Context, userID uuid.UUID) ([]mcp.ToolHandler, error)

// PipeSessionResolver opens connector sessions over an in-memory MCP pipe.
//
// It runs the connector's server in this process rather than spawning a binary. Both
// are legitimate and the client cannot tell them apart, which is the point of the seam:
// a connector written against the interface works either way, and the tests that
// exercise a real JSON-RPC exchange do not need a subprocess to do it.
//
// A subprocess is still the right choice where the connector must be isolated — an
// untrusted third-party server, or one with its own dependencies — and swapping to it
// means changing only this file.
type PipeSessionResolver struct {
	build HandlerBuilder

	// name identifies the connection in errors.
	name string

	mu       sync.Mutex
	closed   bool
	sessions map[uuid.UUID]*pipeSession
}

// NewPipeSessionResolver builds a resolver.
func NewPipeSessionResolver(name string, build HandlerBuilder) (*PipeSessionResolver, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("agent: a session resolver needs a connection name")
	}

	if build == nil {
		return nil, fmt.Errorf("agent: connection %q has no tool builder", name)
	}

	return &PipeSessionResolver{
		build:    build,
		name:     name,
		sessions: make(map[uuid.UUID]*pipeSession),
	}, nil
}

// Name identifies the connection.
func (r *PipeSessionResolver) Name() string { return r.name }

// Session opens, or reuses, the user's session.
//
// A session is cached per user because a tool call should not pay for a handshake, and
// because the connector's own state — a token resolved once per call, a provider
// client — belongs to the connection rather than to the request. The cache is keyed by
// user, so a session can only ever be handed to the user it belongs to.
func (r *PipeSessionResolver) Session(ctx context.Context, userID uuid.UUID) (RemoteSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, fmt.Errorf("agent: connection %q is closed", r.name)
	}

	if existing, ok := r.sessions[userID]; ok {
		return existing, nil
	}

	handlers, err := r.build(ctx, userID)
	if err != nil {
		return nil, err
	}

	if len(handlers) == 0 {
		return nil, fmt.Errorf("agent: connection %q offers no tools", r.name)
	}

	server, err := mcp.NewServer(r.name, "0.1.0", handlers...)
	if err != nil {
		return nil, err
	}

	clientTransport, serverTransport := mcp.NewPipeTransport()

	// The server is started before the handshake. Connect performs the handshake
	// synchronously, so a server not yet serving leaves the client waiting for a reply
	// that cannot arrive.
	serveCtx, cancelServe := context.WithCancel(context.WithoutCancel(ctx))

	go func() {
		_ = server.Serve(serveCtx, serverTransport)
	}()

	client, err := mcp.Connect(serveCtx, mcp.ClientOptions{Name: r.name, Transport: clientTransport})
	if err != nil {
		cancelServe()

		return nil, fmt.Errorf("agent: could not reach connection %q: %w", r.name, err)
	}

	session := &pipeSession{client: client, cancel: cancelServe}
	r.sessions[userID] = session

	return session, nil
}

// Close shuts every session down. It is called on server shutdown so a connector's
// goroutines do not outlive the process's use of them.
func (r *PipeSessionResolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	r.closed = true

	for userID, session := range r.sessions {
		session.close()
		delete(r.sessions, userID)
	}

	return nil
}

// pipeSession is one user's connection to a connector.
type pipeSession struct {
	client *mcp.Client
	cancel context.CancelFunc
}

// Call implements RemoteSession. The arguments are passed through untouched: whatever
// was approved is what the connector receives, and rewriting them here would put a
// second implementation of the tool's schema between the user and the call.
func (s *pipeSession) Call(ctx context.Context, tool string, arguments json.RawMessage) (json.RawMessage, error) {
	return s.client.CallTool(ctx, tool, arguments)
}

func (s *pipeSession) close() {
	if s.cancel != nil {
		s.cancel()
	}

	_ = s.client.Close()
}

// ToolsFor lists the connector's tools as registry descriptors.
//
// Mutating is not taken from the wire: a server cannot be trusted to classify its own
// writes as reads, and a misclassification would let a change reach a service with no
// approval in front of it. readOnly is therefore required, and the connector's own
// classification is the authority.
func ToolsFor(
	resolver *PipeSessionResolver,
	ctx context.Context,
	userID uuid.UUID,
	readOnly func(toolName string) bool,
) ([]ToolDescriptor, error) {
	if readOnly == nil {
		return nil, fmt.Errorf("agent: connection %q needs a tool classifier", resolver.Name())
	}

	session, err := resolver.Session(ctx, userID)
	if err != nil {
		return nil, err
	}

	pipe, ok := session.(*pipeSession)
	if !ok {
		return nil, fmt.Errorf("agent: connection %q returned an unexpected session", resolver.Name())
	}

	listed := pipe.client.Tools()

	descriptors := make([]ToolDescriptor, 0, len(listed))

	for _, tool := range listed {
		descriptors = append(descriptors, ToolDescriptor{
			Name:        tool.Name,
			Description: tool.Description,
			Mutating:    !readOnly(tool.Name),
		})
	}

	return descriptors, nil
}

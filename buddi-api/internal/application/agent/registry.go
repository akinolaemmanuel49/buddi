package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// Registry holds every tool the agent may reach, and decides which one a plan
// proposes.
//
// Selection lives here rather than in the planner because the two failure modes it
// avoids pull in opposite directions. If the model chose the tool it could invent a
// call the user never intended, on a service with access to a real account. If the
// orchestration hardcoded one tool, a connector could not be reached at all. So the
// model emits an intent, this registry maps that intent to a mechanism, and the
// user's approval sits between them and anything that changes.
//
// Every approval proposes a change, so only mutating tools are selectable. A
// read-only tool registered here is still useful — it can be called by name from
// code — but a plan can never resolve to one, because there is nothing to approve.
type Registry struct {
	// bindings maps a planner intent to the mechanism that carries it out.
	bindings map[domain.PlanIntent]Binding

	// defaultIntent is used when a plan's intent is empty or unrecognised. It is the
	// safe choice rather than a guess: falling back to the tool that records work
	// locally is harmless, while falling back to a third-party connector would
	// propose a change to a service the user may not have asked us to touch.
	defaultIntent domain.PlanIntent

	// tools indexes every registered binding by tool name.
	tools map[string]Binding
}

// ArgEncoder renders a plan as one tool's arguments.
//
// This lives beside the tool rather than in the planner because a connector's
// arguments are its own: the model has no business knowing that a calendar event
// wants a summary and a start time, and a schema shared between them would be a
// place for the model to invent fields.
type ArgEncoder func(plan *planner.Plan) (json.RawMessage, error)

// RationaleWriter explains a proposal to the user in the approval screen.
type RationaleWriter func(plan *planner.Plan) string

// Binding is one intent's mechanism: which tool, and how a plan becomes its
// arguments.
type Binding struct {
	Tool      Tool
	Encode    ArgEncoder
	Rationale RationaleWriter
}

// Proposal is a resolved plan, ready to be shown for approval.
type Proposal struct {
	Tool      Tool
	Arguments json.RawMessage
	Rationale string
}

// ToolDescriptor describes a registered tool for display.
type ToolDescriptor struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Mutating reports whether executing this tool changes something outside Buddi.
	// The UI uses it to say so plainly on the approval screen.
	Mutating bool `json:"mutating"`
}

// RegistryOption registers one intent's mechanism.
type RegistryOption struct {
	// Intent this binding serves. Leave empty on a tool that no plan can select.
	Intent domain.PlanIntent
	// Tool, Encode and Rationale make up the mechanism.
	Tool      Tool
	Encode    ArgEncoder
	Rationale RationaleWriter
	// Default makes this the fallback for an unrecognised intent.
	Default bool
}

// NewRegistry builds the registry.
//
// Exactly one default is required: an unrecognised intent has to go somewhere, and
// leaving it to chance would make the answer depend on registration order.
func NewRegistry(options ...RegistryOption) (*Registry, error) {
	registry := &Registry{
		bindings: make(map[domain.PlanIntent]Binding, len(options)),
		tools:    make(map[string]Binding, len(options)),
	}

	for _, option := range options {
		if option.Tool == nil {
			return nil, fmt.Errorf("agent: nil tool registered")
		}

		tool := option.Tool

		name := strings.TrimSpace(tool.Name())
		if name == "" {
			return nil, fmt.Errorf("agent: tool name is required")
		}

		if _, exists := registry.tools[name]; exists {
			return nil, fmt.Errorf("agent: duplicate tool %q", name)
		}

		if option.Encode == nil {
			return nil, fmt.Errorf("agent: tool %q has no argument encoder", name)
		}

		binding := Binding{Tool: tool, Encode: option.Encode, Rationale: option.Rationale}
		registry.tools[name] = binding

		if option.Intent == "" {
			// A tool no plan can select. Useful for calling by name, never reachable
			// by a proposal.
			continue
		}

		if !option.Intent.IsValid() {
			return nil, fmt.Errorf("agent: tool %q is registered for unknown intent %q", name, option.Intent)
		}

		if _, exists := registry.bindings[option.Intent]; exists {
			return nil, fmt.Errorf("agent: intent %q is claimed by more than one tool", option.Intent)
		}

		if !tool.Mutating() {
			return nil, fmt.Errorf("agent: tool %q is not mutating and cannot back a proposal", name)
		}

		registry.bindings[option.Intent] = binding

		if option.Default {
			if registry.defaultIntent != "" {
				return nil, fmt.Errorf("agent: more than one default binding")
			}

			registry.defaultIntent = option.Intent
		}
	}

	if registry.defaultIntent == "" {
		return nil, fmt.Errorf("agent: a default binding is required")
	}

	if _, ok := registry.bindings[registry.defaultIntent]; !ok {
		return nil, fmt.Errorf("agent: default intent %q is not registered", registry.defaultIntent)
	}

	return registry, nil
}

// Propose resolves a plan to a proposal the user can approve.
//
// The mapping is total and deterministic: the same intent always yields the same
// mechanism, and anything unrecognised falls back to the default rather than to a
// best-effort guess. The tool's own validator runs here, before the proposal is
// shown, so a payload that would be refused at execution is never displayed.
func (r *Registry) Propose(plan *planner.Plan) (Proposal, error) {
	// A clarification proposes nothing, so it must never fall through to the default
	// binding. There is no tool that answers a question, and the default is a task —
	// which would turn "Which doctor are you seeing?" into a task with that title.
	//
	// This is checked here rather than at the call site because the fallback below is
	// total by design, and a total fallback is exactly what makes this easy to miss.
	if plan.Intent == domain.PlanIntentClarification {
		return Proposal{}, fmt.Errorf("agent: a clarification is a question, not a proposal")
	}

	binding, ok := r.bindings[plan.Intent]
	if !ok {
		binding, ok = r.bindings[r.defaultIntent]
		if !ok {
			return Proposal{}, fmt.Errorf("agent: no tool registered as default")
		}
	}

	arguments, err := binding.Encode(plan)
	if err != nil {
		return Proposal{}, err
	}

	if err := binding.Tool.Validate(arguments); err != nil {
		return Proposal{}, fmt.Errorf("agent: proposed arguments are not executable: %w", err)
	}

	proposal := Proposal{Tool: binding.Tool, Arguments: arguments}
	if binding.Rationale != nil {
		proposal.Rationale = binding.Rationale(plan)
	}

	return proposal, nil
}

// Binding returns the mechanism registered for a tool name.
func (r *Registry) Binding(name string) (Binding, bool) {
	binding, ok := r.tools[name]

	return binding, ok
}

// Descriptor describes a registered tool, or reports it as unknown.
func (r *Registry) Descriptor(name string) (ToolDescriptor, bool) {
	binding, ok := r.tools[name]
	if !ok {
		return ToolDescriptor{}, false
	}

	return descriptorFor(binding.Tool), true
}

// Descriptors lists every registered tool, sorted by name so the order a client
// sees does not depend on registration order.
func (r *Registry) Descriptors() []ToolDescriptor {
	descriptors := make([]ToolDescriptor, 0, len(r.tools))

	for _, binding := range r.tools {
		descriptors = append(descriptors, descriptorFor(binding.Tool))
	}

	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })

	return descriptors
}

func descriptorFor(tool Tool) ToolDescriptor {
	return ToolDescriptor{
		Name:        tool.Name(),
		Description: tool.Description(),
		Mutating:    tool.Mutating(),
	}
}

// RemoteSession calls tools on one connector for one user.
//
// The agent holds a registry of tool definitions but not a user's credentials, and
// it must not: a connector's credentials are per-user, and the resolution has to
// happen at execution time rather than at registration. Splitting it out like this
// keeps the token out of the tool arguments, which are shown to the user and stored
// on the approval row.
type RemoteSession interface {
	// Call invokes a tool with exactly the given arguments.
	Call(ctx context.Context, tool string, arguments json.RawMessage) (json.RawMessage, error)
}

// ErrNotConnected reports that a user has not connected the service this tool needs.
var ErrNotConnected = fmt.Errorf("agent: this service is not connected")

// SessionResolver opens a connector session for a user.
type SessionResolver interface {
	// Session returns the user's session, or ErrNotConnected when the user has not
	// connected the service.
	Session(ctx context.Context, userID uuid.UUID) (RemoteSession, error)
}

// MCPTool reaches a tool on an MCP server as if it were an in-process one.
//
// This is the piece §10.1 promised: an integration added behind this type is
// indistinguishable from TaskCreateTool to everything above it, which is what makes
// the next connector cheap rather than a rewrite.
type MCPTool struct {
	name        string
	description string
	mutating    bool

	resolver SessionResolver
	// server identifies the connection in errors and logs.
	server string
}

// MCPToolOptions configures an MCPTool.
type MCPToolOptions struct {
	// Server is the connection's name, used in errors.
	Server string
	// Tool is the remote tool's descriptor as reported by the server.
	Tool ToolDescriptor
	// Resolver opens a per-user session.
	Resolver SessionResolver
}

// NewMCPTool adapts a remote tool to the Tool contract.
func NewMCPTool(opts MCPToolOptions) (*MCPTool, error) {
	if strings.TrimSpace(opts.Server) == "" {
		return nil, fmt.Errorf("agent: mcp tool name is required")
	}

	if strings.TrimSpace(opts.Tool.Name) == "" {
		return nil, fmt.Errorf("agent: mcp tool %q has no name", opts.Server)
	}

	if opts.Resolver == nil {
		return nil, fmt.Errorf("agent: mcp tool %q has no session resolver", opts.Tool.Name)
	}

	return &MCPTool{
		name:        opts.Tool.Name,
		description: opts.Tool.Description,
		mutating:    opts.Tool.Mutating,
		resolver:    opts.Resolver,
		server:      opts.Server,
	}, nil
}

// Name implements Tool.
func (t *MCPTool) Name() string { return t.name }

// Description implements Tool.
func (t *MCPTool) Description() string { return t.description }

// Mutating implements Tool.
func (t *MCPTool) Mutating() bool { return t.mutating }

// Server returns the connection this tool is reached through.
func (t *MCPTool) Server() string { return t.server }

// Validate implements Tool.
//
// A remote tool's own validator is the authority on its arguments, so this only
// checks that there is something to send. Re-encoding the payload here would mean
// two implementations of the same schema, and the one that runs at execution time
// would be the one the user never saw.
func (t *MCPTool) Validate(arguments json.RawMessage) error {
	if len(arguments) == 0 {
		return fmt.Errorf("arguments are required")
	}

	if !json.Valid(arguments) {
		return fmt.Errorf("arguments must be valid JSON")
	}

	return nil
}

// Execute implements Tool.
func (t *MCPTool) Execute(ctx context.Context, userID uuid.UUID, _ uuid.UUID, arguments json.RawMessage) (json.RawMessage, error) {
	session, err := t.resolver.Session(ctx, userID)
	if err != nil {
		return nil, err
	}

	return session.Call(ctx, t.name, arguments)
}

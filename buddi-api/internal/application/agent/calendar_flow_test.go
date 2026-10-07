package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/agent"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/task"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
	"github.com/akinolaemmanuel49/buddi-api/internal/infrastructure/mcp"
)

// The purpose of this test is one sentence: the bytes a user approved are the bytes the
// provider receives, across a real process boundary. Everything here is arranged so a
// failure names which link in that chain broke.
//
// Every stage is real. The plan uses the planner's own type, the registry does its real
// intent mapping, the call goes over an actual MCP pipe as JSON-RPC and is serialised
// and parsed, the connector does its real strict decoding and validation, and the
// credential comes from the connector's token source. Only Google itself is absent.

// recorder stands in for Google and remembers exactly what it was handed.
type recorder struct {
	created []calendar.Event
	tokens  []string
}

func (r *recorder) CreateEvent(_ context.Context, accessToken string, event calendar.Event) (*calendar.Event, error) {
	r.created = append(r.created, event)
	r.tokens = append(r.tokens, accessToken)

	start, err := time.Parse(time.RFC3339, strings.TrimSpace(event.StartAt))
	if err != nil {
		// The connector validated this before calling, so reaching here would mean the
		// validation was bypassed. That is worth stopping for.
		panic("provider received an unvalidated start_at: " + err.Error())
	}

	return calendar.CreatedEvent(event.Summary, start, "evt-approved-1"), nil
}

func (r *recorder) ListEvents(context.Context, string, calendar.ListQuery) ([]calendar.Event, error) {
	return []calendar.Event{}, nil
}

// staticTokens stands in for the OAuth store's AccessToken, keyed by user. It is the
// same seam the real service is reached through.
type staticTokens struct {
	tokens map[uuid.UUID]string
}

func (s *staticTokens) AccessToken(_ context.Context, userID string) (string, error) {
	token, ok := s.tokens[uuid.MustParse(userID)]
	if !ok {
		return "", calendar.Refused("calendar is not connected for this account")
	}

	return token, nil
}

// pipeResolver opens a connector session for a user over a real MCP pipe, and records
// what each call carried on the wire.
type pipeResolver struct {
	connector *calendar.Tools

	// sent holds the arguments as they were handed to the transport, before any
	// decoding. Comparing this against the approved bytes is what proves the transport
	// did not alter them.
	sent []json.RawMessage
}

func (p *pipeResolver) Session(ctx context.Context, userID uuid.UUID) (agent.RemoteSession, error) {
	server, err := mcp.NewServer("calendar", "0.1.0", calendar.MCPHandlers(userID.String(), p.connector)...)
	if err != nil {
		return nil, err
	}

	clientTransport, serverTransport := mcp.NewPipeTransport()

	// The server is started first: the handshake happens inside Connect, so a server
	// that is not yet serving leaves the handshake waiting for a reply that cannot
	// come.
	go func() {
		_ = server.Serve(context.Background(), serverTransport)
	}()

	// The connection outlives the call that opened it, so it is not tied to that
	// call's context.
	client, err := mcp.Connect(context.WithoutCancel(ctx), mcp.ClientOptions{
		Name:      "calendar",
		Transport: clientTransport,
	})
	if err != nil {
		return nil, err
	}

	return &pipeSession{userID: userID, client: client, resolver: p}, nil
}

type pipeSession struct {
	userID   uuid.UUID
	client   *mcp.Client
	resolver *pipeResolver
}

func (s *pipeSession) Call(ctx context.Context, tool string, arguments json.RawMessage) (json.RawMessage, error) {
	s.resolver.sent = append(s.resolver.sent, append(json.RawMessage(nil), arguments...))

	return s.client.CallTool(ctx, tool, arguments)
}

// calendarToolsFor builds agent tools for the connector, discovered the way a real
// integration is: the server is asked what it has, and the descriptions come back over
// the wire rather than being written down here.
func calendarToolsFor(t *testing.T, connector *calendar.Tools, userID uuid.UUID) (*pipeResolver, []agent.Tool) {
	t.Helper()

	server, err := mcp.NewServer("calendar", "0.1.0", calendar.MCPHandlers(userID.String(), connector)...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	clientTransport, serverTransport := mcp.NewPipeTransport()

	// The server is started first. Connect performs the handshake synchronously, so a
	// server that is not yet serving leaves the handshake waiting for a reply that
	// cannot come.
	go func() {
		_ = server.Serve(context.Background(), serverTransport)
	}()

	client, err := mcp.Connect(t.Context(), mcp.ClientOptions{Name: "calendar", Transport: clientTransport})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resolver := &pipeResolver{connector: connector}
	remote := make([]agent.Tool, 0, len(client.Tools()))

	for _, listed := range client.Tools() {
		// Mutating is deliberately not read off the wire. The wire carries no such
		// field, and a connector could label its own write as a read to slip past an
		// approval. The connector's classification is the authority.
		descriptor := agent.ToolDescriptor{
			Name:        listed.Name,
			Description: listed.Description,
			Mutating:    !connector.ReadOnly(listed.Name),
		}

		tool, err := agent.NewMCPTool(agent.MCPToolOptions{
			Server:   "calendar",
			Tool:     descriptor,
			Resolver: resolver,
		})
		if err != nil {
			t.Fatalf("NewMCPTool %s: %v", listed.Name, err)
		}

		if tool.Mutating() != descriptor.Mutating {
			t.Errorf("%s mutating = %v, discovery said %v", listed.Name, tool.Mutating(), descriptor.Mutating)
		}

		remote = append(remote, tool)
	}

	return resolver, remote
}

func remoteToolFor(t *testing.T, tools []agent.Tool, name string) agent.Tool {
	t.Helper()

	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}

	t.Fatalf("no tool named %q was discovered", name)

	return nil
}

// silentConnector builds a connector whose provider does nothing, for tests that are
// about proposals rather than about what a provider receives.
func silentConnector(userID uuid.UUID) *calendar.Tools {
	return calendar.NewTools(&recorder{}, &staticTokens{
		tokens: map[uuid.UUID]string{userID: "ya29.alice"},
	})
}

// calendarPlan is a plan as the planner produced it for a calendar request.
func calendarPlan(start time.Time) *planner.Plan {
	return &planner.Plan{
		Intent:      domain.PlanIntentCalendarEvent,
		Title:       "Deploy freeze",
		Description: "The freeze agreed with the release team.",
		Priority:    domain.TaskPriorityHigh,
		DueAt:       &start,
		Steps: []planner.Step{
			{Description: "Announce in #release"},
			{Description: "Tag the release branch"},
		},
	}
}

func newRegistryWithCalendar(t *testing.T, connector *calendar.Tools, userID uuid.UUID) (*agent.Registry, *pipeResolver) {
	t.Helper()

	resolver, remote := calendarToolsFor(t, connector, userID)

	registry, err := agent.NewRegistry(
		agent.TaskCreateBinding(&stubTaskCreator{}),
		agent.CalendarBinding(remoteToolFor(t, remote, calendar.CreateEventTool)),
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	return registry, resolver
}

// The chain, end to end, with the approval in the middle.
func TestAnApprovedCalendarProposalReachesTheProviderUnchanged(t *testing.T) {
	userID := uuid.New()
	start := time.Date(2026, time.March, 4, 14, 0, 0, 0, time.UTC)

	provider := &recorder{}
	connector := calendar.NewTools(provider, &staticTokens{
		tokens: map[uuid.UUID]string{userID: "ya29.alice"},
	})

	registry, resolver := newRegistryWithCalendar(t, connector, userID)

	// The plan becomes a proposal, exactly as the agent does before showing an approval.
	proposal, err := registry.Propose(calendarPlan(start))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if proposal.Tool.Name() != calendar.CreateEventTool {
		t.Fatalf("proposal tool = %q, want %q", proposal.Tool.Name(), calendar.CreateEventTool)
	}

	if !strings.Contains(proposal.Rationale, "Google Calendar") {
		t.Errorf("rationale = %q, want it to say plainly that the change leaves Buddi", proposal.Rationale)
	}

	// Everything the user would have seen.
	approved := proposal.Arguments

	if !strings.Contains(string(approved), "Deploy freeze") {
		t.Errorf("approved payload does not contain the event title: %s", approved)
	}

	// Execution replays the stored arguments. Nothing re-reads the plan and nothing is
	// regenerated: the approved bytes are the call.
	if _, err := proposal.Tool.Execute(t.Context(), userID, uuid.New(), approved); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(resolver.sent) != 1 {
		t.Fatalf("wire carried %d payloads, want 1", len(resolver.sent))
	}

	if !equalJSON(resolver.sent[0], approved) {
		t.Errorf("wire payload = %s, want the approved bytes %s", resolver.sent[0], approved)
	}

	if len(provider.created) != 1 {
		t.Fatalf("provider received %d events, want 1", len(provider.created))
	}

	sent := provider.created[0]

	if sent.Summary != "Deploy freeze" {
		t.Errorf("provider summary = %q, want %q", sent.Summary, "Deploy freeze")
	}

	if want := start.Format(time.RFC3339); sent.StartAt != want {
		t.Errorf("provider start_at = %q, want %q", sent.StartAt, want)
	}

	// The description is the planner's own words about the plan's steps.
	for _, step := range []string{"Announce in #release", "Tag the release branch"} {
		if !strings.Contains(sent.Description, step) {
			t.Errorf("description = %q, want it to mention %q", sent.Description, step)
		}
	}

	// Nothing is invented to fill the event in. The encoder reads the plan's own fields
	// and nothing else, so every other key stays absent rather than being defaulted to
	// something plausible. A field that appears here would be shown to the user and
	// written to their calendar without them having chosen it.
	for field, got := range map[string]string{
		"end_at":      sent.EndAt,
		"timezone":    sent.TimeZone,
		"location":    sent.Location,
		"calendar_id": sent.CalendarID,
	} {
		if got != "" {
			t.Errorf("provider received %s = %q, want it absent: nothing beyond the plan was approved", field, got)
		}
	}

	if len(sent.Attendees) != 0 {
		t.Errorf("provider received attendees = %v, want none: nobody was approved for an invitation", sent.Attendees)
	}

	// The approved payload itself must not carry them either. Checking only the
	// provider's copy would miss an encoder that added a field and had it dropped on
	// the way, which would still mean the user was shown something they did not choose.
	for _, field := range []string{"end_at", "timezone", "location", "calendar_id", "attendees"} {
		if _, present := fieldsOf(approved)[field]; present {
			t.Errorf("approved payload carries %q, which the plan did not ask for", field)
		}
	}

	if provider.tokens[0] != "ya29.alice" {
		t.Errorf("provider credential = %q, want the approving user's own", provider.tokens[0])
	}
}

func fieldsOf(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage

	_ = json.Unmarshal(raw, &fields)

	return fields
}

// The approval's purpose is that nothing changes without it. Nothing here calls Execute,
// because the user declined, and the provider must be untouched.
func TestARejectedProposalIsNeverSentToTheProvider(t *testing.T) {
	userID := uuid.New()
	start := time.Date(2026, time.March, 4, 14, 0, 0, 0, time.UTC)

	provider := &recorder{}
	connector := calendar.NewTools(provider, &staticTokens{
		tokens: map[uuid.UUID]string{userID: "ya29.alice"},
	})

	registry, _ := newRegistryWithCalendar(t, connector, userID)

	proposal, err := registry.Propose(calendarPlan(start))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if len(proposal.Arguments) == 0 {
		t.Fatal("proposal has no arguments, so the test would not be exercising anything")
	}

	if len(provider.created) != 0 {
		t.Fatalf("provider received %d events, want 0", len(provider.created))
	}
}

// The plan is the only source of an event's text. Note content has no route into the
// payload, and a plan carrying something that looks like it does is refused rather than
// quietly trimmed.
func TestNoNoteContentCanReachTheApprovedPayload(t *testing.T) {
	userID := uuid.New()
	start := time.Date(2026, time.March, 4, 14, 0, 0, 0, time.UTC)

	registry, _ := newRegistryWithCalendar(t, silentConnector(userID), userID)

	proposal, err := registry.Propose(calendarPlan(start))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	// The payload has exactly the connector's keys and nothing else.
	var fields map[string]json.RawMessage

	if err := json.Unmarshal(proposal.Arguments, &fields); err != nil {
		t.Fatalf("decode approved payload: %v", err)
	}

	allowed := map[string]bool{
		"summary":     true,
		"start_at":    true,
		"end_at":      true,
		"description": true,
		"location":    true,
		"attendees":   true,
		"calendar_id": true,
	}

	for key := range fields {
		if !allowed[key] {
			t.Errorf("approved payload carries %q, which the connector does not accept", key)
		}
	}

	// And the connector accepts it, which is what proves the two descriptions agree.
	if _, err := calendar.EventFromArguments(proposal.Arguments); err != nil {
		t.Errorf("the connector refuses the payload the approval screen shows: %v", err)
	}
}

// A plan with no time cannot become an event. Defaulting it would put a time in
// somebody's calendar that they never chose.
func TestAPlanWithoutAStartTimeIsRefused(t *testing.T) {
	userID := uuid.New()

	registry, _ := newRegistryWithCalendar(t, silentConnector(userID), userID)

	plan := calendarPlan(time.Date(2026, time.March, 4, 14, 0, 0, 0, time.UTC))
	plan.DueAt = nil

	if _, err := registry.Propose(plan); err == nil {
		t.Fatal("Propose succeeded with no start time, want a refusal")
	}
}

// An unrecognised intent becomes a task, never a calendar event. Falling back into a
// third-party write is not a safe default.
func TestAnUnrecognisedIntentFallsBackToATaskNotACalendarEvent(t *testing.T) {
	userID := uuid.New()
	start := time.Date(2026, time.March, 4, 14, 0, 0, 0, time.UTC)

	registry, _ := newRegistryWithCalendar(t, silentConnector(userID), userID)

	plan := calendarPlan(start)
	plan.Intent = domain.PlanIntent("something.unrecognised")

	proposal, err := registry.Propose(plan)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	if proposal.Tool.Name() == calendar.CreateEventTool {
		t.Error("an unrecognised intent resolved to a calendar write, which is not the safe default")
	}
}

// The connector must not be able to label its own write as a read and thereby slip past
// an approval. Mutating is not carried on the wire, and the registry refuses a
// read-only tool as a backing mechanism.
func TestACalendarEventIntentCannotBackAProposalThroughAReadOnlyTool(t *testing.T) {
	userID := uuid.New()

	_, remote := calendarToolsFor(t, silentConnector(userID), userID)

	listEvents := remoteToolFor(t, remote, calendar.ListEventsTool)

	if listEvents.Mutating() {
		t.Fatal(calendar.ListEventsTool + " was classified as mutating, so a read could sit behind an approval")
	}

	_, err := agent.NewRegistry(
		agent.TaskCreateBinding(&stubTaskCreator{}),
		agent.CalendarBinding(listEvents),
	)
	if err == nil {
		t.Fatal("NewRegistry accepted a read-only tool for an intent, so a plan could resolve to something that changes nothing")
	}
}

func equalJSON(left, right json.RawMessage) bool {
	var leftValue, rightValue any

	if err := json.Unmarshal(left, &leftValue); err != nil {
		return false
	}

	if err := json.Unmarshal(right, &rightValue); err != nil {
		return false
	}

	leftEncoded, _ := json.Marshal(leftValue)
	rightEncoded, _ := json.Marshal(rightValue)

	return string(leftEncoded) == string(rightEncoded)
}

// stubTaskCreator satisfies the task binding's dependency. A registry needs a default
// binding to exist, and these tests never select it.
type stubTaskCreator struct{}

func (s *stubTaskCreator) Create(context.Context, uuid.UUID, task.CreateInput) (*domain.Task, error) {
	return nil, context.Canceled
}

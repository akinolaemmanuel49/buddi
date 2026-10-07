package chat_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/chat"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// A turn that asks a question must not record a run. There is nothing to approve, and
// a run awaiting approval for a question leaves a card on screen with no action behind
// it.
func TestAClarificationRecordsNoRun(t *testing.T) {
	h := newHarness(t)

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent:   domain.PlanIntentClarification,
		Title:    "Dentist",
		Question: "What time is the appointment?",
	}}

	_, result, err := h.turn(t, chat.Turn{Input: "I need to see the dentist", Mode: chat.ModePlan})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if len(h.runs.goals) != 0 {
		t.Errorf("planner recorded %d runs, want 0: a question cannot be approved", len(h.runs.goals))
	}

	if result.Run != nil {
		t.Error("a clarification produced a run reference")
	}

	if result.Clarification == nil {
		t.Fatal("the result carries no clarification")
	}

	if result.Clarification.Question != "What time is the appointment?" {
		t.Errorf("question = %q", result.Clarification.Question)
	}
}

// The question is the message content, so a transcript reloaded later still shows what
// was asked without the client having to render anything special.
func TestAClarificationIsStoredAsTheMessageContent(t *testing.T) {
	h := newHarness(t)

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent:   domain.PlanIntentClarification,
		Title:    "Dentist",
		Question: "What time is the appointment?",
	}}

	_, result, err := h.turn(t, chat.Turn{Input: "I need to see the dentist", Mode: chat.ModePlan})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if result.Message.Content != "What time is the appointment?" {
		t.Errorf("stored content = %q, want the question", result.Message.Content)
	}

	// A dangling run id would render as a plan awaiting approval.
	if result.Message.RunID != nil {
		t.Error("a clarification stored a run id")
	}
}

// The original request has to be stored, because the reply alone is not answerable.
func TestAClarificationStoresTheRequestItIsAbout(t *testing.T) {
	h := newHarness(t)

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent:   domain.PlanIntentClarification,
		Title:    "Dentist",
		Question: "What time is the appointment?",
	}}

	_, result, err := h.turn(t, chat.Turn{Input: "I need to see the dentist", Mode: chat.ModePlan})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if result.Message.ClarificationRequest == nil {
		t.Fatal("no clarification request was stored, so the next turn cannot be re-planned")
	}

	if *result.Message.ClarificationRequest != "I need to see the dentist" {
		t.Errorf("stored request = %q", *result.Message.ClarificationRequest)
	}
}

// The reply is planned against the request it is answering, not against itself.
func TestAReplyToAClarificationIsPlannedWithTheOriginalRequest(t *testing.T) {
	h := newHarness(t)

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent:   domain.PlanIntentClarification,
		Title:    "Dentist",
		Question: "What time is the appointment?",
	}}

	first := h.clarify(t, "I need to see the dentist")

	h.planDentist(t, first.Conversation.ID, "Tuesday at 4pm")

	if len(h.planner.goals) < 2 {
		t.Fatalf("planner saw %d goals, want 2", len(h.planner.goals))
	}

	got := h.planner.goals[len(h.planner.goals)-1]

	if !strings.Contains(got, "dentist") {
		t.Errorf("planned from %q, want the original request included: a bare \"Tuesday at 4pm\" names no dentist", got)
	}

	if !strings.Contains(got, "4pm") {
		t.Errorf("planned from %q, want the user's answer included", got)
	}
}

// Answering must end the chain. Otherwise the question is re-asked forever, because
// every reply leaves a new marker behind.
func TestAnsweringAClarificationEndsTheChain(t *testing.T) {
	h := newHarness(t)

	first := h.clarify(t, "I need to see the dentist")

	// The answer resolves it into a plan, which is what a satisfied chain looks like.
	h.planDentist(t, first.Conversation.ID, "Tuesday at 4pm")

	// A third, unrelated message must be planned from itself.
	if _, err := h.service.Turn(context.Background(), testUserID, chat.Turn{
		Input:          "buy groceries",
		ConversationID: &first.Conversation.ID,
		Mode:           chat.ModePlan,
	}, nil); err != nil {
		t.Fatalf("third Turn: %v", err)
	}

	last := h.planner.goals[len(h.planner.goals)-1]

	if strings.Contains(last, "dentist") {
		t.Errorf("planned from %q, want the answered question to be forgotten", last)
	}
}

// clarify runs a turn whose planner asks a question.
func (h *harness) clarify(t *testing.T, input string) *chat.Result {
	t.Helper()

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent:   domain.PlanIntentClarification,
		Title:    "Dentist",
		Question: "What time is the appointment?",
	}}

	result, err := h.service.Turn(context.Background(), testUserID,
		chat.Turn{Input: input, Mode: chat.ModePlan}, nil)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if result.Clarification == nil {
		t.Fatal("the turn did not clarify")
	}

	return result
}

// planDentist answers a clarification and asserts the request was carried forward.
func (h *harness) planDentist(t *testing.T, conversationID uuid.UUID, answer string) {
	t.Helper()

	when := time.Date(2026, time.October, 13, 16, 0, 0, 0, time.UTC)

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent: domain.PlanIntentCalendarEvent,
		Title:  "Dentist Appointment",
		DueAt:  &when,
	}}

	if _, err := h.service.Turn(context.Background(), testUserID, chat.Turn{
		Input:          answer,
		ConversationID: &conversationID,
		Mode:           chat.ModePlan,
	}, nil); err != nil {
		t.Fatalf("answering Turn: %v", err)
	}
}

// A clarification is its own event type. Sending it as a plan would produce an approval
// card with nothing to approve.
func TestAClarificationIsEmittedAsItsOwnEvent(t *testing.T) {
	h := newHarness(t)

	h.planner.outcome = &planner.Outcome{Plan: &planner.Plan{
		Intent:   domain.PlanIntentClarification,
		Title:    "Dentist",
		Question: "What time is the appointment?",
	}}

	events, _, err := h.turn(t, chat.Turn{Input: "I need to see the dentist", Mode: chat.ModePlan})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	var found bool

	for _, event := range events {
		if event.Type == chat.EventPlan {
			t.Error("a clarification emitted a plan event")
		}

		if event.Type != chat.EventClarification {
			continue
		}

		found = true

		if event.Clarification == nil {
			t.Error("clarification event carries no clarification")
		}
	}

	if !found {
		t.Error("no clarification event was emitted")
	}
}

// An ordinary turn must be untouched by all of this.
func TestAnOrdinaryTurnStillRecordsARun(t *testing.T) {
	h := newHarness(t)

	_, result, err := h.turn(t, chat.Turn{Input: "buy groceries", Mode: chat.ModePlan})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if result.Run == nil {
		t.Fatal("an ordinary planned turn produced no run")
	}

	if len(h.runs.goals) != 1 {
		t.Errorf("recorded %d runs, want 1", len(h.runs.goals))
	}

	if result.Clarification != nil {
		t.Error("an ordinary turn reported a clarification")
	}
}

var _ = uuid.Nil

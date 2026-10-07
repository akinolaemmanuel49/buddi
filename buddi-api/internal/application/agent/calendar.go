package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/calendar"
	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// calendarArguments is the exact payload shown to the user for approval.
//
// It is a struct of strings rather than a map so the set of keys cannot grow at
// runtime, and it mirrors the connector's Event field for field. The connection
// between this type and that one is the approval contract: these bytes are what the
// user reads, and the connector decodes exactly these keys or refuses.
type calendarArguments struct {
	Summary     string   `json:"summary"`
	StartAt     string   `json:"start_at"`
	EndAt       string   `json:"end_at,omitempty"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Attendees   []string `json:"attendees,omitempty"`
	CalendarID  string   `json:"calendar_id,omitempty"`
}

// EncodeCalendarArguments renders a plan as calendar.create_event arguments.
//
// The mapping is deliberately narrow. Only the plan's own fields are read: a title
// becomes a summary, a due time becomes a start, and the steps become a description.
// Nothing else is available to copy, which is what keeps §10.2 true on this path
// rather than only on the connector's side — the connector refuses note content in
// its input, and there is no route by which such content could be put there.
//
// No time zone is filled in. The process's zone is the container's, which is
// almost never the user's, and guessing it would write the wrong instant into
// somebody's calendar while looking correct.
func EncodeCalendarArguments(plan *planner.Plan) (json.RawMessage, error) {
	if plan.DueAt == nil {
		// A calendar event with no time is not a decision the user can make by
		// approving it, so it is refused rather than defaulted. Leaving it out would
		// put an all-day event in their calendar that they never asked for.
		return nil, fmt.Errorf("agent: a calendar event needs a start time, and the plan has none")
	}

	arguments := calendarArguments{
		Summary:     strings.TrimSpace(plan.Title),
		StartAt:     plan.DueAt.UTC().Format(time.RFC3339),
		Description: describeSteps(plan),
	}

	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("could not encode tool arguments: %w", err)
	}

	// The connector's own decoder is run here so a payload can never reach the
	// approval screen in a shape the connector will refuse once the user approves it.
	if _, err := calendar.EventFromArguments(encoded); err != nil {
		return nil, fmt.Errorf("agent: calendar arguments are not executable: %w", err)
	}

	return encoded, nil
}

// CalendarBinding is the registry entry for creating a calendar event.
//
// Unlike TaskCreateBinding it is never the default. A plan this registry cannot
// recognise becomes a task, because writing to a third-party calendar is not
// something to fall back into.
func CalendarBinding(tool Tool) RegistryOption {
	return RegistryOption{
		Intent:    domain.PlanIntentCalendarEvent,
		Tool:      tool,
		Encode:    EncodeCalendarArguments,
		Rationale: calendarRationale,
	}
}

// calendarRationale says plainly that this leaves Buddi.
func calendarRationale(plan *planner.Plan) string {
	var builder strings.Builder

	builder.WriteString("Creates an event in your Google Calendar")

	if len(plan.Steps) > 0 {
		builder.WriteString(" describing these steps: ")

		steps := make([]string, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			steps = append(steps, step.Description)
		}

		builder.WriteString(strings.Join(steps, "; "))
	}

	return builder.String()
}

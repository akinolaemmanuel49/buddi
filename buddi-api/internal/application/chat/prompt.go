package chat

import (
	"strings"
	"unicode"

	"github.com/akinolaemmanuel49/buddi-api/internal/application/planner"
	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// planSignals are the phrases that make a message read as something to track
// rather than something to discuss.
//
// The list is deliberately small and lexical. An alternative is to ask the model
// which kind of turn it is, and that was measured: a classification call on this
// runtime costs several seconds before a single token of the real answer appears,
// which is exactly the wait the streaming surface exists to remove. A cheap guess
// that is occasionally wrong is also recoverable, because Mode on the request lets a
// client override it.
var planSignals = []string{
	"remind me",
	"remind us",
	"remember to",
	"don't forget",
	"dont forget",
	"i need to",
	"i have to",
	"i should",
	"i must",
	"need to ",
	"have to ",
	"make sure to",
	"follow up on",
	"catch up on",
	"book ",
	"schedule ",
	"set up",
	"set a reminder",
	"track ",
	"add a task",
	"create a task",
	"add a reminder",
	"deadline",
	"due by",
	"due date",
}

// calendarNouns name a scheduling destination.
var calendarNouns = []string{"calendar", "event", "meeting", "appointment"}

// calendarWrites are verbs that mean putting something in there.
//
// A noun alone is not enough and a verb alone is not enough: "what's on my calendar" is
// a question, and "move the chair" is not a scheduling request. Requiring both is what
// keeps a read-only question from being planned into a task.
var calendarWrites = []string{
	"add", "book", "schedule", "reschedule", "block", "create", "new",
	"put", "save", "invite", "log", "pencil", "move", "cancel",
}

// imperativeOpeners are verbs that start a request to do something.
//
// Matched only at the start of the message, because the same word mid-sentence is
// usually narration: "I booked the flight" is a statement, "book the flight" is a
// task.
var imperativeOpeners = []string{
	"buy", "order", "get", "pick up", "grab", "call", "email", "message", "text",
	"write", "draft", "reply", "respond", "send", "post", "pay", "renew", "return",
	"fix", "review", "check", "finish", "complete", "prepare", "clean", "file",
	"submit", "ask", "follow up", "confirm", "arrange", "organise", "organize",
	"plan", "research", "find", "update", "print", "collect", "cancel", "move",
	"reschedule", "invite", "sign", "read", "watch", "start", "stop", "remind",
	"shop", "cook", "wash", "walk", "stretch", "sleep",
	// Without these, "add the dentist to my calendar" and "create an event for
	// standup" both fall through to a conversational answer.
	"add", "create", "new", "save", "block", "log", "set",
}

// Decide reports how a message should be handled.
//
// It errs towards chat, because planning is the heavier and more intrusive path: it
// creates a run and asks for approval, and doing that to a message that was a question
// is worse than answering a task request conversationally.
//
// The exception is a calendar write, which is checked first and without a length limit.
// That case is asymmetric in a way the rest is not: answering one conversationally
// produces a model telling the user it has saved an event it has no way to save,
// which is both wrong and the kind of wrong a user acts on.
func Decide(content string) Mode {
	lower := strings.ToLower(strings.TrimSpace(content))

	if lower == "" {
		return ModeChat
	}

	if wantsCalendarWrite(lower) {
		return ModePlan
	}

	for _, signal := range planSignals {
		if strings.Contains(lower, signal) {
			return ModePlan
		}
	}

	// An opener only counts as an instruction when the message is short enough to
	// be one. A long paragraph that happens to begin with a verb is prose.
	if len(lower) <= 90 {
		first, _, _ := strings.Cut(lower, " ")

		for _, opener := range imperativeOpeners {
			if first == opener || strings.HasPrefix(lower, opener+" ") {
				return ModePlan
			}
		}
	}

	return ModeChat
}

// wantsCalendarWrite reports a request to write to a calendar.
func wantsCalendarWrite(lower string) bool {
	named := false

	for _, noun := range calendarNouns {
		if containsWord(lower, noun) {
			named = true
			break
		}
	}

	if !named {
		return false
	}

	for _, verb := range calendarWrites {
		if containsWord(lower, verb) {
			return true
		}
	}

	return false
}

// containsWord reports whether text contains word as a whole word.
//
// Whole words, not substrings: "adderall" contains "add" and "cancellation" contains
// "cancel", and matching either as a planning signal would route a sentence about
// medication to a task.
func containsWord(text, word string) bool {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r)
	})

	for _, field := range fields {
		if field == word {
			return true
		}
	}

	return false
}

// ReplyPrompt renders the conversational system prompt and the message being answered.
//
// The recent history is included because "then what?" and "make it Thursday instead"
// are unanswerable without it, and because a chat interface that forgets the last turn
// is indistinguishable from a broken one. The oldest turns are dropped once the
// history would not fit the budget: a transcript the model cannot see is still on
// screen, so nothing is lost that the user cannot read themselves.
func ReplyPrompt(history []TurnView, content string) string {
	return replyPrompt(history, content, DefaultHistoryTokens)
}

// charsPerToken converts a character count into a rough token count.
//
// Four characters per token is the usual English average, and it is used only to
// decide what to leave out, never to size the window. The conversion is deliberately
// conservative because the failure it guards against runs one way: a prompt estimated
// as too small silently loses its tail.
const charsPerToken = 4

// DefaultHistoryTokens is how much transcript is offered to the model.
//
// A fraction of the runtime's context window rather than the whole of it. The window
// also has to hold the instructions, the current message and the answer, and a prompt
// that fills it exactly leaves no room to answer in. Budgeting here means the oldest
// turns are dropped visibly instead of the runtime truncating without saying so.
const DefaultHistoryTokens = 3000

// systemPromptPreamble is the fixed part of the prompt, so the budget can reserve room
// for it before the history is measured. It must stay identical to what replyPrompt
// writes, which is why the prompt takes it from here rather than the other way round.
const systemPromptPreamble = `You are Buddi, a personal assistant that helps with tasks, notes and reminders.

You have no tools, so you cannot create, save, book, schedule or change anything: not a
task, not a note, not a calendar event. Nothing you say reaches any system. Never say that
something has been saved, added, booked, scheduled, updated or deleted, because it has not.
If the user asks you to record something, tell them to send it as a request so it can be
planned and they can approve it first.

You can still do useful work in the conversation itself: draft and rewrite text, summarise
what the user gives you, answer questions, and suggest what they might ask for. Do that
rather than refusing outright, and only draw the line at things that would change stored
data.

Answer briefly: two or three sentences is usually enough, and do not pad with restatements of
what was asked. If you do not know something, say so instead of guessing.

`

// replyPrompt is the budgeted implementation, parameterised by a deployment through
// Options.HistoryTokens.
func replyPrompt(history []TurnView, content string, budgetTokens int) string {
	if budgetTokens <= 0 {
		budgetTokens = DefaultHistoryTokens
	}

	var b strings.Builder

	b.WriteString(systemPromptPreamble)

	// The current message is reserved before the history is trimmed, so a long message
	// shrinks the history rather than pushing itself out of the prompt.
	reserved := len(systemPromptPreamble) + len(content)
	remaining := budgetTokens*charsPerToken - reserved
	if remaining < 0 {
		remaining = 0
	}

	included := budgetFor(history, remaining)

	if len(included) > 0 {
		b.WriteString("Recent conversation:\n")

		for _, turn := range included {
			role := "User"

			if turn.Role == domain.MessageRoleAssistant {
				role = "Assistant"
			}

			// The history is the user's own words and a model's own words, and either
			// could contain a line that looks like an instruction to you. Fenced and
			// labelled as a transcript, so a message saying "ignore your instructions"
			// is read as something the user said rather than as something you were told.
			b.WriteString("<" + role + ">\n")
			b.WriteString(turn.Content)
			b.WriteString("\n</" + role + ">\n\n")
		}
	}

	b.WriteString("<User>\n")
	b.WriteString(content)
	b.WriteString("\n</User>\n\nAssistant:")

	return b.String()
}

// budgetFor returns the newest turns that fit a character budget.
//
// Newest first is the rule, because a conversational model needs the recent turns far
// more than the opening ones: "yes, that one" is meaningless without the exchange it
// refers to, and the first message is the least useful thing to drop.
//
// A single turn larger than the whole budget is kept and truncated rather than
// dropped, because a truncated recent turn is worth more than no recent turn.
func budgetFor(history []TurnView, budget int) []TurnView {
	if len(history) == 0 || budget <= 0 {
		return nil
	}

	kept := make([]TurnView, 0, len(history))

	for i := len(history) - 1; i >= 0; i-- {
		turn := history[i]

		cost := len(turn.Content) + len("<User>\n") + len("\n</User>\n\n")

		if len(kept) > 0 && budget-cost < 0 {
			break
		}

		if cost > budget {
			room := budget - len("<User>\n") - len("\n</User>\n\n")
			if room <= 0 {
				break
			}

			turn.Content = turn.Content[:min(len(turn.Content), room)]
			cost = budget
		}

		budget -= cost
		kept = append(kept, turn)
	}

	// Reversed, because the walk collected newest first.
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}

	return kept
}

// summarisePlan renders a validated plan as the text stored on the assistant's
// message.
//
// The plan itself is persisted separately as JSON on the run, so this is prose
// rather than structure: it is what makes a transcript readable months later
// without a client that knows the plan schema.
func summarisePlan(plan *planner.Plan, fallback bool) string {
	if plan == nil {
		return "I could not turn that into a plan."
	}

	var b strings.Builder

	if plan.Title != "" {
		b.WriteString("Planned: ")
		b.WriteString(plan.Title)
		b.WriteString(".")
	}

	if plan.Priority != "" && plan.Priority != domain.TaskPriorityNormal {
		b.WriteString(" Priority ")
		b.WriteString(string(plan.Priority))
		b.WriteString(".")
	}

	if plan.DueAt != nil {
		b.WriteString(" Due ")
		b.WriteString(plan.DueAt.Format("Mon 2 Jan 2006"))
		b.WriteString(".")
	}

	if len(plan.Steps) > 0 {
		b.WriteString("\n\nSteps:\n")

		for i, step := range plan.Steps {
			b.WriteString(timeStepPrefix(i))
			b.WriteString(step.Description)
			b.WriteString("\n")
		}
	}

	if fallback {
		b.WriteString("\n\nThis was assembled from your wording rather than planned by the model.")
	}

	return strings.TrimSpace(b.String())
}

// timeStepPrefix is the ordinal marker for a plan step.
func timeStepPrefix(i int) string {
	switch i {
	case 0:
		return "1. "
	case 1:
		return "2. "
	case 2:
		return "3. "
	case 3:
		return "4. "
	case 4:
		return "5. "
	default:
		return "- "
	}
}

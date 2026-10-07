package planner

import (
	"fmt"
	"strings"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

// This file decides which tool should carry a request, from a table that can be read
// in one sitting.
//
// It exists because the model does not route reliably on its own, and the cost of
// getting it wrong is not symmetric. "Buy groceries" becoming a calendar event puts
// an event in the user's real calendar for something that is not one. The model,
// asked to choose from an enum, had no reason to prefer the humble answer, and the
// enum's own wording made task sound like the catch-all.
//
// So the table is here, in code, where it can be tested against the cases that
// actually broke. The model still fills in the plan — title, time, detail, steps —
// because that is genuinely a judgement call. It just does not get to decide which
// tool it is writing to.

// RouteRule is one row of the table.
//
// A rule matches when any of its Keywords appears in the request. The first
// matching row wins, so the table is ordered from most specific to least specific
// and a more specific keyword listed earlier is not shadowed by a broader one later.
type RouteRule struct {
	// Name identifies the row in a note or a test failure.
	Name string

	// Intent is the tool family this kind of request belongs to.
	Intent domain.PlanIntent

	// Keywords are matched as whole words against the lowercased request.
	Keywords []string

	// NeedsTime means this kind of request cannot be carried out without the user
	// stating when. A matching request with no time in it becomes a clarification
	// rather than an event, because the one field that makes it an event is the one
	// we would have to invent.
	NeedsTime bool
}

// routeTable is ordered most specific first.
//
// Errands are listed before appointments on purpose. "Pick up the prescription" and
// "book the dentist" are both things a person does, but only one of them is an event,
// and a keyword like "book" appearing in an errand sentence should not promote it.
var routeTable = []RouteRule{
	{
		Name:   "errand",
		Intent: domain.PlanIntentTask,
		Keywords: []string{
			"groceries", "grocery", "shopping", "buy", "purchase",
			"laundry", "washing", "iron", "tidy", "clean", "vacuum",
			"refill", "post", "mail", "return", "recycle",
		},
	},
	{
		Name:     "errand-verb",
		Intent:   domain.PlanIntentTask,
		Keywords: []string{"pick up", "drop off", "collect", "restock", "stock up"},
	},
	{
		Name:   "appointment",
		Intent: domain.PlanIntentCalendarEvent,
		Keywords: []string{
			"dentist", "doctor", "appointment", "meeting", "booking", "book",
			"haircut", "salon", "barber", "physio", "therapy", "optician",
			"interview", "standup", "retro", "workshop", "lesson", "class",
			"flight", "hotel", "reservation",
			"coffee", "lunch", "brunch", "breakfast", "dinner", "drinks",
		},
		NeedsTime: true,
	},
}

// checkRoute compares what the model chose against what the table says.
//
// It returns an empty string when the plan is acceptable, and otherwise a complaint
// naming the rule that was violated, because the complaint goes back into the retry
// prompt and a vague one just produces the same answer again.
func checkRoute(route Route, plan *Plan) string {
	if plan == nil {
		return ""
	}

	// The table matched but the request never said when. Asking is the correct answer
	// and anything else means inventing the one field that makes it an event.
	if route.NeedsClarification {
		if plan.Intent != domain.PlanIntentClarification {
			return fmt.Sprintf(
				"this request is a %s but never says when it happens, so intent must be "+
					"clarification and question must ask when it is: do not invent a time",
				route.Matched)
		}

		return ""
	}

	// A question nobody needed is its own failure. The user asked for something
	// actionable and is being handed a follow-up instead.
	if plan.Intent == domain.PlanIntentClarification && route.Confident {
		return fmt.Sprintf(
			"this request is already a %s and has everything it needs, so it must be "+
				"intent %s rather than a clarification",
			route.Matched, route.Intent)
	}

	// An unmatched request is left to the model. Overriding it here would mean every
	// unlisted request was forced into whichever rule happened to be longest.
	if !route.Confident {
		return ""
	}

	if plan.Intent != route.Intent {
		return fmt.Sprintf(
			"intent must be %s because %s, not %s",
			route.Intent, route.Reason, plan.Intent)
	}

	return ""
}

// missingTimeQuestion is the fallback question when the model could not be made to
// produce one.
//
// It is built from the request's own words rather than being a generic string, so the
// user can tell which request is being asked about in a conversation with more than
// one thing in it.
func missingTimeQuestion(goal string) string {
	subject := strings.TrimSpace(goal)

	if len(subject) > 120 {
		subject = truncateOnWordBoundary(subject, 120)
	}

	return fmt.Sprintf("What time should I set %q for?", subject)
}

// Route is the outcome of consulting the table.
type Route struct {
	// Intent is what the table says this request is.
	Intent domain.PlanIntent

	// Matched is the rule that decided it, empty when nothing matched.
	Matched string

	// Confident is false when no rule matched and the model was left to choose. The
	// difference matters: an unmatched request is genuinely ambiguous, and claiming
	// otherwise would mean a rule firing on everything.
	Confident bool

	// NeedsClarification is set when a rule matched but a field the request would
	// have to invent was missing from it. The intent stays as the rule's, so the
	// question can be about the right thing.
	NeedsClarification bool

	// Reason explains the decision in one line, for the prompt and for tests.
	Reason string
}

// RouteRequest applies the table to a request.
//
// It is deliberately total: every request gets an answer, and a request nothing
// matched is reported as not confident rather than being forced into the first row.
func RouteRequest(goal string) Route {
	text := lowerASCII(goal)

	for _, rule := range routeTable {
		if !rule.matches(text) {
			continue
		}

		if !rule.NeedsTime || hasTimeSignal(text) {
			return Route{
				Intent:    rule.Intent,
				Matched:   rule.Name,
				Confident: true,
				Reason:    "the request is a " + rule.Name,
			}
		}

		// The rule matched but the request never said when. This is the case that
		// matters: it would be an event, and the start time is the field we would have
		// to invent, and inventing it writes the wrong day into somebody's calendar.
		return Route{
			Intent:             rule.Intent,
			Matched:            rule.Name,
			Confident:          true,
			NeedsClarification: true,
			Reason:             "the request is a " + rule.Name + " but never says when it is",
		}
	}

	return Route{
		Intent:    domain.PlanIntentTask,
		Confident: false,
		Reason:    "nothing in the request matched a known kind of request",
	}
}

// matches reports whether any of the rule's keywords appears in the text.
func (r RouteRule) matches(text string) bool {
	for _, keyword := range r.Keywords {
		if containsWord(text, keyword) {
			return true
		}
	}

	return false
}

// timeSignals are the words that mean the request states a time.
var timeSignals = []string{
	"today", "tomorrow", "tonight", "morning", "afternoon", "evening",
	"noon", "midday", "midnight", "weekend", "weekday",
	"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
	"january", "february", "march", "april", "may", "june",
	"july", "august", "september", "october", "november", "december",
}

// hasTimeSignal reports whether the request states when something happens.
//
// It looks for a weekday or a month by name, a relative day, or a clock reading.
// The clock reading is the important one: "dentist at 3pm" names no weekday at all,
// and a check that only looked for weekdays would call that missing and ask a
// pointless question.
func hasTimeSignal(text string) bool {
	for _, signal := range timeSignals {
		if containsWord(text, signal) {
			return true
		}
	}

	// A bare number is not enough. "3pm", "15:00", "at 9" are times; "3 people" and
	// "buy 2 milk" are not. The number has to be attached to a time word or a colon.
	if strings.Contains(text, ":") {
		return true
	}

	for i := 0; i < len(text); i++ {
		if !isDigit(text[i]) {
			continue
		}

		// The suffix is checked from just after the digit: in "3pm" the text at the
		// digit is "3pm", and testing for a "pm" prefix there never matches.
		rest := text[i+1:]

		for _, suffix := range []string{"pm", "am", "oclock", "hrs", "hours", "h"} {
			if strings.HasPrefix(rest, suffix) {
				return true
			}
		}
	}

	// A date with slashes is a time signal even without a month name.
	if strings.Contains(text, "/") && anyDigit(text) {
		return true
	}

	return false
}

// containsWord reports whether needle appears in haystack delimited by non-word
// characters.
//
// Whole-word matching matters here: "may" is a month and also a modal verb, and
// matching it inside "maybe" would turn an unremarkable sentence into an event.
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}

	offset := 0

	for {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return false
		}

		start := offset + index
		end := start + len(needle)

		if wordBoundaryBefore(haystack, start) && wordBoundaryAfter(haystack, end) {
			return true
		}

		offset = start + 1
	}
}

func wordBoundaryBefore(haystack string, index int) bool {
	if index == 0 {
		return true
	}

	return !isWordByte(haystack[index-1])
}

func wordBoundaryAfter(haystack string, index int) bool {
	if index >= len(haystack) {
		return true
	}

	return !isWordByte(haystack[index])
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func anyDigit(text string) bool {
	for i := 0; i < len(text); i++ {
		if isDigit(text[i]) {
			return true
		}
	}

	return false
}

// lowerASCII lowercases ASCII letters without dragging in Unicode case folding.
//
// The matching vocabulary is plain ASCII, and Unicode folding can change a string's
// length, which would make an index-based word boundary check wrong in a way that is
// very hard to see.
func lowerASCII(s string) string {
	var b strings.Builder

	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		c := s[i]

		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}

		b.WriteByte(c)
	}

	return b.String()
}

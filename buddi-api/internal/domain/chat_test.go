package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akinolaemmanuel49/buddi-api/internal/domain"
)

func TestNewConversationNeedsAnOwner(t *testing.T) {
	if _, err := domain.NewConversation(uuid.Nil, time.Now()); err == nil {
		t.Error("NewConversation accepted a nil user id")
	}
}

func TestSetTitleIgnoresBlank(t *testing.T) {
	conversation, err := domain.NewConversation(uuid.New(), time.Now())
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}

	if conversation.DerivedTitle() != "New conversation" {
		t.Errorf("untitled conversation title = %q", conversation.DerivedTitle())
	}

	conversation.SetTitle("   ")
	if conversation.Title != nil {
		t.Errorf("blank title was stored as %q", *conversation.Title)
	}

	conversation.SetTitle("  Buy milk  ")
	if conversation.Title == nil || *conversation.Title != "Buy milk" {
		t.Errorf("Title = %v, want the trimmed value", conversation.Title)
	}
}

func TestNewMessageRejectsUnusableInput(t *testing.T) {
	now := time.Now()
	conversationID := uuid.New()
	userID := uuid.New()

	cases := map[string]struct {
		conversationID uuid.UUID
		userID         uuid.UUID
		role           domain.MessageRole
		content        string
	}{
		"no conversation": {uuid.Nil, userID, domain.MessageRoleUser, "hello"},
		"no user":         {conversationID, uuid.Nil, domain.MessageRoleUser, "hello"},
		"unknown role":    {conversationID, userID, domain.MessageRole("system"), "hello"},
		"empty content":   {conversationID, userID, domain.MessageRoleUser, "   "},
		"overlong": {conversationID, userID, domain.MessageRoleUser,
			strings.Repeat("x", domain.MaxMessageContentLength+1)},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := domain.NewMessage(
				tc.conversationID, tc.userID, tc.role, tc.content, nil, now,
			); err == nil {
				t.Error("NewMessage accepted unusable input")
			}
		})
	}
}

func TestWithReasoningDistinguishesAbsentFromBlank(t *testing.T) {
	message, err := domain.NewMessage(uuid.New(), uuid.New(), domain.MessageRoleAssistant, "hi", nil, time.Now())
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	if message.Reasoning != nil {
		t.Errorf("Reasoning = %v, want nil before the model said anything", message.Reasoning)
	}

	// Blank reasoning must not become an empty string, or a reader cannot tell a
	// model that explained nothing from one whose explanation was whitespace.
	message.WithReasoning("   ")
	if message.Reasoning != nil {
		t.Errorf("Reasoning = %v, want nil for blank reasoning", *message.Reasoning)
	}

	message.WithReasoning("  weighing options  ")
	if message.Reasoning == nil || *message.Reasoning != "weighing options" {
		t.Errorf("Reasoning = %v, want the trimmed text", message.Reasoning)
	}
}

// An edit has to leave the original readable and move the branch, which is the
// whole reason it is a new row rather than an update.
func TestAsEditSupersedesTheOriginalAndKeepsItsPosition(t *testing.T) {
	parentID := uuid.New()
	original, err := domain.NewMessage(uuid.New(), uuid.New(), domain.MessageRoleUser, "buy milk", &parentID, time.Now())
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	edited, err := original.AsEdit("buy oat milk", original.CreatedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("AsEdit: %v", err)
	}

	if edited.ID == original.ID {
		t.Error("the edit reused the original's id, so it overwrote it")
	}

	if edited.SupersededMessageID == nil || *edited.SupersededMessageID != original.ID {
		t.Errorf("SupersededMessageID = %v, want the original's id", edited.SupersededMessageID)
	}

	// The edit sits where the original was, not after it, or the correction would
	// read as a new turn instead of a revision of the same one.
	if edited.ParentID == nil || *edited.ParentID != parentID {
		t.Errorf("ParentID = %v, want the original's parent", edited.ParentID)
	}

	if edited.Content != "buy oat milk" {
		t.Errorf("Content = %q, want the corrected text", edited.Content)
	}

	// The original must survive: it is the record of what was actually asked.
	if original.Content != "buy milk" {
		t.Errorf("original Content = %q, want it unchanged", original.Content)
	}
}

func TestDeriveTitle(t *testing.T) {
	cases := map[string]struct {
		content string
		want    string
	}{
		"first line only":   {"Buy milk\nalso buy bread", "Buy milk"},
		"single short line": {"Buy milk", "Buy milk"},
		"blank":             {"   ", ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := domain.DeriveTitle(tc.content); got != tc.want {
				t.Errorf("DeriveTitle(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}

	long := strings.Repeat("word ", 40)

	got := domain.DeriveTitle(long)

	if len(got) > 60 {
		t.Errorf("title is %d characters, want at most 60", len(got))
	}

	if strings.HasSuffix(got, "wor") {
		t.Errorf("title %q ends mid-word", got)
	}
}

func TestMessageRoleIsValid(t *testing.T) {
	for _, role := range domain.AllMessageRoles {
		if !role.IsValid() {
			t.Errorf("%q reported invalid", role)
		}
	}

	if domain.MessageRole("tool").IsValid() {
		t.Error("an unknown role reported valid")
	}
}

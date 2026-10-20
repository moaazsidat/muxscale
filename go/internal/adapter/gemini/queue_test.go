package gemini

import (
	"strings"
	"testing"
)

// The layout of Gemini CLI's QueuedMessageDisplay (paddingLeft 2 for the
// heading, 4 for messages, three shown, the rest counted), inside a screen.
func TestQueueFromScreen(t *testing.T) {
	screen := strings.Join([]string{
		"✦ Working on it",
		"",
		"  Queued (press ↑ to edit):",
		"    Message 1",
		"    Message with multiple whitespace",
		"    Message 3",
		"    ... (+2 more)",
		"",
		"╭────────────────────────────╮",
		"│ >   Type your message       │",
	}, "\n")
	got := New().QueueFromScreen(screen)
	if len(got) != 5 {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	if got[1].Text != "Message with multiple whitespace" || !got[1].Partial || got[1].Delivery != "queue" {
		t.Errorf("second = %+v", got[1])
	}
	if got[3].Text != "" || got[4].Text != "" {
		t.Errorf("counted ones should have no text: %+v", got[3:])
	}

	if got := New().QueueFromScreen("✦ Done\n│ >   Type your message │\n"); got == nil || len(got) != 0 {
		t.Errorf("no queue = %+v", got)
	}
}

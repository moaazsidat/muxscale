package pi

import (
	"strings"
	"testing"
)

func TestQueueFromScreen(t *testing.T) {
	screen := strings.Join([]string{
		" Steering: this line is conversation, not the queue",
		"",
		" working on it",
		"",
		" Steering: look at the failing test first",
		" Follow-up: then open a PR",
		" ↳ Alt+Up to edit all queued messages",
		"────────────────",
		" > ",
	}, "\n")
	got := New().QueueFromScreen(screen)
	if len(got) != 2 || got[0].Text != "look at the failing test first" || got[0].Delivery != "steer" ||
		got[1].Text != "then open a PR" || got[1].Delivery != "queue" || !got[1].Partial {
		t.Fatalf("got %+v", got)
	}
	if got := New().QueueFromScreen(" working on it\n > \n"); got == nil || len(got) != 0 {
		t.Errorf("none = %+v", got)
	}
}

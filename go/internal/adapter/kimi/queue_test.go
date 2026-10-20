package kimi

import (
	"strings"
	"testing"
)

func TestQueueFromScreen(t *testing.T) {
	screen := strings.Join([]string{
		"  ❯ a menu item from earlier, not the queue",
		"",
		"  working…",
		"────────────────────────────",
		"  ❯ look at the failing test first",
		"  ❯ $ make test",
		"  ↑ to edit · ctrl-s to steer immediately",
		"╭──────────────────────────╮",
	}, "\n")
	got := New().QueueFromScreen(screen)
	if len(got) != 2 || got[0].Text != "look at the failing test first" || got[1].Text != "$ make test" || !got[0].Partial {
		t.Fatalf("got %+v", got)
	}
	if got := New().QueueFromScreen("  working…\n╭──╮\n"); got == nil || len(got) != 0 {
		t.Errorf("none = %+v", got)
	}
}

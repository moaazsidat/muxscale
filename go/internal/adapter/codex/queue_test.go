package codex

import (
	"strings"
	"testing"
)

// Codex's own snapshots of its queue preview
// (codex-rs/tui/src/bottom_pane/snapshots/*pending_input_preview*), set inside
// a screen the way it appears: transcript above, composer below.
func TestQueueFromScreenMatchesCodexSnapshots(t *testing.T) {
	screen := strings.Join([]string{
		"• Ran ls",
		"  └ README.md",
		"",
		"• Messages to be submitted after next tool call     ",
		"  (press esc to interrupt and send immediately)     ",
		"  ↳ Please continue.                                ",
		"  ↳ Check the last command output.                  ",
		"                                                    ",
		"• Messages to be submitted at end of turn           ",
		"  ↳ Rejected steer that will be retried.            ",
		"                                                    ",
		"• Queued follow-up inputs                           ",
		"  ↳ This is a longer message that should",
		"    be wrapped                          ",
		"  ↳ This is                             ",
		"    a message                           ",
		"    with many                           ",
		"    …                                   ",
		"    ⇧← edit last queued message         ",
		"",
		"› Ask Codex to do anything",
	}, "\n")
	got := New().QueueFromScreen(screen)
	want := []struct{ text, delivery string }{
		{"Please continue.", "steer"},
		{"Check the last command output.", "steer"},
		{"Rejected steer that will be retried.", "queue"},
		{"This is a longer message that should be wrapped", "queue"},
		{"This is a message with many …", "queue"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Text != w.text || got[i].Delivery != w.delivery || !got[i].Partial {
			t.Errorf("%d = %+v, want %q %s", i, got[i], w.text, w.delivery)
		}
	}
}

func TestQueueFromScreenEmpty(t *testing.T) {
	got := New().QueueFromScreen("• Ran ls\n  └ README.md\n\n› Ask Codex to do anything\n")
	if got == nil || len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

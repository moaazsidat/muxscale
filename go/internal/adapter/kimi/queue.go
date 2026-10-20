package kimi

import (
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Kimi Code keeps messages typed while it works in its TUI's memory
// (queuedMessages; nothing reaches the wire file until a turn takes one) and
// draws them above the editor, from apps/kimi-code/src/tui/components/panes/
// queue-pane.ts:
//
//	────────────────────────────
//	  ❯ look at the failing test first
//	  ❯ $ make test
//	  ↑ to edit · ctrl-s to steer immediately
//
// One line each, whitespace collapsed and cut to the screen, so what is read
// is a preview (Partial). The hint anchors the block.

// QueueFromScreen reads the queue Kimi Code draws.
func (*Adapter) QueueFromScreen(screen string) []protocol.QueuedMessage {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "↑ to edit · ") {
			continue
		}
		var out []protocol.QueuedMessage
		for j := i - 1; j >= 0; j-- {
			line := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(line, "❯ ") {
				break // the rule above the pane, or anything else
			}
			out = append([]protocol.QueuedMessage{{
				Text: strings.TrimSpace(strings.TrimPrefix(line, "❯ ")), Delivery: "queue", Partial: true,
			}}, out...)
		}
		if out == nil {
			out = []protocol.QueuedMessage{}
		}
		return out
	}
	return []protocol.QueuedMessage{}
}

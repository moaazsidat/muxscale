package pi

import (
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// pi keeps steering and follow-up messages in memory and draws them above its
// editor, one line each, from updatePendingMessagesDisplay in
// packages/coding-agent/src/modes/interactive/interactive-mode.ts:
//
//	 Steering: look at the failing test first
//	 Follow-up: then open a PR
//	 ↳ Alt+Up to edit all queued messages
//
// Each line is cut to the screen's width (TruncatedText), so what is read is a
// preview (Partial). The hint line anchors the block, so a "Steering:" in the
// conversation above is never mistaken for one.

// QueueFromScreen reads the queue pi draws.
func (*Adapter) QueueFromScreen(screen string) []protocol.QueuedMessage {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		hint := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(hint, "↳ ") || !strings.HasSuffix(hint, "to edit all queued messages") {
			continue
		}
		var out []protocol.QueuedMessage
		for j := i - 1; j >= 0; j-- {
			line := strings.TrimSpace(lines[j])
			var m protocol.QueuedMessage
			switch {
			case strings.HasPrefix(line, "Steering: "):
				m = protocol.QueuedMessage{Text: strings.TrimPrefix(line, "Steering: "), Delivery: "steer"}
			case strings.HasPrefix(line, "Follow-up: "):
				m = protocol.QueuedMessage{Text: strings.TrimPrefix(line, "Follow-up: "), Delivery: "queue"}
			default:
				j = -1
				continue
			}
			m.Partial = true
			out = append([]protocol.QueuedMessage{m}, out...)
		}
		if out == nil {
			out = []protocol.QueuedMessage{}
		}
		return out
	}
	return []protocol.QueuedMessage{}
}

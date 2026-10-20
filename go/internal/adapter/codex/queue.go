package codex

import (
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Codex keeps what a person types while it works in memory only -- nothing
// about it reaches the rollout -- but draws it above the composer. The layout
// is codex-rs/tui/src/bottom_pane/pending_input_preview.rs, pinned by its
// snapshot tests:
//
//	• Messages to be submitted after next tool call
//	  (press esc to interrupt and send immediately)
//	  ↳ Please continue.
//
//	• Messages to be submitted at end of turn
//	  ↳ Rejected steer that will be retried.
//
//	• Queued follow-up inputs
//	  ↳ This is a longer message that should
//	    be wrapped
//	    …
//	    ⇧← edit last queued message
//
// Each message shows at most three lines, wrapped to the screen, so what is
// read here is a preview (Partial).

var codexSections = []struct {
	title    string
	delivery string
}{
	{"Messages to be submitted after next tool call", "steer"},
	{"Messages to be submitted at end of turn", "queue"},
	{"Queued follow-up inputs", "queue"},
}

// QueueFromScreen reads the queue Codex draws.
func (*Adapter) QueueFromScreen(screen string) []protocol.QueuedMessage {
	out := []protocol.QueuedMessage{}
	lines := strings.Split(screen, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " ")
		indent := strings.Index(line, "• ")
		if indent < 0 || strings.TrimSpace(line[:indent]) != "" {
			continue
		}
		delivery := ""
		for _, s := range codexSections {
			if strings.HasPrefix(line[indent+len("• "):], s.title) {
				delivery = s.delivery
			}
		}
		if delivery == "" {
			continue
		}
		pad := strings.Repeat(" ", indent)
		var cur *protocol.QueuedMessage
		flush := func() {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
		}
		for i+1 < len(lines) {
			next := strings.TrimRight(lines[i+1], " ")
			if !strings.HasPrefix(next, pad+"  ") {
				break // a blank line or the next block ends the section
			}
			i++
			rest := next[len(pad):]
			switch {
			case strings.HasPrefix(rest, "  ↳ "):
				flush()
				cur = &protocol.QueuedMessage{
					Text: strings.TrimSpace(rest[len("  ↳ "):]), Delivery: delivery, Partial: true,
				}
			case cur == nil:
				// The header's own wrapped tail, before the first message.
			case strings.Contains(rest, "edit last queued message"):
			case strings.TrimSpace(rest) == "…":
				cur.Text += " …"
			default:
				cur.Text += " " + strings.TrimSpace(rest)
			}
		}
		flush()
	}
	return out
}

package gemini

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Gemini CLI holds messages typed while it works in memory (its messageQueue)
// and draws them above the composer, from
// packages/cli/src/ui/components/QueuedMessageDisplay.tsx:
//
//	  Queued (press ↑ to edit):
//	    First message
//	    Second message
//	    Third message
//	    ... (+2 more)
//
// Each message is one line with its whitespace collapsed and its end cut to
// the screen, and only the first three are drawn. So what is read here is a
// preview (Partial), and the ones only counted come back without text.

var moreRE = regexp.MustCompile(`^\.\.\. \(\+(\d+) more\)$`)

// QueueFromScreen reads the queue Gemini CLI draws.
func (*Adapter) QueueFromScreen(screen string) []protocol.QueuedMessage {
	out := []protocol.QueuedMessage{}
	lines := strings.Split(screen, "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, " ")
		at := strings.Index(line, "Queued (press ")
		if at < 0 || strings.TrimSpace(line[:at]) != "" || !strings.HasSuffix(line, "to edit):") {
			continue
		}
		item := strings.Repeat(" ", at+2)
		out = out[:0]
		for _, next := range lines[i+1:] {
			next = strings.TrimRight(next, " ")
			if !strings.HasPrefix(next, item) || strings.TrimSpace(next) == "" {
				break
			}
			text := strings.TrimSpace(next)
			if m := moreRE.FindStringSubmatch(text); m != nil {
				n, _ := strconv.Atoi(m[1])
				for range min(n, 50) {
					out = append(out, protocol.QueuedMessage{Delivery: "queue", Partial: true})
				}
				break
			}
			out = append(out, protocol.QueuedMessage{Text: text, Delivery: "queue", Partial: true})
		}
	}
	return out
}

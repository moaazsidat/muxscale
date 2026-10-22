package claudecode

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Claude Code's AskUserQuestion dialog, once a hook has let it through to the
// terminal, takes its answer from the keyboard. Checked against Claude Code
// 2.1.308 by typing into live dialogs:
//
//   - One single-choice question: a digit picks that option and submits.
//   - Otherwise each question is a tab, then a review tab. On a single-choice
//     question a digit picks and moves to the next tab; on a multi-choice one
//     digits toggle without moving the cursor off the first row, and Tab moves
//     on. The review tab's 1 is "Submit answers".
//   - The option after the last is "Type something". On a single-choice
//     question its digit opens the field; the text goes in, and Enter takes it
//     and moves on. On a multi-choice question its digit only ticks it: the
//     field takes text once the cursor is on its row, Tab then moves to the
//     question's own Submit row, and Enter moves on.
//   - Esc dismisses the dialog.
//
// Each step is typed on its own: a digit and the text after it, sent as one
// burst, read as a paste.

const (
	keyDown   = "\x1b[B"
	keyTab    = "\t"
	keyEnter  = "\r"
	keyCancel = "\x1b"
)

// ApprovalKeySteps returns Claude Code's keys for an answer to its question.
func (*Adapter) ApprovalKeySteps(req protocol.ApprovalRequest, d protocol.ApprovalDecision) ([][]byte, error) {
	if req.Kind != "question" {
		return nil, fmt.Errorf("Claude Code's permission prompts are answered through its hook, not typed")
	}
	if d.Behavior == "deny" {
		return [][]byte{[]byte(keyCancel)}, nil
	}
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("the request has no questions")
	}
	var steps []string
	review := len(req.Questions) > 1
	for _, q := range req.Questions {
		if q.MultiSelect {
			review = true
		}
		other := len(q.Options) + 1
		if other > 9 {
			return nil, fmt.Errorf("muxscale cannot type an answer to a question with %d options; answer it at the terminal", len(q.Options))
		}
		answer := strings.TrimSpace(d.Answers[q.Question])
		if answer == "" {
			return nil, fmt.Errorf("no answer for %q", q.Question)
		}
		picked, text := splitAnswer(q, answer)
		if !q.MultiSelect {
			if len(picked) == 1 && text == "" {
				steps = append(steps, strconv.Itoa(picked[0]))
				continue
			}
			// Not one of the options: the person's own words.
			steps = append(steps, strconv.Itoa(other), oneLine(answer), keyEnter)
			continue
		}
		for _, i := range picked {
			steps = append(steps, strconv.Itoa(i))
		}
		if text == "" {
			steps = append(steps, keyTab)
			continue
		}
		steps = append(steps, strconv.Itoa(other))
		for range q.Options {
			steps = append(steps, keyDown)
		}
		steps = append(steps, oneLine(text), keyTab, keyEnter)
	}
	if review {
		steps = append(steps, "1") // Submit answers
	}
	out := make([][]byte, len(steps))
	for i, s := range steps {
		out[i] = []byte(s)
	}
	return out, nil
}

// splitAnswer reads an answer as the phone writes it -- the options chosen,
// in the order offered, then any words of the person's own, joined with ", "
// -- into the 1-based numbers of the options and the words. Labels are
// matched from the front rather than split on commas, since a label may hold
// one.
func splitAnswer(q protocol.Question, answer string) (picked []int, text string) {
	rest := answer
	for i, o := range q.Options {
		if o.Label == "" {
			continue
		}
		if rest == o.Label {
			return append(picked, i+1), ""
		}
		if strings.HasPrefix(rest, o.Label+", ") {
			picked = append(picked, i+1)
			rest = rest[len(o.Label)+2:]
		}
	}
	return picked, rest
}

// oneLine flattens text for a one-line field, where Enter would submit
// partway through.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

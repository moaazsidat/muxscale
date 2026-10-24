package omp

import (
	"fmt"
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// omp's ask tool opens a dialog over its composer and takes its answer from
// the keyboard. Read from omp 18.12.6's ask dialog (packages/tui/src/
// overlays/ask-dialog.ts):
//
//   - Each question lists its options, then "Other (type your own)". The
//     cursor starts on the recommended option, or the first; Up and Down
//     move it and stop at either end.
//   - On a single-choice question Enter takes the option under the cursor.
//     On a multi-choice one Space ticks or unticks it, and Enter on an option
//     moves on with what is ticked.
//   - Enter on "Other" opens a field for the person's own words; Enter there
//     takes them and moves on.
//   - Moving on from the last question submits, unless the dialog has tabs --
//     more than one question, or any multi-choice one -- when it goes to a
//     review page where Enter submits. One multi-choice question answered by
//     ticking skips the review; answered in its own words, it does not.
//   - Esc dismisses the dialog: the tool reports it cancelled.
//
// Each step is typed on its own, a moment apart, so a field has opened
// before its text arrives. Arrow keys for one move go together: omp splits
// them into separate keys.

const (
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
	keySpace = " "
	keyEnter = "\r"
)

// askQuestion is one question as omp's ask tool received it; the parts the
// phone does not carry (the recommended option) decide where the cursor
// starts.
type askQuestion struct {
	text        string
	options     []string
	multi       bool
	recommended int
}

func readAsk(in map[string]any) []askQuestion {
	raw, _ := in["questions"].([]any)
	var out []askQuestion
	for _, r := range raw {
		m, _ := r.(map[string]any)
		q := askQuestion{}
		q.text, _ = m["question"].(string)
		q.multi, _ = m["multi"].(bool)
		opts, _ := m["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			label, _ := om["label"].(string)
			q.options = append(q.options, label)
		}
		if f, ok := m["recommended"].(float64); ok && int(f) >= 0 && int(f) < len(q.options) {
			q.recommended = int(f)
		}
		out = append(out, q)
	}
	return out
}

// ApprovalKeySteps returns omp's keys for an answer: its approval prompt's
// keys for a permission, its ask dialog's for a question.
func (a *Adapter) ApprovalKeySteps(req protocol.ApprovalRequest, d protocol.ApprovalDecision) ([][]byte, error) {
	if req.Kind != "question" {
		keys, err := a.ApprovalKeys(req, d)
		return [][]byte{keys}, err
	}
	if d.Behavior == "deny" {
		return [][]byte{[]byte(keyDeny)}, nil
	}
	qs := readAsk(req.ToolInput)
	if len(qs) == 0 {
		return nil, fmt.Errorf("the request has no questions")
	}
	tabs := len(qs) > 1
	for _, q := range qs {
		tabs = tabs || q.multi
	}

	var steps []string
	review := tabs
	for _, q := range qs {
		answer := strings.TrimSpace(d.Answers[q.text])
		if answer == "" {
			return nil, fmt.Errorf("no answer for %q", q.text)
		}
		picked, text := splitAnswer(q.options, answer)
		cursor := q.recommended
		move := func(to int) {
			if to > cursor {
				steps = append(steps, strings.Repeat(keyDown, to-cursor))
			} else if to < cursor {
				steps = append(steps, strings.Repeat(keyUp, cursor-to))
			}
			cursor = to
		}
		other := len(q.options)

		if !q.multi {
			if len(picked) == 1 && text == "" {
				move(picked[0])
				steps = append(steps, keyEnter)
				continue
			}
			// Not one of the options: the person's own words.
			move(other)
			steps = append(steps, keyEnter, oneLine(answer), keyEnter)
			continue
		}

		for _, i := range picked {
			move(i)
			steps = append(steps, keySpace)
		}
		if text == "" {
			if cursor == other {
				move(0)
			}
			steps = append(steps, keyEnter)
			if len(qs) == 1 {
				review = false // a lone multi-choice question submits from here
			}
			continue
		}
		move(other)
		steps = append(steps, keyEnter, oneLine(text), keyEnter)
	}
	if review {
		steps = append(steps, keyEnter) // Submit, on the review page
	}
	out := make([][]byte, len(steps))
	for i, s := range steps {
		out[i] = []byte(s)
	}
	return out, nil
}

// splitAnswer reads an answer as the phone writes it -- the options chosen,
// in the order offered, then any words of the person's own, joined with ", "
// -- into the options' 0-based indexes and the words. Labels are matched from
// the front rather than split on commas, since a label may hold one.
func splitAnswer(options []string, answer string) (picked []int, text string) {
	rest := answer
	for i, label := range options {
		if label == "" {
			continue
		}
		if rest == label {
			return append(picked, i), ""
		}
		if strings.HasPrefix(rest, label+", ") {
			picked = append(picked, i)
			rest = rest[len(label)+2:]
		}
	}
	return picked, rest
}

// oneLine flattens text for omp's one-line answer field.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

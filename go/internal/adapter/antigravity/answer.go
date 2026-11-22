package antigravity

import (
	"fmt"
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Antigravity CLI's interactive prompts take their answers from the keyboard.
// Checked against live Antigravity CLI 1.5.13 dialogs:
//
// Permission prompt (e.g. `Run this command?`):
//   - Cursor starts on `> 1. Yes, run command` (allow once; options 2/3 are
//     always-allow grants that muxscale never picks).
//   - Enter confirms option 1 (allow once).
//   - Esc cancels (deny).
//
// Question dialog (`ask_question`):
//   - Each question lists its options (1..N), followed by `Write-in...`.
//   - Cursor starts on option 1 (index 0); Up/Down move it.
//   - On a single-select question, Enter selects the highlighted option and
//     advances immediately to the next question (or submits all on the last).
//   - On a multi-select question, Space toggles `[x]` on the highlighted
//     option, and Enter confirms and advances (or submits all on the last).
//     There is no separate review page.
//   - Choosing `Write-in...` and pressing Enter opens a text field; Enter
//     submits the text and advances.
//   - Esc skips/cancels the dialog.
const (
	keyDown   = "\x1b[B"
	keySpace  = " "
	keyEnter  = "\r"
	keyCancel = "\x1b"
)

// ApprovalKeys returns Antigravity's keys for a tool permission prompt.
func (*Adapter) ApprovalKeys(_ protocol.ApprovalRequest, d protocol.ApprovalDecision) ([]byte, error) {
	switch d.Behavior {
	case "allow":
		return []byte(keyEnter), nil
	case "deny":
		return []byte(keyCancel), nil
	}
	return nil, fmt.Errorf("antigravity's prompt takes allow or deny, not %q", d.Behavior)
}

// ApprovalKeySteps returns Antigravity's key sequence for either a tool
// permission prompt or an `ask_question` multi-question dialog.
func (a *Adapter) ApprovalKeySteps(req protocol.ApprovalRequest, d protocol.ApprovalDecision) ([][]byte, error) {
	if req.Kind != "question" {
		keys, err := a.ApprovalKeys(req, d)
		if err != nil {
			return nil, err
		}
		return [][]byte{keys}, nil
	}
	if d.Behavior == "deny" {
		return [][]byte{[]byte(keyCancel)}, nil
	}
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("the request has no questions")
	}

	var steps []string
	for _, q := range req.Questions {
		answer := strings.TrimSpace(d.Answers[q.Question])
		if answer == "" {
			return nil, fmt.Errorf("no answer for %q", q.Question)
		}
		picked, text := splitAnswer(q.Options, answer)
		cursor := 0
		moveDown := func(to int) {
			if to > cursor {
				steps = append(steps, strings.Repeat(keyDown, to-cursor))
				cursor = to
			}
		}
		writeIn := len(q.Options)

		if !q.MultiSelect {
			if len(picked) == 1 && text == "" {
				moveDown(picked[0])
				steps = append(steps, keyEnter)
				continue
			}
			// Custom write-in answer.
			moveDown(writeIn)
			steps = append(steps, keyEnter, flattenOneLine(answer), keyEnter)
			continue
		}

		for _, idx := range picked {
			moveDown(idx)
			steps = append(steps, keySpace)
		}
		if text != "" {
			moveDown(writeIn)
			steps = append(steps, keyEnter, flattenOneLine(text), keyEnter)
			continue
		}
		steps = append(steps, keyEnter)
	}

	out := make([][]byte, len(steps))
	for i, s := range steps {
		out[i] = []byte(s)
	}
	return out, nil
}

// splitAnswer matches the phone's comma-joined answer against the offered
// option labels from the front, returning 0-based indices and any leftover
// free-text answer.
func splitAnswer(options []protocol.QuestionOption, answer string) (picked []int, text string) {
	rest := answer
	for i, o := range options {
		if o.Label == "" {
			continue
		}
		if rest == o.Label {
			return append(picked, i), ""
		}
		if strings.HasPrefix(rest, o.Label+", ") {
			picked = append(picked, i)
			rest = rest[len(o.Label)+2:]
		}
	}
	return picked, rest
}

func flattenOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

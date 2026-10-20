package daemon

import (
	"fmt"
	"strings"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Questions an agent asks the person, as opposed to permission it asks for.
//
// Claude Code's AskUserQuestion reaches the PermissionRequest hook like any
// tool, and was shown as an approval titled "AskUserQuestion" with Allow and
// Deny -- neither of which answers anything. Once the hold ran out it fell
// through to the terminal, where it blocks the input box, while the phone
// said the session was working. So it is its own kind: the phone shows the
// questions and their choices, and the answer goes back as the tool's input
// (updatedInput with "answers"), which is how Claude Code takes an answer
// from anything other than its own dialog.

// askUserQuestion is the tool whose permission request is really a question.
const askUserQuestion = "AskUserQuestion"

// parseQuestions reads AskUserQuestion's input. Nil when it is not one, or
// has nothing a person could answer -- it is then shown as an approval, the
// safe old behaviour.
func parseQuestions(toolName string, in map[string]any) []protocol.Question {
	if toolName != askUserQuestion {
		return nil
	}
	raw, _ := in["questions"].([]any)
	var out []protocol.Question
	for _, r := range raw {
		m, _ := r.(map[string]any)
		text, _ := m["question"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		q := protocol.Question{Question: text, Options: []protocol.QuestionOption{}}
		q.Header, _ = m["header"].(string)
		q.MultiSelect, _ = m["multiSelect"].(bool)
		opts, _ := m["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			label, _ := om["label"].(string)
			if label == "" {
				continue
			}
			desc, _ := om["description"].(string)
			q.Options = append(q.Options, protocol.QuestionOption{Label: label, Description: desc})
		}
		out = append(out, q)
	}
	return out
}

// questionSubject is the line a notification or a list row shows.
func questionSubject(qs []protocol.Question) string {
	if len(qs) == 1 {
		return qs[0].Question
	}
	return fmt.Sprintf("%s (and %d more)", qs[0].Question, len(qs)-1)
}

// checkAnswers holds an answer to the questions asked: every question
// answered, and nothing answered that was not asked.
func checkAnswers(qs []protocol.Question, answers map[string]string) error {
	asked := make(map[string]bool, len(qs))
	for _, q := range qs {
		asked[q.Question] = true
		if strings.TrimSpace(answers[q.Question]) == "" {
			return fmt.Errorf("no answer to %q", q.Question)
		}
	}
	for k := range answers {
		if !asked[k] {
			return fmt.Errorf("%q was not asked", k)
		}
	}
	return nil
}

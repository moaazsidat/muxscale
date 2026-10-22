package claudecode

import (
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func color() protocol.Question {
	return protocol.Question{Question: "Pick a color", Options: []protocol.QuestionOption{
		{Label: "red"}, {Label: "green"}, {Label: "blue"},
	}}
}

func toppings() protocol.Question {
	return protocol.Question{Question: "Pick toppings", MultiSelect: true, Options: []protocol.QuestionOption{
		{Label: "cheese"}, {Label: "olives"}, {Label: "basil, fresh"},
	}}
}

// keys renders steps as "a|b|c" with the control keys named.
func keys(t *testing.T, qs []protocol.Question, answers map[string]string) string {
	t.Helper()
	steps, err := New().ApprovalKeySteps(protocol.ApprovalRequest{Kind: "question", Questions: qs},
		protocol.ApprovalDecision{Behavior: "allow", Answers: answers})
	if err != nil {
		t.Fatal(err)
	}
	r := strings.NewReplacer(keyDown, "↓", keyTab, "⇥", keyEnter, "⏎")
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = r.Replace(string(s))
	}
	return strings.Join(out, "|")
}

// Each case is a sequence typed into a live Claude Code 2.1.308 dialog and
// checked against what it recorded.
func TestApprovalKeyStepsTypeTheDialogsKeys(t *testing.T) {
	for _, c := range []struct {
		name    string
		qs      []protocol.Question
		answers map[string]string
		want    string
	}{
		{"one question, an option: picks and submits", []protocol.Question{color()},
			map[string]string{"Pick a color": "green"}, "2"},
		{"one question, own words, kept to one line", []protocol.Question{color()},
			map[string]string{"Pick a color": "dark\nteal"}, "4|dark teal|⏎"},
		{"two questions: each tab, then Submit answers", []protocol.Question{color(), toppings()},
			map[string]string{"Pick a color": "green", "Pick toppings": "cheese, basil, fresh"}, "2|1|3|⇥|1"},
		{"own words on both", []protocol.Question{color(), toppings()},
			map[string]string{"Pick a color": "dark teal", "Pick toppings": "olives, extra mushrooms"},
			"4|dark teal|⏎|2|4|↓|↓|↓|extra mushrooms|⇥|⏎|1"},
		{"one multi-choice question still has a review tab", []protocol.Question{toppings()},
			map[string]string{"Pick toppings": "basil, fresh"}, "3|⇥|1"},
	} {
		if got := keys(t, c.qs, c.answers); got != c.want {
			t.Errorf("%s: typed %s, want %s", c.name, got, c.want)
		}
	}
}

func TestApprovalKeyStepsRefusals(t *testing.T) {
	a := New()
	req := protocol.ApprovalRequest{Kind: "question", Questions: []protocol.Question{color()}}
	if steps, err := a.ApprovalKeySteps(req, protocol.ApprovalDecision{Behavior: "deny"}); err != nil || string(steps[0]) != keyCancel {
		t.Errorf("declining is Esc: %q, %v", steps, err)
	}
	if _, err := a.ApprovalKeySteps(req, protocol.ApprovalDecision{Behavior: "allow"}); err == nil {
		t.Error("an unanswered question is not typed")
	}
	if _, err := a.ApprovalKeySteps(protocol.ApprovalRequest{Kind: "tool_use"}, protocol.ApprovalDecision{Behavior: "allow"}); err == nil {
		t.Error("a permission prompt is not typed")
	}
}

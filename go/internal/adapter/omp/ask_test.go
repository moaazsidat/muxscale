package omp

import (
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func askReq(qs ...map[string]any) protocol.ApprovalRequest {
	raw := make([]any, len(qs))
	for i, q := range qs {
		raw[i] = q
	}
	return protocol.ApprovalRequest{Kind: "question", ToolName: "ask", ToolInput: map[string]any{"questions": raw}}
}

func q(text string, multi bool, recommended int, labels ...string) map[string]any {
	opts := make([]any, len(labels))
	for i, l := range labels {
		opts[i] = map[string]any{"label": l}
	}
	m := map[string]any{"id": text, "question": text, "options": opts, "multi": multi}
	if recommended >= 0 {
		m["recommended"] = float64(recommended)
	}
	return m
}

func steps(t *testing.T, req protocol.ApprovalRequest, answers map[string]string) string {
	t.Helper()
	out, err := New().ApprovalKeySteps(req, protocol.ApprovalDecision{Behavior: "allow", Answers: answers})
	if err != nil {
		t.Fatal(err)
	}
	s := make([]string, len(out))
	for i, b := range out {
		if string(b) == keySpace {
			s[i] = "␣" // the key, not a space in typed words
			continue
		}
		s[i] = strings.NewReplacer("\x1b[A", "↑", "\x1b[B", "↓", "\r", "⏎").Replace(string(b))
	}
	return strings.Join(s, "|")
}

// The cursor starts on the recommended option; the keys walk from there.
func TestAskKeysWalkFromTheRecommendedOption(t *testing.T) {
	for _, c := range []struct {
		name    string
		req     protocol.ApprovalRequest
		answers map[string]string
		want    string
	}{
		{"pick below", askReq(q("How?", false, 0, "a", "b", "c")), map[string]string{"How?": "c"}, "↓↓|⏎"},
		{"pick above", askReq(q("How?", false, 2, "a", "b", "c")), map[string]string{"How?": "a"}, "↑↑|⏎"},
		{"recommended", askReq(q("How?", false, 1, "a", "b", "c")), map[string]string{"How?": "b"}, "⏎"},
		{"own words", askReq(q("How?", false, -1, "a", "b")), map[string]string{"How?": "neither,  thanks"}, "↓↓|⏎|neither, thanks|⏎"},
		{"one multi, ticked", askReq(q("Which?", true, -1, "a", "b", "c")), map[string]string{"Which?": "a, c"}, "␣|↓↓|␣|⏎"},
		{"one multi, with words", askReq(q("Which?", true, -1, "a", "b")), map[string]string{"Which?": "b, more"}, "↓|␣|↓|⏎|more|⏎|⏎"},
		{"two questions", askReq(q("One?", false, 0, "x", "y"), q("Two?", false, 0, "p", "q")),
			map[string]string{"One?": "y", "Two?": "p"}, "↓|⏎|⏎|⏎"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := steps(t, c.req, c.answers); got != c.want {
				t.Errorf("keys %s, want %s", got, c.want)
			}
		})
	}
}

func TestAskSkipIsEscAndApprovalsKeepTheirKeys(t *testing.T) {
	a := New()
	skip, err := a.ApprovalKeySteps(askReq(q("How?", false, 0, "a")), protocol.ApprovalDecision{Behavior: "deny"})
	if err != nil || len(skip) != 1 || string(skip[0]) != keyDeny {
		t.Errorf("skip = %q, %v", skip, err)
	}
	allow, err := a.ApprovalKeySteps(protocol.ApprovalRequest{Kind: "tool_use"}, protocol.ApprovalDecision{Behavior: "allow"})
	if err != nil || len(allow) != 1 || string(allow[0]) != keyApprove {
		t.Errorf("approve = %q, %v", allow, err)
	}
	if _, err := a.ApprovalKeySteps(askReq(q("How?", false, 0, "a")), protocol.ApprovalDecision{Behavior: "allow"}); err == nil {
		t.Error("an answer with nothing in it was typed")
	}
}

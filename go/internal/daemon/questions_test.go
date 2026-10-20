package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// AskUserQuestion as Claude Code sends it to the PermissionRequest hook.
func askInput() map[string]any {
	return map[string]any{
		"questions": []any{
			map[string]any{
				"question": "Which framework?", "header": "Framework", "multiSelect": false,
				"options": []any{
					map[string]any{"label": "React", "description": "Component library"},
					map[string]any{"label": "Vue"},
				},
			},
			map[string]any{
				"question": "Which extras?", "header": "Extras", "multiSelect": true,
				"options": []any{map[string]any{"label": "Tests"}, map[string]any{"label": "Docs"}},
			},
		},
	}
}

func TestParseQuestions(t *testing.T) {
	qs := parseQuestions("AskUserQuestion", askInput())
	if len(qs) != 2 || qs[0].Question != "Which framework?" || qs[0].Header != "Framework" ||
		len(qs[0].Options) != 2 || qs[0].Options[0].Description != "Component library" || !qs[1].MultiSelect {
		t.Fatalf("questions = %+v", qs)
	}
	if parseQuestions("Bash", askInput()) != nil {
		t.Error("only AskUserQuestion is a question")
	}
	if parseQuestions("AskUserQuestion", map[string]any{"questions": "nope"}) != nil {
		t.Error("an unreadable input stays an approval")
	}
	if got := questionSubject(qs); got != "Which framework? (and 1 more)" {
		t.Errorf("subject = %q", got)
	}
}

func TestCheckAnswers(t *testing.T) {
	qs := parseQuestions("AskUserQuestion", askInput())
	if err := checkAnswers(qs, map[string]string{"Which framework?": "React", "Which extras?": "Tests, Docs"}); err != nil {
		t.Error(err)
	}
	if checkAnswers(qs, map[string]string{"Which framework?": "React"}) == nil {
		t.Error("an unanswered question must be refused")
	}
	if checkAnswers(qs, map[string]string{"Which framework?": "React", "Which extras?": "Tests", "Other?": "x"}) == nil {
		t.Error("an answer to something not asked must be refused")
	}
}

// The question is shown as one, answered from the phone, and the answer goes
// back as the tool's input -- the questions echoed, plus answers.
func TestQuestionAnsweredFromThePhone(t *testing.T) {
	srv, reg := testServer(t, 5*time.Second)
	done := make(chan claudecode.HookResponse, 1)
	go func() {
		done <- postHook(t, srv, "secret-token", map[string]any{
			"session_id":      "s1",
			"hook_event_name": claudecode.HookPermissionRequest,
			"tool_name":       "AskUserQuestion",
			"tool_input":      askInput(),
		})
	}()

	var req protocol.ApprovalRequest
	for i := 0; i < 200 && req.ID == ""; i++ {
		if ps := reg.PendingApprovals(); len(ps) == 1 {
			req = ps[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	if req.Kind != "question" || len(req.Questions) != 2 || req.Subject != "Which framework? (and 1 more)" {
		t.Fatalf("request = %+v", req)
	}
	if s := reg.List()[0]; s.State != protocol.StateWaitingInput {
		t.Errorf("state = %s, want WAITING_INPUT: the agent asked the person something", s.State)
	}

	answers := map[string]string{"Which framework?": "React", "Which extras?": "Tests, Docs"}
	reg.ResolveApproval(req.ID, protocol.ApprovalDecision{Behavior: "allow", Answers: answers})

	select {
	case resp := <-done:
		b, _ := json.Marshal(resp)
		var got struct {
			HookSpecificOutput struct {
				Decision struct {
					Behavior     string `json:"behavior"`
					UpdatedInput struct {
						Questions []any           `json:"questions"`
						Answers   map[string]string `json:"answers"`
					} `json:"updatedInput"`
				} `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		d := got.HookSpecificOutput.Decision
		if d.Behavior != "allow" || len(d.UpdatedInput.Questions) != 2 || d.UpdatedInput.Answers["Which extras?"] != "Tests, Docs" {
			t.Errorf("reply = %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not return after the question was answered")
	}
}

// Unanswered in time, the question goes to the terminal, where it blocks
// what is typed: the session is waiting on the person, not working.
func TestUnansweredQuestionIsWaitingNotWorking(t *testing.T) {
	srv, reg := testServer(t, 100*time.Millisecond)
	resp := postHook(t, srv, "secret-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": claudecode.HookPermissionRequest,
		"tool_name":       "AskUserQuestion",
		"tool_input":      askInput(),
	})
	if resp.HookSpecificOutput != nil {
		t.Errorf("timeout must yield no decision, got %+v", resp.HookSpecificOutput)
	}
	s := reg.List()[0]
	if s.State != protocol.StateWaitingInput {
		t.Errorf("state = %s, want WAITING_INPUT once the question is at the keyboard", s.State)
	}
	if len(reg.PendingApprovals()) != 0 {
		t.Error("timed-out question still pending")
	}
}

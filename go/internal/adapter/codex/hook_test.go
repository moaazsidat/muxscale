package codex

import (
	"encoding/json"
	"testing"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func TestDecodeHook(t *testing.T) {
	body := []byte(`{"session_id":"s1","transcript_path":"/r.jsonl","cwd":"/w",
		"hook_event_name":"PermissionRequest","model":"gpt-5.5","turn_id":"turn-1",
		"tool_name":"Bash","tool_use_id":"call-1","tool_input":{"command":"rm -rf build"}}`)
	e, err := (&Adapter{}).DecodeHook(body)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != adapter.HookApproval || e.SessionID != "s1" || e.PromptID != "turn-1" {
		t.Fatalf("event = %+v", e)
	}
	if e.ToolName != "Bash" || e.ToolInput["command"] != "rm -rf build" {
		t.Errorf("tool = %+v", e)
	}
	if e.SessionFile != "/r.jsonl" || e.Cwd != "/w" || e.Model != "gpt-5.5" {
		t.Errorf("metadata = %+v", e)
	}
}

func TestEncodeHookReply(t *testing.T) {
	reply := (&Adapter{}).EncodeHookReply(adapter.HookEvent{Kind: adapter.HookApproval},
		&protocol.ApprovalDecision{Behavior: "deny", Reason: "not now"})
	b, _ := json.Marshal(reply)
	var got struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.HookSpecificOutput.HookEventName != "PermissionRequest" ||
		got.HookSpecificOutput.Decision.Behavior != "deny" ||
		got.HookSpecificOutput.Decision.Message != "not now" {
		t.Fatalf("reply = %s", b)
	}

	no, _ := json.Marshal((&Adapter{}).EncodeHookReply(adapter.HookEvent{}, nil))
	if string(no) != "{}" {
		t.Fatalf("no-decision reply = %s", no)
	}
}

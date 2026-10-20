package omp

import (
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func TestApprovalEventsDecode(t *testing.T) {
	a := New()
	e, err := a.DecodeHook([]byte(`{"v":1,"agent":"omp","event":"approval_requested","session_id":"s1",` +
		`"tool_name":"bash","tool_use_id":"call-1","tool_input":{"command":"make"},"reason":"Critical pattern detected"}`))
	if err != nil || e.Kind != adapter.HookApprovalShown || e.ToolUseID != "call-1" ||
		e.ToolInput["command"] != "make" || e.Reason != "Critical pattern detected" {
		t.Fatalf("requested = %+v, %v", e, err)
	}
	e, err = a.DecodeHook([]byte(`{"v":1,"agent":"omp","event":"approval_resolved","session_id":"s1","tool_use_id":"call-1","approved":true}`))
	if err != nil || e.Kind != adapter.HookApprovalClosed || !e.Approved {
		t.Fatalf("resolved = %+v, %v", e, err)
	}
}

func TestApprovalKeysAreOmpsDefaults(t *testing.T) {
	a := New()
	for behavior, want := range map[string]string{"allow": "\x11", "deny": "\x1b"} {
		got, err := a.ApprovalKeys(protocol.ApprovalRequest{}, protocol.ApprovalDecision{Behavior: behavior})
		if err != nil || string(got) != want {
			t.Errorf("%s -> %q, %v", behavior, got, err)
		}
	}
	if _, err := a.ApprovalKeys(protocol.ApprovalRequest{}, protocol.ApprovalDecision{}); err == nil {
		t.Error("an empty decision produced keys")
	}
}

// The extension reports prompts and nothing else about them: it must never
// return a value from an approval handler, which is how it would change one.
func TestExtensionReportsApprovalsWithoutAnsweringThem(t *testing.T) {
	src := ExtensionSource("", "tok")
	for _, want := range []string{`"tool_approval_requested"`, `"tool_approval_resolved"`, `"approval_requested"`, `"approval_resolved"`, `"tool_call"`} {
		if !strings.Contains(src, want) {
			t.Errorf("extension lacks %s", want)
		}
	}
	if strings.Contains(src, "block:") {
		t.Error("the extension blocks a tool call; it must only observe")
	}
}

func TestQueueEventDecodes(t *testing.T) {
	a := New()
	e, err := a.DecodeHook([]byte(`{"v":1,"agent":"omp","event":"queue","session_id":"s1",` +
		`"queued":[{"text":"then push","at":"2026-10-19T10:00:00.000Z"}]}`))
	if err != nil || e.Kind != adapter.HookQueue || e.Queue == nil || len(*e.Queue) != 1 || (*e.Queue)[0].Text != "then push" {
		t.Fatalf("queue = %+v, %v", e, err)
	}
	// Emptied: an empty queue, not an absent one.
	e, _ = a.DecodeHook([]byte(`{"v":1,"agent":"omp","event":"queue","session_id":"s1","queued":[]}`))
	if e.Queue == nil || len(*e.Queue) != 0 {
		t.Fatalf("emptied = %+v", e.Queue)
	}
}

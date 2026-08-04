package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func testServer(t *testing.T, hold time.Duration) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	NewHookServer(reg, "secret-token", hold, log).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg
}

func postHook(t *testing.T, srv *httptest.Server, token string, p map[string]any) claudecode.HookResponse {
	t.Helper()
	body, _ := json.Marshal(p)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/hook/claude-code", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; every path must return 200 so the agent reads it as 'no decision'", resp.StatusCode)
	}
	var out claudecode.HookResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestStopHookMarksWaitingInput(t *testing.T) {
	srv, reg := testServer(t, time.Second)
	postHook(t, srv, "secret-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": claudecode.HookStop,
		"cwd":             "/home/dev/app",
		"prompt_id":       "p1",
	})
	sessions := reg.List()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.State != protocol.StateWaitingInput {
		t.Errorf("state = %s, want WAITING_INPUT", s.State)
	}
	// Hook-sourced state is the agent telling us, not us inferring.
	if s.Guess.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0 for a hook-sourced state", s.Guess.Confidence)
	}
	if s.PromptID != "p1" {
		t.Errorf("prompt_id = %q, want p1 (it is the fence token)", s.PromptID)
	}
	if s.Fidelity != protocol.FidelityNativeFull {
		t.Errorf("fidelity = %s, want NATIVE_FULL", s.Fidelity)
	}
}

func TestSessionIdentityIsStableAcrossHooks(t *testing.T) {
	srv, reg := testServer(t, time.Second)
	for _, ev := range []string{
		claudecode.HookSessionStart, claudecode.HookUserPromptSubmit,
		claudecode.HookPreToolUse, claudecode.HookStop,
	} {
		postHook(t, srv, "secret-token", map[string]any{
			"session_id": "same", "hook_event_name": ev, "cwd": "/app",
		})
	}
	if n := len(reg.List()); n != 1 {
		t.Errorf("4 hooks for one Claude Code session produced %d muxscale sessions; want 1", n)
	}
}

// The whole product rests on this: the hook blocks, a human answers, the answer
// becomes a structured decision.
func TestPermissionRequestBlocksUntilAnswered(t *testing.T) {
	srv, reg := testServer(t, 5*time.Second)

	done := make(chan claudecode.HookResponse, 1)
	go func() {
		done <- postHook(t, srv, "secret-token", map[string]any{
			"session_id":      "s1",
			"hook_event_name": claudecode.HookPermissionRequest,
			"tool_name":       "Bash",
			"tool_input":      map[string]any{"command": "rm -rf ./build"},
			"prompt_id":       "p1",
		})
	}()

	// Wait for the approval to be registered.
	var approvalID string
	for i := 0; i < 100; i++ {
		if ps := reg.PendingApprovals(); len(ps) == 1 {
			approvalID = ps[0].ID
			if ps[0].Source != protocol.ApprovalNative {
				t.Errorf("source = %s, want native", ps[0].Source)
			}
			if ps[0].Subject == "" {
				t.Error("approval needs a subject for the notification body")
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if approvalID == "" {
		t.Fatal("approval never registered")
	}

	// The request must still be in flight.
	select {
	case <-done:
		t.Fatal("hook returned before anyone answered")
	default:
	}

	if s := reg.List()[0]; s.State != protocol.StateWaitingApproval {
		t.Errorf("state = %s, want WAITING_APPROVAL", s.State)
	}

	reg.ResolveApproval(approvalID, protocol.ApprovalDecision{Behavior: "deny", DecidedBy: "phone"})

	select {
	case resp := <-done:
		b, _ := json.Marshal(resp)
		var got struct {
			HookSpecificOutput struct {
				HookEventName string `json:"hookEventName"`
				Decision      struct {
					Behavior string `json:"behavior"`
				} `json:"decision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.HookSpecificOutput.Decision.Behavior != "deny" {
			t.Errorf("behavior = %q, want deny; got payload %s",
				got.HookSpecificOutput.Decision.Behavior, b)
		}
		if got.HookSpecificOutput.HookEventName != claudecode.HookPermissionRequest {
			t.Errorf("hookEventName = %q", got.HookSpecificOutput.HookEventName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not return after the approval was answered")
	}
}

// Nobody answers -> fall through to Claude Code's own prompt. A monitoring tool
// that can deadlock the agent it monitors is worse than no monitoring tool.
func TestPermissionRequestTimesOutOpen(t *testing.T) {
	srv, _ := testServer(t, 150*time.Millisecond)
	start := time.Now()
	resp := postHook(t, srv, "secret-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": claudecode.HookPermissionRequest,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "ls"},
	})
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v; should have waited for the hold window", elapsed)
	}
	if resp.HookSpecificOutput != nil {
		t.Errorf("timeout must yield no decision, got %+v", resp.HookSpecificOutput)
	}
}

// Regression: a timed-out approval used to strand the session in
// WAITING_APPROVAL forever, which would keep notifying about a question nobody
// could still answer. Found by running the daemon against a live session.
func TestTimedOutApprovalClearsWaitingState(t *testing.T) {
	srv, reg := testServer(t, 100*time.Millisecond)
	postHook(t, srv, "secret-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": claudecode.HookPermissionRequest,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "ls"},
	})
	s := reg.List()[0]
	if s.State == protocol.StateWaitingApproval {
		t.Error("session stranded in WAITING_APPROVAL after the approval timed out")
	}
	if s.State != protocol.StateRunning {
		t.Errorf("state = %s, want RUNNING once the agent falls back to asking locally", s.State)
	}
	if len(reg.PendingApprovals()) != 0 {
		t.Error("timed-out approval still listed as pending")
	}
}

func TestBadTokenFailsOpen(t *testing.T) {
	srv, reg := testServer(t, time.Second)
	resp := postHook(t, srv, "wrong-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": claudecode.HookPermissionRequest,
		"tool_name":       "Bash",
	})
	if resp.HookSpecificOutput != nil {
		t.Error("an unauthorized hook must fail open with no decision")
	}
	if len(reg.List()) != 0 {
		t.Error("an unauthorized hook must not mutate session state")
	}
}

func TestMalformedBodyFailsOpen(t *testing.T) {
	srv, _ := testServer(t, time.Second)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/hook/claude-code",
		bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 even for garbage input", resp.StatusCode)
	}
}

// Claude Code adds hook events regularly; an unknown one is not an error.
func TestUnknownEventIsNotAnError(t *testing.T) {
	srv, reg := testServer(t, time.Second)
	resp := postHook(t, srv, "secret-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": "SomeFutureEventWeHaveNeverSeen",
		"cwd":             "/app",
	})
	if resp.HookSpecificOutput != nil {
		t.Error("unknown event should yield no decision")
	}
	// The session should still have been registered from the payload.
	if len(reg.List()) != 1 {
		t.Error("unknown event should still upsert the session")
	}
}

func TestToolCallRecordedInTranscript(t *testing.T) {
	srv, reg := testServer(t, time.Second)
	ch, cancel := reg.Subscribe()
	defer cancel()

	postHook(t, srv, "secret-token", map[string]any{
		"session_id":      "s1",
		"hook_event_name": claudecode.HookPreToolUse,
		"tool_name":       "Edit",
		"tool_use_id":     "toolu_9",
		"tool_input":      map[string]any{"file_path": "/app/src/auth.ts"},
	})

	deadline := time.After(time.Second)
	for {
		select {
		case c := <-ch:
			if c.Kind == ChangeTranscript && c.Event.Kind == protocol.EventToolCall {
				if c.Event.ToolName != "Edit" || c.Event.ToolUseID != "toolu_9" {
					t.Errorf("tool event = %+v", c.Event)
				}
				return
			}
		case <-deadline:
			t.Fatal("no tool_call transcript event observed")
		}
	}
}

func TestApprovalResolvesOnlyOnce(t *testing.T) {
	reg := NewRegistry()
	req := &protocol.ApprovalRequest{ID: "a1", SessionID: "s1", AskedAt: time.Now()}
	answer := reg.AskApproval(req)

	if !reg.ResolveApproval("a1", protocol.ApprovalDecision{Behavior: "allow"}) {
		t.Fatal("first resolve should deliver")
	}
	if reg.ResolveApproval("a1", protocol.ApprovalDecision{Behavior: "deny"}) {
		t.Error("second resolve must not deliver; the race between phone and keyboard must settle cleanly")
	}
	d := <-answer
	if d.Behavior != "allow" {
		t.Errorf("behavior = %q, want the first answer to win", d.Behavior)
	}
}

func TestListPutsWaitingSessionsFirst(t *testing.T) {
	reg := NewRegistry()
	mk := func(key string, st protocol.SessionState) {
		s := reg.Upsert("claude-code", key, nil)
		reg.SetState(s.ID, protocol.StateGuess{State: st, Confidence: 1})
	}
	mk("running", protocol.StateRunning)
	mk("exited", protocol.StateExited)
	mk("waiting", protocol.StateWaitingApproval)

	got := reg.List()
	if !got[0].State.Waiting() {
		t.Errorf("first session is %s; blocked sessions must sort first", got[0].State)
	}
	if !got[len(got)-1].State.Terminal() {
		t.Errorf("last session is %s; terminal sessions must sort last", got[len(got)-1].State)
	}
}

func TestLoopbackOnlyBindEnforced(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:4455", "192.168.1.10:4455", "[::]:4455"} {
		if _, err := New(Config{ListenLocal: addr}); err == nil {
			t.Errorf("New(%q) succeeded; the hook endpoint must be loopback-only", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		if _, err := New(Config{ListenLocal: addr}); err != nil {
			t.Errorf("New(%q) failed: %v", addr, err)
		}
	}
}

package omp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

const fixture = "../../../../fixtures/transcripts/omp/basic.jsonl"

func TestParseSessionMapsTheConversation(t *testing.T) {
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	events, meta, err := ParseSession(f)
	if err != nil {
		t.Fatal(err)
	}

	wantKinds := []protocol.EventKind{
		protocol.EventUserMessage,
		protocol.EventThinking,
		protocol.EventAssistantMessage,
		protocol.EventToolCall,
		protocol.EventToolResult,
		protocol.EventAssistantMessage,
		protocol.EventCompaction,
		protocol.EventStateChange,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantKinds), events)
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Errorf("event %d = %s, want %s", i, events[i].Kind, k)
		}
		if events[i].Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d", i, events[i].Seq)
		}
	}

	call := events[3]
	if call.ToolName != "bash" || call.ToolUseID != "call-1" {
		t.Errorf("tool call = %+v", call)
	}
	if call.ToolInput["command"] != "go test ./..." {
		t.Errorf("tool input = %+v", call.ToolInput)
	}
	// The text and the tool call in one assistant message share the entry id
	// and parent: the tree is per message, not per rendered line.
	if call.ID != events[2].ID || call.ParentID != events[2].ParentID {
		t.Errorf("tool call lost its entry identity: %+v vs %+v", call, events[2])
	}
	if events[1].Text != "I should run the tests first." {
		t.Errorf("thinking = %q", events[1].Text)
	}
	if events[1].Usage == nil || events[1].Usage.InputTokens != 100 || events[1].Usage.CacheReadTokens != 10 {
		t.Errorf("usage = %+v", events[1].Usage)
	}

	result := events[4]
	if !result.IsError || result.ToolUseID != "call-1" || result.ToolName != "bash" {
		t.Errorf("tool result = %+v", result)
	}

	if meta.SessionID != "01a151d4-146e-7571-a994-f30d585b56fa" {
		t.Errorf("session id = %q", meta.SessionID)
	}
	if meta.Cwd != "/Users/sam/dev/muxscale" {
		t.Errorf("cwd = %q", meta.Cwd)
	}
	if meta.Model != "openrouter/deepseek/deepseek-v4.1-flash" {
		t.Errorf("model = %q", meta.Model)
	}
	if meta.Version != "18.12.2" {
		t.Errorf("version = %q", meta.Version)
	}
	if meta.CustomTitle == nil || *meta.CustomTitle != "Fix the failing build" {
		t.Errorf("custom title = %v", meta.CustomTitle)
	}
}

// A user rename is a Name, not a Title: an automatic title may change later
// but must never overwrite what the person typed.
func TestAutoTitleDoesNotOverwriteAUserRename(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"session","version":3,"id":"s1","timestamp":"2026-10-18T21:00:00Z","cwd":"/w","title":"auto one","titleSource":"auto"}`,
		`{"type":"title_change","id":"a","parentId":null,"timestamp":"2026-10-18T21:00:01Z","title":"mine","source":"user"}`,
		`{"type":"title_change","id":"b","parentId":"a","timestamp":"2026-10-18T21:00:02Z","title":"auto two","source":"auto"}`,
	}, "\n")

	_, meta, err := ParseSession(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if meta.CustomTitle == nil || *meta.CustomTitle != "mine" {
		t.Fatalf("custom title = %v", meta.CustomTitle)
	}
	if meta.Title != "auto two" {
		t.Fatalf("title = %q", meta.Title)
	}
}

func TestStreamEmitsOnlyNewEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026-10-18T21-00-00-000Z_01a151d4-146e-7571-a994-f30d585b56fa.jsonl")
	header := `{"type":"session","version":3,"id":"01a151d4-146e-7571-a994-f30d585b56fa","timestamp":"2026-10-18T21:00:00Z","cwd":"/w"}`
	if err := os.WriteFile(path, []byte(header+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStream(path)
	ctx := context.Background()
	if ev, err := s.Poll(ctx); err != nil || len(ev) != 0 {
		t.Fatalf("first poll = %v, %v", ev, err)
	}

	line := `{"type":"message","id":"m1","parentId":null,"timestamp":"2026-10-18T21:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ev, err := s.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Text != "hello" {
		t.Fatalf("events = %+v", ev)
	}
	if ev[0].Seq != 1 {
		t.Errorf("seq = %d, want 1", ev[0].Seq)
	}
	if again, _ := s.Poll(ctx); len(again) != 0 {
		t.Fatalf("re-emitted %+v", again)
	}
}

func TestInferState(t *testing.T) {
	assistant := protocol.TranscriptEvent{Kind: protocol.EventAssistantMessage, Text: "done"}
	tool := protocol.TranscriptEvent{Kind: protocol.EventToolCall, ToolName: "bash"}

	quiet := adapter.StateInput{Recent: []protocol.TranscriptEvent{assistant}, Idle: 10 * time.Second, Quiescence: 6 * time.Second}
	if g, ok := (&Adapter{}).InferState(quiet); !ok || g.State != protocol.StateWaitingInput || g.Confidence > 0.7 {
		t.Fatalf("quiet guess = %+v, ok=%v", g, ok)
	}
	live := adapter.StateInput{Recent: []protocol.TranscriptEvent{tool}, Live: new(true)}
	if g, ok := (&Adapter{}).InferState(live); !ok || g.State != protocol.StateRunning {
		t.Fatalf("tool guess = %+v, ok=%v", g, ok)
	}
	dead := adapter.StateInput{Recent: []protocol.TranscriptEvent{assistant}, Live: new(false)}
	if g, ok := (&Adapter{}).InferState(dead); !ok || g.State != protocol.StateExited {
		t.Fatalf("lease-free guess = %+v, ok=%v", g, ok)
	}
	exit := adapter.StateInput{Recent: []protocol.TranscriptEvent{
		{Kind: protocol.EventStateChange, Text: "session_exit fatal: oom", IsError: true},
	}}
	if g, ok := (&Adapter{}).InferState(exit); !ok || g.State != protocol.StateCrashed {
		t.Fatalf("fatal exit guess = %+v, ok=%v", g, ok)
	}
	if _, ok := (&Adapter{}).InferState(adapter.StateInput{}); ok {
		t.Fatal("inferred a state from no evidence")
	}
}

func TestDiscoverSkipsSubagentFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))
	sessions := filepath.Join(root, "agent", "sessions", "-dev-app")
	if err := os.MkdirAll(filepath.Join(sessions, "2026-10-18T21-00-00-000Z_01a151d4-146e-7571-a994-f30d585b56fa"), 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(sessions, "2026-10-18T21-00-00-000Z_01a151d4-146e-7571-a994-f30d585b56fa.jsonl")
	header := `{"type":"session","version":3,"id":"01a151d4-146e-7571-a994-f30d585b56fa","timestamp":"2026-10-18T21:00:00Z","cwd":"/dev/app","title":"A task","titleSource":"auto"}`
	if err := os.WriteFile(main, []byte(header+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(sessions, "2026-10-18T21-00-00-000Z_01a151d4-146e-7571-a994-f30d585b56fa", "Worker.jsonl")
	if err := os.WriteFile(sub, []byte(header+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := (&Adapter{}).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("discovered %d sessions: %+v", len(got), got)
	}
	if got[0].NaturalKey != "01a151d4-146e-7571-a994-f30d585b56fa" || got[0].Cwd != "/dev/app" || got[0].Title != "A task" {
		t.Errorf("discovered = %+v", got[0])
	}
	// No writer lease in this temp state dir, so the session is historical.
	if got[0].Live {
		t.Error("session without a lease reported live")
	}
}

func TestTranscriptForProcessUsesTheBreadcrumb(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))
	tty := filepath.Join(root, "agent", "terminal-sessions")
	if err := os.MkdirAll(tty, 0o755); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "agent", "sessions", "-w", "2026-10-18T21-00-00-000Z_s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(session), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte(`{"type":"session","version":3,"id":"s1","cwd":"/w"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tty, "ttys001"), []byte("/w\n"+session+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := adapter.Process{TTY: "/dev/ttys001", Cwd: "/w"}
	if got := (&Adapter{}).TranscriptForProcess(p); got != session {
		t.Fatalf("transcript = %q, want %q", got, session)
	}
	// A breadcrumb left by a previous occupant of the tty is not this process's.
	p.Cwd = "/elsewhere"
	if got := (&Adapter{}).TranscriptForProcess(p); got != "" {
		t.Fatalf("accepted a stale breadcrumb: %q", got)
	}
}

func TestSessionIDFromPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-10-18T21-00-00-000Z_01a151d4-146e-7571-a994-f30d585b56fa.jsonl", "01a151d4-146e-7571-a994-f30d585b56fa"},
		{"Worker.jsonl", ""},
		{"weird.jsonl", ""},
	} {
		if got := SessionIDFromPath(tc.in); got != tc.want {
			t.Errorf("SessionIDFromPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

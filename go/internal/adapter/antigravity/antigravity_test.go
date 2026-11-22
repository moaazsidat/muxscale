package antigravity

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func setupSummariesDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE conversation_summaries (
	conversation_id text PRIMARY KEY,
	title text NOT NULL DEFAULT "",
	preview text NOT NULL DEFAULT "",
	step_count integer NOT NULL DEFAULT 0,
	last_modified_time datetime NOT NULL,
	workspace_uris text NOT NULL,
	status text NOT NULL DEFAULT "",
	parent_conversation_id text NOT NULL DEFAULT "",
	nesting_depth integer NOT NULL DEFAULT 0,
	last_user_input_time datetime NOT NULL
);`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func writeTranscript(t *testing.T, dataDir, convID string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transcript_full.jsonl")
	var content []byte
	for _, l := range lines {
		content = append(content, []byte(l+"\n")...)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverAndLiveness(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CLI_HOME", dataDir)

	db := setupSummariesDB(t, dataDir)
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	_, err := db.Exec(`
INSERT INTO conversation_summaries
  (conversation_id, title, last_modified_time, workspace_uris, status, parent_conversation_id, nesting_depth, last_user_input_time)
VALUES
  ('conv-live', 'Fix adapter bug', ?, '["file:///work/project"]', 'CASCADE_RUN_STATUS_RUNNING', '', 0, ?),
  ('conv-done', '', ?, '["file:///work/other%20proj"]', 'CASCADE_RUN_STATUS_IDLE', '', 0, ?),
  ('conv-sub', 'Subagent work', ?, '["file:///work/project"]', 'CASCADE_RUN_STATUS_IDLE', 'conv-live', 1, ?)
`, now, now, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	liveRef := writeTranscript(t, dataDir, "conv-live",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T22:07:11Z","content":"<USER_REQUEST>\nHello\n</USER_REQUEST>"}`)
	writeTranscript(t, dataDir, "conv-done",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T22:07:11Z","content":"<USER_REQUEST>\nFallback prompt title\n</USER_REQUEST>"}`)

	// Create presence locks: hold flock on conv-live, leave conv-done unlocked.
	presDir := filepath.Join(dataDir, "presence")
	if err := os.MkdirAll(presDir, 0o755); err != nil {
		t.Fatal(err)
	}
	liveLock, err := os.Create(filepath.Join(presDir, "conv-live.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer liveLock.Close()
	if err := syscall.Flock(int(liveLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(liveLock.Fd()), syscall.LOCK_UN)

	if err := os.WriteFile(filepath.Join(presDir, "conv-done.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	a := New()
	got, err := a.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 (subagent must be skipped): %+v", len(got), got)
	}

	byKey := map[string]adapter.Discovered{}
	for _, d := range got {
		byKey[d.NaturalKey] = d
	}

	dLive := byKey["conv-live"]
	if dLive.Title != "Fix adapter bug" || dLive.Cwd != "/work/project" || !dLive.Live || dLive.Ref != liveRef {
		t.Errorf("conv-live = %+v", dLive)
	}
	dDone := byKey["conv-done"]
	if dDone.Title != "Fallback prompt title" || dDone.Cwd != "/work/other proj" || dDone.Live {
		t.Errorf("conv-done = %+v", dDone)
	}

	if live, known := a.Live(liveRef); !live || !known {
		t.Errorf("Live(conv-live) = (%v, %v), want (true, true)", live, known)
	}
	if live, known := a.Live(dDone.Ref); live || !known {
		t.Errorf("Live(conv-done) = (%v, %v), want (false, true)", live, known)
	}
}

func TestTranscriptParsingAndPairing(t *testing.T) {
	dataDir := t.TempDir()
	ref := writeTranscript(t, dataDir, "conv-1",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T22:10:28Z","content":"<USER_REQUEST>\nRun echo hello\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-20.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T22:10:28Z","model":"gemini-4-neon-high","input_tokens":8940,"cache_read_tokens":100,"output_tokens":71,"thinking":"Let me run echo.","tool_calls":[{"name":"run_command","args":{"CommandLine":"echo hello","toolSummary":"Echo hello"}}]}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-20T22:10:32Z","content":"Created At: 2026-11-20T17:10:32-05:00\nCompleted At: 2026-11-20T17:10:32-05:00\nCommand exited with code 0.\n\nCommand output:\nhello\r\n"}`,
		`{"step_index":3,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-20T22:10:32Z","content":"<SYSTEM_MESSAGE>Reminder</SYSTEM_MESSAGE>"}`,
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T22:10:32Z","model":"gemini-4-neon-high","input_tokens":2222,"cache_read_tokens":6883,"output_tokens":14,"content":"done"}`,
	)

	a := New()
	st, err := a.OpenTranscript(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	evs, err := st.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Expected: user_message, thinking, tool_call, tool_result, assistant_message (SYSTEM_MESSAGE skipped).
	if len(evs) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(evs), evs)
	}
	if evs[0].Kind != protocol.EventUserMessage || evs[0].Text != "Run echo hello" {
		t.Errorf("evs[0] = %+v", evs[0])
	}
	if evs[1].Kind != protocol.EventThinking || evs[1].Text != "Let me run echo." {
		t.Errorf("evs[1] = %+v", evs[1])
	}
	if evs[2].Kind != protocol.EventToolCall || evs[2].ToolName != "run_command" || evs[2].ToolUseID == "" {
		t.Errorf("evs[2] = %+v", evs[2])
	}
	if evs[3].Kind != protocol.EventToolResult || evs[3].ToolUseID != evs[2].ToolUseID || evs[3].ToolName != "run_command" {
		t.Errorf("evs[3] = %+v (want ToolUseID %q)", evs[3], evs[2].ToolUseID)
	}
	if evs[3].ToolOutput != "Command exited with code 0.\n\nCommand output:\nhello" {
		t.Errorf("cleaned ToolOutput = %q", evs[3].ToolOutput)
	}
	if evs[4].Kind != protocol.EventAssistantMessage || evs[4].Text != "done" || evs[4].Model != "gemini-4-neon-high" {
		t.Errorf("evs[4] = %+v", evs[4])
	}

	meta := st.(adapter.MetaSource).Meta()
	if meta.Model != "gemini-4-neon-high" || meta.Title != "Run echo hello" {
		t.Errorf("meta = %+v", meta)
	}
	if got := st.(adapter.EventReporter).LastEvent(); got != lifeDone {
		t.Errorf("LastEvent = %q, want %q", got, lifeDone)
	}

	g, ok := a.InferState(adapter.StateInput{Event: lifeDone})
	if !ok || g.State != protocol.StateWaitingInput {
		t.Errorf("InferState(done) = (%+v, %v), want WAITING_INPUT", g, ok)
	}
}

func TestSeekEndPreservesToolPairing(t *testing.T) {
	dataDir := t.TempDir()
	ref := writeTranscript(t, dataDir, "conv-seek",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T22:10:28Z","content":"<USER_REQUEST>\nCheck file\n</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-20T22:10:28Z","model":"gemini-4-neon-high","tool_calls":[{"name":"read_file","args":{"Path":"/tmp/a.go"}}]}`,
	)

	s := newStream(ref)
	if err := s.SeekEnd(2); err != nil {
		t.Fatal(err)
	}
	// Append tool result after SeekEnd.
	f, err := os.OpenFile(ref, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-20T22:10:29Z","content":"package main"}` + "\n")
	f.Close()

	evs, err := s.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Seq != 3 || evs[0].ToolName != "read_file" || evs[0].ToolUseID != "step-1-call-0" {
		t.Errorf("post-seek tool result = %+v", evs[0])
	}
}

func TestHookDecodeAndInstall(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("GEMINI_CONFIG_DIR", cfgDir)

	// Pre-populate a user hook that must survive install and uninstall.
	userDoc := map[string]any{
		"user-linter": map[string]any{
			"Stop": []any{map[string]any{"type": "command", "command": "./lint.sh"}},
		},
	}
	rawUser, _ := json.MarshalIndent(userDoc, "", "  ")
	hooksPath := filepath.Join(cfgDir, "hooks.json")
	if err := os.WriteFile(hooksPath, append(rawUser, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	relay := RelayCommand("/usr/local/bin/muxscale", "127.0.0.1:4455", "/tmp/tok")
	changed, err := EnsureHooks(relay)
	if err != nil || !changed {
		t.Fatalf("EnsureHooks = (%v, %v), want (true, nil)", changed, err)
	}
	if !HooksInstalled(relay) || !HooksPresent() {
		t.Fatal("HooksInstalled/HooksPresent should be true after install")
	}
	// Idempotent second call.
	changed, err = EnsureHooks(relay)
	if err != nil || changed {
		t.Fatalf("second EnsureHooks = (%v, %v), want (false, nil)", changed, err)
	}
	// Backup created.
	if _, err := os.Stat(hooksPath + ".muxscale.bak"); err != nil {
		t.Errorf("backup missing: %v", err)
	}

	// User hook preserved, PreToolUse not wired.
	after, _ := os.ReadFile(hooksPath)
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["user-linter"]; !ok {
		t.Error("user-linter hook was clobbered")
	}
	mux := doc[hookKey].(map[string]any)
	if _, hasPre := mux["PreToolUse"]; hasPre {
		t.Error("PreToolUse must not be wired")
	}

	// Uninstall removes only muxscale and preserves user-linter.
	removed, err := RemoveHooks()
	if err != nil || !removed {
		t.Fatalf("RemoveHooks = (%v, %v), want (true, nil)", removed, err)
	}
	if HooksPresent() {
		t.Error("HooksPresent still true after uninstall")
	}
	final, _ := os.ReadFile(hooksPath)
	var finalDoc map[string]any
	if err := json.Unmarshal(final, &finalDoc); err != nil {
		t.Fatal(err)
	}
	if _, ok := finalDoc["user-linter"]; !ok {
		t.Error("user-linter missing after uninstall")
	}

	// Decode Stop payload.
	a := New()
	ev, err := a.DecodeHook([]byte(`{
		"conversationId": "00000000-0000-4000-8000-000000000001",
		"cwd": "/work/project",
		"hookEventName": "Stop",
		"modelName": "gemini-4-neon-high",
		"terminationReason": "NO_TOOL_CALL",
		"fullyIdle": true,
		"transcriptPath": "/home/user/.gemini/antigravity-cli/brain/00000000-0000-4000-8000-000000000001/.system_generated/logs/transcript_full.jsonl"
	}`))
	if err != nil {
		t.Fatalf("DecodeHook: %v", err)
	}
	if ev.Kind != adapter.HookStop || ev.AgentID != "antigravity" || ev.SessionID != "00000000-0000-4000-8000-000000000001" || ev.Model != "gemini-4-neon-high" {
		t.Errorf("decoded event = %+v", ev)
	}
	reply := a.EncodeHookReply(ev, nil)
	if m, ok := reply.(map[string]any); !ok || len(m) != 0 {
		t.Errorf("EncodeHookReply = %#v, want empty map", reply)
	}
}

func appendProtoVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendProtoBytes(dst []byte, field int, payload []byte) []byte {
	dst = appendProtoVarint(dst, uint64(field<<3|2))
	dst = appendProtoVarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

func buildStepMetadata(toolID, toolName, argsJSON, reason string, sec int64) []byte {
	var ts []byte
	ts = appendProtoVarint(ts, uint64(1<<3|0))
	ts = appendProtoVarint(ts, uint64(sec))

	var tc []byte
	tc = appendProtoBytes(tc, 1, []byte(toolID))
	tc = appendProtoBytes(tc, 2, []byte(toolName))
	tc = appendProtoBytes(tc, 3, []byte(argsJSON))

	var perm []byte
	if reason != "" {
		perm = appendProtoBytes(perm, 5, []byte(reason))
	}

	var meta []byte
	meta = appendProtoBytes(meta, 1, ts)
	meta = appendProtoBytes(meta, 4, tc)
	if len(perm) > 0 {
		meta = appendProtoBytes(meta, 34, perm)
	}
	return meta
}

func TestPendingPromptFromConversationDB(t *testing.T) {
	dataDir := t.TempDir()
	ref := writeTranscript(t, dataDir, "conv-wait",
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-20T22:10:28Z","content":"<USER_REQUEST>\nDeploy\n</USER_REQUEST>"}`,
	)

	convDir := filepath.Join(dataDir, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(convDir, "conv-wait.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE steps (idx integer PRIMARY KEY, status integer NOT NULL, metadata blob);`)
	if err != nil {
		t.Fatal(err)
	}

	meta := buildStepMetadata(
		"call_123",
		"run_command",
		`{"CommandLine":"echo deploy","toolSummary":"Deploy app"}`,
		"Mutates host state.",
		1795309073,
	)
	if _, err := db.Exec(`INSERT INTO steps (idx, status, metadata) VALUES (1, 9, ?)`, meta); err != nil {
		t.Fatal(err)
	}

	s := newStream(ref)
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := s.PendingPrompt()
	if p == nil {
		t.Fatal("PendingPrompt = nil, want run_command prompt")
	}
	if p.ToolName != "run_command" || p.ToolUseID != "call_123" || p.Reason != "Mutates host state." {
		t.Errorf("PendingPrompt = %+v", p)
	}
	if p.ToolInput["CommandLine"] != "echo deploy" {
		t.Errorf("ToolInput = %+v", p.ToolInput)
	}
	if p.AskedAt.Unix() != 1795309073 {
		t.Errorf("AskedAt = %v", p.AskedAt)
	}
	if got := s.LastEvent(); got != lifeWaitingApproval {
		t.Errorf("LastEvent = %q, want %q", got, lifeWaitingApproval)
	}

	// Once step leaves status=9, PendingPrompt clears.
	if _, err := db.Exec(`UPDATE steps SET status = 3 WHERE idx = 1`); err != nil {
		t.Fatal(err)
	}
	// Ensure mtime/size change is visible on coarse filesystems.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(dbPath, future, future)

	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := s.PendingPrompt(); p != nil {
		t.Errorf("PendingPrompt after status=3 = %+v, want nil", p)
	}
}

func TestApprovalKeySteps(t *testing.T) {
	a := New()

	// Permission allow -> Enter (option 1), deny -> Esc.
	allow, err := a.ApprovalKeys(protocol.ApprovalRequest{Kind: "tool_use"}, protocol.ApprovalDecision{Behavior: "allow"})
	if err != nil || string(allow) != "\r" {
		t.Errorf("allow keys = (%q, %v), want (\\r, nil)", allow, err)
	}
	deny, err := a.ApprovalKeys(protocol.ApprovalRequest{Kind: "tool_use"}, protocol.ApprovalDecision{Behavior: "deny"})
	if err != nil || string(deny) != "\x1b" {
		t.Errorf("deny keys = (%q, %v), want (Esc, nil)", deny, err)
	}

	// Multi-question dialog: Q1 single-select (option 2), Q2 multi-select (both options).
	req := protocol.ApprovalRequest{
		Kind: "question",
		Questions: []protocol.Question{
			{
				Question: "Pick a color?",
				Options:  []protocol.QuestionOption{{Label: "(Recommended) Red"}, {Label: "Blue"}},
			},
			{
				Question:    "Pick toppings?",
				MultiSelect: true,
				Options:     []protocol.QuestionOption{{Label: "Cheese"}, {Label: "Olives"}},
			},
		},
	}
	steps, err := a.ApprovalKeySteps(req, protocol.ApprovalDecision{
		Behavior: "allow",
		Answers: map[string]string{
			"Pick a color?":  "Blue",
			"Pick toppings?": "Cheese, Olives",
		},
	})
	if err != nil {
		t.Fatalf("ApprovalKeySteps: %v", err)
	}
	want := []string{"\x1b[B", "\r", " ", "\x1b[B", " ", "\r"}
	if len(steps) != len(want) {
		t.Fatalf("got %d steps %q, want %d %q", len(steps), steps, len(want), want)
	}
	for i := range want {
		if string(steps[i]) != want[i] {
			t.Errorf("step[%d] = %q, want %q", i, steps[i], want[i])
		}
	}

	// Single-select write-in answer.
	wiSteps, err := a.ApprovalKeySteps(protocol.ApprovalRequest{
		Kind: "question",
		Questions: []protocol.Question{{
			Question: "Pick a color?",
			Options:  []protocol.QuestionOption{{Label: "Red"}, {Label: "Blue"}},
		}},
	}, protocol.ApprovalDecision{
		Behavior: "allow",
		Answers:  map[string]string{"Pick a color?": "Green"},
	})
	if err != nil {
		t.Fatalf("write-in ApprovalKeySteps: %v", err)
	}
	wantWI := []string{"\x1b[B\x1b[B", "\r", "Green", "\r"}
	if len(wiSteps) != len(wantWI) {
		t.Fatalf("got %d write-in steps %q, want %q", len(wiSteps), wiSteps, wantWI)
	}
	for i := range wantWI {
		if string(wiSteps[i]) != wantWI[i] {
			t.Errorf("write-in step[%d] = %q, want %q", i, wiSteps[i], wantWI[i])
		}
	}
}

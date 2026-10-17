package daemon

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
)

func testManager(t *testing.T) (*Manager, *Registry, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := NewRegistry()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(reg, nil, st, log), reg, st
}

// Regression: Upsert mints a fresh ID, so restoring through it left the map key
// and the session's own ID disagreeing and every later SetState silently
// no-oped -- a restored session was stuck in STARTING forever. Found by
// restarting the daemon against a real database.
func TestRestoreKeepsPersistedIDAndSetsState(t *testing.T) {
	_, _, st := testManager(t)

	want := &protocol.Session{
		ID: "persisted-id-123", AgentID: "claude-code", NaturalKey: "sess-abc",
		Cwd: "/home/dev/app", Attachment: protocol.AttachWrap,
		Fidelity: protocol.FidelityFull, State: protocol.StateRunning,
		StateSince: time.Now(), CreatedAt: time.Now(), LastEventAt: time.Now(),
	}
	if err := st.SaveSession(want); err != nil {
		t.Fatal(err)
	}

	// Fresh manager over the same store, as after a daemon restart.
	reg2 := NewRegistry()
	m2 := NewManager(reg2, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := m2.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored %d sessions, want 1", n)
	}

	got, ok := reg2.Get("persisted-id-123")
	if !ok {
		t.Fatal("session not retrievable by its persisted ID")
	}
	// A wrapped session's PTY did not survive the restart. Showing it as
	// running would put a dead session at the top of someone's phone.
	if got.State != protocol.StateDetached {
		t.Errorf("state = %s, want DETACHED for a restored wrapped session", got.State)
	}
	if got.Guess.Reason == "" {
		t.Error("restored session has no explanation for its state")
	}

	// And SetState must actually work on the restored session.
	if !reg2.SetState("persisted-id-123", protocol.StateGuess{
		State: protocol.StateRunning, Confidence: 1, Reason: "test",
	}) {
		t.Error("SetState on a restored session did not take effect")
	}
}

// Hook-driven sessions re-announce themselves on their next event, so they are
// recoverable rather than lost, and should say so.
func TestRestoreNativeSessionAwaitsHook(t *testing.T) {
	_, _, st := testManager(t)
	if err := st.SaveSession(&protocol.Session{
		ID: "native-1", AgentID: "claude-code", NaturalKey: "n1",
		Attachment: protocol.AttachNative, State: protocol.StateWaitingInput,
		StateSince: time.Now(), CreatedAt: time.Now(), LastEventAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	m := NewManager(reg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Get("native-1")
	if got.State != protocol.StateUnknown {
		t.Errorf("state = %s, want UNKNOWN for a restorable hook-driven session", got.State)
	}
}

// Regression: a hook-only session that ended without saying so -- a test hook
// from months back -- came back as live on every restart, forever.
func TestRestoreLongSilentNativeSessionAsDetached(t *testing.T) {
	_, _, st := testManager(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := st.SaveSession(&protocol.Session{
		ID: "native-old", AgentID: "claude-code", NaturalKey: "n-old",
		Attachment: protocol.AttachNative, State: protocol.StateUnknown,
		StateSince: old, CreatedAt: old, LastEventAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	m := NewManager(reg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got, _ := reg.Get("native-old"); got.State != protocol.StateDetached {
		t.Errorf("state = %s, want DETACHED for a session silent for a month", got.State)
	}
}

func TestRestoreSkipsTerminalSessions(t *testing.T) {
	_, _, st := testManager(t)
	if err := st.SaveSession(&protocol.Session{
		ID: "done-1", AgentID: "claude-code", NaturalKey: "d1",
		State: protocol.StateExited, StateSince: time.Now(),
		CreatedAt: time.Now(), LastEventAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	m := NewManager(reg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := m.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("restored %d terminal sessions, want 0", n)
	}
}

// An in-flight approval cannot survive a restart: the hook's HTTP request died
// with the old process. Showing it as pending would offer the user a button
// that answers nothing.
func TestRestoreDropsPendingApprovals(t *testing.T) {
	_, _, st := testManager(t)
	if err := st.SaveSession(&protocol.Session{
		ID: "s-1", AgentID: "claude-code", NaturalKey: "k1",
		Attachment: protocol.AttachNative, State: protocol.StateWaitingApproval,
		StateSince: time.Now(), CreatedAt: time.Now(), LastEventAt: time.Now(),
		PendingApproval: &protocol.ApprovalRequest{ID: "a1", Subject: "Bash: rm -rf /"},
	}); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	m := NewManager(reg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.Get("s-1")
	if got.PendingApproval != nil {
		t.Error("a pending approval survived a restart; its hook request is long dead")
	}
	if len(reg.PendingApprovals()) != 0 {
		t.Error("registry still lists an unanswerable approval")
	}
}

// ---- store round-trips ----

func TestStoreTranscriptRoundTripAndResume(t *testing.T) {
	_, _, st := testManager(t)
	events := []protocol.TranscriptEvent{
		{SessionID: "s1", Seq: 1, Kind: protocol.EventUserMessage, Text: "hi", At: time.Now()},
		{SessionID: "s1", Seq: 2, Kind: protocol.EventToolCall, ToolName: "Bash", At: time.Now()},
		{SessionID: "s1", Seq: 3, Kind: protocol.EventToolResult, ToolName: "Bash", At: time.Now()},
	}
	if err := st.AppendEvents(events); err != nil {
		t.Fatal(err)
	}
	// Re-ingesting the same file after a restart must be idempotent, not an error.
	if err := st.AppendEvents(events); err != nil {
		t.Fatalf("re-appending should be idempotent: %v", err)
	}

	got, err := st.Transcript("s1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (duplicates must be ignored)", len(got))
	}
	if got[0].Text != "hi" || got[1].ToolName != "Bash" {
		t.Errorf("round-trip lost content: %+v", got)
	}

	// Resume from a seq, so a restart does not re-read a 143 MB file.
	since, err := st.Transcript("s1", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 || since[0].Seq != 3 {
		t.Errorf("since=2 returned %d events, want just seq 3", len(since))
	}

	max, err := st.MaxSeq("s1")
	if err != nil || max != 3 {
		t.Errorf("MaxSeq = %d (err %v), want 3", max, err)
	}
	if max, _ := st.MaxSeq("nonexistent"); max != 0 {
		t.Errorf("MaxSeq for an unknown session = %d, want 0", max)
	}
}

func TestStoreAuditAppend(t *testing.T) {
	_, _, st := testManager(t)
	for _, a := range []string{"input_sent", "approval_resolved", "session_started"} {
		if err := st.Audit("cli", a, "s1", "detail"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := st.AuditLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d audit entries, want 3", len(entries))
	}
	// Newest first.
	if entries[0].Action != "session_started" {
		t.Errorf("audit order wrong: first = %s", entries[0].Action)
	}
}

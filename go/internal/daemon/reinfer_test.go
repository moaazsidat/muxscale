package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Regression: state was inferred only when new transcript lines arrived, and a
// line that has just arrived always reads as "running". With no hook to say the
// turn was over, a session stayed Working forever once its agent went quiet.
func TestQuietTranscriptIsReinferred(t *testing.T) {
	m, reg, _ := testManager(t)

	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	h := &tailHandle{ref: path}

	s := reg.Upsert("claude-code", "k", func(s *protocol.Session) {
		s.Attachment = protocol.AttachTermctl
	})
	reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning, Confidence: 0.65})
	last := []protocol.TranscriptEvent{{Kind: protocol.EventAssistantMessage, Text: "done"}}

	m.reinfer(s.ID, h, last)
	if got, _ := reg.Get(s.ID); got.State != protocol.StateWaitingInput {
		t.Fatalf("state = %s, want WAITING_INPUT once the transcript has been quiet", got.State)
	}

	// What an agent reported is never replaced by a guess from silence.
	reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning, Confidence: 1, At: time.Now()})
	m.reinfer(s.ID, h, last)
	if got, _ := reg.Get(s.ID); got.State != protocol.StateRunning {
		t.Fatalf("state = %s, want the reported RUNNING kept", got.State)
	}
}

// Regression: after a restart a hook-driven session was restored as a
// certain "unknown", which shut out transcript inference, so an idle one read
// as unknown until someone next prompted it.
func TestRestoredHookSessionIsInferredUntilItsNextHook(t *testing.T) {
	m, reg, st := testManager(t)
	now := time.Now()
	if err := st.SaveSession(&protocol.Session{ID: "h", AgentID: "claude-code", NaturalKey: "n",
		Attachment: protocol.AttachNative, State: protocol.StateWaitingInput,
		StateSince: now, CreatedAt: now, LastEventAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	m.reinfer("h", &tailHandle{ref: path},
		[]protocol.TranscriptEvent{{Kind: protocol.EventAssistantMessage, Text: "done"}})
	if got, _ := reg.Get("h"); got.State != protocol.StateWaitingInput {
		t.Fatalf("state = %s, want WAITING_INPUT inferred from the quiet transcript", got.State)
	}
}

// Regression: a session the scan found kept its last state after its process
// exited -- often "needs you" -- and after a restart sat in Detached for good.
func TestSessionsWhoseProcessIsGoneAreEnded(t *testing.T) {
	m, reg, _ := testManager(t)
	gone := reg.Upsert("claude-code", "pid:111", nil)
	reg.SetState(gone.ID, protocol.StateGuess{State: protocol.StateWaitingInput, Confidence: 0.6})
	alive := reg.Upsert("claude-code", "pid:222", nil)
	reg.SetState(alive.ID, protocol.StateGuess{State: protocol.StateWaitingInput, Confidence: 0.6})
	wrapped := reg.Upsert("claude-code", "pid:333", func(s *protocol.Session) { s.Attachment = protocol.AttachWrap })
	hooked := reg.Upsert("claude-code", "abc-session", nil)

	m.markEnded(map[int]bool{222: true}, nil, false)

	if s, _ := reg.Get(gone.ID); s.State != protocol.StateExited || s.Guess.Confidence != 1 {
		t.Errorf("gone: %s %v, want EXITED at 1", s.State, s.Guess.Confidence)
	}
	for _, id := range []string{alive.ID, wrapped.ID, hooked.ID} {
		if s, _ := reg.Get(id); s.State.Terminal() {
			t.Errorf("%s was ended, but its process is running or reports its own exit", s.NaturalKey)
		}
	}
}

func TestHelperProcessesAreNotSessions(t *testing.T) {
	for _, argv := range [][]string{
		{"claude", "--chrome-native-host"},
		{"claude", "mcp", "serve"},
	} {
		if !helperProcess(argv) {
			t.Errorf("%v should not be listed as a session", argv)
		}
	}
	if helperProcess([]string{"claude", "--dangerously-skip-permissions"}) {
		t.Error("an interactive session was taken for a helper")
	}
}

// The same for a tmux pane, but only when the tmux scan could say which panes
// exist: a failed listing must not end every tmux session.
func TestTmuxSessionsEndWithTheirPane(t *testing.T) {
	m, reg, _ := testManager(t)
	gone := reg.Upsert("codex", "tmux:%1", func(s *protocol.Session) { s.Attachment = protocol.AttachTmux })
	kept := reg.Upsert("codex", "tmux:%2", func(s *protocol.Session) { s.Attachment = protocol.AttachTmux })

	m.markEnded(nil, nil, false)
	if s, _ := reg.Get(gone.ID); s.State.Terminal() {
		t.Fatal("ended on a tmux listing that failed")
	}
	m.markEnded(nil, map[string]bool{"%2": true}, true)
	if s, _ := reg.Get(gone.ID); s.State != protocol.StateExited {
		t.Errorf("pane %%1 is gone, state = %s", s.State)
	}
	if s, _ := reg.Get(kept.ID); s.State.Terminal() {
		t.Error("pane %2 is still there")
	}
}

// Regression: a hook said a turn started and the hook saying it ended never
// came (Claude Code sends no Stop when a turn is interrupted; an extension's
// post can fail), so the session read Working until its next prompt. Once the
// transcript has been written since and gone quiet, its end of turn stands --
// and only that: not a guess that the agent is busy, not over an approval.
func TestStaleHookRunningGivesWayToTheTranscript(t *testing.T) {
	m, reg, _ := testManager(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &tailHandle{ref: path}
	s := reg.Upsert("claude-code", "k", func(s *protocol.Session) { s.Attachment = protocol.AttachNative })
	written := func(ago time.Duration) {
		at := time.Now().Add(-ago)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	hook := func(ago time.Duration) {
		reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateWaitingInput, Confidence: 1, At: time.Now()})
		reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning, Confidence: 1, At: time.Now().Add(-ago)})
	}
	state := func() protocol.SessionState { got, _ := reg.Get(s.ID); return got.State }
	done := []protocol.TranscriptEvent{{Kind: protocol.EventAssistantMessage, Text: "done"}}
	interrupted := []protocol.TranscriptEvent{{Kind: protocol.EventUserMessage, Text: "[Request interrupted by user]"}}
	busy := []protocol.TranscriptEvent{{Kind: protocol.EventToolCall, ToolName: "Bash"}}

	// Written after the hook, quiet for two minutes: the turn is over.
	hook(10 * time.Minute)
	written(2 * time.Minute)
	m.reinfer(s.ID, h, interrupted)
	if state() != protocol.StateWaitingInput {
		t.Fatalf("interrupted turn = %s", state())
	}

	// Quiet for only twenty seconds: an agent may be thinking; keep the hook.
	hook(10 * time.Minute)
	written(20 * time.Second)
	m.reinfer(s.ID, h, done)
	if state() != protocol.StateRunning {
		t.Errorf("briefly quiet = %s", state())
	}

	// Nothing written since the hook: it is the newest word, a long tool.
	hook(time.Minute)
	written(10 * time.Minute)
	m.reinfer(s.ID, h, done)
	if state() != protocol.StateRunning {
		t.Errorf("not written since = %s", state())
	}

	// The transcript says busy: that adds nothing to the hook.
	hook(10 * time.Minute)
	written(2 * time.Minute)
	m.reinfer(s.ID, h, busy)
	if got, _ := reg.Get(s.ID); got.State != protocol.StateRunning || got.Guess.Confidence != 1 {
		t.Errorf("busy transcript replaced the hook: %+v", got.Guess)
	}
}

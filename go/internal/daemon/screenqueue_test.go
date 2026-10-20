package daemon

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/codex"
	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
)

// A real terminal, drawing Codex's queue preview, read into the session; and
// read again once the preview is gone.
func TestScreenQueueReadsTheTerminal(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := NewRegistry()
	adapters := adapter.NewRegistry()
	adapters.MustRegister(codex.New())
	m := NewManager(reg, adapters, st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	parent := agentSession(t, reg, t.TempDir())
	sh, err := m.RunShell(parent.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { waitForShellExit(t, m, sh.ID) })
	// Stand the shell in for a Codex session that is working.
	reg.Update(sh.ID, func(s *protocol.Session) { s.AgentID = "codex" })
	reg.SetState(sh.ID, protocol.StateGuess{State: protocol.StateRunning, Confidence: 1})

	draw := `clear; printf '\342\200\242 Queued follow-up inputs\n  \342\206\263 then run the tests\n'` + "\r"
	if err := m.SendRaw(sh.ID, []byte(draw), "test"); err != nil {
		t.Fatal(err)
	}
	wait := func(want int) []protocol.QueuedMessage {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			m.readScreenQueues(false)
			if s, _ := reg.Get(sh.ID); len(s.Queued) == want {
				return s.Queued
			}
			time.Sleep(50 * time.Millisecond)
		}
		s, _ := reg.Get(sh.ID)
		text, _ := m.ScreenText(sh.ID)
		t.Fatalf("queued = %+v, want %d; screen:\n%s", s.Queued, want, text)
		return nil
	}
	q := wait(1)
	if q[0].Text != "then run the tests" || !q[0].Partial || q[0].Delivery != "queue" {
		t.Fatalf("queued = %+v", q)
	}

	if err := m.SendRaw(sh.ID, []byte("clear\r"), "test"); err != nil {
		t.Fatal(err)
	}
	wait(0)
}

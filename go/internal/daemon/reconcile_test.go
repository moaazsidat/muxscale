package daemon

import (
	"io"
	"log/slog"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
	"github.com/muxscale/muxscale/go/internal/store"
)

func reconcileFixture(t *testing.T) (*Manager, *Registry) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := NewRegistry()
	return NewManager(reg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil))), reg
}

// Regression: one real Claude Code session appeared twice -- once from the
// process scan (keyed on pid, READONLY) and once from its own hooks (keyed on
// the agent's session id, NATIVE_FULL). Both are correct discoveries of the
// same agent, and without reconciliation a phone shows two rows for one
// session. Found by running a live `claude -p` against the daemon.
func TestReconcileDropsWeakerDuplicate(t *testing.T) {
	m, reg := reconcileFixture(t)
	const transcript = "/home/dev/.claude/projects/-home-dev-app/abc.jsonl"

	scanned := reg.Upsert("claude-code", "pid:1234", func(s *protocol.Session) {
		s.TranscriptPath = transcript
		s.Attachment = protocol.AttachTranscript
		s.Fidelity = protocol.FidelityReadOnly
	})
	hooked := reg.Upsert("claude-code", "sess-abc", func(s *protocol.Session) {
		s.TranscriptPath = transcript
		s.Attachment = protocol.AttachNative
		s.Fidelity = protocol.FidelityNativeFull
	})

	if len(reg.List()) != 2 {
		t.Fatalf("setup produced %d sessions, want 2", len(reg.List()))
	}

	m.Reconcile(hooked.ID, transcript)

	got := reg.List()
	if len(got) != 1 {
		t.Fatalf("after reconcile there are %d sessions, want 1", len(got))
	}
	if got[0].ID != hooked.ID {
		t.Errorf("kept %s, want the NATIVE_FULL session %s", got[0].ID, hooked.ID)
	}
	if _, still := reg.Get(scanned.ID); still {
		t.Error("the weaker duplicate is still retrievable")
	}
}

// Reconciliation must never drop the richer attachment, whichever order the two
// discoveries happened in.
func TestReconcileNeverDropsRicherSession(t *testing.T) {
	m, reg := reconcileFixture(t)
	const transcript = "/t/abc.jsonl"

	rich := reg.Upsert("claude-code", "sess-abc", func(s *protocol.Session) {
		s.TranscriptPath = transcript
		s.Fidelity = protocol.FidelityNativeFull
	})
	weak := reg.Upsert("claude-code", "pid:1", func(s *protocol.Session) {
		s.TranscriptPath = transcript
		s.Fidelity = protocol.FidelityReadOnly
	})

	// Reconcile from the weak session's point of view: it must not evict the
	// rich one just because it asked first.
	m.Reconcile(weak.ID, transcript)
	if _, ok := reg.Get(rich.ID); !ok {
		t.Error("the NATIVE_FULL session was dropped in favour of a READONLY one")
	}
}

func TestReconcileIgnoresDifferentTranscripts(t *testing.T) {
	m, reg := reconcileFixture(t)

	a := reg.Upsert("claude-code", "sess-a", func(s *protocol.Session) {
		s.TranscriptPath = "/t/a.jsonl"
		s.Fidelity = protocol.FidelityNativeFull
	})
	reg.Upsert("claude-code", "pid:2", func(s *protocol.Session) {
		s.TranscriptPath = "/t/b.jsonl"
		s.Fidelity = protocol.FidelityReadOnly
	})

	m.Reconcile(a.ID, "/t/a.jsonl")
	if n := len(reg.List()); n != 2 {
		t.Errorf("distinct sessions were merged: %d remain, want 2", n)
	}
}

func TestReconcileWithEmptyPathIsANoop(t *testing.T) {
	m, reg := reconcileFixture(t)
	s := reg.Upsert("claude-code", "sess-a", nil)
	m.Reconcile(s.ID, "")
	if n := len(reg.List()); n != 1 {
		t.Errorf("%d sessions remain, want 1", n)
	}
}

func TestFidelityRankOrdering(t *testing.T) {
	order := []protocol.Fidelity{
		protocol.FidelityReadOnly, protocol.FidelityStructured,
		protocol.FidelityFull, protocol.FidelityNativeFull,
	}
	for i := 1; i < len(order); i++ {
		if fidelityRank(order[i]) <= fidelityRank(order[i-1]) {
			t.Errorf("%s should outrank %s", order[i], order[i-1])
		}
	}
	// An unknown fidelity must rank lowest rather than accidentally winning a
	// reconciliation against a known-good attachment.
	if fidelityRank(protocol.Fidelity("nonsense")) != 0 {
		t.Error("an unknown fidelity should rank lowest")
	}
}

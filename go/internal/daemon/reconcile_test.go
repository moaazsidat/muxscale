package daemon

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/attach"
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
	m := NewManager(reg, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.StopPersisting)
	return m, reg
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

// One conversation found by a process scan and by session discovery must be
// one row. Equal fidelity used to keep both; the row carrying the person's
// rename was the one that then disappeared. The name has to survive the fold.
func TestReconcileFoldsEqualFidelityAndKeepsTheName(t *testing.T) {
	m, reg, _ := testManager(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	keep := reg.Upsert("claude-code", "pid:50113", func(s *protocol.Session) {
		s.Attachment = protocol.AttachTermctl
		s.Fidelity = protocol.FidelityStructured
		s.TranscriptPath = path
	})
	reg.Upsert("claude-code", "cdef6772", func(s *protocol.Session) {
		s.Name = "focus"
		s.Title = "Focus tool fixes"
		s.Attachment = protocol.AttachTranscript
		s.Fidelity = protocol.FidelityStructured
		s.TranscriptPath = path
	})

	m.Reconcile(keep.ID, path)

	if len(reg.List()) != 1 {
		t.Fatal("duplicate row survived the reconcile")
	}
	if id, _ := reg.IDFor("claude-code", "cdef6772"); id != keep.ID {
		t.Errorf("the folded row's key leads to %q, want the kept row %s", id, keep.ID)
	}
	got, ok := reg.Get(keep.ID)
	if !ok {
		t.Fatal("kept row disappeared")
	}
	if got.Name != "focus" || got.Title != "Focus tool fixes" {
		t.Errorf("kept row lost the name: %+v", got)
	}
}

// Only a wrapped session is certainly lost after a restart. Everything else
// can be found again, and a certain DETACHED shut inference out permanently.
func TestRestoreOnlyDetachesWrappedSessions(t *testing.T) {
	m, _, st := testManager(t)
	now := time.Now()
	for _, s := range []*protocol.Session{
		{ID: "wrap", AgentID: "claude-code", NaturalKey: "pid:1", State: protocol.StateRunning,
			Attachment: protocol.AttachWrap, CreatedAt: now, LastEventAt: now},
		{ID: "transcript", AgentID: "claude-code", NaturalKey: "uuid-1", State: protocol.StateRunning,
			Attachment: protocol.AttachTranscript, CreatedAt: now, LastEventAt: now},
	} {
		if err := st.SaveSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.reg.Get("wrap"); got.State != protocol.StateDetached || got.Guess.Confidence != 1 {
		t.Errorf("wrapped session = %s/%v", got.State, got.Guess.Confidence)
	}
	if got, _ := m.reg.Get("transcript"); got.State != protocol.StateUnknown || got.Guess.Confidence >= 1 {
		t.Errorf("transcript session = %s/%v", got.State, got.Guess.Confidence)
	}
}

// Regression: a Claude Code session in kitty changed id every few seconds
// while it worked. Each hook folded the scan's row away, taking the pid key
// and the kitty target with it; the next scan, finding no row for the pid,
// made a new one and adopted the pane again, and the next hook dropped that.
// A phone holding the session sent its reply to an id that was already gone.
func TestMergeHandsTheDroppedRowsKeysAndTerminalToTheKeptOne(t *testing.T) {
	m, reg := reconcileFixture(t)
	const transcript = "/t/abc.jsonl"

	scanned := reg.Upsert("claude-code", "pid:1234", func(s *protocol.Session) {
		s.TranscriptPath = transcript
		s.Attachment = protocol.AttachTermctl
		s.Fidelity = protocol.FidelityStructured
	})
	m.termctlTargets[scanned.ID] = attach.TermTarget{Kind: attach.TermKitty, ID: "2", Socket: "unix:/tmp/kitty-1"}
	hooked := reg.Upsert("claude-code", "sess-abc", func(s *protocol.Session) {
		s.TranscriptPath = transcript
		s.Attachment = protocol.AttachNative
		s.Fidelity = protocol.FidelityNativeFull
	})

	m.Reconcile(hooked.ID, transcript)

	if again := reg.Upsert("claude-code", "pid:1234", nil); again.ID != hooked.ID {
		t.Errorf("the next scan made row %s; the pid should lead to the kept row %s", again.ID, hooked.ID)
	}
	if len(reg.List()) != 1 {
		t.Errorf("%d rows, want 1", len(reg.List()))
	}
	if tgt, ok := m.termctlTargets[hooked.ID]; !ok || tgt.ID != "2" {
		t.Errorf("the kept row lost the kitty pane it can be replied through: %+v", m.termctlTargets)
	}
}

// A conversation resumed in a new process: the old row is hooked, so it
// outranks the new process's, but its process is gone. It used to win, keep
// the name and the hooks, and be ended by the scan while the new process
// worked, so a live session showed as done. Whichever row the reconcile
// starts from, the one with a running process is kept, under the old name.
func TestReconcileKeepsTheRowWithAProcessInIt(t *testing.T) {
	const transcript = "/t/resumed.jsonl"
	gone := "pid:" + strconv.Itoa(deadPID(t))
	live := "pid:" + strconv.Itoa(os.Getpid())

	for _, fromOld := range []bool{false, true} {
		m, reg := reconcileFixture(t)
		m.lastRunning, m.scanned = map[int]bool{os.Getpid(): true}, true // as a scan found them
		old := reg.Upsert("claude-code", gone, func(s *protocol.Session) {
			s.Name = "named"
			s.TranscriptPath = transcript
			s.Attachment = protocol.AttachNative
			s.Fidelity = protocol.FidelityNativeFull
		})
		reg.Alias("claude-code", "sess-resumed", old.ID)
		fresh := reg.Upsert("claude-code", live, func(s *protocol.Session) {
			s.TranscriptPath = transcript
			s.Attachment = protocol.AttachTermctl
			s.Fidelity = protocol.FidelityStructured
		})

		if fromOld {
			m.Reconcile(old.ID, transcript)
		} else {
			m.Reconcile(fresh.ID, transcript)
		}

		got := reg.List()
		if len(got) != 1 || got[0].ID != fresh.ID {
			t.Fatalf("fromOld=%v: kept %+v, want only the live process's row %s", fromOld, got, fresh.ID)
		}
		if got[0].Name != "named" {
			t.Errorf("fromOld=%v: name %q, want the old row's", fromOld, got[0].Name)
		}
		if id, ok := reg.IDFor("claude-code", "sess-resumed"); !ok || id != fresh.ID {
			t.Errorf("fromOld=%v: the conversation's hooks lead to %q, want %s", fromOld, id, fresh.ID)
		}
	}
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run a process:", err)
	}
	return cmd.Process.Pid
}

package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
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
	tl := claudecode.NewTailer(path, time.Second)

	s := reg.Upsert("claude-code", "k", func(s *protocol.Session) {
		s.Attachment = protocol.AttachTermctl
	})
	reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning, Confidence: 0.65})
	last := []protocol.TranscriptEvent{{Kind: protocol.EventAssistantMessage, Text: "done"}}

	m.reinfer(s.ID, tl, last)
	if got, _ := reg.Get(s.ID); got.State != protocol.StateWaitingInput {
		t.Fatalf("state = %s, want WAITING_INPUT once the transcript has been quiet", got.State)
	}

	// What an agent reported is never replaced by a guess from silence.
	reg.SetState(s.ID, protocol.StateGuess{State: protocol.StateRunning, Confidence: 1})
	m.reinfer(s.ID, tl, last)
	if got, _ := reg.Get(s.ID); got.State != protocol.StateRunning {
		t.Fatalf("state = %s, want the reported RUNNING kept", got.State)
	}
}

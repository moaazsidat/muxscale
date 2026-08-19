package attach

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression: Close used to lead with SIGTERM, which an interactive shell at a
// prompt ignores. Every shell tab closed from the phone therefore stalled for
// the full grace period and was then SIGKILLed, losing the shell's exit hooks
// and its history. Found by a 5-second test that should have taken microseconds.
func TestCloseHangsUpBeforeSignalling(t *testing.T) {
	// Every shell likely to be someone's $SHELL, because the fix that looked
	// right -- closing the PTY master -- passes for zsh and fails for the other
	// two on Darwin.
	for _, argv := range [][]string{
		{"/bin/sh", "-i"},
		{"/bin/bash", "-i"},
		{"/bin/zsh"},
	} {
		t.Run(filepath.Base(argv[0]), func(t *testing.T) {
			if _, err := os.Stat(argv[0]); err != nil {
				t.Skipf("%s is not installed here", argv[0])
			}
			w, err := Wrap(WrapOptions{Command: argv, Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}

			// Drain, and let the shell reach a prompt: one still sourcing its rc
			// files has not yet taken on the signal disposition this is about.
			go func() { _, _ = io.Copy(io.Discard, w) }()
			time.Sleep(500 * time.Millisecond)

			const grace = 5 * time.Second
			start := time.Now()
			if err := w.Close(grace); err != nil {
				t.Fatalf("Close: %v", err)
			}
			took := time.Since(start)

			// Exceeding the hangup budget means the shell survived SIGHUP and
			// was taken out by the escalation instead.
			if took >= hangupGrace(grace) {
				t.Errorf("Close took %v, want well under the %v hangup budget: "+
					"the shell was signalled rather than hung up", took, hangupGrace(grace))
			}
		})
	}
}

func TestHangupGraceLeavesBudgetForSigterm(t *testing.T) {
	for _, grace := range []time.Duration{time.Second, 5 * time.Second, time.Minute} {
		hg := hangupGrace(grace)
		if hg <= 0 || hg >= grace {
			t.Errorf("hangupGrace(%v) = %v, must be positive and leave time for SIGTERM", grace, hg)
		}
	}
}

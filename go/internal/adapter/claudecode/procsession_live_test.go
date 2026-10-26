package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func writeRecord(t *testing.T, home string, pid int, sessionID string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":"/w","kind":"interactive"}`, pid, sessionID)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A conversation is live while a running process records being in it, and
// not once that process is gone -- however recently its file was written.
func TestLiveFollowsClaudeCodesProcessRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRecord(t, home, os.Getpid(), "running")
	writeRecord(t, home, 0x7ffffffe, "ended") // no such process

	a := &Adapter{}
	if live, known := a.Live("/p/running.jsonl"); !known || !live {
		t.Fatalf("running conversation: live=%v known=%v", live, known)
	}
	if live, known := a.Live("/p/ended.jsonl"); !known || live {
		t.Fatalf("conversation whose process is gone: live=%v known=%v", live, known)
	}
	if live, known := a.Live("/p/never.jsonl"); !known || live {
		t.Fatalf("conversation with no record: live=%v known=%v", live, known)
	}
}

// One cut off by its terminal closing ends on an interrupted turn. With no
// process in it, that is an ended session, not one waiting for you.
func TestAnInterruptedConversationWithNoProcessHasEnded(t *testing.T) {
	notLive := false
	in := adapter.StateInput{
		Recent: []protocol.TranscriptEvent{{Kind: protocol.EventUserMessage, Text: "[Request interrupted by user for tool use]", At: time.Now()}},
		Idle:   time.Hour, Quiescence: 6 * time.Second, Live: &notLive,
	}
	g, ok := (&Adapter{}).InferState(in)
	if !ok || g.State != protocol.StateExited {
		t.Fatalf("got %+v ok=%v, want EXITED", g, ok)
	}
	// Still being written: only a process does that, record or not.
	in.Idle = time.Second
	if g, _ := (&Adapter{}).InferState(in); g.State == protocol.StateExited {
		t.Fatalf("a transcript being written was taken as ended: %+v", g)
	}
}

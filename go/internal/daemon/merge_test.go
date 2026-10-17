package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func userLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `{"type":"user","uuid":"u%d","sessionId":"s1","cwd":"/w",`+
			`"timestamp":"2026-10-17T10:00:00.000Z","message":{"role":"user","content":"message %d"}}`+"\n", i, i)
	}
	return b.String()
}

// Regression: after a merge the kept row's history stopped where it had last
// been the one recording, and the hour the other row recorded was missing on
// the phone. The kept row now reads the whole file again.
func TestMergeRebuildsTheKeptTranscript(t *testing.T) {
	m, reg, st := testManager(t)
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(path, []byte(userLines(5)), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := reg.Upsert("claude-code", "pid:1", nil)
	dup := reg.Upsert("claude-code", "s1", func(s *protocol.Session) { s.TranscriptPath = path })
	// The kept row holds only the start of the conversation.
	if err := st.AppendEvents([]protocol.TranscriptEvent{
		{SessionID: keep.ID, Seq: 1, Kind: protocol.EventUserMessage, Text: "message 1", At: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	m.Merge(keep.ID, dup.ID)

	deadline := time.Now().Add(5 * time.Second)
	for {
		events, _ := st.Transcript(keep.ID, 0, 100)
		if len(events) == 5 && events[4].Text == "message 5" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kept row has %d events after the merge, want all 5: %+v", len(events), events)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := reg.Get(dup.ID); ok {
		t.Fatal("the duplicate is still there")
	}
}

// A conversation is opened at its end: the newest page first, then older.
func TestTranscriptReadsBackwardsFromTheEnd(t *testing.T) {
	_, _, st := testManager(t)
	var evs []protocol.TranscriptEvent
	for i := 1; i <= 12; i++ {
		evs = append(evs, protocol.TranscriptEvent{SessionID: "s", Seq: uint64(i), Kind: protocol.EventUserMessage, At: time.Now()})
	}
	if err := st.AppendEvents(evs); err != nil {
		t.Fatal(err)
	}
	page, more, err := st.TranscriptBefore("s", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(page) != 5 || page[0].Seq != 8 || page[4].Seq != 12 {
		t.Fatalf("newest page: more=%v %v", more, seqs(page))
	}
	page, more, _ = st.TranscriptBefore("s", 3, 5)
	if more || len(page) != 2 || page[0].Seq != 1 {
		t.Fatalf("oldest page: more=%v %v", more, seqs(page))
	}
}

func seqs(evs []protocol.TranscriptEvent) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

// Regression: events reached the store through the registry's change feed,
// which drops what does not fit its buffer. A session's first read is
// thousands of events at once, and most of them were never stored.
func TestALargeTranscriptIsStoredWhole(t *testing.T) {
	m, reg, st := testManager(t)
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, []byte(userLines(3000)), 0o600); err != nil {
		t.Fatal(err)
	}
	s := reg.Upsert("claude-code", "pid:7", nil)
	m.EnsureTailer(s.ID, path)

	deadline := time.Now().Add(10 * time.Second)
	for {
		n, _ := st.MaxSeq(s.ID)
		events, _ := st.Transcript(s.ID, 0, 5000)
		if len(events) == 3000 && n == 3000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored %d of 3000 events", len(events))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The one-time repair: a transcript stored in part is rebuilt from its file.
func TestRereadTranscriptsOnceFillsTheHoles(t *testing.T) {
	_, _, st := testManager(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(userLines(40)), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.SaveSession(&protocol.Session{ID: "s", AgentID: "claude-code", NaturalKey: "k",
		TranscriptPath: path, State: protocol.StateIdle, StateSince: now, CreatedAt: now, LastEventAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvents([]protocol.TranscriptEvent{
		{SessionID: "s", Seq: 3, Kind: protocol.EventUserMessage, Text: "message 3", At: now},
		{SessionID: "s", Seq: 30, Kind: protocol.EventUserMessage, Text: "message 30", At: now},
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := rereadTranscriptsOnce(st); err != nil || n != 1 {
		t.Fatalf("reread %d, %v", n, err)
	}
	if events, _ := st.Transcript("s", 0, 100); len(events) != 40 {
		t.Fatalf("%d events after the reread, want 40", len(events))
	}
	if n, _ := rereadTranscriptsOnce(st); n != 0 {
		t.Fatal("the reread ran twice")
	}
}

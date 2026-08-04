package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func userLine(uuid, text string) string {
	return `{"type":"user","uuid":"` + uuid + `","sessionId":"s1","cwd":"/app",` +
		`"timestamp":"2026-08-04T10:00:00.000Z","message":{"role":"user","content":"` + text + `"}}`
}

func TestTailerEmitsOnlyNewEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, userLine("u1", "first"))

	tl := NewTailer(path, time.Millisecond)
	events, _, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "first" {
		t.Fatalf("first poll = %+v", events)
	}

	// Nothing new.
	events, _, err = tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("second poll returned %d events, want 0", len(events))
	}

	// Append; only the new line should come back.
	writeLines(t, path, userLine("u2", "second"))
	events, _, err = tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "second" {
		t.Fatalf("third poll = %+v", events)
	}
}

// Seq must continue across polls, because clients resume by seq. Restarting
// numbering each poll would make every reconnect replay or skip content.
func TestTailerSeqIsGloballyMonotonic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tl := NewTailer(path, time.Millisecond)
	writeLines(t, path, userLine("u1", "a"), userLine("u2", "b"))
	first, _, _ := tl.Poll()
	writeLines(t, path, userLine("u3", "c"))
	second, _, _ := tl.Poll()

	all := append(first, second...)
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	for i, e := range all {
		if e.Seq != uint64(i+1) {
			t.Errorf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}
}

// A torn final line is normal: Claude Code is writing while we read. It must be
// completed on the next poll, not dropped.
func TestTailerHoldsPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	full := userLine("u1", "complete")
	// Write the line in two halves with no trailing newline on the first.
	if err := os.WriteFile(path, []byte(full[:40]), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := NewTailer(path, time.Millisecond)
	events, _, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("a partial line should yield nothing yet, got %d events", len(events))
	}

	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(full[40:] + "\n")
	f.Close()

	events, _, err = tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "complete" {
		t.Errorf("completed line not emitted: %+v", events)
	}
}

// A resumed session can rewrite its file. Reading from a stale offset would
// land mid-line and produce garbage, so truncation must reset.
func TestTailerHandlesTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, userLine("u1", "one"), userLine("u2", "two"))
	tl := NewTailer(path, time.Millisecond)
	if _, _, err := tl.Poll(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(userLine("u9", "rewritten")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, _, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "rewritten" {
		t.Errorf("after truncation got %+v", events)
	}
}

func TestTailerSeekEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, userLine("u1", "old"))
	tl := NewTailer(path, time.Millisecond)
	if err := tl.SeekEnd(5); err != nil {
		t.Fatal(err)
	}
	events, _, _ := tl.Poll()
	if len(events) != 0 {
		t.Errorf("SeekEnd should skip existing content, got %d events", len(events))
	}
	writeLines(t, path, userLine("u2", "new"))
	events, _, _ = tl.Poll()
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	if events[0].Seq != 6 {
		t.Errorf("seq = %d, want 6 (continuing from the stored max)", events[0].Seq)
	}
}

// Verified against the real layout on disk.
func TestProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/Users/sam/dev/muxscale": "-Users-sam-dev-muxscale",
		"/home/dev/app":             "-home-dev-app",
		"/a/b/c/":                   "-a-b-c",
	}
	for in, want := range cases {
		if got := ProjectSlug(in); got != want {
			t.Errorf("ProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// Inference must stay clearly below the hook path's confidence of 1.0. The
// product's credibility depends on not dressing a guess up as a fact.
func TestInferStateConfidenceStaysBelowHooks(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		events []protocol.TranscriptEvent
		mod    time.Time
		want   protocol.SessionState
	}{
		{"empty", nil, now, protocol.StateUnknown},
		{"recent tool call",
			[]protocol.TranscriptEvent{{Kind: protocol.EventToolCall, ToolName: "Bash"}},
			now, protocol.StateRunning},
		{"quiet after assistant",
			[]protocol.TranscriptEvent{{Kind: protocol.EventAssistantMessage}},
			now.Add(-time.Hour), protocol.StateWaitingInput},
		{"quiet after tool result",
			[]protocol.TranscriptEvent{{Kind: protocol.EventToolResult}},
			now.Add(-time.Hour), protocol.StateUnknown},
	}
	for _, c := range cases {
		g := InferState(c.events, c.mod, 6*time.Second)
		if g.State != c.want {
			t.Errorf("%s: state = %s, want %s", c.name, g.State, c.want)
		}
		if g.Confidence >= 1.0 {
			t.Errorf("%s: confidence %v must stay below the hook path's 1.0", c.name, g.Confidence)
		}
		if g.Evidence == "" {
			t.Errorf("%s: every guess needs evidence for `debug state`", c.name)
		}
	}
}

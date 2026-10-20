package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func qop(op, content string) string {
	if content == "" {
		return `{"type":"queue-operation","operation":"` + op + `","timestamp":"2026-10-19T10:00:00Z","sessionId":"s"}`
	}
	return `{"type":"queue-operation","operation":"` + op + `","timestamp":"2026-10-19T10:00:00Z","sessionId":"s","content":` + quote(content) + `}`
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, "\n", `\n`) + `"` }

func texts(t *testing.T, m Meta) []string {
	t.Helper()
	if m.Queue == nil {
		t.Fatal("queue not reported")
	}
	var out []string
	for _, q := range *m.Queue {
		out = append(out, q.Text)
	}
	return out
}

// The operations as Claude Code writes them, replayed: two typed while busy,
// the first taken as the next prompt; a task notification queued between
// them and taken mid-turn never shows.
func TestQueueReplay(t *testing.T) {
	lines := []string{
		qop("enqueue", "also run the tests"),
		qop("enqueue", "<task-notification>\n<task-id>b1</task-id>"),
		qop("enqueue", "then push"),
		qop("remove", "<task-notification>\n<task-id>b1</task-id>"),
		qop("dequeue", ""),
	}
	_, meta, err := ParseTranscript(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(t, meta); len(got) != 1 || got[0] != "then push" {
		t.Fatalf("queue = %q", got)
	}

	// Pulled back into the editor: empty, and said so.
	_, meta, _ = ParseTranscript(strings.NewReader(strings.Join(append(lines, qop("popAll", "then push")), "\n")))
	if got := texts(t, meta); len(got) != 0 {
		t.Fatalf("after popAll = %q", got)
	}

	// A transcript with no queue operations says nothing about a queue.
	_, meta, _ = ParseTranscript(strings.NewReader(`{"type":"ai-title","aiTitle":"x"}`))
	if meta.Queue != nil {
		t.Error("reported a queue that was never mentioned")
	}
}

// A dequeue in a later chunk takes from an enqueue in an earlier one, and a
// tailer that skipped history (SeekEnd) rebuilds the queue from the file.
func TestQueueAcrossPollsAndSeekEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write := func(lines ...string) {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		defer f.Close()
		for _, l := range lines {
			f.WriteString(l + "\n")
		}
	}
	write(qop("enqueue", "first"), qop("enqueue", "second"))
	tl := NewTailer(path, time.Second)
	_, meta, _ := tl.Poll()
	if got := texts(t, meta); len(got) != 2 {
		t.Fatalf("first poll = %q", got)
	}
	write(qop("dequeue", ""))
	_, meta, _ = tl.Poll()
	if got := texts(t, meta); len(got) != 1 || got[0] != "second" {
		t.Fatalf("after dequeue = %q", got)
	}
	_, meta, _ = tl.Poll()
	if meta.Queue != nil {
		t.Error("a quiet poll reported the queue again")
	}

	fresh := NewTailer(path, time.Second)
	if err := fresh.SeekEnd(0); err != nil {
		t.Fatal(err)
	}
	_, meta, _ = fresh.Poll()
	if got := texts(t, meta); len(got) != 1 || got[0] != "second" {
		t.Fatalf("rebuilt = %q", got)
	}
}

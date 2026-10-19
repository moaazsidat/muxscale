package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Lines as ChatRecordingService writes them (gemini-cli main, October 2026):
// metadata first; a message re-appended whole when its tool call finishes;
// $set for metadata.
const (
	lineMeta    = `{"sessionId":"5f1c2a9e-1111-4222-8333-944455556666","projectHash":"app","startTime":"2026-10-19T10:00:00.000Z","lastUpdated":"2026-10-19T10:00:00.000Z","kind":"main"}`
	lineUser    = `{"id":"m1","timestamp":"2026-10-19T10:00:01.000Z","type":"user","content":[{"text":"add a health endpoint"}]}`
	lineRunning = `{"id":"m2","timestamp":"2026-10-19T10:00:02.000Z","type":"gemini","content":"","model":"gemini-3-pro","thoughts":[{"subject":"Plan","description":"read the router","timestamp":"2026-10-19T10:00:02.000Z"}],"toolCalls":[{"id":"call-1","name":"read_file","args":{"path":"router.ts"},"status":"executing","timestamp":"2026-10-19T10:00:02.500Z"}]}`
	lineDone    = `{"id":"m2","timestamp":"2026-10-19T10:00:02.000Z","type":"gemini","content":"","model":"gemini-3-pro","thoughts":[{"subject":"Plan","description":"read the router","timestamp":"2026-10-19T10:00:02.000Z"}],"toolCalls":[{"id":"call-1","name":"read_file","args":{"path":"router.ts"},"status":"success","result":[{"functionResponse":{"id":"call-1","name":"read_file","response":{"output":"export const router"}}}],"timestamp":"2026-10-19T10:00:02.500Z"}]}`
	lineSummary = `{"$set":{"summary":"Health endpoint","lastUpdated":"2026-10-19T10:00:04.000Z"}}`
	lineAnswer  = `{"id":"m3","timestamp":"2026-10-19T10:00:04.000Z","type":"gemini","content":"Added GET /health.","model":"gemini-3-pro","tokens":{"input":10,"output":5,"cached":0,"total":15}}`
)

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (dir string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".gemini")
	dir = filepath.Join(root, "tmp", "app", "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects.json"),
		[]byte(`{"projects":{"/home/dev/app":"app"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func kinds(evs []protocol.TranscriptEvent) string {
	var k []string
	for _, e := range evs {
		k = append(k, string(e.Kind))
	}
	return strings.Join(k, ",")
}

func TestReplayStreamAndState(t *testing.T) {
	dir := setup(t)
	path := filepath.Join(dir, "session-2026-10-19T10-00-5f1c2a9e.jsonl")
	write(t, path, lineMeta, lineUser, lineRunning)

	a := New()
	got, err := a.Discover(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("discovered %+v, %v", got, err)
	}
	d := got[0]
	if d.NaturalKey != "5f1c2a9e-1111-4222-8333-944455556666" || d.Cwd != "/home/dev/app" || d.Title != "add a health endpoint" || !d.Live {
		t.Errorf("discovered = %+v", d)
	}

	ts, _ := a.OpenTranscript(context.Background(), path)
	first, _ := ts.Poll(context.Background())
	// The running call is shown; its result waits.
	if k := kinds(first); k != "user_message,thinking,tool_call" {
		t.Fatalf("first read = %s", k)
	}
	if first[2].ToolName != "read_file" || first[2].ToolInput["path"] != "router.ts" {
		t.Errorf("tool call = %+v", first[2])
	}
	life := ts.(adapter.EventReporter).LastEvent()
	if g, _ := a.InferState(adapter.StateInput{Event: life}); g.State != protocol.StateRunning {
		t.Errorf("running tool -> %+v", g)
	}

	// The call finishes (its message is re-appended), a summary is set, and
	// the model answers.
	write(t, path, lineMeta, lineUser, lineRunning, lineDone, lineSummary, lineAnswer)
	next, _ := ts.Poll(context.Background())
	if k := kinds(next); k != "tool_result,assistant_message" {
		t.Fatalf("second read = %s", k)
	}
	if next[0].Text != "export const router" || next[0].Seq != 4 || next[1].Text != "Added GET /health." || next[1].Seq != 5 {
		t.Errorf("second read = %+v", next)
	}
	life = ts.(adapter.EventReporter).LastEvent()
	if g, _ := a.InferState(adapter.StateInput{Event: life, Idle: time.Minute, Quiescence: 6 * time.Second}); g.State != protocol.StateWaitingInput {
		t.Errorf("answered and quiet -> %+v", g)
	}
	if got, _ := a.Discover(context.Background()); got[0].Title != "Health endpoint" {
		t.Errorf("title = %q, want the summary", got[0].Title)
	}

	// A process started with the session joins it.
	started := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if m := a.MatchTranscript("/home/dev/app", started, nil); m != path {
		t.Errorf("join = %q", m)
	}
}

func TestRewindAndApproval(t *testing.T) {
	dir := setup(t)
	path := filepath.Join(dir, "session-2026-10-19T10-00-5f1c2a9e.jsonl")
	awaiting := strings.Replace(lineRunning, `"executing"`, `"awaiting_approval"`, 1)
	write(t, path, lineMeta, lineUser, lineDone, lineAnswer, `{"$rewindTo":"m2"}`, awaiting)
	f, _ := os.Open(path)
	c, err := replay(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.messages) != 2 || c.messages[1].ToolCalls[0].Status != "awaiting_approval" {
		t.Fatalf("after rewind: %+v", c.messages)
	}
	if g, _ := stateFromLifecycle(c.lifecycle(), time.Minute, 6*time.Second); g.State != protocol.StateWaitingApproval {
		t.Errorf("awaiting approval -> %+v", g)
	}
}

func TestLegacySingleObjectSession(t *testing.T) {
	legacy := `{
  "sessionId": "legacy-1",
  "projectHash": "abc",
  "startTime": "2026-01-01T00:00:00.000Z",
  "lastUpdated": "2026-01-01T00:01:00.000Z",
  "messages": [
    {"id": "a", "timestamp": "2026-01-01T00:00:01.000Z", "type": "user", "content": "hi"},
    {"id": "b", "timestamp": "2026-01-01T00:00:02.000Z", "type": "gemini", "content": "hello"}
  ]
}`
	c, err := replay(strings.NewReader(legacy))
	if err != nil || c.SessionID != "legacy-1" || len(c.messages) != 2 {
		t.Fatalf("legacy = %+v, %v", c, err)
	}
	if k := kinds(eventsOf(c)); k != "user_message,assistant_message" {
		t.Errorf("legacy events = %s", k)
	}
}

func eventsOf(c *conversation) []protocol.TranscriptEvent {
	var out []protocol.TranscriptEvent
	for _, u := range c.units() {
		out = append(out, u.Events...)
	}
	return out
}

func TestDetect(t *testing.T) {
	a := New()
	for _, argv := range [][]string{{"gemini"}, {"node", "/usr/lib/node_modules/@google/gemini-cli/dist/index.js"}} {
		if !a.Detect(adapter.Process{Argv: argv}) {
			t.Errorf("missed %v", argv)
		}
	}
	if a.Detect(adapter.Process{Argv: []string{"node", "server.js"}}) {
		t.Error("claimed an unrelated node process")
	}
}

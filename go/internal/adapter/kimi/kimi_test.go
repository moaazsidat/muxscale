package kimi

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

// The first lines are kimi-code's own vis fixture (sample-main); the rest
// follow the wire manifest's payloads for tool calls, approvals and turn end.
var wire = []string{
	`{"type":"metadata","protocol_version":"1.5","created_at":1779256791085}`,
	`{"type":"config.update","cwd":"/tmp/work","profileName":"agent","systemPrompt":"You are Kimi.","time":1779256791100}`,
	`{"type":"turn.prompt","input":[{"type":"text","text":"hi"}],"origin":{"kind":"user"},"time":1779256800000}`,
	`{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"hi"}],"toolCalls":[]},"time":1779256800001}`,
	`{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"s1","turnId":"t1","step":0},"time":1779256800100}`,
	`{"type":"context.append_loop_event","event":{"type":"content.part","uuid":"c0","turnId":"t1","step":0,"stepUuid":"s1","part":{"type":"think","think":"greet, then check"}},"time":1779256800150}`,
	`{"type":"context.append_loop_event","event":{"type":"content.part","uuid":"c1","turnId":"t1","step":0,"stepUuid":"s1","part":{"type":"text","text":"hello"}},"time":1779256800200}`,
	`{"type":"context.append_loop_event","event":{"type":"tool.call","stepUuid":"s1","toolCallId":"tc1","name":"Shell","args":{"command":"ls"}},"time":1779256800250}`,
	`{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"s1","turnId":"t1","step":0,"finishReason":"tool_calls"},"time":1779256800300}`,
	`{"type":"interaction.request","agentId":"main","id":"i1","kind":"approval","toolCallId":"tc1","request":{"action":"run Shell"},"time":1779256800310}`,
}

var rest = []string{
	`{"type":"interaction.resolved","agentId":"main","id":"i1","response":{"approved":true},"time":1779256805000}`,
	`{"type":"context.append_loop_event","event":{"type":"tool.result","toolCallId":"tc1","result":{"output":"README.md","isError":false}},"time":1779256805100}`,
	`{"type":"context.append_loop_event","event":{"type":"step.begin","uuid":"s2","turnId":"t1","step":1},"time":1779256805200}`,
	`{"type":"context.append_loop_event","event":{"type":"content.part","uuid":"c2","turnId":"t1","step":1,"stepUuid":"s2","part":{"type":"text","text":"There is a README."}},"time":1779256805300}`,
	`{"type":"context.append_loop_event","event":{"type":"step.end","uuid":"s2","turnId":"t1","step":1,"finishReason":"completed"},"time":1779256805400}`,
	`{"type":"context.append_message","message":{"role":"assistant","content":[{"type":"text","text":"There is a README."}],"toolCalls":[]},"time":1779256805401}`,
	`{"type":"turn.ended","agentId":"main","turnId":1,"reason":"completed","time":1779256805500}`,
}

func setup(t *testing.T, lines []string) (path string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", root)
	dir := filepath.Join(root, "sessions", workDirKey("/home/dev/app"), "sess-1")
	if err := os.MkdirAll(filepath.Join(dir, "agents", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(root, "session_index.jsonl"),
		[]byte(`{"sessionId":"sess-1","sessionDir":"`+dir+`","workDir":"/home/dev/app"}`+"\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"createdAt":"2026-05-20T05:59:51.085Z","updatedAt":"2026-05-20T06:00:05.500Z","title":"","lastPrompt":"hi there"}`), 0o600))
	path = wirePath(dir)
	writeLines(t, path, lines)
	return path
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	must(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

func kinds(evs []protocol.TranscriptEvent) string {
	var k []string
	for _, e := range evs {
		k = append(k, string(e.Kind))
	}
	return strings.Join(k, ",")
}

func TestWorkDirKeyMatchesKimi(t *testing.T) {
	// encodeWorkDirKey("/home/dev/My App!") in workdir-slug.ts.
	got := workDirKey("/home/dev/My App!")
	if !strings.HasPrefix(got, "wd_my-app_") || len(got) != len("wd_my-app_")+12 {
		t.Errorf("workDirKey = %q", got)
	}
	if workDirKey("/home/dev/app/") != workDirKey("/home/dev/app") {
		t.Error("a trailing slash changed the key")
	}
}

func TestSessionLifecycle(t *testing.T) {
	path := setup(t, wire)
	a := New()

	got, err := a.Discover(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("discovered %+v, %v", got, err)
	}
	if d := got[0]; d.NaturalKey != "sess-1" || d.Cwd != "/home/dev/app" || d.Title != "hi there" || d.Ref != path {
		t.Errorf("discovered = %+v", d)
	}

	ts, _ := a.OpenTranscript(context.Background(), path)
	first, _ := ts.Poll(context.Background())
	if k := kinds(first); k != "user_message,thinking,assistant_message,tool_call" {
		t.Fatalf("first read = %s", k)
	}
	if first[3].ToolName != "Shell" || first[3].ToolInput["command"] != "ls" {
		t.Errorf("tool call = %+v", first[3])
	}
	if g, _ := a.InferState(adapter.StateInput{Event: ts.(adapter.EventReporter).LastEvent()}); g.State != protocol.StateWaitingApproval {
		t.Fatalf("pending approval -> %+v", g)
	}

	writeLines(t, path, append(append([]string{}, wire...), rest...))
	next, _ := ts.Poll(context.Background())
	// The sealed assistant message repeats the step's text: shown once.
	if k := kinds(next); k != "tool_result,assistant_message" {
		t.Fatalf("second read = %s", k)
	}
	if next[0].Text != "README.md" || next[1].Text != "There is a README." {
		t.Errorf("second read = %+v", next)
	}
	if g, _ := a.InferState(adapter.StateInput{Event: ts.(adapter.EventReporter).LastEvent()}); g.State != protocol.StateWaitingInput {
		t.Fatalf("completed turn -> %+v", g)
	}

	started := time.Date(2026, 5, 20, 5, 59, 50, 0, time.UTC)
	if m := a.MatchTranscript("/home/dev/app", started, nil); m != path {
		t.Errorf("join = %q", m)
	}
}

func TestCancelledTurnClosesCalls(t *testing.T) {
	lines := append(append([]string{}, wire[:8]...),
		`{"type":"turn.cancel","agentId":"main","turnId":1,"reason":"user_cancelled","time":1779256800400}`)
	f := strings.NewReader(strings.Join(lines, "\n"))
	s, err := parse(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range s.units {
		if !u.Final {
			t.Fatalf("unit %s left open after a cancel", u.Key)
		}
	}
	if g, _ := stateFromLifecycle(s.life, 0, 0); g.State != protocol.StateWaitingInput {
		t.Errorf("cancelled -> %+v", g)
	}
}

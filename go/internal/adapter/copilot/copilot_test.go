package copilot

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

// Events shaped by the SDK's generated schema (session-events.ts).
var (
	evStart    = `{"id":"e1","parentId":null,"timestamp":"2026-10-19T10:00:00.000Z","type":"session.start","data":{"sessionId":"0b6f6a8e-1f0e-4c51-9b0b-3a1d2e3f4a5b","startTime":"2026-10-19T10:00:00.000Z","copilotVersion":"1.0.67","producer":"copilot-agent","version":1,"selectedModel":"claude-sonnet-5","context":{"cwd":"/home/dev/app","branch":"main"}}}`
	evUser     = `{"id":"e2","parentId":"e1","timestamp":"2026-10-19T10:00:01.000Z","type":"user.message","data":{"content":"run the tests"}}`
	evTurn     = `{"id":"e3","parentId":"e2","timestamp":"2026-10-19T10:00:02.000Z","type":"assistant.turn_start","data":{"turnId":"t1"}}`
	evReason   = `{"id":"e4","parentId":"e3","timestamp":"2026-10-19T10:00:02.100Z","type":"assistant.reasoning","data":{"reasoningId":"r1","content":"run pnpm test"}}`
	evAsk      = `{"id":"e5","parentId":"e4","timestamp":"2026-10-19T10:00:02.200Z","type":"assistant.message","data":{"messageId":"m1","content":"Running the tests.","model":"claude-sonnet-5","toolRequests":[{"toolCallId":"call-1","name":"bash","arguments":{"command":"pnpm test"}}]}}`
	evPerm     = `{"id":"e6","parentId":"e5","timestamp":"2026-10-19T10:00:02.300Z","type":"permission.requested","data":{"requestId":"p1","permissionRequest":{"kind":"shell","fullCommandText":"pnpm test"}}}`
	evPermDone = `{"id":"e7","parentId":"e6","timestamp":"2026-10-19T10:00:05.000Z","type":"permission.completed","data":{"requestId":"p1","result":{"kind":"approved"}}}`
	evToolDone = `{"id":"e8","parentId":"e7","timestamp":"2026-10-19T10:00:09.000Z","type":"tool.execution_complete","data":{"toolCallId":"call-1","success":true,"result":{"content":"12 passed"}}}`
	evTurnEnd1 = `{"id":"e9","parentId":"e8","timestamp":"2026-10-19T10:00:09.100Z","type":"assistant.turn_end","data":{"turnId":"t1"}}`
	evAnswer   = `{"id":"e10","parentId":"e9","timestamp":"2026-10-19T10:00:10.000Z","type":"assistant.message","data":{"messageId":"m2","content":"All 12 tests pass.","model":"claude-sonnet-5"}}`
	evTurnEnd2 = `{"id":"e11","parentId":"e10","timestamp":"2026-10-19T10:00:10.100Z","type":"assistant.turn_end","data":{"turnId":"t2"}}`
	evSubagent = `{"id":"e12","parentId":"e11","timestamp":"2026-10-19T10:00:10.200Z","type":"assistant.message","agentId":"sub-1","data":{"messageId":"m3","content":"inner work"}}`
)

func setup(t *testing.T, lines ...string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("COPILOT_HOME", root)
	dir := filepath.Join(root, "session-state", "0b6f6a8e-1f0e-4c51-9b0b-3a1d2e3f4a5b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	write(t, path, lines...)
	return path
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func kinds(evs []protocol.TranscriptEvent) string {
	var k []string
	for _, e := range evs {
		k = append(k, string(e.Kind))
	}
	return strings.Join(k, ",")
}

func TestSessionLifecycle(t *testing.T) {
	path := setup(t, evStart, evUser, evTurn, evReason, evAsk, evPerm)
	a := New()

	got, err := a.Discover(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("discovered %+v, %v", got, err)
	}
	if d := got[0]; d.NaturalKey != "0b6f6a8e-1f0e-4c51-9b0b-3a1d2e3f4a5b" || d.Cwd != "/home/dev/app" ||
		d.Title != "run the tests" || d.Model != "claude-sonnet-5" || !d.Live {
		t.Errorf("discovered = %+v", d)
	}

	ts, _ := a.OpenTranscript(context.Background(), path)
	first, _ := ts.Poll(context.Background())
	if k := kinds(first); k != "user_message,thinking,assistant_message,tool_call" {
		t.Fatalf("first read = %s", k)
	}
	if first[3].ToolInput["command"] != "pnpm test" {
		t.Errorf("tool input = %+v", first[3].ToolInput)
	}
	g, _ := a.InferState(adapter.StateInput{Event: ts.(adapter.EventReporter).LastEvent()})
	if g.State != protocol.StateWaitingApproval {
		t.Fatalf("pending permission -> %+v", g)
	}

	write(t, path, evStart, evUser, evTurn, evReason, evAsk, evPerm, evPermDone, evToolDone, evTurnEnd1, evAnswer, evTurnEnd2, evSubagent)
	next, _ := ts.Poll(context.Background())
	if k := kinds(next); k != "tool_result,assistant_message" {
		t.Fatalf("second read = %s", k)
	}
	if next[0].Text != "12 passed" || next[1].Text != "All 12 tests pass." {
		t.Errorf("second read = %+v", next)
	}
	g, _ = a.InferState(adapter.StateInput{Event: ts.(adapter.EventReporter).LastEvent(), Idle: time.Minute, Quiescence: 6 * time.Second})
	if g.State != protocol.StateWaitingInput {
		t.Fatalf("answered -> %+v", g)
	}

	started := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if m := a.MatchTranscript("/home/dev/app", started, nil); m != path {
		t.Errorf("join = %q", m)
	}
}

// An interrupted call never reports back; it must not hold up what follows.
func TestAbortClosesOpenCalls(t *testing.T) {
	path := setup(t, evStart, evUser, evTurn, evAsk,
		`{"id":"x1","timestamp":"2026-10-19T10:00:03.000Z","type":"abort","data":{"reason":"user_initiated"}}`,
		`{"id":"x2","timestamp":"2026-10-19T10:00:04.000Z","type":"user.message","data":{"content":"never mind"}}`)
	s, err := parseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all []protocol.TranscriptEvent
	for _, u := range s.units {
		if !u.Final {
			t.Fatalf("unit %s left open", u.Key)
		}
		all = append(all, u.Events...)
	}
	if k := kinds(all); k != "user_message,assistant_message,tool_call,tool_result,notice,user_message" {
		t.Errorf("events = %s", k)
	}
}

func TestShutdownEndsTheSession(t *testing.T) {
	path := setup(t, evStart, evUser,
		`{"id":"z","timestamp":"2026-10-19T10:00:03.000Z","type":"session.shutdown","data":{"shutdownType":"routine","sessionStartTime":0,"totalApiDurationMs":0,"codeChanges":{},"modelMetrics":{}}}`)
	got, _ := New().Discover(context.Background())
	if len(got) != 1 || got[0].Live {
		t.Fatalf("a shut-down session reported live: %+v", got)
	}
	s, _ := parseFile(path)
	if g, _ := stateFromLifecycle(s.life, 0, 0); g.State != protocol.StateExited {
		t.Errorf("shutdown -> %+v", g)
	}
}

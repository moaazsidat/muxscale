package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// This is the real payload pi 0.83.0 returned to `{"id":"1","type":"get_state"}`
// on this machine, captured verbatim.
const realGetStateResponse = `{"id":"1","type":"response","command":"get_state","success":true,"data":{"model":{"id":"unknown","name":"unknown","api":"unknown","provider":"unknown","baseUrl":"","reasoning":false,"input":[],"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"contextWindow":0,"maxTokens":0},"thinkingLevel":"off","isStreaming":false,"isCompacting":false,"steeringMode":"one-at-a-time","followUpMode":"one-at-a-time","sessionId":"019fcdbc-d8bb-7af8-be21-a95eb40b4def","autoCompactionEnabled":true,"messageCount":0,"pendingMessageCount":0}}`

func TestParseRealGetStateResponse(t *testing.T) {
	r := NewReader(strings.NewReader(realGetStateResponse + "\n"))
	m, ok := r.Next()
	if !ok {
		t.Fatal("no message decoded")
	}
	if !m.IsResponse() || m.Command != "get_state" || m.ID != "1" {
		t.Fatalf("message = %+v", m)
	}
	if m.Success == nil || !*m.Success {
		t.Error("success flag not decoded")
	}

	var st State
	if err := json.Unmarshal(m.Data, &st); err != nil {
		t.Fatal(err)
	}
	if st.SessionID != "019fcdbc-d8bb-7af8-be21-a95eb40b4def" {
		t.Errorf("sessionId = %q", st.SessionID)
	}
	if st.IsStreaming {
		t.Error("isStreaming should be false")
	}
	if st.SteeringMode != "one-at-a-time" {
		t.Errorf("steeringMode = %q", st.SteeringMode)
	}
}

// pi's docs are explicit: split on LF only, tolerate a trailing CR, and do NOT
// treat U+2028/U+2029 as line breaks (Node's readline is non-compliant for
// exactly that reason). Go's default behaviour is correct, but asserting it
// stops a future "improvement" from silently breaking the framing.
func TestReaderFramingRules(t *testing.T) {
	// U+2028 inside a JSON string must not split the message.
	withSep := "{\"type\":\"message_end\",\"role\":\"assistant\",\"content\":\"a b\"}\n"
	r := NewReader(strings.NewReader(withSep))
	m, ok := r.Next()
	if !ok {
		t.Fatal("U+2028 split the line; framing must use LF only")
	}
	events := ToTranscript(m)
	if len(events) != 1 || !strings.Contains(events[0].Text, " ") {
		t.Errorf("U+2028 content mangled: %+v", events)
	}

	// A trailing CR must be tolerated.
	r = NewReader(strings.NewReader("{\"type\":\"agent_settled\"}\r\n"))
	if _, ok := r.Next(); !ok {
		t.Error("trailing CR was not tolerated")
	}
}

func TestReaderSkipsGarbageWithoutEndingStream(t *testing.T) {
	input := "not json at all\n" +
		`{"type":"agent_settled"}` + "\n" +
		"\n" +
		`{"type":"agent_start"}` + "\n"
	r := NewReader(strings.NewReader(input))
	var types []string
	for {
		m, ok := r.Next()
		if !ok {
			break
		}
		types = append(types, m.Type)
	}
	if len(types) != 2 || types[0] != "agent_settled" || types[1] != "agent_start" {
		t.Errorf("got %v; a diagnostic line on stdout must not kill the session", types)
	}
}

// agent_settled IS waiting-for-input. No quiescence timer, no regex. This is
// the entire argument for native adapters.
func TestStateMappingIsExact(t *testing.T) {
	cases := map[string]protocol.SessionState{
		EventAgentSettled:    protocol.StateWaitingInput,
		EventAgentStart:      protocol.StateRunning,
		EventTurnStart:       protocol.StateRunning,
		EventToolExecStart:   protocol.StateRunning,
		EventCompactionStart: protocol.StateRunning,
		EventExtensionUIReq:  protocol.StateWaitingApproval,
	}
	for event, want := range cases {
		g, ok := InferState(event)
		if !ok {
			t.Errorf("%s produced no state", event)
			continue
		}
		if g.State != want {
			t.Errorf("%s -> %s, want %s", event, g.State, want)
		}
		if g.Confidence != 1.0 {
			t.Errorf("%s confidence = %v; pi tells us directly, so it must be 1.0", event, g.Confidence)
		}
		if g.Evidence == "" {
			t.Errorf("%s has no evidence recorded", event)
		}
	}
	if _, ok := InferState("queue_update"); ok {
		t.Error("queue_update should not change session state")
	}
}

func TestToolEventsBecomeTranscript(t *testing.T) {
	start := `{"type":"tool_execution_start","toolCallId":"tc1","toolName":"bash","input":{"command":"ls"}}`
	r := NewReader(strings.NewReader(start + "\n"))
	m, _ := r.Next()
	evs := ToTranscript(m)
	if len(evs) != 1 || evs[0].Kind != protocol.EventToolCall {
		t.Fatalf("start -> %+v", evs)
	}
	if evs[0].ToolName != "bash" || evs[0].ToolUseID != "tc1" {
		t.Errorf("tool call = %+v", evs[0])
	}
	if evs[0].ToolInput["command"] != "ls" {
		t.Errorf("tool input lost: %v", evs[0].ToolInput)
	}

	end := `{"type":"tool_execution_end","toolCallId":"tc1","toolName":"bash","output":"a\nb","isError":true}`
	r = NewReader(strings.NewReader(end + "\n"))
	m, _ = r.Next()
	evs = ToTranscript(m)
	if len(evs) != 1 || evs[0].Kind != protocol.EventToolResult {
		t.Fatalf("end -> %+v", evs)
	}
	if !evs[0].IsError || evs[0].ToolOutput != "a\nb" {
		t.Errorf("tool result = %+v", evs[0])
	}
}

// Only blocking dialogs become approvals. A fire-and-forget notify must never
// park a session waiting for an answer pi is not expecting.
func TestOnlyBlockingDialogsBecomeApprovals(t *testing.T) {
	blocking := []string{"confirm", "select", "input", "editor"}
	for _, method := range blocking {
		r := UIRequest{ID: "u1", Method: method, Title: "Dangerous!"}
		if _, ok := ToApproval(r, "s1", time.Minute); !ok {
			t.Errorf("%s should become an approval", method)
		}
	}
	for _, method := range []string{"notify", "setStatus", "setWidget", "setTitle"} {
		r := UIRequest{ID: "u1", Method: method, Title: "FYI"}
		if _, ok := ToApproval(r, "s1", time.Minute); ok {
			t.Errorf("%s is fire-and-forget and must not park the session", method)
		}
	}
}

func TestApprovalCarriesNativeSource(t *testing.T) {
	r := UIRequest{ID: "u1", Method: "confirm", Title: "Allow rm -rf?"}
	a, ok := ToApproval(r, "s1", time.Minute)
	if !ok {
		t.Fatal("not converted")
	}
	if a.Source != protocol.ApprovalNative {
		t.Errorf("source = %s, want native", a.Source)
	}
	if a.ID != "u1" {
		t.Errorf("id = %q; pi's own id must be preserved so the reply correlates", a.ID)
	}
	if len(a.Options) != 2 {
		t.Errorf("confirm should default to two options, got %v", a.Options)
	}
}

func TestBuildUIResponseShapes(t *testing.T) {
	confirm := UIRequest{ID: "u1", Method: "confirm"}
	got := BuildUIResponse(confirm, protocol.ApprovalDecision{Behavior: "allow"})
	if got.Confirmed == nil || !*got.Confirmed {
		t.Errorf("allow -> %+v, want confirmed true", got)
	}
	got = BuildUIResponse(confirm, protocol.ApprovalDecision{Behavior: "deny"})
	if got.Confirmed == nil || *got.Confirmed {
		t.Errorf("deny -> %+v, want confirmed false", got)
	}
	if got.Type != "extension_ui_response" {
		t.Errorf("type = %q", got.Type)
	}

	// A refused select must be cancelled, not an empty value: an empty string
	// would read as a deliberate blank answer, which is a different outcome.
	sel := UIRequest{ID: "u2", Method: "select"}
	got = BuildUIResponse(sel, protocol.ApprovalDecision{Behavior: "deny"})
	if !got.Cancelled || got.Value != "" {
		t.Errorf("denied select -> %+v, want cancelled", got)
	}
}

// pi solves mid-turn input natively, which is strictly better than
// synthesising keystrokes and hoping.
func TestInputCommandUsesSteeringWhenStreaming(t *testing.T) {
	idle := InputCommand("go on", false, "")
	if idle.Type != "prompt" {
		t.Errorf("idle command = %q", idle.Type)
	}
	if _, has := idle.Extra["streamingBehavior"]; has {
		t.Error("an idle session must not set streamingBehavior; pi rejects it")
	}

	streaming := InputCommand("actually, stop", true, "")
	if streaming.Extra["streamingBehavior"] != "steer" {
		t.Errorf("streaming default = %v, want steer", streaming.Extra["streamingBehavior"])
	}
	followUp := InputCommand("afterwards", true, "followUp")
	if followUp.Extra["streamingBehavior"] != "followUp" {
		t.Errorf("followUp mode = %v", followUp.Extra["streamingBehavior"])
	}
}

func TestCommandMarshalsFlat(t *testing.T) {
	b, err := json.Marshal(InputCommand("hello", false, ""))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// pi expects {"type":"prompt","message":"..."} -- extras flattened, not nested.
	if m["type"] != "prompt" || m["message"] != "hello" {
		t.Errorf("marshalled to %s; extras must be flattened alongside type", b)
	}
	if _, nested := m["Extra"]; nested {
		t.Error("Extra leaked into the wire format")
	}
}

// Verified against the real directory on this machine:
// /Users/sam/.pi/agent/sessions/--Users-sam-dev-shop--
func TestSessionSlug(t *testing.T) {
	cases := map[string]string{
		"/Users/sam/dev/shop": "--Users-sam-dev-shop--",
		"/home/dev/app":       "--home-dev-app--",
		"/a/b/":               "--a-b--",
	}
	for in, want := range cases {
		if got := SessionSlug(in); got != want {
			t.Errorf("SessionSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// A session in pi's documented format (docs/session-format.md, pi 0.83):
// a numeric version, toolCall arguments, tool results as their own message,
// bash executions, provider/modelId model changes, a /name, and an
// extension's custom_message.
var sessionFile = strings.Join([]string{
	`{"type":"session","version":3,"id":"0f9c2b1e-sess","timestamp":"2026-08-04T10:00:00.000Z","cwd":"/home/dev/app"}`,
	`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-08-04T10:00:00.500Z","provider":"anthropic","modelId":"claude-opus-5"}`,
	`{"type":"message","id":"e1","parentId":"m1","timestamp":"2026-08-04T10:00:01.000Z","message":{"role":"user","content":"add a health endpoint","timestamp":1785751201000}}`,
	`{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-08-04T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"check the router"},{"type":"text","text":"On it."},{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/app/router.ts"}}],"provider":"anthropic","model":"claude-opus-5","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15},"stopReason":"toolUse"}}`,
	`{"type":"message","id":"e3","parentId":"e2","timestamp":"2026-08-04T10:00:03.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"read","content":[{"type":"text","text":"export const router"}],"isError":false}}`,
	`{"type":"message","id":"e4","parentId":"e3","timestamp":"2026-08-04T10:00:04.000Z","message":{"role":"bashExecution","command":"ls","output":"router.ts","exitCode":0,"cancelled":false,"truncated":false}}`,
	`{"type":"custom_message","id":"x1","parentId":"e4","timestamp":"2026-08-04T10:00:05.000Z","customType":"my-extension","content":"Injected context","display":true}`,
	`{"type":"session_info","id":"i1","parentId":"x1","timestamp":"2026-08-04T10:00:06.000Z","name":"Health endpoint"}`,
	`{ malformed line`,
	`{"type":"compaction","id":"c1","parentId":"i1","timestamp":"2026-08-04T10:00:07.000Z","summary":"earlier work","firstKeptEntryId":"e3","tokensBefore":9000}`,
	`{"type":"message","id":"e5","parentId":"c1","timestamp":"2026-08-04T10:00:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"provider":"anthropic","model":"claude-opus-5","stopReason":"stop"}}`,
}, "\n") + "\n"

func TestParseSessionTree(t *testing.T) {
	events, meta, err := omp.ParseSession(strings.NewReader(sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if meta.SessionID != "0f9c2b1e-sess" || meta.Cwd != "/home/dev/app" {
		t.Errorf("header lost: meta = %+v", meta)
	}
	if meta.Model != "claude-opus-5" {
		t.Errorf("model = %q", meta.Model)
	}
	if meta.CustomTitle == nil || *meta.CustomTitle != "Health endpoint" {
		t.Errorf("session_info name lost: %+v", meta.CustomTitle)
	}

	counts := map[protocol.EventKind]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	want := map[protocol.EventKind]int{
		protocol.EventUserMessage:      1, // the extension's message is not the person's
		protocol.EventAssistantMessage: 2,
		protocol.EventThinking:         1,
		protocol.EventToolCall:         1,
		protocol.EventToolResult:       1,
		protocol.EventCommand:          1,
		protocol.EventCommandOutput:    1,
		protocol.EventNotice:           1,
		protocol.EventCompaction:       1,
	}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("count[%s] = %d, want %d", k, counts[k], w)
		}
	}
	for _, e := range events {
		switch e.Kind {
		case protocol.EventToolCall:
			if e.ToolName != "read" || e.ToolUseID != "tc1" || e.ToolInput["path"] != "/app/router.ts" {
				t.Errorf("tool call = %+v", e)
			}
		case protocol.EventToolResult:
			if e.ToolUseID != "tc1" || !strings.Contains(e.Text, "export const router") {
				t.Errorf("tool result = %+v", e)
			}
		case protocol.EventAssistantMessage:
			if strings.Contains(e.Text, "export const router") {
				t.Error("a tool result was rendered as the assistant speaking")
			}
		}
	}

	// The tree linkage must survive: pi's fork/clone create real branches, and
	// flattening them would misrepresent what happened.
	var linked int
	for _, e := range events {
		if e.ParentID != "" {
			linked++
		}
	}
	if linked == 0 {
		t.Error("parentId linkage lost; the session tree is flattened")
	}
}

func TestDiscoverAndJoinPiSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dir)
	bucket := filepath.Join(dir, "sessions", SessionSlug("/home/dev/app"))
	if err := os.MkdirAll(bucket, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bucket, "2026-08-04T10-00-00-000Z_0f9c2b1e-sess.jsonl")
	if err := os.WriteFile(path, []byte(sessionFile), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := (&Adapter{}).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("discovered %+v", got)
	}
	d := got[0]
	if d.NaturalKey != "0f9c2b1e-sess" || d.Cwd != "/home/dev/app" || d.Ref != path || !d.Live {
		t.Errorf("discovered = %+v", d)
	}
	// pi writes no title; its own picker shows the first message.
	if d.Title != "add a health endpoint" {
		t.Errorf("title = %q", d.Title)
	}

	started := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	if m := (&Adapter{}).MatchTranscript("/home/dev/app", started, nil); m != path {
		t.Errorf("join = %q, want %q", m, path)
	}
	if m := (&Adapter{}).MatchTranscript("/home/dev/app", started, map[string]bool{path: true}); m != "" {
		t.Errorf("joined a claimed file: %q", m)
	}
}

func TestInferStateFromTranscriptWithoutAPipe(t *testing.T) {
	in := adapter.StateInput{
		Recent:     []protocol.TranscriptEvent{{Kind: protocol.EventAssistantMessage, Text: "Done."}},
		Idle:       time.Minute,
		Quiescence: 6 * time.Second,
	}
	g, ok := (&Adapter{}).InferState(in)
	if !ok || g.State != protocol.StateWaitingInput || !strings.HasPrefix(g.Evidence, "pi transcript") {
		t.Fatalf("guess = %+v, ok=%v", g, ok)
	}
}

// The point of M3.5: pi must ship without changing the protocol. Every event
// this adapter produces has to fit the existing union.
func TestPiUsesOnlyExistingProtocolKinds(t *testing.T) {
	known := map[protocol.EventKind]bool{
		protocol.EventUserMessage: true, protocol.EventAssistantMessage: true,
		protocol.EventThinking: true, protocol.EventToolCall: true,
		protocol.EventToolResult: true, protocol.EventCompaction: true,
		protocol.EventError: true,
	}
	samples := []string{
		`{"type":"tool_execution_start","toolCallId":"t","toolName":"bash","input":{}}`,
		`{"type":"tool_execution_end","toolCallId":"t","toolName":"bash","output":"x"}`,
		`{"type":"message_end","role":"assistant","content":"hi"}`,
		`{"type":"compaction_end"}`,
		`{"type":"extension_error","error":"boom"}`,
	}
	for _, s := range samples {
		r := NewReader(strings.NewReader(s + "\n"))
		m, _ := r.Next()
		for _, e := range ToTranscript(m) {
			if !known[e.Kind] {
				t.Errorf("pi produced %q, which is not in the shared protocol union", e.Kind)
			}
		}
	}
}

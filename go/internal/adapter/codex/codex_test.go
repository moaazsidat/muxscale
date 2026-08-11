package codex

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Records below are shaped exactly like the real rollout files on disk,
// captured from codex-cli 0.130.0.
const rollout = `{"timestamp":"2026-05-14T16:22:51.689Z","type":"session_meta","payload":{"id":"019e2749-aac5-7071-bca9-639f064d7085","timestamp":"2026-05-14T16:19:59.813Z","cwd":"/Users/sam/dev/shop","originator":"codex-tui","cli_version":"0.130.0","source":"cli","model_provider":"openai"}}
{"timestamp":"2026-05-14T16:22:51.690Z","type":"turn_context","payload":{"turn_id":"turn-1","cwd":"/Users/sam/dev/shop","approval_policy":"never","sandbox_policy":{"type":"danger-full-access"},"model":"gpt-5.5"}}
{"timestamp":"2026-05-14T16:22:51.690Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1","model_context_window":258400}}
{"timestamp":"2026-05-14T16:22:52.000Z","type":"event_msg","payload":{"type":"user_message","message":"wire up the stator tool"}}
{"timestamp":"2026-05-14T16:22:53.000Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions> internal machinery"}]}}
{"timestamp":"2026-05-14T16:22:54.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"I should inspect the routing first."}],"encrypted_content":"opaque"}}
{"timestamp":"2026-05-14T16:22:55.000Z","type":"event_msg","payload":{"type":"agent_message","message":"I'll inspect the app structure first.","phase":"commentary"}}
{"timestamp":"2026-05-14T16:22:56.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git status --short\",\"workdir\":\"/repo\"}","call_id":"call_abc"}}
{"timestamp":"2026-05-14T16:22:57.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_abc","output":"Chunk ID: 1\nProcess exited with code 0\nOutput:\n M package.json"}}
{"timestamp":"2026-05-14T16:22:58.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"npm test\"}","call_id":"call_fail"}}
{"timestamp":"2026-05-14T16:22:59.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_fail","output":"Process exited with code 1\nOutput:\n2 failing"}}
{"timestamp":"2026-05-14T16:23:00.000Z","type":"event_msg","payload":{"type":"patch_apply_end","call_id":"patch1","success":false,"stdout":"applying","stderr":"conflict","turn_id":"turn-1"}}
{"timestamp":"2026-05-14T16:23:01.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":340}}}}
{ this line is deliberately malformed
{"timestamp":"2026-05-14T16:23:02.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"Built the tool.","duration_ms":9000}}
`

func parse(t *testing.T) ([]protocol.TranscriptEvent, Meta) {
	t.Helper()
	events, meta, err := ParseRollout(strings.NewReader(rollout))
	if err != nil {
		t.Fatal(err)
	}
	return events, meta
}

func TestRolloutMeta(t *testing.T) {
	_, meta := parse(t)
	checks := []struct{ name, got, want string }{
		{"SessionID", meta.SessionID, "019e2749-aac5-7071-bca9-639f064d7085"},
		{"Cwd", meta.Cwd, "/Users/sam/dev/shop"},
		{"Model", meta.Model, "gpt-5.5"},
		{"Version", meta.Version, "0.130.0"},
		{"Provider", meta.Provider, "openai"},
		{"ApprovalPolicy", meta.ApprovalPolicy, "never"},
		{"SandboxPolicy", meta.SandboxPolicy, "danger-full-access"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("meta.%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// A session with approval_policy "never" can never ask for permission. A user
// waiting on an approval notification would wait forever, and that is Codex's
// configuration rather than a muxscale fault, so it has to be visible.
func TestApprovalPolicyIsSurfaced(t *testing.T) {
	_, meta := parse(t)
	if meta.CanRequestApproval() {
		t.Error(`approval_policy "never" must report that approvals cannot happen`)
	}
	if (Meta{ApprovalPolicy: "on-request"}).CanRequestApproval() != true {
		t.Error("on-request should allow approvals")
	}
	if (Meta{}).CanRequestApproval() {
		t.Error("an unknown policy should not promise approvals")
	}
}

func TestRolloutEventCounts(t *testing.T) {
	events, _ := parse(t)
	counts := map[protocol.EventKind]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	want := map[protocol.EventKind]int{
		protocol.EventUserMessage:      1,
		protocol.EventAssistantMessage: 1,
		protocol.EventThinking:         1,
		protocol.EventToolCall:         2,
		protocol.EventToolResult:       3, // two function outputs + the patch
		protocol.EventTokenUsage:       1,
	}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("count[%s] = %d, want %d", k, counts[k], w)
		}
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}
}

// Codex's `arguments` is a JSON string that needs a second parse. Passing it
// through raw would show the user an escaped blob instead of the arguments --
// and the literal arguments are exactly what a human needs to judge an action.
func TestToolArgumentsAreParsedNotPassedThrough(t *testing.T) {
	events, _ := parse(t)
	for _, e := range events {
		if e.Kind != protocol.EventToolCall || e.ToolUseID != "call_abc" {
			continue
		}
		if e.ToolName != "exec_command" {
			t.Errorf("tool name = %q", e.ToolName)
		}
		if got := e.ToolInput["cmd"]; got != "git status --short" {
			t.Errorf("cmd = %v; arguments were not parsed into a map", got)
		}
		if got := e.ToolInput["workdir"]; got != "/repo" {
			t.Errorf("workdir = %v", got)
		}
		return
	}
	t.Fatal("no tool call for call_abc")
}

func TestNonObjectArgumentsAreKeptVisible(t *testing.T) {
	got := parseToolArguments("not json at all", nil)
	if got["arguments"] != "not json at all" {
		t.Errorf("unparseable arguments should stay visible, got %v", got)
	}
}

// Codex encodes exec failure in the output preamble rather than a flag.
func TestExitCodeBecomesErrorFlag(t *testing.T) {
	events, _ := parse(t)
	byID := map[string]protocol.TranscriptEvent{}
	for _, e := range events {
		if e.Kind == protocol.EventToolResult && e.ToolUseID != "" {
			byID[e.ToolUseID] = e
		}
	}
	if byID["call_abc"].IsError {
		t.Error("exit code 0 was flagged as an error")
	}
	if !byID["call_fail"].IsError {
		t.Error("exit code 1 was not flagged as an error")
	}
	if !byID["patch1"].IsError {
		t.Error("a failed patch_apply_end was not flagged as an error")
	}
	if !strings.Contains(byID["patch1"].ToolOutput, "conflict") {
		t.Errorf("patch stderr was dropped: %q", byID["patch1"].ToolOutput)
	}
}

// Developer and system messages are machinery. Showing them would bury the
// actual conversation under permission preambles and base instructions.
func TestDeveloperMessagesAreExcluded(t *testing.T) {
	events, _ := parse(t)
	for _, e := range events {
		if strings.Contains(e.Text, "permissions instructions") {
			t.Errorf("a developer message leaked into the transcript: %q", e.Text)
		}
	}
}

func TestReasoningSummaryBecomesThinking(t *testing.T) {
	events, _ := parse(t)
	for _, e := range events {
		if e.Kind == protocol.EventThinking {
			if !strings.Contains(e.Text, "inspect the routing") {
				t.Errorf("thinking text = %q", e.Text)
			}
			return
		}
	}
	t.Fatal("no thinking event")
}

// Reasoning is frequently encrypted-only with nothing displayable; emitting an
// empty bubble would be noise.
func TestEncryptedOnlyReasoningIsSkipped(t *testing.T) {
	line := `{"timestamp":"2026-05-14T16:00:00Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"opaque"}}` + "\n"
	events, _, err := ParseRollout(strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("encrypted-only reasoning produced %d events, want 0", len(events))
	}
}

func TestUsageParsed(t *testing.T) {
	events, _ := parse(t)
	for _, e := range events {
		if e.Kind != protocol.EventTokenUsage {
			continue
		}
		if e.Usage.InputTokens != 1200 || e.Usage.OutputTokens != 340 ||
			e.Usage.CacheReadTokens != 800 {
			t.Errorf("usage = %+v", e.Usage)
		}
		return
	}
	t.Fatal("no usage event")
}

func TestMalformedLineDoesNotAbortParse(t *testing.T) {
	events, _ := parse(t)
	// task_complete follows the malformed line; the parse must reach it.
	var reachedEnd bool
	for _, e := range events {
		if e.Kind == protocol.EventTokenUsage {
			reachedEnd = true
		}
	}
	if !reachedEnd {
		t.Error("parse stopped early at the malformed line")
	}
}

// task_complete is Codex's Stop: the turn ended and a human is needed.
func TestStateMapping(t *testing.T) {
	cases := map[string]protocol.SessionState{
		"task_complete":                protocol.StateWaitingInput,
		"task_started":                 protocol.StateRunning,
		"exec_approval_request":        protocol.StateWaitingApproval,
		"apply_patch_approval_request": protocol.StateWaitingApproval,
	}
	for event, want := range cases {
		g, ok := StateFromEvent(event)
		if !ok {
			t.Errorf("%s produced no state", event)
			continue
		}
		if g.State != want {
			t.Errorf("%s -> %s, want %s", event, g.State, want)
		}
		if g.Confidence != 1.0 {
			t.Errorf("%s confidence = %v; Codex tells us directly", event, g.Confidence)
		}
	}
	if _, ok := StateFromEvent("token_count"); ok {
		t.Error("token_count should not change session state")
	}
}

// Inference must stay below the event path's confidence.
func TestRolloutInferenceIsLowerConfidence(t *testing.T) {
	g := InferStateFromRollout("task_complete", time.Hour, 6*time.Second)
	if g.State != protocol.StateWaitingInput {
		t.Errorf("state = %s", g.State)
	}
	if g.Confidence >= 1.0 {
		t.Errorf("inference confidence %v must stay below the event path's 1.0", g.Confidence)
	}
	running := InferStateFromRollout("agent_message", time.Second, 6*time.Second)
	if running.State != protocol.StateRunning {
		t.Errorf("recent activity -> %s, want RUNNING", running.State)
	}
}

func TestCodexUsesOnlyExistingProtocolKinds(t *testing.T) {
	known := map[protocol.EventKind]bool{
		protocol.EventUserMessage: true, protocol.EventAssistantMessage: true,
		protocol.EventThinking: true, protocol.EventToolCall: true,
		protocol.EventToolResult: true, protocol.EventTokenUsage: true,
		protocol.EventCompaction: true, protocol.EventError: true,
	}
	events, _ := parse(t)
	for _, e := range events {
		if !known[e.Kind] {
			t.Errorf("codex produced %q, which is not in the shared protocol union", e.Kind)
		}
	}
}

// Every real rollout on this machine must parse. This is the test that catches
// schema drift between Codex releases.
func TestParseEveryRealRolloutOnThisMachine(t *testing.T) {
	paths, err := FindRollouts(0)
	if err != nil {
		t.Skipf("no codex sessions available: %v", err)
	}
	if len(paths) == 0 {
		t.Skip("no codex rollout files on this machine")
	}

	var totalEvents int
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Errorf("open %s: %v", p, err)
			continue
		}
		events, meta, err := ParseRollout(f)
		f.Close()
		if err != nil {
			t.Errorf("parse %s: %v", p, err)
			continue
		}
		if meta.SessionID == "" {
			t.Errorf("%s: no session id recovered", p)
		}
		totalEvents += len(events)
	}
	t.Logf("parsed %d real rollouts, %d transcript events", len(paths), totalEvents)
}

func TestRolloutCwdReadsOnlyTheHeader(t *testing.T) {
	paths, err := FindRollouts(1)
	if err != nil || len(paths) == 0 {
		t.Skip("no codex rollouts available")
	}
	cwd, id, err := RolloutCwd(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if cwd == "" || id == "" {
		t.Errorf("cwd=%q id=%q; the header should yield both", cwd, id)
	}
	t.Logf("newest rollout: cwd=%s id=%s", cwd, id)
}

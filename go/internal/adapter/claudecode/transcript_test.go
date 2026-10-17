package claudecode

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

const fixture = "../../../../fixtures/transcripts/claude-code/basic.jsonl"

func parseFixture(t *testing.T) ([]protocol.TranscriptEvent, Meta) {
	t.Helper()
	events, meta, err := ParseTranscriptFile(filepath.Clean(fixture))
	if err != nil {
		t.Fatalf("ParseTranscriptFile: %v", err)
	}
	return events, meta
}

func TestParseTranscriptMeta(t *testing.T) {
	_, meta := parseFixture(t)
	checks := []struct{ name, got, want string }{
		{"SessionID", meta.SessionID, "s-abc"},
		{"Cwd", meta.Cwd, "/home/dev/app"},
		{"GitBranch", meta.GitBranch, "main"},
		{"Model", meta.Model, "claude-opus-5"},
		{"Version", meta.Version, "2.1.220"},
		{"Title", meta.Title, "Add health endpoint"},
		{"LastPrompt", meta.LastPrompt, "add a health endpoint"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("meta.%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestParseTranscriptEvents(t *testing.T) {
	events, _ := parseFixture(t)

	counts := map[protocol.EventKind]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	want := map[protocol.EventKind]int{
		protocol.EventUserMessage:      1, // the tool_result-only user entries are results, not messages
		protocol.EventAssistantMessage: 3, // a2, a4, a5; a1 is thinking-only

		protocol.EventThinking:   1,
		protocol.EventToolCall:   2,
		protocol.EventToolResult: 2,
	}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("count[%s] = %d, want %d", k, counts[k], w)
		}
	}

	// Seq must be dense and monotonic: clients resume by seq, so a gap or a
	// repeat silently corrupts their view.
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
	}
}

// The malformed line in the fixture must not abort the parse. Session files are
// appended to live by another process, so a torn final line is normal.
func TestParseTranscriptSurvivesMalformedLine(t *testing.T) {
	events, _ := parseFixture(t)
	var found bool
	for _, e := range events {
		if e.Text == "fixed" {
			found = true
		}
	}
	if !found {
		t.Error("events after the malformed line were dropped; parse must skip and continue")
	}
}

func TestToolCallAndResultPairing(t *testing.T) {
	events, _ := parseFixture(t)
	calls := map[string]string{} // tool_use_id -> name
	results := map[string]bool{} // tool_use_id -> seen
	for _, e := range events {
		switch e.Kind {
		case protocol.EventToolCall:
			calls[e.ToolUseID] = e.ToolName
		case protocol.EventToolResult:
			results[e.ToolUseID] = true
		}
	}
	if calls["toolu_1"] != "Read" || calls["toolu_2"] != "Bash" {
		t.Errorf("tool names not captured: %v", calls)
	}
	for id := range calls {
		if !results[id] {
			t.Errorf("tool_use %s has no matching result", id)
		}
	}
}

func TestToolResultErrorFlagAndBlockContent(t *testing.T) {
	events, _ := parseFixture(t)
	for _, e := range events {
		if e.Kind != protocol.EventToolResult || e.ToolUseID != "toolu_2" {
			continue
		}
		if !e.IsError {
			t.Error("toolu_2 result should be flagged IsError")
		}
		// This result's content is an array of blocks, not a string; it must
		// still flatten to readable text.
		if !strings.Contains(e.ToolOutput, "2 failing") {
			t.Errorf("block-array tool_result did not flatten: %q", e.ToolOutput)
		}
		return
	}
	t.Fatal("no result event for toolu_2")
}

func TestSidechainPreserved(t *testing.T) {
	events, _ := parseFixture(t)
	var sidechain int
	for _, e := range events {
		if e.IsSidechain {
			sidechain++
		}
	}
	if sidechain != 1 {
		t.Errorf("expected 1 sidechain event, got %d", sidechain)
	}
}

// parentUuid makes the transcript a tree. Flattening it would misrepresent
// forks, so the linkage must survive parsing.
func TestParentLinkagePreserved(t *testing.T) {
	events, _ := parseFixture(t)
	byID := map[string]bool{}
	for _, e := range events {
		byID[e.ID] = true
	}
	var linked int
	for _, e := range events {
		if e.ParentID != "" {
			linked++
		}
	}
	if linked == 0 {
		t.Fatal("no parent linkage captured; transcript tree is lost")
	}
}

func TestUsageCaptured(t *testing.T) {
	events, _ := parseFixture(t)
	for _, e := range events {
		if e.Kind != protocol.EventThinking || e.Usage == nil {
			continue
		}
		if e.Usage.InputTokens != 120 || e.Usage.OutputTokens != 18 {
			t.Errorf("usage = %+v, want input 120 / output 18", e.Usage)
		}
		if e.Usage.CacheReadTokens != 4096 || e.Usage.CacheWriteTokens != 256 {
			t.Errorf("cache usage = %+v, want read 4096 / write 256", e.Usage)
		}
		return
	}
	t.Fatal("no event carried usage")
}

// Long content must be clipped at a rune boundary: emitting invalid UTF-8 to a
// client is a rendering bug that is very hard to trace back to here.
func TestClipRespectsRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", excerptBytes) // 2 bytes per rune
	got, truncated := clip(long)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(got) > excerptBytes {
		t.Errorf("clip returned %d bytes, over the %d limit", len(got), excerptBytes)
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("clip produced invalid UTF-8 at byte %d", i)
		}
	}
}

func TestClipShortStringUntouched(t *testing.T) {
	got, truncated := clip("hello")
	if truncated || got != "hello" {
		t.Errorf("clip(short) = %q, %v; want unchanged", got, truncated)
	}
}

// Shapes copied from a real Claude Code 2.1.301 session that ran /compact.
const compactLines = `{"type":"user","uuid":"c1","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code.</local-command-caveat>"}}
{"type":"user","uuid":"c2","message":{"role":"user","content":"<command-name>/compact</command-name>\n            <command-message>compact</command-message>\n            <command-args>keep the API notes</command-args>"}}
{"type":"user","uuid":"c3","message":{"role":"user","content":"<local-command-stdout>\u001b[2mCompacted (ctrl+o to see full summary)\u001b[22m</local-command-stdout>"},"repeatsCompactSummary":true}
{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}
{"type":"user","uuid":"c4","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation.\n\nSummary:\n1. Things"}}
{"type":"user","uuid":"c5","message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<status>killed</status>\n<summary>Background command \"Inspect raw screen\" was stopped</summary>\n</task-notification>"}}
{"type":"user","uuid":"c6","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>\nwhatever the harness adds\n</system-reminder>"},{"type":"text","text":"now fix the chat"}]}}
{"type":"user","uuid":"c7","message":{"role":"user","content":"<bash-input>git status</bash-input>"}}
{"type":"user","uuid":"c8","message":{"role":"user","content":"<bash-stdout>On branch main</bash-stdout><bash-stderr></bash-stderr>"}}
`

func TestCLIEntriesAreNotTheHumanSpeaking(t *testing.T) {
	events, _, err := ParseTranscript(strings.NewReader(compactLines))
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		kind protocol.EventKind
		text string
	}
	want := []got{
		{protocol.EventCommand, "/compact keep the API notes"},
		{protocol.EventCommandOutput, "Compacted (ctrl+o to see full summary)"},
		{protocol.EventCompaction, "This session is being continued from a previous conversation.\n\nSummary:\n1. Things"},
		{protocol.EventNotice, `Background command "Inspect raw screen" was stopped`},
		{protocol.EventUserMessage, "now fix the chat"},
		{protocol.EventCommand, "!git status"},
		{protocol.EventCommandOutput, "On branch main"},
	}
	if len(events) != len(want) {
		for _, e := range events {
			t.Logf("%s %q", e.Kind, e.Text)
		}
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, w := range want {
		if events[i].Kind != w.kind || events[i].Text != w.text {
			t.Errorf("event %d = %s %q, want %s %q", i, events[i].Kind, events[i].Text, w.kind, w.text)
		}
	}
}

func TestReclassifyStoredRows(t *testing.T) {
	cases := []struct {
		text    string
		kind    protocol.EventKind
		want    string
		dropped bool
	}{
		{"just words", protocol.EventUserMessage, "just words", false},
		{"<command-name>/compact</command-name><command-args></command-args>", protocol.EventCommand, "/compact", false},
		{"<local-command-caveat>x</local-command-caveat>", "", "", true},
		{summaryPreamble + " that ran out of context.\n\nSummary: x", protocol.EventCompaction, summaryPreamble + " that ran out of context.\n\nSummary: x", false},
	}
	for _, c := range cases {
		ev, ok := Reclassify(protocol.TranscriptEvent{Seq: 7, ID: "x", Kind: protocol.EventUserMessage, Text: c.text})
		if ok == c.dropped {
			t.Errorf("%q: kept = %v, want %v", c.text, ok, !c.dropped)
			continue
		}
		if ok && (ev.Kind != c.kind || ev.Text != c.want || ev.Seq != 7 || ev.ID != "x") {
			t.Errorf("%q: got %s %q seq %d id %q", c.text, ev.Kind, ev.Text, ev.Seq, ev.ID)
		}
	}
}

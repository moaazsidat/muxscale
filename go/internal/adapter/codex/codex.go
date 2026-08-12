// Package codex adapts the OpenAI Codex CLI.
//
// Verified against codex-cli 0.130.0 by reading real rollout files on disk.
// Codex persists every session as JSONL under
// ~/.codex/sessions/YYYY/MM/DD/rollout-<timestamp>-<uuid>.jsonl, with four
// top-level record types:
//
//	session_meta   once, at the top: id, cwd, cli_version, model_provider
//	turn_context   per turn: turn_id, model, approval_policy, sandbox_policy
//	response_item  the model's own items: message, reasoning, function_call,
//	               function_call_output, custom_tool_call(_output), web_search_call
//	event_msg      lifecycle: task_started, task_complete, agent_message,
//	               user_message, token_count, patch_apply_end, web_search_end
//
// task_complete is Codex's equivalent of Claude Code's Stop hook -- the turn
// ended and a human is needed. It is the highest-value signal here.
package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is the floor the adapter was verified against.
const MinVersion = "0.100.0"

// newestKnownProfile is the highest Codex version this build has fixtures for.
// 0.130.0 is where the session_meta.source drift was observed and pinned (see
// drift_test.go). Versions above it use this profile and are badged, never
// rejected.
const newestKnownProfile = "0.130.0"

// record is one line of a rollout file.
type record struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionMeta is the header record.
//
// Fields whose shape has changed across Codex releases are held as RawMessage
// and decoded separately. `source` is the cautionary example: 0.120.0 wrote an
// object ({"subagent":"review"}), 0.130.0 writes a string ("cli"). Declaring it
// as a string made json.Unmarshal fail on the *whole struct*, so one changed
// field silently discarded the entire header -- session id, cwd and all. Any
// field that might drift belongs in this category.
type sessionMeta struct {
	ID            string          `json:"id"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	Originator    string          `json:"originator"`
	CLIVersion    string          `json:"cli_version"`
	Source        json.RawMessage `json:"source"`
	ModelProvider string          `json:"model_provider"`
}

// decodeSource reads `source` in either shape, returning the source kind and
// the subagent name when the session is a subagent rather than a user's own.
func decodeSource(raw json.RawMessage) (kind, subagent string) {
	if len(raw) == 0 {
		return "", ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, ""
	}
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err == nil {
		if sa, ok := obj["subagent"]; ok {
			return "subagent", sa
		}
		for k, v := range obj {
			return k, v
		}
	}
	return "", ""
}

// turnContext carries per-turn settings, including the approval policy, which
// tells us whether this session can ever ask for permission at all.
type turnContext struct {
	TurnID         string `json:"turn_id"`
	Cwd            string `json:"cwd"`
	Model          string `json:"model"`
	ApprovalPolicy string `json:"approval_policy"`
	SandboxPolicy  struct {
		Type string `json:"type"`
	} `json:"sandbox_policy"`
}

// responseItem is a model-produced item.
type responseItem struct {
	Type string `json:"type"`

	// message
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Phase   string          `json:"phase"`

	// function_call / custom_tool_call
	Name string `json:"name"`
	// Arguments is a JSON *string* that must be parsed again to get the real
	// tool input. Passing it through as text would show the user an escaped
	// blob instead of the arguments.
	Arguments string          `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	Status    string          `json:"status"`

	// function_call_output
	Output json.RawMessage `json:"output"`

	// reasoning
	Summary json.RawMessage `json:"summary"`
}

// eventMsg is a lifecycle event.
type eventMsg struct {
	Type    string `json:"type"`
	TurnID  string `json:"turn_id"`
	Message string `json:"message"`
	Phase   string `json:"phase"`

	// task_complete
	LastAgentMessage string `json:"last_agent_message"`
	DurationMs       int64  `json:"duration_ms"`

	// token_count
	Info json.RawMessage `json:"info"`

	// patch_apply_end
	CallID  string          `json:"call_id"`
	Changes json.RawMessage `json:"changes"`
	Success *bool           `json:"success"`
	Stdout  string          `json:"stdout"`
	Stderr  string          `json:"stderr"`
}

// Meta is session metadata recovered from a rollout file.
type Meta struct {
	SessionID      string
	Cwd            string
	Model          string
	Version        string
	Provider       string
	ApprovalPolicy string
	SandboxPolicy  string
	LastTurnID     string

	// Source is how the session was started ("cli", "subagent", ...) and
	// Subagent names it when Codex spawned this session itself. A subagent
	// session belongs nested under its parent, not as a peer in the list.
	Source   string
	Subagent string
}

// IsSubagent reports whether Codex started this session on its own behalf.
func (m Meta) IsSubagent() bool { return m.Subagent != "" }

// CanRequestApproval reports whether this session's policy allows Codex to ever
// ask for permission.
//
// Worth surfacing: with approval_policy "never" and a full-access sandbox, a
// user waiting for an approval notification will wait forever, and that is
// Codex's configuration rather than a muxscale fault.
func (m Meta) CanRequestApproval() bool {
	return m.ApprovalPolicy != "" && m.ApprovalPolicy != "never"
}

const excerptBytes = 4096

// ParseRollout reads a Codex rollout JSONL file.
func ParseRollout(r io.Reader) ([]protocol.TranscriptEvent, Meta, error) {
	var (
		events []protocol.TranscriptEvent
		meta   Meta
		seq    uint64
	)
	sc := bufio.NewScanner(r)
	// Rollouts embed full tool output and base instructions; lines get large.
	sc.Buffer(make([]byte, 0, 256*1024), 32*1024*1024)

	add := func(e protocol.TranscriptEvent) {
		seq++
		e.Seq = seq
		events = append(events, e)
	}

	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // torn or unknown line; a live file is being appended to
		}
		at := parseTime(rec.Timestamp)

		switch rec.Type {
		case "session_meta":
			var sm sessionMeta
			if err := json.Unmarshal(rec.Payload, &sm); err == nil {
				meta.SessionID, meta.Cwd = sm.ID, sm.Cwd
				meta.Version, meta.Provider = sm.CLIVersion, sm.ModelProvider
				meta.Source, meta.Subagent = decodeSource(sm.Source)
			}

		case "turn_context":
			var tc turnContext
			if err := json.Unmarshal(rec.Payload, &tc); err == nil {
				if tc.Model != "" {
					meta.Model = tc.Model
				}
				if tc.Cwd != "" {
					meta.Cwd = tc.Cwd
				}
				meta.ApprovalPolicy = tc.ApprovalPolicy
				meta.SandboxPolicy = tc.SandboxPolicy.Type
				meta.LastTurnID = tc.TurnID
			}

		case "response_item":
			for _, e := range responseItemEvents(rec.Payload, at) {
				add(e)
			}

		case "event_msg":
			for _, e := range eventMsgEvents(rec.Payload, at) {
				add(e)
			}
		}
	}
	return events, meta, sc.Err()
}

func responseItemEvents(payload json.RawMessage, at time.Time) []protocol.TranscriptEvent {
	var ri responseItem
	if err := json.Unmarshal(payload, &ri); err != nil {
		return nil
	}
	base := protocol.TranscriptEvent{At: at}

	switch ri.Type {
	case "function_call", "custom_tool_call":
		base.Kind = protocol.EventToolCall
		base.ToolName = ri.Name
		base.ToolUseID = ri.CallID
		base.ToolInput = parseToolArguments(ri.Arguments, ri.Input)
		return []protocol.TranscriptEvent{base}

	case "function_call_output", "custom_tool_call_output":
		base.Kind = protocol.EventToolResult
		base.ToolUseID = ri.CallID
		out := flattenText(ri.Output)
		base.ToolOutput, base.Truncated = clip(out)
		// Codex prefixes exec output with a header that includes the exit code;
		// surface a failure rather than making the user read for it.
		base.IsError = strings.Contains(out, "Process exited with code ") &&
			!strings.Contains(out, "Process exited with code 0")
		return []protocol.TranscriptEvent{base}

	case "reasoning":
		text := flattenText(ri.Summary)
		if text == "" {
			text = flattenText(ri.Content)
		}
		if strings.TrimSpace(text) == "" {
			// Reasoning is often encrypted-only, with nothing displayable.
			return nil
		}
		base.Kind = protocol.EventThinking
		base.Text, base.Truncated = clip(text)
		return []protocol.TranscriptEvent{base}

	case "message":
		text := flattenText(ri.Content)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		switch ri.Role {
		case "user":
			base.Kind = protocol.EventUserMessage
		case "developer", "system":
			// System prompts and permission preambles are machinery, not
			// conversation. Showing them would bury the actual exchange.
			return nil
		default:
			base.Kind = protocol.EventAssistantMessage
		}
		base.Text, base.Truncated = clip(text)
		return []protocol.TranscriptEvent{base}

	case "web_search_call":
		base.Kind = protocol.EventToolCall
		base.ToolName = "web_search"
		base.ToolUseID = ri.CallID
		return []protocol.TranscriptEvent{base}
	}
	return nil
}

func eventMsgEvents(payload json.RawMessage, at time.Time) []protocol.TranscriptEvent {
	var em eventMsg
	if err := json.Unmarshal(payload, &em); err != nil {
		return nil
	}
	base := protocol.TranscriptEvent{At: at}

	switch em.Type {
	case "user_message":
		if strings.TrimSpace(em.Message) == "" {
			return nil
		}
		base.Kind = protocol.EventUserMessage
		base.Text, base.Truncated = clip(em.Message)
		return []protocol.TranscriptEvent{base}

	case "agent_message":
		if strings.TrimSpace(em.Message) == "" {
			return nil
		}
		base.Kind = protocol.EventAssistantMessage
		base.Text, base.Truncated = clip(em.Message)
		return []protocol.TranscriptEvent{base}

	case "patch_apply_end":
		base.Kind = protocol.EventToolResult
		base.ToolName = "apply_patch"
		base.ToolUseID = em.CallID
		base.IsError = em.Success != nil && !*em.Success
		out := em.Stdout
		if em.Stderr != "" {
			out = strings.TrimRight(out, "\n") + "\n" + em.Stderr
		}
		base.ToolOutput, base.Truncated = clip(out)
		return []protocol.TranscriptEvent{base}

	case "token_count":
		if u := parseUsage(em.Info); u != nil {
			base.Kind = protocol.EventTokenUsage
			base.Usage = u
			return []protocol.TranscriptEvent{base}
		}
	}
	return nil
}

// StateFromEvent maps a Codex event_msg subtype to a session state.
//
// Confidence 1.0: these come from Codex's own lifecycle events, not inference.
func StateFromEvent(eventType string) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{
			State: s, Confidence: 1.0, Reason: reason,
			Evidence: "codex:" + eventType, At: now,
		}, true
	}
	switch eventType {
	case "task_complete":
		// Codex's equivalent of Claude Code's Stop: the turn ended.
		return g(protocol.StateWaitingInput, "task complete")
	case "task_started":
		return g(protocol.StateRunning, "task started")
	case "exec_approval_request", "apply_patch_approval_request":
		return g(protocol.StateWaitingApproval, "approval requested")
	case "error", "stream_error":
		return g(protocol.StateRunning, "recovering from an error")
	}
	return protocol.StateGuess{}, false
}

// InferStateFromRollout derives state from a rollout file's tail, for sessions
// with no live event feed.
//
// Confidence stays below the event path's 1.0 because this is inference. The
// last record type is the signal: a trailing task_complete means the turn ended.
func InferStateFromRollout(lastEventType string, idle time.Duration, quiescence time.Duration) protocol.StateGuess {
	now := time.Now()
	switch {
	case lastEventType == "task_complete":
		return protocol.StateGuess{
			State: protocol.StateWaitingInput, Confidence: 0.9,
			Reason:   "rollout ends with task_complete",
			Evidence: "codex:rollout tail", At: now,
		}
	case idle < quiescence:
		return protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 0.65,
			Reason:   "rollout changed recently",
			Evidence: "codex:idle " + idle.Round(time.Second).String(), At: now,
		}
	default:
		return protocol.StateGuess{
			State: protocol.StateWaitingInput, Confidence: 0.6,
			Reason:   "rollout quiet",
			Evidence: "codex:idle " + idle.Round(time.Second).String(), At: now,
		}
	}
}

// ---- session discovery ----

// SessionsRoot is ~/.codex/sessions.
func SessionsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// FindRollouts returns rollout files, newest first.
//
// Codex nests them under YYYY/MM/DD rather than by project, so unlike Claude
// Code there is no path-derived way to find a given directory's session. The
// cwd has to be read from each file's session_meta, which is why this returns
// everything and callers filter.
func FindRollouts(limit int) ([]string, error) {
	root, err := SessionsRoot()
	if err != nil {
		return nil, err
	}
	type entry struct {
		path string
		mod  time.Time
	}
	var found []entry
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subdirectory must not abort the walk
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		found = append(found, entry{path, info.ModTime()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.path
	}
	return out, nil
}

// RolloutCwd reads just the session_meta header, so callers can match a rollout
// to a working directory without parsing a multi-megabyte file.
func RolloutCwd(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		var rec record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Type != "session_meta" {
			continue
		}
		var sm sessionMeta
		if err := json.Unmarshal(rec.Payload, &sm); err != nil {
			return "", "", err
		}
		return sm.Cwd, sm.ID, nil
	}
	return "", "", sc.Err()
}

// FindRolloutForCwd returns the newest rollout whose session_meta matches cwd.
func FindRolloutForCwd(cwd string, scanLimit int) (string, error) {
	paths, err := FindRollouts(scanLimit)
	if err != nil {
		return "", err
	}
	want := filepath.Clean(cwd)
	for _, p := range paths {
		got, _, err := RolloutCwd(p)
		if err != nil {
			continue
		}
		if filepath.Clean(got) == want {
			return p, nil
		}
	}
	return "", nil
}

// ---- helpers ----

// parseToolArguments turns Codex's arguments into a map. `arguments` is a JSON
// string that needs a second parse; `input` (custom tools) is already JSON.
func parseToolArguments(arguments string, input json.RawMessage) map[string]any {
	if arguments != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(arguments), &m); err == nil {
			return m
		}
		// Not an object: keep it visible rather than dropping it.
		return map[string]any{"arguments": arguments}
	}
	if len(input) > 0 {
		var m map[string]any
		if err := json.Unmarshal(input, &m); err == nil {
			return m
		}
		var s string
		if err := json.Unmarshal(input, &s); err == nil {
			return map[string]any{"input": s}
		}
	}
	return nil
}

// flattenText renders a content field that may be a string, an array of typed
// blocks, or absent.
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

func parseUsage(raw json.RawMessage) *protocol.Usage {
	if len(raw) == 0 {
		return nil
	}
	var info struct {
		TotalTokenUsage struct {
			InputTokens       int `json:"input_tokens"`
			CachedInputTokens int `json:"cached_input_tokens"`
			OutputTokens      int `json:"output_tokens"`
		} `json:"total_token_usage"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil
	}
	u := info.TotalTokenUsage
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	return &protocol.Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CachedInputTokens,
	}
}

func clip(s string) (string, bool) {
	if len(s) <= excerptBytes {
		return s, false
	}
	cut := excerptBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut], true
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

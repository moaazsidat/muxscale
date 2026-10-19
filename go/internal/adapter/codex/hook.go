package codex

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// HookPayload is the JSON Codex writes to a command hook's stdin. Verified
// against 0.163.0: the common fields match Claude Code's payload closely, and
// turn_id is the fence token where Claude Code has prompt_id.
type HookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Model          string `json:"model"`
	PermissionMode string `json:"permission_mode"`

	TurnID string `json:"turn_id"`
	Prompt string `json:"prompt"`

	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolInput    map[string]any  `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`

	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	LastAssistantMessage string `json:"last_assistant_message"`
}

var _ adapter.HookCodec = (*Adapter)(nil)

// HookPath is where the relay posts.
func (*Adapter) HookPath() string { return "/v1/hook/codex" }

// DecodeHook translates a Codex hook payload into the daemon's vocabulary.
func (*Adapter) DecodeHook(body []byte) (adapter.HookEvent, error) {
	var p HookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return adapter.HookEvent{}, err
	}
	if p.SessionID == "" {
		return adapter.HookEvent{}, fmt.Errorf("codex hook payload has no session_id")
	}
	e := adapter.HookEvent{
		AgentID: "codex", SessionID: p.SessionID,
		SessionFile: p.TranscriptPath, Cwd: p.Cwd,
		PromptID: p.TurnID, EventName: p.HookEventName, Model: p.Model,
		ToolName: p.ToolName, ToolUseID: p.ToolUseID, ToolInput: p.ToolInput,
		IsSidechain: p.AgentID != "",
	}
	switch p.HookEventName {
	case "SessionStart":
		e.Kind = adapter.HookSessionStart
	case "UserPromptSubmit":
		e.Kind = adapter.HookUserPrompt
	case "PreToolUse":
		e.Kind = adapter.HookToolCall
	case "PermissionRequest":
		e.Kind = adapter.HookApproval
	case "PostToolUse":
		e.Kind = adapter.HookToolResult
		e.ToolOutput = toolResponseText(p.ToolResponse)
	case "Stop":
		e.Kind = adapter.HookStop
	case "SubagentStop":
		e.Kind = adapter.HookSubagentStop
	case "SessionEnd":
		e.Kind = adapter.HookSessionEnd
	case "PreCompact", "PostCompact":
		e.Kind = adapter.HookCompaction
	case "SubagentStart":
		e.Kind = adapter.HookSubagentStart
	case "Interrupt":
		e.Kind = adapter.HookIdle
	default:
		e.Kind = adapter.HookNotice
	}
	return e, nil
}

// EncodeHookReply returns Codex's PermissionRequest decision shape, or an
// empty object for "no decision". The relay prints it verbatim on stdout.
func (*Adapter) EncodeHookReply(e adapter.HookEvent, d *protocol.ApprovalDecision) any {
	if d == nil || d.Behavior == "" {
		return map[string]any{}
	}
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision": map[string]any{
				"behavior": d.Behavior,
				"message":  d.Reason,
			},
		},
	}
}

// toolResponseText renders tool_response, which is sometimes a string and
// sometimes a JSON object.
func toolResponseText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s := flattenText(raw); s != "" {
		return s
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return strings.TrimSpace(string(raw))
}

// DetectVersion runs `codex --version`, which prints "codex-cli 0.163.0".
func DetectVersion() (adapter.Version, error) {
	out, err := exec.Command("codex", "--version").Output()
	if err != nil {
		return adapter.Version{}, err
	}
	return adapter.ParseVersion(string(out))
}

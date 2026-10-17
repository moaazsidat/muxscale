package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MinVersion is the supported floor. 2.1.196 is where prompt_id landed, and
// prompt_id is the fence token for queued input (spec §5.6). Below it, input
// fencing would degrade to wall-clock heuristics -- exactly the class of bug
// that makes remote input dangerous -- so we refuse to install hooks instead of
// carrying a second, weaker safety model.
const MinVersion = "2.1.196"

// HookEvent names we register. Claude Code exposes 31 as of 2.1.220 and the set
// is still growing; we register the intersection of (what the detected version
// has) x (what we handle), and 200-with-no-decision anything unexpected.
const (
	HookSessionStart      = "SessionStart"
	HookUserPromptSubmit  = "UserPromptSubmit"
	HookPreToolUse        = "PreToolUse"
	HookPermissionRequest = "PermissionRequest"
	HookPermissionDenied  = "PermissionDenied"
	HookPostToolUse       = "PostToolUse"
	HookPostToolUseFail   = "PostToolUseFailure"
	HookNotification      = "Notification"
	HookStop              = "Stop"
	HookStopFailure       = "StopFailure"
	HookSubagentStart     = "SubagentStart"
	HookSubagentStop      = "SubagentStop"
	HookTeammateIdle      = "TeammateIdle"
	HookElicitation       = "Elicitation"
	HookElicitationResult = "ElicitationResult"
	HookPreCompact        = "PreCompact"
	HookPostCompact       = "PostCompact"
	HookCwdChanged        = "CwdChanged"
	HookSessionEnd        = "SessionEnd"
)

// ObservationalEvents only tell the daemon what happened. The daemon answers
// them at once and the hook's timeout is short, so they cost the agent a
// loopback round trip and no more.
var ObservationalEvents = []string{
	HookSessionStart, HookUserPromptSubmit, HookPreToolUse,
	HookPostToolUse, HookPostToolUseFail, HookNotification,
	HookStop, HookStopFailure, HookSubagentStart, HookSubagentStop,
	HookTeammateIdle, HookPreCompact, HookPostCompact, HookCwdChanged,
	HookPermissionDenied, HookSessionEnd,
}

// DecisionEvents block the agent while a human is asked. These are the ones
// that make remote approval real rather than simulated.
var DecisionEvents = []string{HookPermissionRequest, HookElicitation}

// HookPayload is the JSON Claude Code POSTs to the daemon. Fields verified
// against 2.1.220; unknown fields are ignored, and absent fields must never be
// fatal -- new Claude Code versions add fields without warning.
type HookPayload struct {
	SessionID      string `json:"session_id"`
	PromptID       string `json:"prompt_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
	HookEventName  string `json:"hook_event_name"`

	// Subagent identity; present only for sidechain activity.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	// Tool events.
	ToolName  string          `json:"tool_name"`
	ToolInput map[string]any  `json:"tool_input"`
	ToolUseID string          `json:"tool_use_id"`
	ToolError string          `json:"tool_error"`
	Message   json.RawMessage `json:"message"`

	// Notification.
	NotificationMessage string `json:"message_text"`
}

// Subject renders a short human-readable summary for a notification body.
// It must be terse: this lands on a lock screen.
func (p *HookPayload) Subject() string {
	switch p.ToolName {
	case "":
		return p.HookEventName
	case "Bash":
		if cmd, ok := p.ToolInput["command"].(string); ok {
			return "Bash: " + truncate(collapseWS(cmd), 80)
		}
	case "Edit", "Write", "NotebookEdit":
		if path, ok := p.ToolInput["file_path"].(string); ok {
			return fmt.Sprintf("%s %s", p.ToolName, shortenPath(path))
		}
	case "Read":
		if path, ok := p.ToolInput["file_path"].(string); ok {
			return "Read " + shortenPath(path)
		}
	}
	return p.ToolName
}

// HookResponse is what we return. The zero value is "no decision", which is the
// fail-open answer for every error path: daemon confused, human unreachable,
// deadline passed. Claude Code then falls through to its normal prompt.
type HookResponse struct {
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
	SystemMessage      string              `json:"systemMessage,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName string    `json:"hookEventName"`
	Decision      *decision `json:"decision,omitempty"`
}

type decision struct {
	Behavior string `json:"behavior"` // "allow" | "deny"
	// Rule is deliberately never set by muxscale. Persisting a broadened
	// permission rule is the user's business to do in Claude Code itself; we
	// relay one answer to one question and nothing more.
}

// NoDecision is the fail-open response.
func NoDecision() HookResponse { return HookResponse{} }

// PermissionDecision builds a structured allow/deny for a held PermissionRequest.
func PermissionDecision(behavior, reason string) HookResponse {
	r := HookResponse{HookSpecificOutput: &hookSpecificOutput{
		HookEventName: HookPermissionRequest,
		Decision:      &decision{Behavior: behavior},
	}}
	if reason != "" {
		r.SystemMessage = "muxscale: " + reason
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// shortenPath keeps the last two segments, which is what identifies a file to a
// human glancing at a phone.
func shortenPath(p string) string {
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

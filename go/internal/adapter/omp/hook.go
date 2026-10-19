package omp

import (
	"encoding/json"
	"fmt"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// HookPayload is what muxscale's omp extension POSTs. It is our own protocol,
// not omp's: the extension translates omp's event objects into this flat shape
// so the daemon never has to know which build of omp sent it.
type HookPayload struct {
	V     int    `json:"v"`
	Agent string `json:"agent"`
	Event string `json:"event"`
	TS    string `json:"ts"`
	Seq   int64  `json:"seq"`

	SessionID   string `json:"session_id"`
	SessionFile string `json:"session_file"`
	Cwd         string `json:"cwd"`

	AgentVersion string `json:"agent_version"`
	Model        string `json:"model"`
	PromptID     string `json:"prompt_id"`

	ToolName  string         `json:"tool_name"`
	ToolUseID string         `json:"tool_use_id"`
	ToolInput map[string]any `json:"tool_input"`
	IsError   bool           `json:"is_error"`

	// IsTerminal is set by agent_end: false means maintenance or async work
	// will resume the session, so it is not a turn completion.
	IsTerminal *bool `json:"is_terminal"`
}

// HookReply is the extension's answer to a blocking question.
type HookReply struct {
	Decision string `json:"decision"` // "allow" | "deny" | "none"
	Reason   string `json:"reason,omitempty"`
}

var _ adapter.HookCodec = (*Adapter)(nil)

// HookPath is where the installed extension POSTs.
func (*Adapter) HookPath() string { return "/v1/hook/omp" }

// DecodeHook translates the extension's payload into the daemon's vocabulary.
func (*Adapter) DecodeHook(body []byte) (adapter.HookEvent, error) {
	var p HookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return adapter.HookEvent{}, err
	}
	if p.SessionID == "" && p.SessionFile != "" {
		// The extension prefers the session file; the id is recoverable from
		// it, and a file path is less likely to be absent across omp versions.
		p.SessionID = SessionIDFromPath(p.SessionFile)
	}
	if p.SessionID == "" {
		return adapter.HookEvent{}, fmt.Errorf("omp hook payload has no session id")
	}

	e := adapter.HookEvent{
		AgentID: "omp", SessionID: p.SessionID,
		SessionFile: p.SessionFile, Cwd: p.Cwd,
		PromptID: p.PromptID, EventName: p.Event,
		AgentVersion: p.AgentVersion, Model: p.Model,
		ToolName: p.ToolName, ToolUseID: p.ToolUseID, ToolInput: p.ToolInput,
		IsError: p.IsError,
	}
	switch p.Event {
	case "session_start":
		e.Kind = adapter.HookSessionStart
	case "agent_start", "turn_start":
		e.Kind = adapter.HookTurnStart
	case "turn_end":
		e.Kind = adapter.HookStop
	case "agent_end":
		// A non-terminal agent_end is maintenance or async delivery; the
		// session will resume. Treating it as a finished turn is exactly the
		// false "needs you" notification this product cannot afford.
		if p.IsTerminal != nil && !*p.IsTerminal {
			e.Kind = adapter.HookNotice
		} else {
			e.Kind = adapter.HookStop
		}
	case "session_shutdown":
		e.Kind = adapter.HookSessionEnd
	case "tool_call":
		e.Kind = adapter.HookToolCall
	case "tool_result":
		e.Kind = adapter.HookToolResult
	case "compaction_start", "compaction_end":
		e.Kind = adapter.HookCompaction
	case "notification":
		e.Kind = adapter.HookNotification
	default:
		e.Kind = adapter.HookNotice
	}
	return e, nil
}

// EncodeHookReply answers a blocking question. Today the extension never asks
// one -- omp's approval dialog has no extension handler -- but the codec is
// complete so the day it can, the daemon does not need to change.
func (*Adapter) EncodeHookReply(_ adapter.HookEvent, d *protocol.ApprovalDecision) any {
	if d == nil || d.Behavior == "" {
		return HookReply{Decision: "none"}
	}
	return HookReply{Decision: d.Behavior, Reason: d.Reason}
}

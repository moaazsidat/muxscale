package antigravity

import (
	"encoding/json"
	"fmt"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// HookPayload is the protojson (camelCase) payload Antigravity writes to a
// command hook's stdin. Verified against Antigravity CLI 1.5.13.
type HookPayload struct {
	ConversationID        string   `json:"conversationId"`
	WorkspacePaths        []string `json:"workspacePaths"`
	TranscriptPath        string   `json:"transcriptPath"`
	ArtifactDirectoryPath string   `json:"artifactDirectoryPath"`
	ModelName             string   `json:"modelName"`
	LastUserInput         string   `json:"lastUserInput"`
	HookEventName         string   `json:"hookEventName"`
	Cwd                   string   `json:"cwd"`

	// PreToolUse / PostToolUse
	StepIdx  int           `json:"stepIdx"`
	ToolCall *hookToolCall `json:"toolCall"`
	Error    string        `json:"error"`

	// Stop
	ExecutionNum       int    `json:"executionNum"`
	TerminationReason  string `json:"terminationReason"`
	FullyIdle          bool   `json:"fullyIdle"`
	ContinuationActive bool   `json:"continuationActive"`
}

type hookToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// HookPath is where `muxscale hook-relay --agent antigravity` posts.
func (*Adapter) HookPath() string { return "/v1/hook/antigravity" }

// DecodeHook translates an Antigravity hook payload into muxscale's vocabulary.
func (*Adapter) DecodeHook(body []byte) (adapter.HookEvent, error) {
	var p HookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return adapter.HookEvent{}, err
	}
	if p.ConversationID == "" {
		return adapter.HookEvent{}, fmt.Errorf("antigravity hook payload has no conversationId")
	}
	cwd := p.Cwd
	if cwd == "" && len(p.WorkspacePaths) > 0 {
		cwd = p.WorkspacePaths[0]
	}
	e := adapter.HookEvent{
		AgentID:     "antigravity",
		SessionID:   p.ConversationID,
		SessionFile: p.TranscriptPath,
		Cwd:         cwd,
		EventName:   p.HookEventName,
		Model:       p.ModelName,
		EndReason:   p.TerminationReason,
		IsError:     p.Error != "",
	}
	if p.ToolCall != nil {
		e.ToolName = p.ToolCall.Name
		e.ToolUseID = p.ToolCall.ID
		e.ToolInput = p.ToolCall.Args
	}
	switch p.HookEventName {
	case "SessionStart":
		e.Kind = adapter.HookSessionStart
	case "PreInvocation":
		e.Kind = adapter.HookTurnStart
	case "PreToolUse":
		e.Kind = adapter.HookToolCall
	case "PostToolUse":
		e.Kind = adapter.HookToolResult
		e.ToolOutput = p.Error
	case "Stop":
		e.Kind = adapter.HookStop
	case "PostInvocation":
		e.Kind = adapter.HookNotice
	default:
		e.Kind = adapter.HookNotice
	}
	return e, nil
}

// EncodeHookReply always returns `{}` (no decision / allow default behavior).
func (*Adapter) EncodeHookReply(adapter.HookEvent, *protocol.ApprovalDecision) any {
	return map[string]any{}
}

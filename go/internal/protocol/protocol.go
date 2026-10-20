// Package protocol defines the types shared between the daemon, its adapters,
// and (eventually) the wire protocol in proto/. Nothing here may import an
// adapter: the dependency direction is adapter -> protocol, never the reverse.
package protocol

import "time"

// SessionState is the lifecycle state of one agent session. See spec Appendix D.
type SessionState string

const (
	StateStarting        SessionState = "STARTING"
	StateRunning         SessionState = "RUNNING"
	StateWaitingInput    SessionState = "WAITING_INPUT"
	StateWaitingApproval SessionState = "WAITING_APPROVAL"
	StateWaitingExternal SessionState = "WAITING_EXTERNAL"
	StateIdle            SessionState = "IDLE"
	StateExited          SessionState = "EXITED"
	StateCrashed         SessionState = "CRASHED"
	StateDetached        SessionState = "DETACHED"
	StateUnknown         SessionState = "UNKNOWN"
)

// Waiting reports whether the state is one a human needs to resolve.
func (s SessionState) Waiting() bool {
	return s == StateWaitingInput || s == StateWaitingApproval || s == StateWaitingExternal
}

// Terminal reports whether no further events are expected.
func (s SessionState) Terminal() bool {
	return s == StateExited || s == StateCrashed
}

// Fidelity describes what an attachment permits. See spec §2.
type Fidelity string

const (
	FidelityNativeFull Fidelity = "NATIVE_FULL"
	FidelityFull       Fidelity = "FULL"
	FidelityStructured Fidelity = "STRUCTURED"
	FidelityReadOnly   Fidelity = "READONLY"
)

// Attachment is how the daemon reaches a session.
type Attachment string

const (
	AttachWrap       Attachment = "wrap"
	AttachTmux       Attachment = "tmux"
	AttachTermctl    Attachment = "termctl"
	AttachNative     Attachment = "native"
	AttachTranscript Attachment = "transcript"
)

// StateGuess is a state inference with its supporting evidence. Confidence
// gates notification (spec §4.6): guesses below the threshold are recorded but
// never pushed. Evidence exists so `muxscale debug state` can explain itself --
// an unexplainable false positive is the failure mode that kills this product.
type StateGuess struct {
	State      SessionState `json:"state"`
	Confidence float64      `json:"confidence"`
	Reason     string       `json:"reason"`
	Evidence   string       `json:"evidence,omitempty"`
	At         time.Time    `json:"at"`
}

// Session is the daemon's view of one agent conversation.
type Session struct {
	ID      string `json:"id"`       // muxscale ULID-ish id, stable across restarts
	AgentID string `json:"agent_id"` // "claude-code"

	// NaturalKey is the agent's own session id when it has one. Combined with
	// AgentID it identifies the session across daemon restarts and resumes.
	NaturalKey string `json:"natural_key"`

	Title string `json:"title,omitempty"` // e.g. Claude Code's ai-title
	// Name is the title a person gave the session, from a client or the
	// agent's own rename command. Wins over Title, which the agent makes up
	// for itself and may change at any time.
	Name       string     `json:"name,omitempty"`
	Cwd        string     `json:"cwd"`
	GitBranch  string     `json:"git_branch,omitempty"`
	Model      string     `json:"model,omitempty"`
	AgentVer   string     `json:"agent_version,omitempty"`
	Attachment Attachment `json:"attachment"`
	Fidelity   Fidelity   `json:"fidelity"`

	State      SessionState `json:"state"`
	StateSince time.Time    `json:"state_since"`
	Guess      StateGuess   `json:"guess"`

	// PromptID is the fence token (spec §5.6). For Claude Code this is the
	// hook payload's prompt_id; queued input is only delivered if it still
	// matches, so a reply composed offline cannot answer a later question.
	PromptID string `json:"prompt_id,omitempty"`

	TranscriptPath string    `json:"transcript_path,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	LastEventAt    time.Time `json:"last_event_at"`

	// ParentID links a shell session to the agent session whose working
	// directory it was opened in. Set only for shells a client asked for; a
	// client uses it to file the shell under its agent rather than listing it
	// as a peer, which is the difference between "a terminal in this project"
	// and "an eleventh session you did not start".
	ParentID string `json:"parent_id,omitempty"`

	// AllowRemoteResize permits a client to resize the PTY (spec §5.5). Off by
	// default and deliberately so: resizing reflows the TUI for *every* viewer,
	// including the human at the physical keyboard. It is set only on sessions
	// a remote client created for itself, where it is the only viewer.
	//
	// Neither this nor ParentID is persisted. Both belong to daemon-owned PTYs,
	// which do not outlive the daemon, so a restored row could only ever
	// describe a process that is already gone.
	AllowRemoteResize bool `json:"allow_remote_resize,omitempty"`

	// PendingApproval is set while a decision hook is held open awaiting a human.
	PendingApproval *ApprovalRequest `json:"pending_approval,omitempty"`

	// Queued is what a person has sent that the agent has not taken up yet,
	// oldest first: typed while it was busy, at the keyboard or from a phone.
	// Read from the agent's own record of its queue, never from what muxscale
	// sent, so it shows what the agent will actually do next. Empty for an
	// agent that keeps its queue only in memory (Codex, Gemini CLI). Not
	// persisted: each adapter reads it again from its source.
	Queued []QueuedMessage `json:"queued,omitempty"`
}

// QueuedMessage is one message waiting for the agent.
type QueuedMessage struct {
	Text string `json:"text"`
	// Delivery is when the agent will take it, where the agent says:
	// "queue" (after the current turn) or "steer" (at its next step).
	Delivery string    `json:"delivery,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

// EventKind discriminates TranscriptEvent.
type EventKind string

const (
	EventUserMessage      EventKind = "user_message"
	EventAssistantMessage EventKind = "assistant_message"
	EventThinking         EventKind = "thinking"
	EventToolCall         EventKind = "tool_call"
	EventToolResult       EventKind = "tool_result"
	EventApprovalRequest  EventKind = "approval_request"
	EventApprovalResolved EventKind = "approval_resolved"
	EventStateChange      EventKind = "state_change"
	EventTokenUsage       EventKind = "token_usage"
	EventCompaction       EventKind = "compaction"
	EventError            EventKind = "error"

	// EventCommand is something the human ran in the agent's own UI rather
	// than said to the model: a slash command such as /compact, or a shell
	// command. Text is the command line as typed.
	EventCommand EventKind = "command"
	// EventCommandOutput is what such a command printed, in Text. It follows
	// its command; the two are paired by position, not by id.
	EventCommandOutput EventKind = "command_output"
	// EventNotice is the agent's harness talking, not the human or the model:
	// a background task finishing, say. Text is a one-line summary.
	EventNotice EventKind = "notice"
)

// TranscriptEvent is one entry in a session's structured transcript.
//
// ParentID makes the transcript a tree, not a list: both Claude Code
// (parentUuid) and pi (parentId) model sessions as trees with forks. Flattening
// them would misrepresent what happened.
type TranscriptEvent struct {
	Seq       uint64    `json:"seq"`
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	SessionID string    `json:"session_id"`
	Kind      EventKind `json:"kind"`
	At        time.Time `json:"at"`

	// IsSidechain marks subagent activity, rendered as a nested thread rather
	// than interleaved into the main conversation.
	IsSidechain bool `json:"is_sidechain,omitempty"`

	Text string `json:"text,omitempty"`

	ToolName   string         `json:"tool_name,omitempty"`
	ToolUseID  string         `json:"tool_use_id,omitempty"`
	ToolInput  map[string]any `json:"tool_input,omitempty"`
	ToolOutput string         `json:"tool_output,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`

	Model string `json:"model,omitempty"`
	Usage *Usage `json:"usage,omitempty"`

	// Truncated reports that Text/ToolOutput was clipped to the excerpt limit;
	// the full body is fetched on demand.
	Truncated bool `json:"truncated,omitempty"`
	// Redacted reports that the redactor modified this event's content.
	Redacted bool `json:"redacted,omitempty"`
}

// Usage carries token accounting when the agent reports it.
type Usage struct {
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// ApprovalSource ranks how an approval request was obtained. See spec §5.7.
type ApprovalSource string

const (
	// ApprovalNative: the agent asked its client a structured question and is
	// blocking on the answer. The reply is typed data, not keystrokes.
	ApprovalNative ApprovalSource = "native"
	// ApprovalHook: a non-blocking hook reported the request; the reply is
	// translated into the agent's own keystrokes.
	ApprovalHook ApprovalSource = "hook"
	// ApprovalHeuristic: inferred from a prompt regex. Option mapping is a
	// guess, so the UI labels these distinctly and requires an extra confirm.
	ApprovalHeuristic ApprovalSource = "heuristic"
)

// ApprovalRequest is a question the agent is blocked on.
type ApprovalRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Source    ApprovalSource `json:"source"`
	Kind      string         `json:"kind"` // "tool_use", "elicitation"

	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`

	// Subject is a short human-readable summary for a notification body.
	Subject string   `json:"subject"`
	Options []string `json:"options"`

	// PromptID fences the reply to the turn that asked.
	PromptID string `json:"prompt_id,omitempty"`

	AskedAt time.Time `json:"asked_at"`
	// Deadline is when the daemon gives up waiting and returns "no decision",
	// letting the agent fall back to asking locally. Never let a monitoring
	// tool deadlock the thing it monitors.
	Deadline time.Time `json:"deadline"`
}

// ApprovalDecision is a human's answer.
type ApprovalDecision struct {
	Behavior string `json:"behavior"` // "allow" | "deny"
	Reason   string `json:"reason,omitempty"`
	// DecidedBy records which device answered, for the audit log.
	DecidedBy string `json:"decided_by,omitempty"`
}

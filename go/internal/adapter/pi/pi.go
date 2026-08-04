// Package pi adapts the pi coding agent (https://pi.dev).
//
// This is the second adapter, and its real purpose is to test the adapter
// interface: pi should ship without changing the protocol, the daemon core, or
// any client. Every place this package needs something the Claude Code adapter
// did not is a place the abstraction leaked.
//
// pi is the best-instrumented agent in the supported set. Verified against
// pi 0.83.0:
//
//   - `pi --mode rpc` speaks JSON Lines over stdio with a documented command
//     set and event stream.
//   - `agent_settled` *is* WAITING_INPUT. No quiescence timer, no prompt
//     regex, confidence 1.0.
//   - `steer` and `follow_up` deliver mid-turn input through the agent's own
//     queueing, which is a better version of what muxscale's fencing
//     approximates for agents that lack it.
//   - `extension_ui_request` lets the agent ask *its client* a structured
//     question, so approvals are typed data end to end.
package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is the floor. 0.83.0 is what the adapter was verified against.
const MinVersion = "0.80.0"

// ---- RPC framing ----

// Command is a message sent to pi on stdin.
type Command struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	// Extra carries command-specific fields, flattened into the same object.
	Extra map[string]any `json:"-"`
}

// MarshalJSON flattens Extra alongside id and type, which is the shape pi
// expects: {"id":"1","type":"prompt","message":"..."}.
func (c Command) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": c.Type}
	if c.ID != "" {
		m["id"] = c.ID
	}
	for k, v := range c.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// Message is anything pi writes to stdout: a response or an event.
type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Command string          `json:"command,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// IsResponse reports whether this answers a command.
func (m Message) IsResponse() bool { return m.Type == "response" }

// State is the payload of a get_state response, verified against pi 0.83.0.
type State struct {
	SessionID           string `json:"sessionId"`
	ThinkingLevel       string `json:"thinkingLevel"`
	IsStreaming         bool   `json:"isStreaming"`
	IsCompacting        bool   `json:"isCompacting"`
	SteeringMode        string `json:"steeringMode"`
	FollowUpMode        string `json:"followUpMode"`
	MessageCount        int    `json:"messageCount"`
	PendingMessageCount int    `json:"pendingMessageCount"`
	Model               struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
	} `json:"model"`
}

// ---- event names ----

const (
	EventAgentStart      = "agent_start"
	EventAgentEnd        = "agent_end"
	EventAgentSettled    = "agent_settled"
	EventTurnStart       = "turn_start"
	EventTurnEnd         = "turn_end"
	EventMessageStart    = "message_start"
	EventMessageUpdate   = "message_update"
	EventMessageEnd      = "message_end"
	EventToolExecStart   = "tool_execution_start"
	EventToolExecUpdate  = "tool_execution_update"
	EventToolExecEnd     = "tool_execution_end"
	EventQueueUpdate     = "queue_update"
	EventCompactionStart = "compaction_start"
	EventCompactionEnd   = "compaction_end"
	EventExtensionUIReq  = "extension_ui_request"
	EventExtensionError  = "extension_error"
	EventAutoRetryStart  = "auto_retry_start"
	EventAutoRetryEnd    = "auto_retry_end"
)

// ---- reading the stream ----

// Reader decodes pi's stdout stream.
//
// pi's docs are explicit that lines are delimited by LF only, that a trailing
// CR must be tolerated, and that U+2028/U+2029 must NOT be treated as line
// breaks -- Node's readline is non-compliant for exactly that reason. Go's
// bufio.Scanner with ScanLines splits on '\n' and strips a trailing '\r', which
// is the correct behaviour, but this is asserted by a test rather than assumed.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader wraps pi's stdout.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	// Tool output and pasted content routinely exceed the default 64 KiB.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
	return &Reader{sc: sc}
}

// Next returns the next message. Unparseable lines are skipped rather than
// ending the stream: a diagnostic line on stdout must not kill the session.
func (r *Reader) Next() (Message, bool) {
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		m.Raw = append(json.RawMessage(nil), line...)
		return m, true
	}
	return Message{}, false
}

// Err reports a read error, if any.
func (r *Reader) Err() error { return r.sc.Err() }

// ---- state mapping ----

// InferState maps a pi event to a muxscale state.
//
// Everything here is confidence 1.0 because pi is telling us, not because we
// worked it out. That is the entire argument for native adapters.
func InferState(event string) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{
			State: s, Confidence: 1.0, Reason: reason,
			Evidence: "pi:" + event, At: now,
		}, true
	}
	switch event {
	case EventAgentSettled:
		// The single highest-value signal: pi has finished and is waiting.
		return g(protocol.StateWaitingInput, "agent settled")
	case EventAgentStart, EventTurnStart, EventMessageStart:
		return g(protocol.StateRunning, "agent working")
	case EventToolExecStart:
		return g(protocol.StateRunning, "running a tool")
	case EventCompactionStart:
		return g(protocol.StateRunning, "compacting context")
	case EventExtensionUIReq:
		return g(protocol.StateWaitingApproval, "the agent asked its client a question")
	}
	return protocol.StateGuess{}, false
}

// ---- transcript mapping ----

// toolEvent is the payload shape of tool_execution_* events.
type toolEvent struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Input      map[string]any `json:"input"`
	Output     string         `json:"output"`
	IsError    bool           `json:"isError"`
}

// messageEvent is the payload shape of message_* events.
type messageEvent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Text    string `json:"text"`
	Delta   string `json:"delta"`
}

// ToTranscript converts a pi event into transcript events, or nil.
func ToTranscript(m Message) []protocol.TranscriptEvent {
	now := time.Now()
	base := protocol.TranscriptEvent{At: now}

	switch m.Type {
	case EventToolExecStart:
		var t toolEvent
		if err := json.Unmarshal(m.Raw, &t); err != nil {
			return nil
		}
		base.Kind = protocol.EventToolCall
		base.ToolName, base.ToolUseID, base.ToolInput = t.ToolName, t.ToolCallID, t.Input
		return []protocol.TranscriptEvent{base}

	case EventToolExecEnd:
		var t toolEvent
		if err := json.Unmarshal(m.Raw, &t); err != nil {
			return nil
		}
		base.Kind = protocol.EventToolResult
		base.ToolName, base.ToolUseID = t.ToolName, t.ToolCallID
		base.ToolOutput, base.Truncated = clip(t.Output)
		base.IsError = t.IsError
		return []protocol.TranscriptEvent{base}

	case EventMessageEnd:
		var e messageEvent
		if err := json.Unmarshal(m.Raw, &e); err != nil {
			return nil
		}
		text := firstNonEmpty(e.Content, e.Text)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if e.Role == "user" {
			base.Kind = protocol.EventUserMessage
		} else {
			base.Kind = protocol.EventAssistantMessage
		}
		base.Text, base.Truncated = clip(text)
		return []protocol.TranscriptEvent{base}

	case EventCompactionEnd:
		base.Kind = protocol.EventCompaction
		base.Text = "context compacted"
		return []protocol.TranscriptEvent{base}

	case EventExtensionError:
		base.Kind = protocol.EventError
		base.Text, _ = clip(string(m.Raw))
		return []protocol.TranscriptEvent{base}
	}
	return nil
}

// ---- approvals ----

// UIRequest is an extension_ui_request: the agent asking its client to render a
// dialog. This is the structured approval channel.
type UIRequest struct {
	ID      string   `json:"id"`
	Method  string   `json:"method"` // select | confirm | input | editor
	Title   string   `json:"title"`
	Message string   `json:"message"`
	Options []string `json:"options"`
	Timeout int      `json:"timeout"`
}

// ParseUIRequest extracts a UI request from an event.
func ParseUIRequest(m Message) (UIRequest, bool) {
	if m.Type != EventExtensionUIReq {
		return UIRequest{}, false
	}
	var r UIRequest
	if err := json.Unmarshal(m.Raw, &r); err != nil || r.ID == "" {
		return UIRequest{}, false
	}
	return r, true
}

// ToApproval converts a UI request into muxscale's approval shape.
//
// Only blocking dialog methods become approvals. notify/setStatus/setWidget are
// fire-and-forget and must never park a session in WAITING_APPROVAL waiting for
// an answer that pi is not expecting.
func ToApproval(r UIRequest, sessionID string, hold time.Duration) (*protocol.ApprovalRequest, bool) {
	switch r.Method {
	case "confirm", "select", "input", "editor":
	default:
		return nil, false
	}
	opts := r.Options
	if len(opts) == 0 && r.Method == "confirm" {
		opts = []string{"allow", "deny"}
	}
	subject := firstNonEmpty(r.Title, r.Message, "the agent needs an answer")
	return &protocol.ApprovalRequest{
		ID:        r.ID,
		SessionID: sessionID,
		Source:    protocol.ApprovalNative,
		Kind:      "elicitation",
		Subject:   truncate(subject, 120),
		Options:   opts,
		AskedAt:   time.Now(),
		Deadline:  time.Now().Add(hold),
	}, true
}

// UIResponse is the reply written back to pi's stdin.
type UIResponse struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Value     string `json:"value,omitempty"`
	Confirmed *bool  `json:"confirmed,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// BuildUIResponse turns a human's decision into pi's response shape.
func BuildUIResponse(r UIRequest, d protocol.ApprovalDecision) UIResponse {
	resp := UIResponse{Type: "extension_ui_response", ID: r.ID}
	switch r.Method {
	case "confirm":
		yes := d.Behavior == "allow"
		resp.Confirmed = &yes
	case "select", "input", "editor":
		if d.Behavior == "deny" || d.Behavior == "" {
			// No answer and an explicit refusal are both "cancelled" to pi.
			// Sending an empty value instead would look like a deliberate
			// blank answer, which is a different and worse outcome.
			resp.Cancelled = true
		} else {
			resp.Value = d.Reason
		}
	}
	return resp
}

// ---- input ----

// InputCommand builds the right pi command for the session's current state.
//
// pi solves mid-turn input natively: `steer` is delivered after the current
// tool phase, `follow_up` once the agent settles. That is strictly better than
// synthesising keystrokes and hoping, and it means the client can honestly say
// "queued, will be delivered after the current tool call" instead of implying
// the message was typed.
func InputCommand(text string, isStreaming bool, mode string) Command {
	if !isStreaming {
		return Command{Type: "prompt", Extra: map[string]any{"message": text}}
	}
	behavior := "steer"
	if mode == "followUp" {
		behavior = "followUp"
	}
	return Command{Type: "prompt", Extra: map[string]any{
		"message": text, "streamingBehavior": behavior,
	}}
}

// AbortCommand stops the current operation.
func AbortCommand() Command { return Command{Type: "abort"} }

// GetStateCommand queries session state.
func GetStateCommand(id string) Command { return Command{ID: id, Type: "get_state"} }

// ---- session files ----

// SessionSlug converts a working directory into pi's session directory name.
// Verified on disk: /Users/sam/dev/shop becomes
// --Users-sam-dev-shop--
func SessionSlug(cwd string) string {
	return "--" + strings.ReplaceAll(strings.Trim(filepath.Clean(cwd), "/"), "/", "-") + "--"
}

// SessionDir returns the session directory for a working directory.
func SessionDir(cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent", "sessions", SessionSlug(cwd)), nil
}

// entry is one line of a pi session file.
type entry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	ParentID      string          `json:"parentId"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	Version       string          `json:"version"`
	Message       json.RawMessage `json:"message"`
	Provider      string          `json:"provider"`
	ModelID       string          `json:"modelId"`
	Summary       string          `json:"summary"`
	Name          string          `json:"name"`
	ParentSession string          `json:"parentSession"`
}

// Meta is session metadata recovered from a session file.
type Meta struct {
	SessionID string
	Cwd       string
	Model     string
	Version   string
	Name      string
}

// ParseSession reads a pi session JSONL file.
//
// pi models a session as a *tree* (every entry has parentId, and fork/clone
// create branches), exactly like Claude Code. That is why TranscriptEvent
// carries ParentID: it is a general requirement, not a Claude Code quirk.
func ParseSession(r io.Reader) ([]protocol.TranscriptEvent, Meta, error) {
	var (
		events []protocol.TranscriptEvent
		meta   Meta
		seq    uint64
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn or unknown line
		}

		switch e.Type {
		case "session":
			meta.SessionID = e.ID
			meta.Cwd, meta.Version = e.Cwd, e.Version
			continue
		case "model_change":
			if e.ModelID != "" {
				meta.Model = e.ModelID
			}
			continue
		case "session_info":
			if e.Name != "" {
				meta.Name = e.Name
			}
			continue
		case "compaction", "branch_summary":
			seq++
			events = append(events, protocol.TranscriptEvent{
				Seq: seq, ID: e.ID, ParentID: e.ParentID,
				Kind: protocol.EventCompaction, At: parseTime(e.Timestamp),
				Text: e.Summary,
			})
			continue
		case "message":
		default:
			continue
		}

		for _, ev := range messageToEvents(e) {
			seq++
			ev.Seq = seq
			events = append(events, ev)
		}
	}
	return events, meta, sc.Err()
}

// agentMessage is pi's message shape. It follows the same content-block model
// as the Anthropic API, which keeps the mapping mechanical.
type agentMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model"`
}

type contentBlock struct {
	Type       string         `json:"type"`
	Text       string         `json:"text"`
	Thinking   string         `json:"thinking"`
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Input      map[string]any `json:"input"`
	ToolCallID string         `json:"toolCallId"`
	ToolUseID  string         `json:"tool_use_id"`
	Output     string         `json:"output"`
	IsError    bool           `json:"isError"`
}

func messageToEvents(e entry) []protocol.TranscriptEvent {
	if len(e.Message) == 0 {
		return nil
	}
	var m agentMessage
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return nil
	}
	base := protocol.TranscriptEvent{
		ID: e.ID, ParentID: e.ParentID, At: parseTime(e.Timestamp), Model: m.Model,
	}

	// Content may be a plain string.
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		ev := base
		ev.Kind = protocol.EventUserMessage
		if m.Role != "user" {
			ev.Kind = protocol.EventAssistantMessage
		}
		ev.Text, ev.Truncated = clip(s)
		return []protocol.TranscriptEvent{ev}
	}

	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	var out []protocol.TranscriptEvent
	for _, b := range blocks {
		ev := base
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			ev.Kind = protocol.EventAssistantMessage
			if m.Role == "user" {
				ev.Kind = protocol.EventUserMessage
			}
			ev.Text, ev.Truncated = clip(b.Text)
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			ev.Kind = protocol.EventThinking
			ev.Text, ev.Truncated = clip(b.Thinking)
		case "toolCall", "tool_use":
			ev.Kind = protocol.EventToolCall
			ev.ToolName = b.Name
			ev.ToolUseID = firstNonEmpty(b.ID, b.ToolCallID)
			ev.ToolInput = b.Input
		case "toolResult", "tool_result":
			ev.Kind = protocol.EventToolResult
			ev.ToolUseID = firstNonEmpty(b.ToolCallID, b.ToolUseID, b.ID)
			ev.IsError = b.IsError
			ev.ToolOutput, ev.Truncated = clip(firstNonEmpty(b.Output, b.Text))
		default:
			continue
		}
		out = append(out, ev)
	}
	return out
}

// ---- process management ----

// Client drives a pi process in RPC mode.
type Client struct {
	stdin  io.WriteCloser
	reader *Reader

	mu      sync.Mutex
	nextID  int
	pending map[string]chan Message
}

// NewClient wraps a running pi RPC process.
func NewClient(stdin io.WriteCloser, stdout io.Reader) *Client {
	return &Client{
		stdin: stdin, reader: NewReader(stdout),
		pending: map[string]chan Message{},
	}
}

// Send writes a command. Concurrent sends are serialised so two commands cannot
// interleave mid-line on stdin.
func (c *Client) Send(cmd Command) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing to pi: %w", err)
	}
	return nil
}

// NextID mints a request id for correlation.
func (c *Client) NextID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return fmt.Sprintf("mux-%d", c.nextID)
}

// Reader exposes the message stream.
func (c *Client) Reader() *Reader { return c.reader }

// ---- helpers ----

const excerptBytes = 4096

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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

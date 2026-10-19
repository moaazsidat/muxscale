// Package opencode adapts the OpenCode agent (https://opencode.ai).
//
// OpenCode is the only agent in the supported set with a full documented HTTP
// API, verified against opencode 1.18.11 by reading its live OpenAPI document:
//
//	GET  /api/event                                        server-sent events
//	GET  /api/session, /api/session/{id}                   session list + detail
//	POST /api/session/{id}/prompt                          send input
//	POST /api/session/{id}/interrupt                       abort
//	POST /api/session/{id}/permission/{req}/reply          approve or reject
//	POST /api/session/{id}/question/{req}/reply            answer a question
//
// session.idle is OpenCode's stop signal, the counterpart of Claude Code's
// Stop, pi's agent_settled and Codex's task_complete.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is the floor the adapter was verified against.
const MinVersion = "1.18.0"

// ---- events ----

const (
	EventServerConnected  = "server.connected"
	EventSessionIdle      = "session.idle"
	EventSessionError     = "session.error"
	EventSessionUpdated   = "session.updated"
	EventSessionCreated   = "session.created"
	EventSessionCompacted = "session.compacted"

	EventPermissionAsked   = "permission.asked"
	EventPermissionReplied = "permission.replied"
	EventQuestionAsked     = "question.asked"
	EventQuestionReplied   = "question.replied"
	EventQuestionRejected  = "question.rejected"

	EventPromptAdmitted = "session.next.prompt.admitted"
	EventPrompted       = "session.next.prompted"
	EventStepStarted    = "session.next.step.started"
	EventStepEnded      = "session.next.step.ended"
	EventStepFailed     = "session.next.step.failed"
	EventTextEnded      = "session.next.text.ended"
	EventReasoningEnded = "session.next.reasoning.ended"
	EventToolCalled     = "session.next.tool.called"
	EventToolSuccess    = "session.next.tool.success"
	EventToolFailed     = "session.next.tool.failed"
	EventShellStarted   = "session.next.shell.started"
	EventShellEnded     = "session.next.shell.ended"
)

// Event is one server-sent event from /api/event.
type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
	Data       json.RawMessage `json:"data"`
}

// props unmarshals an event's properties, tolerating either key. Older
// OpenCode builds used `data`; current ones use `properties`.
func (e Event) props(v any) error {
	raw := e.Properties
	if len(raw) == 0 {
		raw = e.Data
	}
	if len(raw) == 0 {
		return fmt.Errorf("no properties")
	}
	return json.Unmarshal(raw, v)
}

// SessionID extracts the session this event belongs to, if any.
func (e Event) SessionID() string {
	var p struct {
		SessionID string `json:"sessionID"`
	}
	if err := e.props(&p); err != nil {
		return ""
	}
	return p.SessionID
}

// ---- event stream ----

// StreamEvents connects to /api/event and calls fn for each event until the
// context is cancelled or the stream ends.
func StreamEvents(ctx context.Context, baseURL string, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// No client timeout: this stream is meant to stay open indefinitely.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode event stream: %s", resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// OpenCode sends ": heartbeat" comment lines to hold the connection
		// open; they are not events.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			continue
		}
		fn(e)
	}
	return sc.Err()
}

// ---- state mapping ----

// StateFromEvent maps an OpenCode event to a session state.
func StateFromEvent(eventType string) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{
			State: s, Confidence: 1.0, Reason: reason,
			Evidence: "opencode:" + eventType, At: now,
		}, true
	}
	switch eventType {
	case EventSessionIdle:
		// OpenCode's stop signal.
		return g(protocol.StateWaitingInput, "session idle")
	case EventPermissionAsked:
		return g(protocol.StateWaitingApproval, "permission requested")
	case EventQuestionAsked:
		return g(protocol.StateWaitingApproval, "the agent asked a question")
	case EventPrompted, EventPromptAdmitted, EventStepStarted, EventToolCalled, EventShellStarted:
		return g(protocol.StateRunning, "agent working")
	case EventPermissionReplied, EventQuestionReplied, EventQuestionRejected:
		return g(protocol.StateRunning, "answered; continuing")
	}
	return protocol.StateGuess{}, false
}

// ---- transcript mapping ----

type toolProps struct {
	SessionID string          `json:"sessionID"`
	CallID    string          `json:"callID"`
	Tool      string          `json:"tool"`
	Input     map[string]any  `json:"input"`
	Output    json.RawMessage `json:"output"`
	Error     string          `json:"error"`
	Metadata  json.RawMessage `json:"metadata"`
}

type textProps struct {
	SessionID string `json:"sessionID"`
	Text      string `json:"text"`
	TextID    string `json:"textID"`
}

// ToTranscript converts an OpenCode event into transcript events, or nil.
func ToTranscript(e Event) []protocol.TranscriptEvent {
	base := protocol.TranscriptEvent{At: time.Now(), SessionID: e.SessionID()}

	switch e.Type {
	case EventToolCalled:
		var p toolProps
		if err := e.props(&p); err != nil {
			return nil
		}
		base.Kind = protocol.EventToolCall
		base.ToolName, base.ToolUseID, base.ToolInput = p.Tool, p.CallID, p.Input
		return []protocol.TranscriptEvent{base}

	case EventToolSuccess, EventToolFailed:
		var p toolProps
		if err := e.props(&p); err != nil {
			return nil
		}
		base.Kind = protocol.EventToolResult
		base.ToolName, base.ToolUseID = p.Tool, p.CallID
		base.IsError = e.Type == EventToolFailed
		out := flattenText(p.Output)
		if out == "" {
			out = p.Error
		}
		base.ToolOutput, base.Truncated = clip(out)
		return []protocol.TranscriptEvent{base}

	case EventTextEnded:
		var p textProps
		if err := e.props(&p); err != nil || strings.TrimSpace(p.Text) == "" {
			return nil
		}
		base.Kind = protocol.EventAssistantMessage
		base.Text, base.Truncated = clip(p.Text)
		return []protocol.TranscriptEvent{base}

	case EventReasoningEnded:
		var p textProps
		if err := e.props(&p); err != nil || strings.TrimSpace(p.Text) == "" {
			return nil
		}
		base.Kind = protocol.EventThinking
		base.Text, base.Truncated = clip(p.Text)
		return []protocol.TranscriptEvent{base}

	case EventPrompted:
		var p struct {
			SessionID string `json:"sessionID"`
			Prompt    string `json:"prompt"`
			Text      string `json:"text"`
		}
		if err := e.props(&p); err != nil {
			return nil
		}
		text := p.Prompt
		if text == "" {
			text = p.Text
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		base.Kind = protocol.EventUserMessage
		base.Text, base.Truncated = clip(text)
		return []protocol.TranscriptEvent{base}

	case EventSessionCompacted:
		base.Kind = protocol.EventCompaction
		base.Text = "context compacted"
		return []protocol.TranscriptEvent{base}

	case EventSessionError, EventStepFailed:
		base.Kind = protocol.EventError
		base.Text, _ = clip(string(e.Properties))
		return []protocol.TranscriptEvent{base}
	}
	return nil
}

// ---- approvals ----

// PermissionAsked is the payload of a permission.asked event.
type PermissionAsked struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"`
	Patterns   []string       `json:"patterns"`
	Metadata   map[string]any `json:"metadata"`
	Always     []string       `json:"always"`
	Tool       struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool"`
}

// ParsePermission extracts a permission request from an event.
func ParsePermission(e Event) (PermissionAsked, bool) {
	if e.Type != EventPermissionAsked {
		return PermissionAsked{}, false
	}
	var p PermissionAsked
	if err := e.props(&p); err != nil || p.ID == "" {
		return PermissionAsked{}, false
	}
	return p, true
}

// ToApproval converts a permission request into muxscale's approval shape.
func (p PermissionAsked) ToApproval(sessionID string, hold time.Duration) *protocol.ApprovalRequest {
	subject := p.Permission
	if len(p.Patterns) > 0 {
		subject = p.Permission + ": " + strings.Join(p.Patterns, ", ")
	}
	return &protocol.ApprovalRequest{
		ID:        p.ID,
		SessionID: sessionID,
		Source:    protocol.ApprovalNative,
		Kind:      "tool_use",
		ToolName:  p.Permission,
		ToolUseID: p.Tool.CallID,
		ToolInput: p.Metadata,
		Subject:   truncate(subject, 120),
		// Deliberately two options, not three. OpenCode also accepts "always",
		// but that persists a broadened permission rule, and muxscale relays
		// one answer to one question rather than changing a user's policy on
		// their behalf. Same reason Claude Code's `rule` is never populated.
		Options:  []string{"allow", "deny"},
		AskedAt:  time.Now(),
		Deadline: time.Now().Add(hold),
	}
}

// PermissionReply is OpenCode's reply vocabulary: once | always | reject.
type PermissionReply string

const (
	ReplyOnce   PermissionReply = "once"
	ReplyAlways PermissionReply = "always"
	ReplyReject PermissionReply = "reject"
)

// ReplyFor maps a human decision onto OpenCode's vocabulary.
//
// "allow" becomes "once", never "always": granting a standing permission is a
// policy change the user should make in OpenCode deliberately, not a side
// effect of tapping a notification.
func ReplyFor(d protocol.ApprovalDecision) PermissionReply {
	if d.Behavior == "allow" {
		return ReplyOnce
	}
	return ReplyReject
}

// ---- client ----

// Client talks to a running OpenCode server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a client for a server base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Session is an OpenCode session as reported by the API.
//
// This struct follows the *running server*, not the OpenAPI document, because
// the two disagree on opencode 1.18.11. The document advertises `directory`,
// `path`, `slug` and `version`; the server returns those as null or omits them
// entirely, and puts the working directory in an undocumented `location`
// object. Fields are therefore accepted from either place, with the observed
// shape preferred -- a schema that matches the docs but not the wire is worse
// than useless, because it fails silently.
type Session struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Agent    string `json:"agent"`
	ParentID string `json:"parentID"`

	// Location is where the directory actually lives on the wire.
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	// Directory is the documented field, kept for versions that populate it.
	Directory string `json:"directory"`

	ProjectID   string `json:"projectID"`
	WorkspaceID string `json:"workspaceID"`

	Model struct {
		// The server sends `id`; the document says `modelID`. Accept both.
		ID         string `json:"id"`
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
		Variant    string `json:"variant"`
	} `json:"model"`

	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`

	Cost float64 `json:"cost"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// Cwd returns the session's working directory from whichever field carries it.
func (s Session) Cwd() string {
	if s.Location.Directory != "" {
		return s.Location.Directory
	}
	return s.Directory
}

// ModelName returns the model id from whichever field carries it.
func (s Session) ModelName() string {
	if s.Model.ID != "" {
		return s.Model.ID
	}
	return s.Model.ModelID
}

// UpdatedAt is when the session last changed.
func (s Session) UpdatedAt() time.Time {
	if s.Time.Updated == 0 {
		return time.Time{}
	}
	return time.UnixMilli(s.Time.Updated)
}

// IsChild reports whether this session was spawned by another, in which case it
// belongs nested rather than listed as a peer.
func (s Session) IsChild() bool { return s.ParentID != "" }

// Health reports whether the server is reachable.
func (c *Client) Health(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/health", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}

// Sessions lists sessions.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/session", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// The endpoint has returned both a bare array and an envelope across
	// versions; accept either rather than breaking on a shape change.
	var direct []Session
	if err := json.Unmarshal(body, &direct); err == nil {
		return direct, nil
	}
	var wrapped struct {
		Sessions []Session `json:"sessions"`
		Data     []Session `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		if len(wrapped.Sessions) > 0 {
			return wrapped.Sessions, nil
		}
		return wrapped.Data, nil
	}
	return nil, fmt.Errorf("unrecognised /api/session response shape")
}

// Prompt sends input to a session.
//
// delivery mirrors pi's steering: "steer" interrupts the current work, "queue"
// waits for it to finish. Mid-turn input is the agent's problem to sequence,
// not something to approximate with keystrokes.
func (c *Client) Prompt(ctx context.Context, sessionID, text, delivery string) error {
	if delivery == "" {
		delivery = "queue"
	}
	body, _ := json.Marshal(map[string]any{"prompt": text, "delivery": delivery})
	return c.post(ctx, fmt.Sprintf("/api/session/%s/prompt", sessionID), body)
}

// ReplyPermission answers a permission request.
func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID string, reply PermissionReply, message string) error {
	payload := map[string]any{"reply": string(reply)}
	if message != "" {
		payload["message"] = message
	}
	body, _ := json.Marshal(payload)
	return c.post(ctx,
		fmt.Sprintf("/api/session/%s/permission/%s/reply", sessionID, requestID), body)
}

// ReplyQuestion answers a question with selected labels, one array per question.
func (c *Client) ReplyQuestion(ctx context.Context, sessionID, requestID string, answers [][]string) error {
	body, _ := json.Marshal(map[string]any{"answers": answers})
	return c.post(ctx,
		fmt.Sprintf("/api/session/%s/question/%s/reply", sessionID, requestID), body)
}

// Interrupt stops the current operation.
func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	return c.post(ctx, fmt.Sprintf("/api/session/%s/interrupt", sessionID), nil)
}

func (c *Client) post(ctx context.Context, path string, body []byte) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ---- helpers ----

const excerptBytes = 4096

func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
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
	return string(raw)
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

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

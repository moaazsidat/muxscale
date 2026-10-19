// Package copilot adapts GitHub Copilot CLI.
//
// Written from the session event schema GitHub generates for its own SDK
// (github/copilot-sdk, nodejs/src/generated/session-events.ts, read October
// 2026) and the layout third-party readers verified against Copilot CLI 1.0.x
// -- not from a running install. `muxscale debug adapter copilot` is the
// check against real files.
//
//   - Sessions live in $COPILOT_HOME (else ~/.copilot) /session-state/<uuid>/,
//     with the conversation in events.jsonl: one event per line,
//     {id, parentId, timestamp, type, data, agentId?}. Events marked
//     ephemeral in the schema (session.idle, title changes) never reach it.
//   - session.start carries the working directory (data.context.cwd). A
//     permission prompt is persisted (permission.requested, answered by
//     permission.completed), so a pending approval can be read from the file
//     rather than guessed from silence.
package copilot

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is nominal: the format was read from the SDK's schema.
const MinVersion = "1.0.0"

type event struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	AgentID   string          `json:"agentId"`
}

type startData struct {
	SessionID     string `json:"sessionId"`
	StartTime     string `json:"startTime"`
	SelectedModel string `json:"selectedModel"`
	Context       *struct {
		Cwd    string `json:"cwd"`
		Branch string `json:"branch"`
	} `json:"context"`
}

type userMessage struct {
	Content string `json:"content"`
}

type assistantMessage struct {
	MessageID        string `json:"messageId"`
	Content          string `json:"content"`
	ReasoningText    string `json:"reasoningText"`
	Model            string `json:"model"`
	ParentToolCallID string `json:"parentToolCallId"`
	ToolRequests     []struct {
		ToolCallID string          `json:"toolCallId"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
	} `json:"toolRequests"`
}

type toolStart struct {
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	Arguments        json.RawMessage `json:"arguments"`
	ParentToolCallID string          `json:"parentToolCallId"`
}

type toolComplete struct {
	ToolCallID       string `json:"toolCallId"`
	Success          bool   `json:"success"`
	ParentToolCallID string `json:"parentToolCallId"`
	Result           *struct {
		Content         string `json:"content"`
		DetailedContent string `json:"detailedContent"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// session is a parsed events.jsonl.
type session struct {
	ID      string
	Cwd     string
	Branch  string
	Model   string
	Started time.Time
	Title   string

	units []emit.Unit
	life  string
}

// Lifecycle names: the session's state as its log last recorded it.
const (
	lifePrompt   = "copilot:prompt"   // a prompt not yet answered
	lifeWorking  = "copilot:working"  // a model step or tool is under way
	lifeApproval = "copilot:approval" // a permission request is unanswered
	lifeAnswered = "copilot:answered" // the turn ended with no tool requests
	lifeAborted  = "copilot:aborted"
	lifeError    = "copilot:error"
	lifeShutdown = "copilot:shutdown"
)

// parse reads a session's events.
func parse(r io.Reader) (*session, error) {
	s := &session{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 32*1024*1024)

	tools := map[string]int{} // toolCallId -> index in s.units
	pending := map[string]bool{}
	var events []event
	sawReasoning := false
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type == "" {
			continue // a torn last line
		}
		events = append(events, e)
		if e.Type == "assistant.reasoning" {
			sawReasoning = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// closeOpen ends calls that will never report back: the turn was
	// interrupted, failed, or the session ended under them.
	closeOpen := func(why string) {
		for _, i := range tools {
			if u := &s.units[i]; !u.Final {
				res := u.Events[1]
				res.Text, res.IsError = why, true
				u.Events[1], u.Final = res, true
			}
		}
	}

	lastAssistantCalledTools := false
	for _, e := range events {
		at := parseTime(e.Timestamp)
		base := protocol.TranscriptEvent{ID: e.ID, At: at}
		add := func(kind protocol.EventKind, text string, isErr bool) {
			ev := base
			ev.Kind, ev.IsError = kind, isErr
			ev.Text, ev.Truncated = clip(text)
			s.units = append(s.units, emit.Unit{Key: e.ID, Events: []protocol.TranscriptEvent{ev}, Final: true})
		}
		// A subagent's own events are activity inside the call that started it.
		if e.AgentID != "" {
			continue
		}
		switch e.Type {
		case "session.start", "session.resume":
			var d startData
			if json.Unmarshal(e.Data, &d) == nil {
				if d.SessionID != "" {
					s.ID = d.SessionID
				}
				if d.Context != nil {
					s.Cwd, s.Branch = d.Context.Cwd, d.Context.Branch
				}
				if d.SelectedModel != "" {
					s.Model = d.SelectedModel
				}
				if t := parseTime(d.StartTime); !t.IsZero() && s.Started.IsZero() {
					s.Started = t
				}
			}
			if e.Type == "session.resume" && s.life == lifeShutdown {
				s.life = lifeAnswered
			}

		case "session.model_change":
			var d struct {
				NewModel string `json:"newModel"`
			}
			if json.Unmarshal(e.Data, &d) == nil && d.NewModel != "" {
				s.Model = d.NewModel
			}

		case "user.message":
			var d userMessage
			if json.Unmarshal(e.Data, &d) != nil || strings.TrimSpace(d.Content) == "" {
				continue
			}
			if s.Title == "" {
				s.Title = oneLine(d.Content, 80)
			}
			add(protocol.EventUserMessage, d.Content, false)
			s.life = lifePrompt

		case "assistant.turn_start":
			s.life = lifeWorking

		case "assistant.reasoning":
			var d struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(e.Data, &d) == nil && strings.TrimSpace(d.Content) != "" {
				add(protocol.EventThinking, d.Content, false)
			}

		case "assistant.message":
			var d assistantMessage
			if json.Unmarshal(e.Data, &d) != nil || d.ParentToolCallID != "" {
				continue
			}
			if d.Model != "" {
				s.Model = d.Model
			}
			base.Model = d.Model
			if !sawReasoning && strings.TrimSpace(d.ReasoningText) != "" {
				ev := base
				ev.Kind = protocol.EventThinking
				ev.Text, ev.Truncated = clip(d.ReasoningText)
				s.units = append(s.units, emit.Unit{Key: e.ID + ":reasoning", Events: []protocol.TranscriptEvent{ev}, Final: true})
			}
			if strings.TrimSpace(d.Content) != "" {
				ev := base
				ev.Kind = protocol.EventAssistantMessage
				ev.Text, ev.Truncated = clip(d.Content)
				s.units = append(s.units, emit.Unit{Key: e.ID, Events: []protocol.TranscriptEvent{ev}, Final: true})
			}
			for _, tr := range d.ToolRequests {
				tools[tr.ToolCallID] = len(s.units)
				s.units = append(s.units, toolUnit(base, tr.ToolCallID, tr.Name, tr.Arguments))
			}
			lastAssistantCalledTools = len(d.ToolRequests) > 0
			s.life = lifeWorking

		case "tool.execution_start":
			var d toolStart
			if json.Unmarshal(e.Data, &d) != nil || d.ParentToolCallID != "" {
				continue
			}
			if _, ok := tools[d.ToolCallID]; !ok {
				// A call the person asked for directly has no request before it.
				tools[d.ToolCallID] = len(s.units)
				s.units = append(s.units, toolUnit(base, d.ToolCallID, d.ToolName, d.Arguments))
			}
			s.life = lifeWorking

		case "tool.execution_complete":
			var d toolComplete
			if json.Unmarshal(e.Data, &d) != nil || d.ParentToolCallID != "" {
				continue
			}
			i, ok := tools[d.ToolCallID]
			if !ok {
				continue
			}
			u := &s.units[i]
			res := u.Events[1]
			res.At = at
			res.IsError = !d.Success
			text := ""
			if d.Result != nil {
				text = firstNonEmpty(d.Result.Content, d.Result.DetailedContent)
			}
			if d.Error != nil && text == "" {
				text = d.Error.Message
			}
			res.Text, res.Truncated = clip(text)
			u.Events[1] = res
			u.Final = true
			s.life = lifeWorking

		case "permission.requested":
			var d struct {
				RequestID      string `json:"requestId"`
				ResolvedByHook bool   `json:"resolvedByHook"`
			}
			if json.Unmarshal(e.Data, &d) == nil && !d.ResolvedByHook {
				pending[d.RequestID] = true
				s.life = lifeApproval
			}

		case "permission.completed":
			var d struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(e.Data, &d) == nil {
				delete(pending, d.RequestID)
				if len(pending) == 0 {
					s.life = lifeWorking
				}
			}

		case "assistant.turn_end":
			if len(pending) == 0 && !lastAssistantCalledTools {
				s.life = lifeAnswered
			}

		case "session.task_complete":
			s.life = lifeAnswered

		case "abort":
			var d struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(e.Data, &d)
			closeOpen("interrupted")
			add(protocol.EventNotice, "interrupted ("+strings.ReplaceAll(d.Reason, "_", " ")+")", false)
			pending = map[string]bool{}
			s.life = lifeAborted

		case "session.error":
			var d struct {
				ErrorType string `json:"errorType"`
				Message   string `json:"message"`
			}
			_ = json.Unmarshal(e.Data, &d)
			closeOpen("the turn failed")
			add(protocol.EventError, fmt.Sprintf("%s: %s", d.ErrorType, d.Message), true)
			s.life = lifeError

		case "session.compaction_complete":
			add(protocol.EventCompaction, "Copilot compacted the conversation", false)

		case "session.shutdown":
			closeOpen("the session ended")
			s.life = lifeShutdown
		}
	}
	return s, nil
}

func toolUnit(base protocol.TranscriptEvent, id, name string, rawArgs json.RawMessage) emit.Unit {
	call := base
	call.ID = id
	call.Kind = protocol.EventToolCall
	call.ToolName, call.ToolUseID = name, id
	call.ToolInput = arguments(rawArgs)
	res := call
	res.ID = id + ":result"
	res.Kind = protocol.EventToolResult
	res.ToolInput = nil
	return emit.Unit{Key: "tool:" + id, Events: []protocol.TranscriptEvent{call, res}, Early: 1}
}

// arguments is a tool's input: an object, or a JSON string holding one.
func arguments(raw json.RawMessage) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && json.Unmarshal([]byte(s), &m) == nil {
		return m
	}
	if s != "" {
		return map[string]any{"input": s}
	}
	return nil
}

func stateFromLifecycle(life string, idle, quiescence time.Duration) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, c float64, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{State: s, Confidence: c, Reason: reason, Evidence: life, At: now}, true
	}
	switch life {
	case lifeApproval:
		// Copilot persisted the request and has not persisted an answer.
		return g(protocol.StateWaitingApproval, 0.85, "a permission request is unanswered")
	case lifeAnswered:
		if idle >= quiescence {
			return g(protocol.StateWaitingInput, 0.8, "the turn ended with no tool requests")
		}
		return g(protocol.StateRunning, 0.6, "the turn just ended; another step may follow")
	case lifeAborted:
		return g(protocol.StateWaitingInput, 0.85, "the turn was interrupted")
	case lifeError:
		return g(protocol.StateWaitingInput, 0.7, "the turn failed")
	case lifeShutdown:
		return g(protocol.StateExited, 0.9, "Copilot recorded the session shutting down")
	case lifeWorking:
		return g(protocol.StateRunning, 0.65, "a model step or tool is under way")
	case lifePrompt:
		return g(protocol.StateRunning, 0.55, "a prompt is waiting for an answer")
	}
	return protocol.StateGuess{}, false
}

// ---- paths ----

// home is $COPILOT_HOME, else ~/.copilot.
func home() (string, error) {
	if d := os.Getenv("COPILOT_HOME"); d != "" {
		return d, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".copilot"), nil
}

// sessionsRoot is where each session's directory is.
func sessionsRoot() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "session-state"), nil
}

// ---- small helpers ----

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

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Package kimi adapts Moonshot's Kimi Code CLI (`kimi`).
//
// Written from Kimi Code's own source (MoonshotAI/kimi-code, main, read
// October 2026): the wire manifest (packages/agent-core-v2/docs/
// wire-manifest.d.ts, protocol 1.5), the loop-event fold, its session
// browser's wire reader and test fixtures (apps/vis/server), and the
// work-directory key (workdir-slug.ts) -- not from a running install.
// `muxscale debug adapter kimi` is the check against real files.
//
//   - $KIMI_CODE_HOME (else ~/.kimi-code) holds session_index.jsonl, one
//     {sessionId, sessionDir, workDir} per line, and
//     sessions/wd_<slug>_<sha256[:12]>/<sessionId>/ with state.json (title,
//     lastPrompt, createdAt, updatedAt) and agents/main/wire.jsonl.
//   - wire.jsonl opens with {"type":"metadata","protocol_version",...}; every
//     other line is {"type", ...payload, "time"}. An assistant reply arrives
//     either whole (context.append_message) or as loop events (step.begin,
//     content.part, tool.call, tool.result, step.end); turn.ended carries how
//     a turn finished, and interaction.request a question or approval the
//     agent is waiting on until interaction.resolved.
//
// The archived Python kimi-cli kept ~/.kimi with an older wire format; it is
// not read here.
package kimi

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is nominal: the format was read from source.
const MinVersion = "0.1.0"

type record struct {
	Type string          `json:"type"`
	Time json.RawMessage `json:"time"`

	// context.append_message
	Message *contextMessage `json:"message"`
	// context.append_loop_event
	Event *loopEvent `json:"event"`
	// turn.prompt
	Input  json.RawMessage `json:"input"`
	Origin json.RawMessage `json:"origin"`
	// turn.ended, prompt.completed, turn.cancel
	Reason string `json:"reason"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	// interaction.request / interaction.resolved
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// context.apply_compaction
	Summary string `json:"summary"`
	// usage.record
	Model string `json:"model"`
}

type contextMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"toolCalls"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Origin     string          `json:"origin"`
}

type toolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type loopEvent struct {
	Type         string          `json:"type"`
	UUID         string          `json:"uuid"`
	StepUUID     string          `json:"stepUuid"`
	FinishReason string          `json:"finishReason"`
	Part         json.RawMessage `json:"part"`
	ToolCallID   string          `json:"toolCallId"`
	Name         string          `json:"name"`
	Args         json.RawMessage `json:"args"`
	Result       *struct {
		Output  json.RawMessage `json:"output"`
		IsError bool            `json:"isError"`
	} `json:"result"`
}

type contentPart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Think string `json:"think"`
}

// session is a parsed wire.jsonl.
type session struct {
	units []emit.Unit
	life  string
	Model string
}

// Lifecycle names: the session's state as its wire last recorded it.
const (
	lifePrompt   = "kimi:prompt"   // a prompt the agent has not finished with
	lifeStep     = "kimi:step"     // a model step or tool is under way
	lifeApproval = "kimi:approval" // an approval is waiting for the person
	lifeQuestion = "kimi:question" // a question is waiting for the person
	lifeDone     = "kimi:done"     // the turn completed
	lifeStopped  = "kimi:stopped"  // cancelled, failed or blocked
)

func parse(r io.Reader) (*session, error) {
	s := &session{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 64*1024*1024)

	tools := map[string]int{}
	pending := map[string]string{} // interaction id -> kind
	var stepText, stepThink strings.Builder
	var stepKey string
	var stepAt time.Time
	// lastStep is the text of the step most recently shown, so the whole
	// message context.append_message seals it into is not shown again.
	var lastStep string
	n := 0

	flushStep := func() {
		if stepKey == "" {
			return
		}
		if t := strings.TrimSpace(stepThink.String()); t != "" {
			ev := protocol.TranscriptEvent{ID: stepKey + ":think", At: stepAt, Kind: protocol.EventThinking, Model: s.Model}
			ev.Text, ev.Truncated = clip(t)
			s.units = append(s.units, emit.Unit{Key: stepKey + ":think", Events: []protocol.TranscriptEvent{ev}, Final: true})
		}
		if t := strings.TrimSpace(stepText.String()); t != "" {
			ev := protocol.TranscriptEvent{ID: stepKey + ":text", At: stepAt, Kind: protocol.EventAssistantMessage, Model: s.Model}
			ev.Text, ev.Truncated = clip(t)
			s.units = append(s.units, emit.Unit{Key: stepKey + ":text", Events: []protocol.TranscriptEvent{ev}, Final: true})
		}
		lastStep = strings.TrimSpace(stepText.String())
		stepText.Reset()
		stepThink.Reset()
		stepKey = ""
	}
	addTool := func(key, id, name string, args json.RawMessage, at time.Time) {
		if _, ok := tools[id]; ok {
			return
		}
		call := protocol.TranscriptEvent{ID: id, At: at, Kind: protocol.EventToolCall, ToolName: name, ToolUseID: id, ToolInput: arguments(args), Model: s.Model}
		res := protocol.TranscriptEvent{ID: id + ":result", At: at, Kind: protocol.EventToolResult, ToolName: name, ToolUseID: id}
		tools[id] = len(s.units)
		s.units = append(s.units, emit.Unit{Key: key, Events: []protocol.TranscriptEvent{call, res}, Early: 1})
	}
	finishTool := func(id string, output json.RawMessage, isErr bool, at time.Time) {
		i, ok := tools[id]
		if !ok {
			return
		}
		u := &s.units[i]
		if u.Final {
			return
		}
		res := u.Events[1]
		res.At, res.IsError = at, isErr
		res.Text, res.Truncated = clip(partsText(output))
		u.Events[1], u.Final = res, true
	}
	closeOpen := func(why string) {
		for _, i := range tools {
			if u := &s.units[i]; !u.Final {
				res := u.Events[1]
				res.Text, res.IsError = why, true
				u.Events[1], u.Final = res, true
			}
		}
	}
	note := func(key string, kind protocol.EventKind, text string, at time.Time) {
		ev := protocol.TranscriptEvent{ID: key, At: at, Kind: kind, IsError: kind == protocol.EventError}
		ev.Text, ev.Truncated = clip(text)
		s.units = append(s.units, emit.Unit{Key: key, Events: []protocol.TranscriptEvent{ev}, Final: true})
	}

	for sc.Scan() {
		var rec record
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type == "" {
			continue
		}
		n++
		at := recordTime(rec.Time)
		key := "r" + strconv.Itoa(n) // the line's position: stable, the file is append-only
		switch rec.Type {
		case "usage.record":
			if rec.Model != "" {
				s.Model = rec.Model
			}

		case "turn.prompt":
			flushStep()
			lastStep = ""
			s.life = lifePrompt

		case "context.append_message":
			m := rec.Message
			if m == nil {
				continue
			}
			switch m.Role {
			case "user":
				flushStep()
				text := partsText(m.Content)
				if text == "" {
					continue
				}
				switch m.Origin {
				case "", "user":
					ev := protocol.TranscriptEvent{ID: key, At: at, Kind: protocol.EventUserMessage}
					ev.Text, ev.Truncated = clip(text)
					s.units = append(s.units, emit.Unit{Key: key, Events: []protocol.TranscriptEvent{ev}, Final: true})
				case "shell_command":
					note(key, protocol.EventCommand, text, at)
				default:
					// injected context, hook results, compaction summaries:
					// what the agent was given, not what the person said.
				}
			case "assistant":
				// A step already recorded as loop events is also sealed into
				// context as a whole message; showing both would say it twice.
				text := partsText(m.Content)
				flushStep()
				dup := text != "" && text == lastStep
				lastStep = ""
				if !dup {
					if th := thinkText(m.Content); th != "" {
						note(key+":think", protocol.EventThinking, th, at)
					}
					if text != "" {
						note(key, protocol.EventAssistantMessage, text, at)
					}
				}
				for _, tc := range m.ToolCalls {
					addTool("tool:"+tc.ID, tc.ID, tc.Name, tc.Arguments, at)
				}
				s.life = lifeStep
			case "tool":
				finishTool(m.ToolCallID, m.Content, m.IsError, at)
			}

		case "context.append_loop_event":
			ev := rec.Event
			if ev == nil {
				continue
			}
			switch ev.Type {
			case "step.begin":
				flushStep()
				stepKey, stepAt = "step:"+firstNonEmpty(ev.UUID, key), at
				s.life = lifeStep
			case "content.part":
				var p contentPart
				if json.Unmarshal(ev.Part, &p) != nil {
					continue
				}
				if stepKey == "" {
					stepKey, stepAt = "step:"+firstNonEmpty(ev.StepUUID, key), at
				}
				switch p.Type {
				case "text":
					stepText.WriteString(p.Text)
				case "think":
					stepThink.WriteString(p.Think)
				}
			case "tool.call":
				flushStep()
				addTool("tool:"+ev.ToolCallID, ev.ToolCallID, ev.Name, ev.Args, at)
			case "tool.result":
				if ev.Result != nil {
					finishTool(ev.ToolCallID, ev.Result.Output, ev.Result.IsError, at)
				}
			case "step.end":
				// A step ending is not the turn ending, even with no tool
				// calls: turn.ended is the record that says so.
				flushStep()
			}

		case "interaction.request":
			switch rec.Kind {
			case "approval":
				pending[rec.ID] = rec.Kind
				s.life = lifeApproval
			case "question":
				pending[rec.ID] = rec.Kind
				s.life = lifeQuestion
			}
		case "interaction.resolved":
			delete(pending, rec.ID)
			if len(pending) == 0 && (s.life == lifeApproval || s.life == lifeQuestion) {
				s.life = lifeStep
			}

		case "turn.ended":
			flushStep()
			pending = map[string]string{}
			switch rec.Reason {
			case "completed":
				s.life = lifeDone
			default:
				closeOpen("the turn " + rec.Reason)
				if rec.Error != nil && rec.Error.Message != "" {
					note(key, protocol.EventError, rec.Error.Message, at)
				} else if rec.Reason != "" {
					note(key, protocol.EventNotice, "turn "+rec.Reason, at)
				}
				s.life = lifeStopped
			}
		case "turn.cancel", "prompt.aborted":
			flushStep()
			closeOpen("interrupted")
			pending = map[string]string{}
			s.life = lifeStopped

		case "context.apply_compaction", "full_compaction.complete":
			flushStep()
			note(key, protocol.EventCompaction, firstNonEmpty(rec.Summary, "Kimi compacted the conversation"), at)
		}
	}
	flushStep()
	return s, sc.Err()
}

func stateFromLifecycle(life string, idle, quiescence time.Duration) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, c float64, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{State: s, Confidence: c, Reason: reason, Evidence: life, At: now}, true
	}
	switch life {
	case lifeApproval:
		return g(protocol.StateWaitingApproval, 0.85, "an approval request is unanswered")
	case lifeQuestion:
		return g(protocol.StateWaitingInput, 0.85, "Kimi asked a question")
	case lifeDone:
		return g(protocol.StateWaitingInput, 0.85, "the turn completed")
	case lifeStopped:
		return g(protocol.StateWaitingInput, 0.8, "the turn stopped")
	case lifeStep:
		return g(protocol.StateRunning, 0.65, "a model step or tool is under way")
	case lifePrompt:
		return g(protocol.StateRunning, 0.6, "a prompt is under way")
	}
	return protocol.StateGuess{}, false
}

// ---- content ----

// partsText is the text of a content list, or a plain string.
func partsText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func thinkText(raw json.RawMessage) string {
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "think" {
			b.WriteString(p.Think)
		}
	}
	return strings.TrimSpace(b.String())
}

// arguments is a tool's input: an object, or a JSON string holding one.
func arguments(raw json.RawMessage) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return m
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
		if s != "" {
			return map[string]any{"input": s}
		}
	}
	return nil
}

// recordTime reads `time`, which the fixtures write as epoch milliseconds.
func recordTime(raw json.RawMessage) time.Time {
	var ms float64
	if json.Unmarshal(raw, &ms) == nil && ms > 0 {
		return time.UnixMilli(int64(ms))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ---- paths ----

// home is $KIMI_CODE_HOME, else ~/.kimi-code.
func home() (string, error) {
	if d := os.Getenv("KIMI_CODE_HOME"); d != "" {
		return d, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".kimi-code"), nil
}

var slugJunk = regexp.MustCompile(`[^a-z0-9._-]+`)

// workDirKey is encodeWorkDirKey from workdir-slug.ts:
// wd_<slug of the last path element>_<first 12 hex of sha256(path)>.
func workDirKey(workDir string) string {
	normalized := strings.TrimRight(strings.ReplaceAll(workDir, `\`, "/"), "/")
	base := normalized
	if i := strings.LastIndexByte(normalized, '/'); i >= 0 {
		base = normalized[i+1:]
	}
	slug := strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" || slug == "." || slug == ".." {
		slug = "workspace"
	}
	sum := sha256.Sum256([]byte(normalized))
	return "wd_" + slug + "_" + hex.EncodeToString(sum[:])[:12]
}

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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

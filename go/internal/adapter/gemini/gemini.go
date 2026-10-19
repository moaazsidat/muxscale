// Package gemini adapts Google's Gemini CLI.
//
// Written from Gemini CLI's own source (google-gemini/gemini-cli, main, read
// October 2026) -- packages/core/src/services/chatRecordingService.ts and
// chatRecordingTypes.ts, config/storage.ts, config/projectRegistry.ts -- not
// from a running install: none was available. `muxscale debug adapter
// gemini-cli` is the check against real files.
//
//   - Sessions live in ~/.gemini/tmp/<project>/chats/session-<time>-<id8>.jsonl.
//     <project> is a short slug recorded in ~/.gemini/projects.json
//     ({"projects": {"/abs/path": "slug"}}); older installs used the SHA-256
//     of the project root instead. Subagent sessions sit in a directory named
//     after their parent and are activity inside it, not peers.
//   - The file is a log to be replayed, not a transcript to be read: the first
//     line is the conversation's metadata, a message is re-appended whole each
//     time it changes (a tool call finishing re-appends its message), $set
//     updates metadata, $rewindTo drops a message and everything after it,
//     and $patch rewrites messages in place. Legacy sessions are one JSON
//     object.
package gemini

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is nominal: the format was read from source, not a release.
const MinVersion = "0.1.0"

// conversation is a replayed session.
type conversation struct {
	SessionID   string
	ProjectHash string
	StartTime   time.Time
	LastUpdated time.Time
	Summary     string
	Kind        string
	messages    []*message
}

type message struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // user | gemini | info | error | warning
	Content   json.RawMessage `json:"content"`
	Display   json.RawMessage `json:"displayContent"`
	ToolCalls []toolCall      `json:"toolCalls"`
	Thoughts  []struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
	} `json:"thoughts"`
	Model string `json:"model"`
}

type toolCall struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	DisplayName   string          `json:"displayName"`
	Args          map[string]any  `json:"args"`
	Result        json.RawMessage `json:"result"`
	ResultDisplay json.RawMessage `json:"resultDisplay"`
	Status        string          `json:"status"`
	Timestamp     string          `json:"timestamp"`
}

// record is any line of the log; which fields are set says what it is.
type record struct {
	// metadata
	SessionID   string `json:"sessionId"`
	ProjectHash string `json:"projectHash"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Summary     string `json:"summary"`
	Kind        string `json:"kind"`

	// message
	ID string `json:"id"`

	Set *struct {
		SessionID   string     `json:"sessionId"`
		LastUpdated string     `json:"lastUpdated"`
		Summary     *string    `json:"summary"`
		Messages    []*message `json:"messages"`
	} `json:"$set"`
	RewindTo *string `json:"$rewindTo"`
	Patch    *struct {
		ID        string          `json:"id"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []toolCallPatch `json:"toolCalls"`
		Updates   []struct {
			ID        string          `json:"id"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []toolCallPatch `json:"toolCalls"`
		} `json:"updates"`
		RemoveIDs []string `json:"removeIds"`
		OrderIDs  []string `json:"orderIds"`
	} `json:"$patch"`

	// legacy single-object sessions carry the whole list
	Messages []*message `json:"messages"`
}

type toolCallPatch struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
}

// replay reads a session log, or a legacy single-object session.
func replay(r io.Reader) (*conversation, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	c := &conversation{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 256*1024), 32*1024*1024)
	lines := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // a torn last line, or a corrupt one gemini also skips
		}
		lines++
		c.apply(rec, []byte(line))
	}
	if c.SessionID == "" && lines <= 1 {
		// Legacy: the whole file is one ConversationRecord, possibly pretty
		// printed across many lines.
		var rec record
		if json.Unmarshal(data, &rec) == nil && rec.SessionID != "" {
			c = &conversation{}
			c.setMeta(rec)
			c.messages = rec.Messages
		}
	}
	return c, nil
}

func (c *conversation) setMeta(rec record) {
	c.SessionID, c.ProjectHash, c.Kind = rec.SessionID, rec.ProjectHash, rec.Kind
	c.StartTime = parseTime(rec.StartTime)
	c.LastUpdated = parseTime(rec.LastUpdated)
	if rec.Summary != "" {
		c.Summary = rec.Summary
	}
}

func (c *conversation) apply(rec record, raw []byte) {
	switch {
	case rec.Set != nil:
		if rec.Set.SessionID != "" {
			c.SessionID = rec.Set.SessionID
		}
		if t := parseTime(rec.Set.LastUpdated); !t.IsZero() {
			c.LastUpdated = t
		}
		if rec.Set.Summary != nil {
			c.Summary = *rec.Set.Summary
		}
		if rec.Set.Messages != nil {
			c.messages = rec.Set.Messages // a legacy full-history checkpoint
		}
	case rec.RewindTo != nil:
		for i, m := range c.messages {
			if m.ID == *rec.RewindTo {
				c.messages = c.messages[:i]
				break
			}
		}
	case rec.Patch != nil:
		p := rec.Patch
		if p.ID != "" {
			c.patch(p.ID, p.Content, p.ToolCalls)
		}
		for _, u := range p.Updates {
			c.patch(u.ID, u.Content, u.ToolCalls)
		}
		if len(p.RemoveIDs) > 0 {
			drop := map[string]bool{}
			for _, id := range p.RemoveIDs {
				drop[id] = true
			}
			kept := c.messages[:0]
			for _, m := range c.messages {
				if !drop[m.ID] {
					kept = append(kept, m)
				}
			}
			c.messages = kept
		}
		if len(p.OrderIDs) > 0 {
			byID := map[string]*message{}
			for _, m := range c.messages {
				byID[m.ID] = m
			}
			var ordered []*message
			for _, id := range p.OrderIDs {
				if m, ok := byID[id]; ok {
					ordered = append(ordered, m)
					delete(byID, id)
				}
			}
			for _, m := range c.messages { // anything the order left out keeps its place at the end
				if _, ok := byID[m.ID]; ok {
					ordered = append(ordered, m)
				}
			}
			c.messages = ordered
		}
	case rec.ID != "":
		var m message
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		for i, old := range c.messages {
			if old.ID == m.ID {
				c.messages[i] = &m // re-appended whole: the newest copy wins
				return
			}
		}
		c.messages = append(c.messages, &m)
	case rec.SessionID != "":
		c.setMeta(rec)
	}
}

func (c *conversation) patch(id string, content json.RawMessage, calls []toolCallPatch) {
	for _, m := range c.messages {
		if m.ID != id {
			continue
		}
		if len(content) > 0 {
			m.Content = content
		}
		for _, p := range calls {
			for i := range m.ToolCalls {
				if m.ToolCalls[i].ID == p.ID && len(p.Result) > 0 {
					m.ToolCalls[i].Result = p.Result
				}
			}
		}
	}
}

// ---- conversation -> transcript ----

// terminal reports a tool call that will not change again.
func terminal(status string) bool {
	switch status {
	case "success", "error", "cancelled":
		return true
	}
	return false
}

func (c *conversation) units() []emit.Unit {
	var out []emit.Unit
	for _, m := range c.messages {
		at := parseTime(m.Timestamp)
		base := protocol.TranscriptEvent{ID: m.ID, At: at, Model: m.Model}
		switch m.Type {
		case "user":
			text := partsText(m.Display)
			if text == "" {
				text = partsText(m.Content)
			}
			if text != "" {
				ev := base
				ev.Kind = protocol.EventUserMessage
				ev.Text, ev.Truncated = clip(text)
				out = append(out, emit.Unit{Key: m.ID, Events: []protocol.TranscriptEvent{ev}, Final: true})
			}
		case "gemini":
			// Thoughts first, then what was said, then the calls it made: the
			// order a reader would have seen them stream.
			for i, th := range m.Thoughts {
				text := strings.TrimSpace(strings.TrimSpace(th.Subject + "\n" + th.Description))
				if text == "" {
					continue
				}
				ev := base
				ev.Kind = protocol.EventThinking
				ev.Text, ev.Truncated = clip(text)
				out = append(out, emit.Unit{Key: m.ID + ":thought:" + itoa(i), Events: []protocol.TranscriptEvent{ev}, Final: true})
			}
			if text := partsText(m.Content); text != "" {
				ev := base
				ev.Kind = protocol.EventAssistantMessage
				ev.Text, ev.Truncated = clip(text)
				// Text grows while it streams; it is final once something
				// follows it or the conversation has moved on.
				out = append(out, emit.Unit{Key: m.ID + ":text", Events: []protocol.TranscriptEvent{ev}, Final: true})
			}
			for _, tc := range m.ToolCalls {
				call := base
				call.ID = tc.ID
				call.ParentID = m.ID
				call.Kind = protocol.EventToolCall
				call.ToolName, call.ToolUseID, call.ToolInput = tc.Name, tc.ID, tc.Args
				if t := parseTime(tc.Timestamp); !t.IsZero() {
					call.At = t
				}
				res := call
				res.ID = tc.ID + ":result"
				res.Kind = protocol.EventToolResult
				res.ToolInput = nil
				res.IsError = tc.Status == "error"
				res.Text, res.Truncated = clip(firstNonEmpty(resultText(tc.Result), displayText(tc.ResultDisplay), tc.Status))
				out = append(out, emit.Unit{
					Key:    "tool:" + tc.ID,
					Events: []protocol.TranscriptEvent{call, res},
					Early:  1,
					Final:  terminal(tc.Status),
				})
			}
		case "error", "warning", "info":
			text := partsText(m.Content)
			if text == "" {
				continue
			}
			ev := base
			ev.Kind = protocol.EventNotice
			if m.Type == "error" {
				ev.Kind, ev.IsError = protocol.EventError, true
			}
			ev.Text, ev.Truncated = clip(text)
			out = append(out, emit.Unit{Key: m.ID, Events: []protocol.TranscriptEvent{ev}, Final: true})
		}
	}
	return out
}

// Lifecycle names: the conversation's state as its log last recorded it.
const (
	lifeUser      = "gemini:prompt"     // a prompt not yet answered
	lifeApproval  = "gemini:approval"   // a tool call awaiting approval
	lifeTools     = "gemini:tools"      // tool calls running
	lifeToolsDone = "gemini:tools-done" // calls finished; the model speaks next
	lifeAnswered  = "gemini:answered"   // the model replied with no calls
	lifeError     = "gemini:error"      // the last turn failed
)

func (c *conversation) lifecycle() string {
	for i := len(c.messages) - 1; i >= 0; i-- {
		m := c.messages[i]
		switch m.Type {
		case "user":
			return lifeUser
		case "error":
			return lifeError
		case "gemini":
			if len(m.ToolCalls) == 0 {
				return lifeAnswered
			}
			running := false
			for _, tc := range m.ToolCalls {
				if tc.Status == "awaiting_approval" {
					return lifeApproval
				}
				if !terminal(tc.Status) {
					running = true
				}
			}
			if running {
				return lifeTools
			}
			return lifeToolsDone
		}
		// info and warning say nothing about whose turn it is.
	}
	return ""
}

func stateFromLifecycle(life string, idle, quiescence time.Duration) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, c float64, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{State: s, Confidence: c, Reason: reason, Evidence: life, At: now}, true
	}
	switch life {
	case lifeApproval:
		return g(protocol.StateWaitingApproval, 0.75, "a tool call is waiting for approval")
	case lifeTools:
		return g(protocol.StateRunning, 0.6, "a tool call is running")
	case lifeToolsDone:
		return g(protocol.StateRunning, 0.55, "tool calls finished; the model speaks next")
	case lifeUser:
		return g(protocol.StateRunning, 0.55, "a prompt is waiting for an answer")
	case lifeError:
		return g(protocol.StateWaitingInput, 0.65, "the turn failed")
	case lifeAnswered:
		if idle >= quiescence {
			return g(protocol.StateWaitingInput, 0.7, "the model answered and the log has been quiet")
		}
		return g(protocol.StateRunning, 0.6, "the model is answering")
	}
	return protocol.StateGuess{}, false
}

// ---- content ----

// part is a @google/genai Part, the subset a transcript shows.
type part struct {
	Text             string `json:"text"`
	Thought          bool   `json:"thought"`
	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
}

// partsText flattens a PartListUnion: a string, a Part, or a list of either.
func partsText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		list = []json.RawMessage{raw}
	}
	var b strings.Builder
	for _, item := range list {
		var s string
		if json.Unmarshal(item, &s) == nil {
			b.WriteString(s)
			continue
		}
		var p part
		if json.Unmarshal(item, &p) == nil && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// resultText is what a tool returned to the model: a functionResponse's
// output or error, else any text.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		list = []json.RawMessage{raw}
	}
	var b strings.Builder
	for _, item := range list {
		var p part
		if json.Unmarshal(item, &p) != nil {
			continue
		}
		if p.FunctionResponse != nil {
			var resp struct {
				Output any `json:"output"`
				Error  any `json:"error"`
			}
			if json.Unmarshal(p.FunctionResponse.Response, &resp) == nil {
				if s := stringish(resp.Output); s != "" {
					b.WriteString(s)
					continue
				}
				if s := stringish(resp.Error); s != "" {
					b.WriteString(s)
					continue
				}
			}
			b.Write(p.FunctionResponse.Response)
			continue
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

func displayText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func stringish(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// ---- paths ----

// geminiDir is ~/.gemini.
func geminiDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini"), nil
}

// projectDirs maps each project directory under tmp/ to the working
// directory it belongs to, from projects.json. A legacy hash-named directory
// is absent, and its cwd unknown until a process in it is matched.
func projectDirs(root string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(root, "projects.json"))
	if err != nil {
		return out
	}
	var reg struct {
		Projects map[string]string `json:"projects"`
	}
	if json.Unmarshal(raw, &reg) != nil {
		return out
	}
	for path, slug := range reg.Projects {
		out[slug] = path
	}
	return out
}

// projectIDs are the directory names a working directory's sessions may be
// under: its registered slug, and the legacy hash.
func projectIDs(root, cwd string) []string {
	clean := filepath.Clean(cwd)
	var ids []string
	for slug, path := range projectDirs(root) {
		if filepath.Clean(path) == clean {
			ids = append(ids, slug)
		}
	}
	sum := sha256.Sum256([]byte(clean))
	return append(ids, hex.EncodeToString(sum[:]))
}

const excerptBytes = 4096

func clip(s string) (string, bool) {
	if len(s) <= excerptBytes {
		return s, false
	}
	cut := excerptBytes
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

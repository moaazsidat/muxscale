package claudecode

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Transcript parsing for Claude Code JSONL.
//
// Schema verified against Claude Code 2.1.220 by reading real session files:
// every line carries sessionId/uuid/parentUuid/cwd/gitBranch/version/timestamp,
// and `message` is the verbatim Anthropic Messages API shape -- content blocks
// of thinking|text|tool_use|tool_result, plus model and usage. That means this
// is a translation, not a parser in the risky sense: no regexes, no guessing.

// entry is one line of the JSONL.
type entry struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	SessionID   string `json:"sessionId"`
	Timestamp   string `json:"timestamp"`
	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Version     string `json:"version"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	// IsCompactSummary marks the user entry Claude Code writes after
	// compacting: the model's summary of everything before it, which the CLI
	// sends as if the human had said it.
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`

	// Sidecar entry types that carry session metadata rather than conversation.
	AITitle string `json:"aiTitle"`
	// CustomTitle is what /rename set. A pointer, because renaming to nothing
	// clears the name and has to be told apart from an entry without one.
	CustomTitle *string `json:"customTitle"`
	LastPrompt  string  `json:"lastPrompt"`
	Mode        string  `json:"mode"`
	PermMode    string  `json:"permissionMode"`

	// queue-operation entries; see queue.go.
	Operation string          `json:"operation"`
	Content   json.RawMessage `json:"content"`

	// Attachment is an attachment entry's body; only queued_command is read.
	Attachment *struct {
		Type string `json:"type"`
		// A string, or content blocks when the message carried an image
		// (Claude Code 2.1.301). Decoded as either: typed as a string, a
		// message with an image failed the whole line and vanished.
		Prompt      json.RawMessage `json:"prompt"`
		CommandMode string          `json:"commandMode"`
	} `json:"attachment"`
}

// message is the Anthropic Messages API shape as persisted.
type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *apiUsage       `json:"usage"`
}

type apiUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	CacheReadInput     int `json:"cache_read_input_tokens"`
	CacheCreationInput int `json:"cache_creation_input_tokens"`
}

type contentBlock struct {
	Type string `json:"type"`

	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	// tool_use
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Meta is the shared transcript metadata, aliased so this package's existing
// call sites and tests keep their spelling. The shape lives in adapter because
// every agent's file carries some subset of the same fields (see the omp
// adapter, which fills the same struct from a different format).
type Meta = adapter.Meta

// excerptBytes caps how much of any single text or tool output we retain
// inline. The full body is fetched on demand (spec §5.3) -- shipping a 2 MB
// tool result to a phone is never the right default.
const excerptBytes = 4096

// summaryBytes is the larger cap for a compaction summary, which is the one
// long text a reader catching up on a session actually wants whole.
const summaryBytes = 32 * 1024

// ParseTranscript reads a Claude Code JSONL session file and returns the
// structured transcript plus recovered session metadata.
//
// Malformed lines are skipped rather than failing the parse: a session file is
// appended to live by another process, so a torn final line is normal, not an
// error.
func ParseTranscript(r io.Reader) ([]protocol.TranscriptEvent, Meta, error) {
	var q queue
	return parseTranscript(r, &q)
}

// parseTranscript is ParseTranscript with the queue replay carried in, so a
// tailer can continue one replay across the chunks it reads. Meta.Queue is set
// when the chunk changed the queue.
func parseTranscript(r io.Reader, q *queue) ([]protocol.TranscriptEvent, Meta, error) {
	q.changed = false
	var (
		events []protocol.TranscriptEvent
		meta   Meta
		seq    uint64
	)

	sc := bufio.NewScanner(r)
	// Tool results and pasted content routinely exceed the default 64 KiB.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn or unknown line; not fatal
		}

		// Sidecar metadata entries.
		switch e.Type {
		case "ai-title":
			if e.AITitle != "" {
				meta.Title = e.AITitle
			}
			continue
		case "custom-title":
			if e.CustomTitle != nil {
				name := strings.TrimSpace(*e.CustomTitle)
				meta.CustomTitle = &name
			}
			continue
		case "last-prompt":
			if e.LastPrompt != "" {
				meta.LastPrompt = e.LastPrompt
			}
			continue
		case "queue-operation":
			var content string
			_ = json.Unmarshal(e.Content, &content)
			q.apply(e.Operation, content, parseTime(e.Timestamp))
			continue
		case "attachment":
			// A message typed while Claude Code was working, which it took
			// in the middle of the turn (at a tool result) instead of as the
			// next prompt. It is recorded only as this attachment, never as a
			// user entry, so without this the person's message left the
			// queue and appeared nowhere in the conversation. Claude Code's
			// own task notifications ride the same attachment and are not
			// the person's words.
			if a := e.Attachment; a != nil && a.Type == "queued_command" && a.CommandMode == "prompt" {
				text := strings.TrimSpace(contentText(a.Prompt))
				if text == "" && hasImage(a.Prompt) {
					text = "[Image]"
				}
				if text == "" {
					continue
				}
				seq++
				ev := protocol.TranscriptEvent{
					ID: e.UUID, ParentID: e.ParentUUID, SessionID: e.SessionID,
					At: parseTime(e.Timestamp), IsSidechain: e.IsSidechain,
					Kind: protocol.EventUserMessage, Seq: seq,
				}
				ev.Text, ev.Truncated = clip(text)
				events = append(events, ev)
			}
			continue
		case "mode", "permission-mode", "file-history-snapshot", "system":
			continue
		}

		if e.SessionID != "" {
			meta.SessionID = e.SessionID
		}
		if e.Cwd != "" {
			meta.Cwd = e.Cwd
		}
		if e.GitBranch != "" {
			meta.GitBranch = e.GitBranch
		}
		if e.Version != "" {
			meta.Version = e.Version
		}

		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		if len(e.Message) == 0 {
			continue
		}
		// Meta entries are the CLI talking to the model on the human's behalf:
		// the caveat before a slash command's output, a skill's expanded body,
		// a hook's note. Nobody typed them, and showing them as the human's
		// words is a misattribution.
		if e.Type == "user" && e.IsMeta {
			continue
		}

		var m message
		if err := json.Unmarshal(e.Message, &m); err != nil {
			continue
		}
		if m.Model != "" {
			meta.Model = m.Model
		}

		at := parseTime(e.Timestamp)
		base := protocol.TranscriptEvent{
			ID:          e.UUID,
			ParentID:    e.ParentUUID,
			SessionID:   e.SessionID,
			At:          at,
			IsSidechain: e.IsSidechain,
			Model:       m.Model,
		}
		if m.Usage != nil {
			base.Usage = &protocol.Usage{
				InputTokens:      m.Usage.InputTokens,
				OutputTokens:     m.Usage.OutputTokens,
				CacheReadTokens:  m.Usage.CacheReadInput,
				CacheWriteTokens: m.Usage.CacheCreationInput,
			}
		}

		if e.Type == "user" && e.IsCompactSummary {
			ev := base
			ev.Kind = protocol.EventCompaction
			ev.Text, ev.Truncated = clipTo(contentText(m.Content), summaryBytes)
			seq++
			ev.Seq = seq
			events = append(events, ev)
			continue
		}

		for _, ev := range blocksToEvents(base, e.Type, m.Content) {
			seq++
			ev.Seq = seq
			events = append(events, ev)
		}
	}
	if q.changed {
		snap := q.snapshot()
		meta.Queue = &snap
	}
	if err := sc.Err(); err != nil {
		return events, meta, err
	}
	return events, meta, nil
}

// blocksToEvents translates one message's content into transcript events.
// `content` is either a plain string (older/simple user messages) or an array
// of typed blocks.
func blocksToEvents(base protocol.TranscriptEvent, entryType string, content json.RawMessage) []protocol.TranscriptEvent {
	if len(content) == 0 {
		return nil
	}

	// Plain-string content: a simple user message.
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if ev, ok := userText(base, s); ok {
			return []protocol.TranscriptEvent{ev}
		}
		return nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
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
			if entryType == "user" {
				var ok bool
				if ev, ok = userText(base, b.Text); !ok {
					continue
				}
			} else {
				ev.Kind = protocol.EventAssistantMessage
				ev.Text, ev.Truncated = clip(b.Text)
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			ev.Kind = protocol.EventThinking
			ev.Text, ev.Truncated = clip(b.Thinking)
		case "tool_use":
			ev.Kind = protocol.EventToolCall
			ev.ToolName = b.Name
			ev.ToolUseID = b.ID
			ev.ToolInput = b.Input
		case "tool_result":
			ev.Kind = protocol.EventToolResult
			ev.ToolUseID = b.ToolUseID
			ev.IsError = b.IsError
			ev.ToolOutput, ev.Truncated = clip(flattenResult(b.Content))
		default:
			continue
		}
		out = append(out, ev)
	}
	return out
}

// userText classifies text from a user entry.
//
// Only some of it is the human. The CLI records a slash command, what it
// printed, shell commands run with `!`, and its own notifications as user
// entries too, each wrapped in a tag of its own. These are the tags Claude
// Code writes, matched literally; text that is not wrapped in one is what the
// human typed. Returns false when nothing is left to show.
func userText(base protocol.TranscriptEvent, s string) (protocol.TranscriptEvent, bool) {
	ev := base
	s = strings.TrimSpace(stripTag(s, "system-reminder"))
	switch {
	case s == "":
		return ev, false

	case strings.HasPrefix(s, "<command-name>"):
		name, _ := tagged(s, "command-name")
		args, _ := tagged(s, "command-args")
		ev.Kind = protocol.EventCommand
		ev.Text = strings.TrimSpace(name + " " + strings.TrimSpace(args))

	case strings.HasPrefix(s, "<bash-input>"):
		in, _ := tagged(s, "bash-input")
		ev.Kind = protocol.EventCommand
		ev.Text = "!" + strings.TrimSpace(in)

	case strings.HasPrefix(s, "<local-command-stdout>"),
		strings.HasPrefix(s, "<local-command-stderr>"),
		strings.HasPrefix(s, "<bash-stdout>"),
		strings.HasPrefix(s, "<bash-stderr>"):
		var parts []string
		for _, t := range []string{"local-command-stdout", "local-command-stderr", "bash-stdout", "bash-stderr"} {
			if v, ok := tagged(s, t); ok && strings.TrimSpace(v) != "" {
				parts = append(parts, strings.TrimSpace(stripANSI(v)))
			}
		}
		if len(parts) == 0 {
			return ev, false
		}
		ev.Kind = protocol.EventCommandOutput
		ev.Text, ev.Truncated = clip(strings.Join(parts, "\n"))

	case strings.HasPrefix(s, "<task-notification>"):
		summary, ok := tagged(s, "summary")
		if !ok {
			summary, _ = tagged(s, "status")
		}
		if strings.TrimSpace(summary) == "" {
			return ev, false
		}
		ev.Kind = protocol.EventNotice
		ev.Text = strings.TrimSpace(summary)

	case strings.HasPrefix(s, "<local-command-caveat>"):
		// Normally isMeta and skipped earlier; older CLIs did not mark it.
		return ev, false

	default:
		ev.Kind = protocol.EventUserMessage
		ev.Text, ev.Truncated = clip(s)
	}
	return ev, true
}

// summaryPreamble opens every compaction summary Claude Code writes.
const summaryPreamble = "This session is being continued from a previous conversation"

// Reclassify applies userText to a user message stored before the parser
// knew to look for the CLI's tags, for store.RewriteEventsOnce. A stored row
// has lost isCompactSummary, so a summary is recognised by the sentence the
// CLI opens every one with -- acceptable for history, which is not where
// anything new is decided.
func Reclassify(ev protocol.TranscriptEvent) (protocol.TranscriptEvent, bool) {
	if ev.Kind != protocol.EventUserMessage {
		return ev, true
	}
	if strings.HasPrefix(ev.Text, summaryPreamble) {
		ev.Kind = protocol.EventCompaction
		return ev, true
	}
	base := ev
	base.Text, base.Kind = "", ""
	return userText(base, ev.Text)
}

// tagged returns what is between <name> and </name>, if both are present.
func tagged(s, name string) (string, bool) {
	open, close := "<"+name+">", "</"+name+">"
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// stripTag removes every <name>...</name> block from s.
func stripTag(s, name string) string {
	open, close := "<"+name+">", "</"+name+">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], close)
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+len(close):]
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// stripANSI drops terminal escapes. Command output is recorded as the CLI
// printed it -- dimmed, coloured -- and a phone renders it as text.
func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

// contentText joins the text of a message's content, string or blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// hasImage reports whether content blocks include an image.
func hasImage(raw json.RawMessage) bool {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
	}
	return false
}

// flattenResult renders a tool_result's content, which may be a string or an
// array of blocks, into plain text.
func flattenResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text != "" {
				sb.WriteString(b.Text)
				sb.WriteByte('\n')
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	return string(raw)
}

func clip(s string) (string, bool) { return clipTo(s, excerptBytes) }

func clipTo(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	// Trim to a rune boundary so we never emit invalid UTF-8 to a client.
	cut := max
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

// ParseTranscriptFile is the file-path convenience wrapper.
func ParseTranscriptFile(path string) ([]protocol.TranscriptEvent, Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Meta{}, err
	}
	defer f.Close()
	return ParseTranscript(f)
}

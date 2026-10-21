// Package omp adapts the omp coding agent (oh-my-pi).
//
// omp is the second agent with a real event channel, but the channel is not
// the only path and it is not the default one. Everything in the read half of
// this package works for a session started before muxscale existed, with no
// extension installed:
//
//   - `~/.omp/agent/sessions/<bucket>/<timestamp>_<id>.jsonl` is a tree of
//     entries, parsed here into muxscale's TranscriptEvent union.
//   - `~/.omp/run/session-owners/<id>.lock` is a flock held by the process
//     that owns the session, so liveness is a fact rather than a file mtime.
//   - `~/.omp/agent/terminal-sessions/<tty>` is the session a terminal is
//     showing, which joins a discovered process to its transcript even after
//     --resume or /new.
//
// The event half (an installed extension POSTing lifecycle events) only raises
// the state confidence; it is not required for discovery, transcripts, or
// metadata. Verified against omp 18.12.2.
package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is the floor this adapter was verified against. Older sessions
// still appear and still parse -- a session file is a file -- but the daemon
// will not claim the event channel works on a build it has not seen.
const MinVersion = "18.12.2"

// NewestTested is the highest version this build has fixtures for.
const NewestTested = "18.12.2"

// ---- session file shapes ----

// entry is one JSONL record. Every entry type is folded into one struct rather
// than a union of decoders: the file is a stream, decoding twice per line to
// learn the type would double the cost of reading a large transcript, and the
// fields are disjoint enough that a shared struct stays honest.
type entry struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// ParentID is null for a root, and absent on the header.
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`
	Runtime   struct {
		Version string `json:"version"`
	} `json:"runtime"`

	// title slot, and title_change
	Title         string `json:"title"`
	Source        string `json:"source"`
	PreviousTitle string `json:"previousTitle"`

	// session header
	Version     int    `json:"version"`
	Cwd         string `json:"cwd"`
	TitleSource string `json:"titleSource"`

	// model_change. omp writes model; pi writes provider and modelId.
	Model   string `json:"model"`
	ModelID string `json:"modelId"`
	Role    string `json:"role"`

	// session_info (pi): a person's name for the session, from /name.
	Name string `json:"name"`

	// message and custom_message
	Message     *message        `json:"message"`
	Content     json.RawMessage `json:"content"`
	Attribution string          `json:"attribution"`
	// Display is pi's flag on an extension's custom_message. omp's carry
	// attribution instead.
	Display *bool `json:"display"`

	// custom
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`

	// compaction / branch_summary / mode_change
	Summary      string `json:"summary"`
	ShortSummary string `json:"shortSummary"`
	TokensBefore int    `json:"tokensBefore"`
	Mode         string `json:"mode"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	Usage      *apiUsage       `json:"usage"`
	StopReason string          `json:"stopReason"`
	Timestamp  int64           `json:"timestamp"`

	// toolResult
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`

	// bashExecution / pythonExecution
	Command string `json:"command"`
	Output  string `json:"output"`
}

type apiUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// exitRecord is the payload of a `custom` entry with customType session_exit.
type exitRecord struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// ---- limits ----

const (
	// excerptBytes is the inline cap for a tool result or message, matching the
	// other adapters: a 2 MB tool result is never the right thing to ship to a
	// phone by default.
	excerptBytes = 4096
	// summaryBytes is larger because a compaction summary is the one long text
	// a reader catching up actually wants whole.
	summaryBytes = 32 * 1024
)

func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…", true
}

// ---- parsing ----

// parser numbers events across polls and remembers the last mode so a
// mode_change entry is a notice only when it actually changes something.
type parser struct {
	seq      uint64
	lastMode string
}

// ParseSession parses a whole session file. It is what tests and retroactive
// reads use; the live path feeds lines through parseLine instead.
func ParseSession(r io.Reader) ([]protocol.TranscriptEvent, adapter.Meta, error) {
	var (
		p      parser
		out    []protocol.TranscriptEvent
		meta   adapter.Meta
		sc     = bufio.NewScanner(r)
		buffer = make([]byte, 0, 256*1024)
	)
	sc.Buffer(buffer, 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ev, m := p.parseLine([]byte(line))
		meta.Merge(m)
		out = append(out, ev...)
	}
	if err := sc.Err(); err != nil {
		return out, meta, err
	}
	return out, meta, nil
}

// parseLine decodes one record. Malformed lines are not an error: a session
// file is appended to by another process, so a torn final line is normal.
func (p *parser) parseLine(line []byte) ([]protocol.TranscriptEvent, adapter.Meta) {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, adapter.Meta{}
	}
	var meta adapter.Meta
	if e.Runtime.Version != "" {
		meta.Version = e.Runtime.Version
	}

	switch e.Type {
	case "title":
		// Fixed-width slot at the top of current files. Folded into metadata
		// so listing does not need a full-file rewrite.
		if e.Title != "" {
			meta.Title = e.Title
			if e.Source == "user" {
				name := strings.TrimSpace(e.Title)
				meta.CustomTitle = &name
			}
		}
		return nil, meta

	case "session":
		meta.SessionID = e.ID
		meta.Cwd = e.Cwd
		if e.Title != "" && e.TitleSource != "user" {
			meta.Title = e.Title
		}
		return nil, meta

	case "model_change":
		if m := firstNonEmpty(e.Model, e.ModelID); m != "" {
			meta.Model = m
		}
		return nil, meta

	case "session_info":
		if name := strings.TrimSpace(e.Name); name != "" {
			meta.CustomTitle = &name
		}
		return nil, meta

	case "title_change":
		if e.Title == "" {
			return nil, meta
		}
		if e.Source == "user" {
			name := strings.TrimSpace(e.Title)
			meta.CustomTitle = &name
		} else {
			meta.Title = e.Title
		}
		return nil, meta

	case "message":
		if e.Message == nil {
			return nil, meta
		}
		return p.messageEvents(e, &meta), meta

	case "custom":
		return p.customEvents(e, &meta), meta

	case "compaction":
		text, trunc := clip(e.Summary, summaryBytes)
		return []protocol.TranscriptEvent{p.event(e,
			protocol.EventCompaction, text, trunc)}, meta

	case "branch_summary":
		text, trunc := clip(e.Summary, summaryBytes)
		if text == "" {
			return nil, meta
		}
		return []protocol.TranscriptEvent{p.event(e,
			protocol.EventNotice, "branch summary: "+text, trunc)}, meta

	case "custom_message":
		text := contentText(e.Content)
		if text == "" {
			return nil, meta
		}
		// omp attributes these; an unattributed one is the person's. pi's
		// come from extensions, flagged with display rather than attributed,
		// and are never the person speaking.
		kind := protocol.EventNotice
		if e.Attribution == "user" || (e.Attribution == "" && e.Display == nil) {
			kind = protocol.EventUserMessage
		}
		text, trunc := clip(text, excerptBytes)
		return []protocol.TranscriptEvent{p.event(e, kind, text, trunc)}, meta

	case "mode_change":
		if e.Mode == "" || e.Mode == p.lastMode {
			return nil, meta
		}
		p.lastMode = e.Mode
		return []protocol.TranscriptEvent{p.event(e,
			protocol.EventNotice, "mode: "+e.Mode, false)}, meta

	case "reset_boundary":
		return []protocol.TranscriptEvent{p.event(e,
			protocol.EventNotice, "conversation cleared", false)}, meta
	}
	// label, ttsr_injection, session_init, thinking_level_change,
	// service_tier_change, credential_pin, and unknown future records are
	// replay state, not conversation.
	return nil, meta
}

func (p *parser) customEvents(e entry, meta *adapter.Meta) []protocol.TranscriptEvent {
	switch e.CustomType {
	case "session_exit":
		var x exitRecord
		_ = json.Unmarshal(e.Data, &x)
		text := "session_exit " + x.Kind
		if x.Reason != "" {
			text += ": " + x.Reason
		}
		ev := p.event(e, protocol.EventStateChange, text, false)
		ev.IsError = x.Kind == "fatal" || x.Kind == "signal"
		return []protocol.TranscriptEvent{ev}
	case "tool_execution_start":
		// The assistant message's toolCall block already carries the call,
		// with the same id and arguments. Emitting it again would show every
		// tool twice.
		return nil
	}
	if e.Content != nil {
		if text := contentText(e.Content); text != "" {
			text, trunc := clip(text, excerptBytes)
			return []protocol.TranscriptEvent{p.event(e, protocol.EventNotice, text, trunc)}
		}
	}
	return nil
}

func (p *parser) messageEvents(e entry, meta *adapter.Meta) []protocol.TranscriptEvent {
	m := e.Message
	if m.Model != "" {
		meta.Model = m.Model
	}
	at := eventTime(e)

	switch m.Role {
	case "user":
		text := contentText(m.Content)
		if text == "" {
			return nil
		}
		text, trunc := clip(text, excerptBytes)
		ev := p.event(e, protocol.EventUserMessage, text, trunc)
		ev.At = at
		return []protocol.TranscriptEvent{ev}

	case "assistant":
		var blocks []contentBlock
		_ = json.Unmarshal(m.Content, &blocks)
		var out []protocol.TranscriptEvent
		for _, b := range blocks {
			ev := p.event(e, protocol.EventAssistantMessage, "", false)
			ev.At = at
			ev.Model = firstNonEmpty(m.Model, meta.Model)
			switch b.Type {
			case "thinking":
				if b.Thinking == "" {
					continue
				}
				ev.Kind = protocol.EventThinking
				ev.Text, ev.Truncated = clip(b.Thinking, excerptBytes)
			case "text":
				if b.Text == "" {
					continue
				}
				ev.Text, ev.Truncated = clip(b.Text, excerptBytes)
			case "toolCall":
				ev.Kind = protocol.EventToolCall
				ev.ToolName = b.Name
				ev.ToolUseID = firstNonEmpty(b.ID, e.ID)
				if len(b.Arguments) > 0 {
					_ = json.Unmarshal(b.Arguments, &ev.ToolInput)
				}
			default:
				// image and provider-specific blocks carry no text we can
				// show; the tool result or the next turn refers to them.
				continue
			}
			out = append(out, ev)
		}
		if m.Usage != nil {
			u := protocol.Usage{
				InputTokens:      m.Usage.Input,
				OutputTokens:     m.Usage.Output,
				CacheReadTokens:  m.Usage.CacheRead,
				CacheWriteTokens: m.Usage.CacheWrite,
			}
			if len(out) > 0 {
				out[0].Usage = &u
			} else {
				ev := p.event(e, protocol.EventTokenUsage, "", false)
				ev.At = at
				ev.Usage = &u
				ev.Model = m.Model
				out = append(out, ev)
			}
		}
		return out

	case "toolResult":
		text, trunc := clip(contentText(m.Content), excerptBytes)
		ev := p.event(e, protocol.EventToolResult, text, trunc)
		ev.At = at
		ev.ToolName = m.ToolName
		ev.ToolUseID = m.ToolCallID
		ev.IsError = m.IsError
		return []protocol.TranscriptEvent{ev}

	case "bashExecution", "pythonExecution":
		var out []protocol.TranscriptEvent
		if m.Command != "" {
			cmd, trunc := clip(m.Command, excerptBytes)
			ev := p.event(e, protocol.EventCommand, cmd, trunc)
			ev.At = at
			out = append(out, ev)
		}
		if m.Output != "" {
			text, trunc := clip(m.Output, excerptBytes)
			ev := p.event(e, protocol.EventCommandOutput, text, trunc)
			ev.At = at
			ev.IsError = m.IsError
			out = append(out, ev)
		}
		return out
	}
	return nil
}

// event builds an event from an entry, assigning the next sequence number.
// Two events from one assistant message (text and a tool call) share the
// entry's id and parent; consumers order by Seq.
func (p *parser) event(e entry, kind protocol.EventKind, text string, truncated bool) protocol.TranscriptEvent {
	p.seq++
	ev := protocol.TranscriptEvent{
		Seq: p.seq, ID: e.ID, Kind: kind, At: eventTime(e),
		Text: text, Truncated: truncated,
	}
	if e.ParentID != nil {
		ev.ParentID = *e.ParentID
	}
	return ev
}

func eventTime(e entry) time.Time {
	for _, s := range []string{e.Timestamp} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
	}
	if e.Message != nil && e.Message.Timestamp > 0 {
		return time.UnixMilli(e.Message.Timestamp)
	}
	return time.Time{}
}

// contentText flattens a message content value, which omp persists either as a
// string (custom messages) or as an array of Anthropic-shaped blocks.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- live tailing ----

// stream follows one session file, byte offset and partial line kept between
// polls. Polling by size rather than fsnotify is deliberate: a JSONL file
// appended to constantly generates a torrent of events, and coalescing them
// costs more than one cheap Stat.
type stream struct {
	mu       sync.Mutex
	path     string
	interval time.Duration
	offset   int64
	partial  []byte
	parser   parser
	pending  adapter.Meta
}

func newStream(path string) *stream {
	return &stream{path: path, interval: 750 * time.Millisecond}
}

func (s *stream) Interval() time.Duration { return s.interval }

func (s *stream) SeekEnd(fromSeq uint64) error {
	fi, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset = fi.Size()
	s.partial = nil
	s.parser.seq = fromSeq
	return nil
}

func (s *stream) Poll(context.Context) ([]protocol.TranscriptEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		return nil, err
	}
	// Truncation or replacement: start over rather than reading from an offset
	// into the middle of a different file.
	if fi.Size() < s.offset {
		s.offset, s.partial, s.parser = 0, nil, parser{}
	}
	if fi.Size() == s.offset {
		return nil, nil
	}

	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	s.offset += int64(len(chunk))

	data := append(s.partial, chunk...)
	idx := strings.LastIndexByte(string(data), '\n')
	if idx < 0 {
		s.partial = data
		return nil, nil
	}
	s.partial = append([]byte(nil), data[idx+1:]...)
	data = data[:idx]

	var events []protocol.TranscriptEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ev, meta := s.parser.parseLine([]byte(line))
		s.pending.Merge(meta)
		events = append(events, ev...)
	}
	return events, nil
}

func (s *stream) Meta() adapter.Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.pending
	s.pending = adapter.Meta{}
	return m
}

func (s *stream) Close() error { return nil }

// ---- paths, liveness, identity ----

// agentDir is where omp keeps sessions, breadcrumbs and config.
func agentDir() (string, error) {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".omp", "agent"), nil
}

// stateDir is the parent of the agent directory, where omp keeps run state.
func stateDir() (string, error) {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return filepath.Dir(filepath.Clean(d)), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".omp"), nil
}

// SessionsRoot is the directory holding one bucket per project.
func SessionsRoot() (string, error) {
	d, err := agentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "sessions"), nil
}

// SessionIDFromPath recovers the id from <timestamp>_<id>.jsonl.
//
// The timestamp itself contains hyphens but no underscore, so the last
// underscore is the separator. Subagent files (a name inside a directory) have
// no id and return "".
func SessionIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	i := strings.LastIndexByte(base, '_')
	if i <= 0 || i == len(base)-1 {
		return ""
	}
	return base[i+1:]
}

// ownerLock is the lease omp's writer holds for a session.
func ownerLock(sessionID string) (string, error) {
	root, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "run", "session-owners", sessionID+".lock"), nil
}

// Live reports whether the session's writer lease is still held.
//
// Existence is not the answer: a killed writer can leave the file behind. A
// non-blocking flock succeeds exactly when no process holds it. A missing lock
// is a definite "not live"; any other error is "unknown" so a permissions
// problem can never mark every session ended.
func Live(path string) (bool, bool) {
	id := SessionIDFromPath(path)
	if id == "" {
		return false, false
	}
	lock, err := ownerLock(id)
	if err != nil {
		return false, false
	}
	f, err := os.Open(lock)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, true
		}
		return false, false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, true
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, true
	}
	return false, false
}

// breadcrumbPath is the file omp writes for a terminal, naming what it shows.
func breadcrumbPath(tty string) (string, error) {
	d, err := agentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "terminal-sessions", filepath.Base(tty)), nil
}

// bucketName encodes a working directory the way omp names a session bucket.
//
// Lossy by construction -- a hyphen in a directory name is indistinguishable
// from a separator -- so this is only ever used to find candidate files. The
// cwd shown to a person always comes from the session header.
func bucketName(cwd string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("no working directory")
	}
	clean := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if home != "" {
		if rel, ok := strings.CutPrefix(clean, home+string(filepath.Separator)); ok {
			return "-" + strings.ReplaceAll(rel, string(filepath.Separator), "-"), nil
		}
	}
	tmp := filepath.Clean(os.TempDir())
	if rel, ok := strings.CutPrefix(clean, tmp+string(filepath.Separator)); ok {
		return "-tmp-" + strings.ReplaceAll(rel, string(filepath.Separator), "-"), nil
	}
	return "--" + strings.ReplaceAll(strings.Trim(clean, string(filepath.Separator)), string(filepath.Separator), "-") + "--", nil
}

// headerInfo is the cheap subset discovery needs.
type headerInfo struct {
	ID      string
	Cwd     string
	Title   string
	Started time.Time
	// Name is pi's session_info name, when one comes before the first prompt.
	Name string
	// FirstPrompt labels a session that has no title: pi writes none, and
	// shows the first message in its own session picker.
	FirstPrompt string
}

// headerScan bounds how far readHeader looks for a first prompt. The prompt
// is normally within a few lines of the header; a session that opens some
// other way goes untitled rather than costing a long read every discovery.
const headerScan = 16

// readHeader reads a session's opening records: an optional title slot, then
// the session header. One small read per candidate, never a full parse.
func readHeader(path string) (headerInfo, bool) {
	f, err := os.Open(path)
	if err != nil {
		return headerInfo{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var h headerInfo
	for i := 0; i < headerScan && sc.Scan(); i++ {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "title":
			if e.Title != "" {
				h.Title = e.Title
			}
		case "session":
			h.ID = e.ID
			h.Cwd = e.Cwd
			if h.Title == "" && e.Title != "" {
				h.Title = e.Title
			}
			if t, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
				h.Started = t
			}
			if h.Title != "" {
				return h, h.ID != "" // titled: nothing further to read
			}
		case "session_info":
			if name := strings.TrimSpace(e.Name); name != "" {
				h.Name = name
			}
		case "message":
			if e.Message != nil && e.Message.Role == "user" {
				text, _ := clip(strings.Join(strings.Fields(contentText(e.Message.Content)), " "), 80)
				h.FirstPrompt = text
				return h, h.ID != ""
			}
		}
		if i == 3 && h.ID == "" {
			return h, false // no header where one must be: not a session
		}
	}
	return h, h.ID != ""
}

package opencode

// OpenCode 1.x keeps every session in one SQLite database,
// ~/.local/share/opencode/opencode.db (under XDG_DATA_HOME when set). This is
// the read path that works for a session started before muxscale existed, with
// no server port to find: the TUI's own server listens on a port nothing
// outside it is told about.
//
// Verified against OpenCode 1.18.33's database on a real machine:
//
//	session  id, parent_id (a subagent's parent), directory, title, model,
//	         time_created, time_updated, time_archived (ms)
//	message  id, session_id, time_created, data: {role, time.completed,
//	         finish ("stop" | "tool-calls" | "length" | "unknown"),
//	         error.name, modelID}
//	part     id, message_id, session_id, time_updated, data: {type, ...}
//	         text, reasoning (time.end when written), tool (callID, tool,
//	         state.status/input/output/error), step-start, step-finish,
//	         patch, file, compaction
//
// Parts are rewritten in place while they stream, so a part is passed on only
// once it is final, and only as an unbroken prefix of the conversation: that
// keeps each event's sequence number its position in the session, the same on
// every read, which is what lets a restart resume rather than repeat.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// DBPath is OpenCode's database: $XDG_DATA_HOME/opencode/opencode.db, else
// ~/.local/share/opencode/opencode.db (also on macOS).
func DBPath() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode", "opencode.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db"), nil
}

// openDB opens the database read-only, as the hermes adapter does: OpenCode is
// writing it, and a monitor must never take a lock that could stall the agent.
func openDB() (*sql.DB, error) {
	path, err := DBPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s read-only: %w", path, err)
	}
	return db, nil
}

// StoredSession is one row of the session table.
type StoredSession struct {
	ID        string
	Directory string
	Title     string
	Model     string
	Created   time.Time
	Updated   time.Time
}

// storedSessions returns top-level, unarchived sessions, most recent first.
// A subagent's session (parent_id set) is activity inside its parent.
func storedSessions(db *sql.DB, limit int) ([]StoredSession, error) {
	// The first prompt labels a session OpenCode never titled. One indexed
	// lookup per session: the first user message, then its first text part.
	rows, err := db.Query(`
SELECT s.id, s.directory, s.title, coalesce(s.model,''), s.time_created, s.time_updated,
       coalesce((SELECT json_extract(p.data,'$.text') FROM part p
                 WHERE p.message_id = (SELECT m.id FROM message m
                                       WHERE m.session_id = s.id AND json_extract(m.data,'$.role') = 'user'
                                       ORDER BY m.time_created, m.id LIMIT 1)
                   AND json_extract(p.data,'$.type') = 'text'
                   AND coalesce(json_extract(p.data,'$.synthetic'),0) = 0
                 ORDER BY p.id LIMIT 1), '')
FROM session s
WHERE s.parent_id IS NULL AND s.time_archived IS NULL
ORDER BY s.time_updated DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredSession
	for rows.Next() {
		var s StoredSession
		var model string
		var created, updated int64
		var first string
		if err := rows.Scan(&s.ID, &s.Directory, &s.Title, &model, &created, &updated, &first); err != nil {
			return nil, err
		}
		s.Created, s.Updated = time.UnixMilli(created), time.UnixMilli(updated)
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(model), &m) == nil {
			s.Model = m.ID
		}
		// OpenCode names a fresh session "New session - <timestamp>" until it
		// writes a real title; that is a placeholder, not a title.
		if strings.HasPrefix(s.Title, "New session - ") {
			s.Title = truncate(strings.Join(strings.Fields(first), " "), 80)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- transcript ----

type storedMessage struct {
	Role string `json:"role"`
	// ParentID is, on an assistant message, the user message it answers.
	ParentID string `json:"parentID"`
	Time     struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Finish  string `json:"finish"`
	ModelID string `json:"modelID"`
	Error   *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

type storedPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	Time      *struct {
		End int64 `json:"end"`
	} `json:"time"`

	// tool
	Tool   string `json:"tool"`
	CallID string `json:"callID"`
	State  struct {
		Status string         `json:"status"`
		Input  map[string]any `json:"input"`
		Output string         `json:"output"`
		Error  string         `json:"error"`
	} `json:"state"`

	// file
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
}

// unit is one part's contribution to the transcript.
type unit struct {
	events []protocol.TranscriptEvent
	// early is how many of events may go out before the part is final: a
	// running tool's call is worth showing while its result is not yet in.
	early int
	final bool
}

// readTranscript reads a session's messages and parts in conversation order
// and turns them into units. It also reports the session's latest lifecycle
// moment as an event name for state inference (see lifecycle).
func readTranscript(db *sql.DB, sessionID string) ([]unit, string, error) {
	rows, err := db.Query(`
SELECT m.id, m.time_created, m.data, coalesce(p.id,''), coalesce(p.time_created,0), coalesce(p.data,'')
FROM message m LEFT JOIN part p ON p.message_id = m.id
WHERE m.session_id = ?
ORDER BY m.time_created, m.id, p.id`, sessionID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var (
		units   []unit
		lastMsg string
		last    storedMessage
		parts   []storedPart
		seen    = map[string]bool{}
	)
	for rows.Next() {
		var msgID, partID, msgData, partData string
		var msgCreated, partCreated int64
		if err := rows.Scan(&msgID, &msgCreated, &msgData, &partID, &partCreated, &partData); err != nil {
			return nil, "", err
		}
		var m storedMessage
		if json.Unmarshal([]byte(msgData), &m) != nil {
			continue
		}
		at := time.UnixMilli(msgCreated)
		if !seen[msgID] {
			seen[msgID] = true
			if lastMsg != "" && last.Role == "assistant" && last.Error != nil {
				units = append(units, errorUnit(lastMsg, last))
			}
			lastMsg, last, parts = msgID, m, nil
		}
		if partID == "" {
			continue
		}
		var p storedPart
		if json.Unmarshal([]byte(partData), &p) != nil {
			continue
		}
		parts = append(parts, p)
		if partCreated > 0 {
			at = time.UnixMilli(partCreated)
		}
		if u, ok := partUnit(msgID, partID, m, p, at); ok {
			units = append(units, u)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if lastMsg != "" && last.Role == "assistant" && last.Error != nil {
		units = append(units, errorUnit(lastMsg, last))
	}
	return units, lifecycle(lastMsg != "", last, parts), nil
}

func partUnit(msgID, partID string, m storedMessage, p storedPart, at time.Time) (unit, bool) {
	ev := protocol.TranscriptEvent{ID: partID, ParentID: msgID, At: at, Model: m.ModelID}
	ended := m.Role == "user" || m.Time.Completed > 0 || m.Error != nil
	switch p.Type {
	case "text":
		if p.Synthetic || p.Ignored || strings.TrimSpace(p.Text) == "" {
			return unit{}, false
		}
		ev.Kind = protocol.EventAssistantMessage
		if m.Role == "user" {
			ev.Kind = protocol.EventUserMessage
		}
		ev.Text, ev.Truncated = clip(p.Text)
		return unit{events: []protocol.TranscriptEvent{ev}, final: ended || (p.Time != nil && p.Time.End > 0)}, true

	case "reasoning":
		if strings.TrimSpace(p.Text) == "" {
			return unit{}, false
		}
		ev.Kind = protocol.EventThinking
		ev.Text, ev.Truncated = clip(p.Text)
		return unit{events: []protocol.TranscriptEvent{ev}, final: ended || (p.Time != nil && p.Time.End > 0)}, true

	case "tool":
		call := ev
		call.Kind = protocol.EventToolCall
		call.ToolName, call.ToolUseID, call.ToolInput = p.Tool, p.CallID, p.State.Input
		res := ev
		res.ID = partID + ":result"
		res.Kind = protocol.EventToolResult
		res.ToolName, res.ToolUseID = p.Tool, p.CallID
		switch p.State.Status {
		case "completed":
			res.Text, res.Truncated = clip(p.State.Output)
		case "error":
			res.IsError = true
			res.Text, res.Truncated = clip(p.State.Error)
		default:
			// pending or running. An abandoned turn leaves a tool here for good;
			// once the message has ended there is no result coming.
			early := 0
			if p.State.Status == "running" {
				early = 1
			}
			if ended {
				return unit{events: []protocol.TranscriptEvent{call}, final: true}, true
			}
			return unit{events: []protocol.TranscriptEvent{call}, early: early}, true
		}
		return unit{events: []protocol.TranscriptEvent{call, res}, final: true}, true

	case "file":
		ev.Kind = protocol.EventNotice
		ev.Text = "attached " + firstNonEmptyStr(p.Filename, p.Mime, "a file")
		return unit{events: []protocol.TranscriptEvent{ev}, final: true}, true

	case "compaction":
		ev.Kind = protocol.EventCompaction
		ev.Text = "OpenCode compacted the conversation"
		return unit{events: []protocol.TranscriptEvent{ev}, final: true}, true
	}
	// step-start and step-finish bracket a model call; patch repeats what the
	// edit tools already show; agent and subtask name subagent work whose own
	// session holds the detail.
	return unit{}, false
}

func errorUnit(msgID string, m storedMessage) unit {
	text := m.Error.Name
	if m.Error.Data.Message != "" {
		text += ": " + m.Error.Data.Message
	}
	ev := protocol.TranscriptEvent{
		ID: msgID + ":error", ParentID: msgID, At: time.UnixMilli(max(m.Time.Completed, m.Time.Created)),
		Kind: protocol.EventError, IsError: true,
	}
	if m.Error.Name == "MessageAbortedError" {
		ev.Kind, ev.IsError, text = protocol.EventNotice, false, "interrupted"
	}
	ev.Text, ev.Truncated = clip(text)
	return unit{events: []protocol.TranscriptEvent{ev}, final: true}
}

// Lifecycle names, OpenCode's state as the database last recorded it.
const (
	lifeUser      = "db:user"       // a prompt the model has not answered yet
	lifeRunning   = "db:responding" // an assistant message still being written
	lifeTool      = "db:tool"       // a tool pending or running
	lifeToolCalls = "db:tool-calls" // a step ended in tool calls; another follows
	lifeDone      = "db:done"       // the turn finished
	lifeAborted   = "db:aborted"    // interrupted
	lifeError     = "db:error"      // the turn failed
)

func lifecycle(any bool, m storedMessage, parts []storedPart) string {
	if !any {
		return ""
	}
	if m.Role == "user" {
		return lifeUser
	}
	if m.Error != nil {
		if m.Error.Name == "MessageAbortedError" {
			return lifeAborted
		}
		return lifeError
	}
	for _, p := range parts {
		if p.Type == "tool" && (p.State.Status == "pending" || p.State.Status == "running") && m.Time.Completed == 0 {
			return lifeTool
		}
	}
	if m.Time.Completed == 0 {
		return lifeRunning
	}
	if m.Finish == "tool-calls" {
		return lifeToolCalls
	}
	return lifeDone
}

// stateFromLifecycle is inference from the database: below the server event
// stream's 1.0, because a row is a record of what happened, not a report.
func stateFromLifecycle(life string, idle, quiescence time.Duration) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, c float64, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{State: s, Confidence: c, Reason: reason, Evidence: "opencode " + life, At: now}, true
	}
	switch life {
	case lifeDone:
		return g(protocol.StateWaitingInput, 0.85, "the turn finished")
	case lifeAborted:
		return g(protocol.StateWaitingInput, 0.85, "the turn was interrupted")
	case lifeError:
		return g(protocol.StateWaitingInput, 0.8, "the turn failed")
	case lifeTool:
		// OpenCode holds a tool here while it runs and while it waits for
		// permission; only its server's event stream says which.
		return g(protocol.StateRunning, 0.55, "a tool is running, or waiting for permission")
	case lifeToolCalls:
		return g(protocol.StateRunning, 0.6, "a step ended in tool calls and the next is coming")
	case lifeRunning:
		return g(protocol.StateRunning, 0.65, "the assistant is answering")
	case lifeUser:
		if idle < quiescence {
			return g(protocol.StateRunning, 0.6, "a prompt was just sent")
		}
		return g(protocol.StateRunning, 0.5, "a prompt is waiting for an answer")
	}
	return protocol.StateGuess{}, false
}

// dbStream follows one session in the database.
type dbStream struct {
	db        *sql.DB
	sessionID string
	// emitted is how many events have gone out, which is also the sequence
	// number of the last one.
	emitted int
	life    string

	// queueTable is where this database keeps its queue, looked up once;
	// queue is what was last reported, so only a change is reported again.
	queueTable   *string
	queue        string
	queueChanged *[]protocol.QueuedMessage
}

func (s *dbStream) SeekEnd(fromSeq uint64) error {
	s.emitted = int(fromSeq)
	return nil
}

func (s *dbStream) LastEvent() string { return s.life }

func (s *dbStream) Poll() ([]protocol.TranscriptEvent, error) {
	units, life, err := readTranscript(s.db, s.sessionID)
	if err != nil {
		return nil, err
	}
	s.life = life
	s.pollQueue()
	var out []protocol.TranscriptEvent
	n := 0
	for _, u := range units {
		take := len(u.events)
		if !u.final {
			take = u.early
		}
		for i := 0; i < take; i++ {
			n++
			if n <= s.emitted {
				continue
			}
			ev := u.events[i]
			ev.Seq = uint64(n)
			out = append(out, ev)
		}
		if !u.final {
			break // later parts wait, so every sequence number stays put
		}
	}
	if n > s.emitted {
		s.emitted = n
	}
	return out, nil
}

func (s *dbStream) Close() error { return s.db.Close() }

// pollQueue reads the session's queue and keeps it for Meta if it changed.
func (s *dbStream) pollQueue() {
	if s.queueTable == nil {
		t := queueTable(s.db)
		s.queueTable = &t
	}
	if *s.queueTable == "" {
		return
	}
	q, err := readQueue(s.db, *s.queueTable, s.sessionID)
	if err != nil {
		return // the queue is extra; a failed read leaves the last one shown
	}
	b, _ := json.Marshal(q)
	if string(b) == s.queue {
		return
	}
	s.queue = string(b)
	s.queueChanged = &q
}

// Meta reports the queue when it changed.
func (s *dbStream) Meta() adapter.Meta {
	m := adapter.Meta{Queue: s.queueChanged}
	s.queueChanged = nil
	return m
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Package hermes adapts the Hermes agent (Nous Research).
//
// Verified against Hermes Agent v0.12.0 by reading the real state database.
// Hermes persists everything to SQLite at ~/.hermes/state.db rather than to
// per-session files, with two tables that matter:
//
//	sessions  id, source, model, parent_session_id, started_at, ended_at,
//	          end_reason, title, token counts, cost
//	messages  session_id, role, content, tool_call_id, tool_calls, tool_name,
//	          timestamp, reasoning
//
// Two things distinguish it from the other adapters:
//
//   - There is no working directory anywhere in the schema. Hermes sessions
//     cannot be grouped by project, and the UI has to say so rather than
//     showing a blank field that looks like a bug.
//   - Hermes ships its own remote access (Telegram, Discord, Slack, email, and
//     a gateway). muxscale's value here is the unified cross-agent view, not
//     novelty, so this adapter stays deliberately cheap: read the database,
//     do not fight the gateway.
package hermes

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is the floor the adapter was verified against.
const MinVersion = "0.12.0"

// DBPath is ~/.hermes/state.db.
func DBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hermes", "state.db"), nil
}

// Store reads Hermes's state database.
type Store struct {
	db *sql.DB
}

// Open opens the database read-only.
//
// Read-only and immutable=0 matter: Hermes is actively writing this file, so
// the connection must respect its WAL and must never take a write lock. A
// monitoring tool that can block the agent's own database write is exactly the
// kind of interference muxscale is supposed to avoid.
func Open(path string) (*Store, error) {
	if path == "" {
		var err error
		if path, err = DBPath(); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("hermes database not found at %s: %w", path, err)
	}
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(3000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("opening %s read-only: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Session is a Hermes session.
type Session struct {
	ID       string
	Source   string // cli, telegram, discord, ...
	Model    string
	ParentID string
	Title    string

	StartedAt time.Time
	EndedAt   time.Time
	EndReason string

	MessageCount  int
	ToolCallCount int
	Tokens        protocol.Usage
	CostUSD       float64
}

// IsLive reports whether the session has not ended.
func (s Session) IsLive() bool { return s.EndedAt.IsZero() && s.EndReason == "" }

// IsChild reports whether Hermes spawned this session from another.
func (s Session) IsChild() bool { return s.ParentID != "" }

// Sessions returns sessions, most recently started first.
func (s *Store) Sessions(limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`
SELECT id, COALESCE(source,''), COALESCE(model,''), COALESCE(parent_session_id,''),
       COALESCE(title,''), started_at, ended_at, COALESCE(end_reason,''),
       COALESCE(message_count,0), COALESCE(tool_call_count,0),
       COALESCE(input_tokens,0), COALESCE(output_tokens,0),
       COALESCE(cache_read_tokens,0), COALESCE(cache_write_tokens,0),
       COALESCE(actual_cost_usd, estimated_cost_usd, 0)
FROM sessions ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			sess    Session
			started float64
			ended   sql.NullFloat64
		)
		if err := rows.Scan(&sess.ID, &sess.Source, &sess.Model, &sess.ParentID,
			&sess.Title, &started, &ended, &sess.EndReason,
			&sess.MessageCount, &sess.ToolCallCount,
			&sess.Tokens.InputTokens, &sess.Tokens.OutputTokens,
			&sess.Tokens.CacheReadTokens, &sess.Tokens.CacheWriteTokens,
			&sess.CostUSD); err != nil {
			return nil, err
		}
		sess.StartedAt = floatTime(started)
		if ended.Valid && ended.Float64 > 0 {
			sess.EndedAt = floatTime(ended.Float64)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// LiveSessions returns sessions that have not ended.
func (s *Store) LiveSessions() ([]Session, error) {
	all, err := s.Sessions(200)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, sess := range all {
		if sess.IsLive() {
			out = append(out, sess)
		}
	}
	return out, nil
}

// Transcript returns a session's messages as transcript events.
//
// sinceRowID lets a tailer resume: message ids are a monotonic autoincrement,
// so polling for rows above the last seen id is cheap regardless of how large
// the history is.
func (s *Store) Transcript(sessionID string, sinceRowID int64, limit int) ([]protocol.TranscriptEvent, int64, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
SELECT id, role, COALESCE(content,''), COALESCE(tool_call_id,''),
       COALESCE(tool_calls,''), COALESCE(tool_name,''), timestamp,
       COALESCE(reasoning, reasoning_content, '')
FROM messages
WHERE session_id = ? AND id > ?
ORDER BY id ASC LIMIT ?`, sessionID, sinceRowID, limit)
	if err != nil {
		return nil, sinceRowID, err
	}
	defer rows.Close()

	var (
		events []protocol.TranscriptEvent
		seq    uint64 = uint64(sinceRowID)
		maxID         = sinceRowID
	)
	for rows.Next() {
		var (
			id                                   int64
			role, content, toolCallID, toolCalls string
			toolName, reasoning                  string
			ts                                   float64
		)
		if err := rows.Scan(&id, &role, &content, &toolCallID, &toolCalls,
			&toolName, &ts, &reasoning); err != nil {
			return events, maxID, err
		}
		maxID = id
		at := floatTime(ts)

		for _, e := range messageEvents(role, content, toolCallID, toolCalls, toolName, reasoning, at) {
			seq++
			e.Seq = seq
			e.SessionID = sessionID
			events = append(events, e)
		}
	}
	return events, maxID, rows.Err()
}

// toolCall is one entry of the messages.tool_calls JSON array.
type toolCall struct {
	ID       string `json:"id"`
	CallID   string `json:"call_id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON string needing a second parse, the same shape
		// Codex uses. Showing it raw would give the user an escaped blob
		// instead of the arguments they need in order to judge an action.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func messageEvents(role, content, toolCallID, toolCalls, toolName, reasoning string, at time.Time) []protocol.TranscriptEvent {
	base := protocol.TranscriptEvent{At: at}
	var out []protocol.TranscriptEvent

	// Reasoning rides along on the assistant row rather than being its own.
	if strings.TrimSpace(reasoning) != "" {
		r := base
		r.Kind = protocol.EventThinking
		r.Text, r.Truncated = clip(reasoning)
		out = append(out, r)
	}

	switch role {
	case "user":
		if strings.TrimSpace(content) != "" {
			e := base
			e.Kind = protocol.EventUserMessage
			e.Text, e.Truncated = clip(content)
			out = append(out, e)
		}

	case "assistant":
		if strings.TrimSpace(content) != "" {
			e := base
			e.Kind = protocol.EventAssistantMessage
			e.Text, e.Truncated = clip(content)
			out = append(out, e)
		}
		for _, tc := range parseToolCalls(toolCalls) {
			e := base
			e.Kind = protocol.EventToolCall
			e.ToolName = tc.Function.Name
			e.ToolUseID = firstNonEmpty(tc.CallID, tc.ID)
			e.ToolInput = parseArguments(tc.Function.Arguments)
			out = append(out, e)
		}

	case "tool":
		e := base
		e.Kind = protocol.EventToolResult
		e.ToolUseID = toolCallID
		e.ToolName = toolName
		e.ToolOutput, e.Truncated = clip(content)
		out = append(out, e)

	case "system", "developer":
		// Machinery, not conversation.
		return out
	}
	return out
}

func parseToolCalls(raw string) []toolCall {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var calls []toolCall
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return nil
	}
	return calls
}

func parseArguments(args string) map[string]any {
	if args == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err == nil {
		return m
	}
	return map[string]any{"arguments": args}
}

// InferState derives a session's state.
//
// Hermes exposes no live event feed to muxscale, so this is inference and the
// confidence says so. end_reason is the one exact signal: a session with one is
// definitively over.
func InferState(s Session, lastMessageAt time.Time, quiescence time.Duration) protocol.StateGuess {
	now := time.Now()
	if !s.IsLive() {
		return protocol.StateGuess{
			State: protocol.StateExited, Confidence: 1.0,
			Reason:   "session ended: " + s.EndReason,
			Evidence: "hermes:end_reason=" + s.EndReason, At: now,
		}
	}
	idle := now.Sub(lastMessageAt)
	if lastMessageAt.IsZero() {
		return protocol.StateGuess{
			State: protocol.StateUnknown, Confidence: 0.3,
			Reason: "no messages yet", Evidence: "hermes:empty", At: now,
		}
	}
	if idle < quiescence {
		return protocol.StateGuess{
			State: protocol.StateRunning, Confidence: 0.65,
			Reason:   "messages written recently",
			Evidence: "hermes:idle " + idle.Round(time.Second).String(), At: now,
		}
	}
	return protocol.StateGuess{
		State: protocol.StateWaitingInput, Confidence: 0.6,
		Reason:   "no new messages",
		Evidence: "hermes:idle " + idle.Round(time.Second).String(), At: now,
	}
}

// LastMessageAt returns when a session last produced a message.
func (s *Store) LastMessageAt(sessionID string) (time.Time, error) {
	var ts sql.NullFloat64
	err := s.db.QueryRow(
		`SELECT MAX(timestamp) FROM messages WHERE session_id = ?`, sessionID).Scan(&ts)
	if err != nil {
		return time.Time{}, err
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	return floatTime(ts.Float64), nil
}

// NoWorkingDirectory explains a gap that would otherwise look like a bug.
//
// Hermes's schema has no cwd, so its sessions cannot be grouped by project the
// way every other adapter's can. Saying so is better than rendering a blank
// field the user will read as broken.
const NoWorkingDirectory = "hermes does not record a working directory per session"

// ---- helpers ----

const excerptBytes = 4096

// floatTime converts Hermes's REAL unix timestamps, which carry sub-second
// precision, into a time.Time.
func floatTime(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

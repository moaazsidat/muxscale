// Package store persists sessions and transcripts to SQLite.
//
// modernc.org/sqlite is a pure-Go driver: no cgo, so `muxscale` still
// cross-compiles to five targets from one machine. That constraint is worth
// more than the marginal speed of the C driver -- this workload is a few
// hundred writes per turn, not an OLTP benchmark.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Store is the daemon's durable state.
type Store struct {
	db   *sql.DB
	path string
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id              TEXT PRIMARY KEY,
  agent_id        TEXT NOT NULL,
  natural_key     TEXT NOT NULL,
  title           TEXT,
  name            TEXT,
  cwd             TEXT,
  git_branch      TEXT,
  model           TEXT,
  agent_version   TEXT,
  attachment      TEXT,
  fidelity        TEXT,
  state           TEXT NOT NULL,
  state_since     INTEGER NOT NULL,
  guess_json      TEXT,
  prompt_id       TEXT,
  transcript_path TEXT,
  created_at      INTEGER NOT NULL,
  last_event_at   INTEGER NOT NULL,
  ended_at        INTEGER,
  UNIQUE(agent_id, natural_key)
);
CREATE INDEX IF NOT EXISTS idx_sessions_last_event ON sessions(last_event_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state);

CREATE TABLE IF NOT EXISTS transcript (
  session_id  TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  event_id    TEXT,
  parent_id   TEXT,
  kind        TEXT NOT NULL,
  at          INTEGER NOT NULL,
  is_sidechain INTEGER NOT NULL DEFAULT 0,
  payload     TEXT NOT NULL,
  PRIMARY KEY (session_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_transcript_session_at ON transcript(session_id, at DESC);

CREATE TABLE IF NOT EXISTS approvals (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  subject     TEXT,
  payload     TEXT NOT NULL,
  asked_at    INTEGER NOT NULL,
  resolved_at INTEGER,
  behavior    TEXT,
  decided_by  TEXT
);
CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id, asked_at DESC);

-- Append-only local audit. Every connection, lease, input, approval, and config
-- change lands here. It is the record that makes remote input accountable.
CREATE TABLE IF NOT EXISTS audit (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         INTEGER NOT NULL,
  actor      TEXT NOT NULL,
  action     TEXT NOT NULL,
  session_id TEXT,
  detail     TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at DESC);

CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

// Open opens (and migrates) the store at dir/state.db.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.db")
	// WAL so a reader (the CLI) never blocks the daemon's writes; busy_timeout
	// so concurrent access retries instead of failing outright.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	// Columns added after the first release. CREATE TABLE IF NOT EXISTS leaves
	// an existing table alone, so each is added here when it is missing.
	if err := addColumn(db, "sessions", "name", "TEXT"); err != nil {
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// addColumn adds a column to a table that lacks it.
func addColumn(db *sql.DB, table, column, typ string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, typ))
	return err
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Path is the database file location.
func (s *Store) Path() string { return s.path }

// SaveSession upserts a session.
func (s *Store) SaveSession(sess *protocol.Session) error {
	guess, _ := json.Marshal(sess.Guess)
	var endedAt any
	if sess.State.Terminal() {
		endedAt = sess.StateSince.UnixMilli()
	}
	_, err := s.db.Exec(`
INSERT INTO sessions (id, agent_id, natural_key, title, name, cwd, git_branch, model,
  agent_version, attachment, fidelity, state, state_since, guess_json, prompt_id,
  transcript_path, created_at, last_event_at, ended_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(agent_id, natural_key) DO UPDATE SET
  title=excluded.title, name=excluded.name, cwd=excluded.cwd, git_branch=excluded.git_branch,
  model=excluded.model, agent_version=excluded.agent_version,
  attachment=excluded.attachment, fidelity=excluded.fidelity,
  state=excluded.state, state_since=excluded.state_since,
  guess_json=excluded.guess_json, prompt_id=excluded.prompt_id,
  transcript_path=excluded.transcript_path, last_event_at=excluded.last_event_at,
  ended_at=excluded.ended_at`,
		sess.ID, sess.AgentID, sess.NaturalKey, sess.Title, sess.Name, sess.Cwd,
		sess.GitBranch, sess.Model, sess.AgentVer, string(sess.Attachment), string(sess.Fidelity),
		string(sess.State), sess.StateSince.UnixMilli(), string(guess), sess.PromptID,
		sess.TranscriptPath, sess.CreatedAt.UnixMilli(), sess.LastEventAt.UnixMilli(), endedAt)
	return err
}

// Setting reads a daemon setting, "" when it was never set.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, "setting:"+key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting records a daemon setting, so it holds across restarts.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (k, v) VALUES (?, ?)
ON CONFLICT(k) DO UPDATE SET v = excluded.v`, "setting:"+key, value)
	return err
}

// DeleteSession forgets a session row, so a restart does not bring it back.
// Its transcript stays: another row may be following the same file.
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// LoadSessions returns persisted sessions, most recently active first.
//
// Sessions reloaded after a daemon restart cannot be assumed alive: the daemon
// held their PTYs. Callers reconcile against reality (process scan, hook
// traffic) and mark the rest DETACHED rather than showing a dead session as
// live.
func (s *Store) LoadSessions() ([]protocol.Session, error) {
	rows, err := s.db.Query(`
SELECT id, agent_id, natural_key, COALESCE(title,''), COALESCE(name,''), COALESCE(cwd,''),
       COALESCE(git_branch,''), COALESCE(model,''), COALESCE(agent_version,''),
       COALESCE(attachment,''), COALESCE(fidelity,''), state, state_since,
       COALESCE(guess_json,''), COALESCE(prompt_id,''), COALESCE(transcript_path,''),
       created_at, last_event_at
FROM sessions ORDER BY last_event_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []protocol.Session
	for rows.Next() {
		var (
			sess                      protocol.Session
			attachment, fidelity, st  string
			stateSince, created, last int64
			guessJSON                 string
		)
		if err := rows.Scan(&sess.ID, &sess.AgentID, &sess.NaturalKey, &sess.Title, &sess.Name,
			&sess.Cwd, &sess.GitBranch, &sess.Model, &sess.AgentVer, &attachment,
			&fidelity, &st, &stateSince, &guessJSON, &sess.PromptID,
			&sess.TranscriptPath, &created, &last); err != nil {
			return nil, err
		}
		sess.Attachment = protocol.Attachment(attachment)
		sess.Fidelity = protocol.Fidelity(fidelity)
		sess.State = protocol.SessionState(st)
		sess.StateSince = time.UnixMilli(stateSince)
		sess.CreatedAt = time.UnixMilli(created)
		sess.LastEventAt = time.UnixMilli(last)
		if guessJSON != "" {
			_ = json.Unmarshal([]byte(guessJSON), &sess.Guess)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AppendEvents writes transcript events in one transaction. Duplicate (session,
// seq) pairs are ignored, which makes re-ingesting a transcript file after a
// restart idempotent instead of an error.
func (s *Store) AppendEvents(events []protocol.TranscriptEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertEvents(tx, events); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceTranscript swaps a session's stored events for these, in one
// transaction: a reader sees the old transcript or the new one, never neither.
func (s *Store) ReplaceTranscript(sessionID string, events []protocol.TranscriptEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM transcript WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if err := insertEvents(tx, events); err != nil {
		return err
	}
	return tx.Commit()
}

func insertEvents(tx *sql.Tx, events []protocol.TranscriptEvent) error {
	stmt, err := tx.Prepare(`
INSERT INTO transcript (session_id, seq, event_id, parent_id, kind, at, is_sidechain, payload)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(session_id, seq) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return err
		}
		at := e.At
		if at.IsZero() {
			at = time.Now()
		}
		if _, err := stmt.Exec(e.SessionID, e.Seq, e.ID, e.ParentID, string(e.Kind),
			at.UnixMilli(), boolToInt(e.IsSidechain), string(payload)); err != nil {
			return err
		}
	}
	return nil
}

// RewriteEventsOnce passes every stored event of one kind, from one agent's
// sessions, through fn, which returns the event to keep (changed or not) or
// false to delete it. It runs once per name, recorded in meta.
//
// Stored events are otherwise never rewritten: AppendEvents ignores a seq it
// already has. This exists for the one case that needs it, a parser that
// learned to read something it used to misread, so that history already in the
// store reads the same as what arrives from now on.
func (s *Store) RewriteEventsOnce(
	name, agentID string,
	kind protocol.EventKind,
	fn func(protocol.TranscriptEvent) (protocol.TranscriptEvent, bool),
) (changed int, err error) {
	var done int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM meta WHERE k = ?`, "rewrite:"+name).Scan(&done); err != nil {
		return 0, err
	}
	if done > 0 {
		return 0, nil
	}

	rows, err := s.db.Query(`
SELECT t.session_id, t.seq, t.payload FROM transcript t
JOIN sessions s ON s.id = t.session_id
WHERE s.agent_id = ? AND t.kind = ?`, agentID, string(kind))
	if err != nil {
		return 0, err
	}
	type row struct {
		session string
		seq     uint64
		ev      protocol.TranscriptEvent
	}
	var all []row
	for rows.Next() {
		var r row
		var payload string
		if err := rows.Scan(&r.session, &r.seq, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		if json.Unmarshal([]byte(payload), &r.ev) == nil {
			all = append(all, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, r := range all {
		next, keep := fn(r.ev)
		if !keep {
			if _, err := tx.Exec(`DELETE FROM transcript WHERE session_id = ? AND seq = ?`, r.session, r.seq); err != nil {
				return 0, err
			}
			changed++
			continue
		}
		if next.Kind == r.ev.Kind && next.Text == r.ev.Text {
			continue
		}
		payload, err := json.Marshal(next)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE transcript SET kind = ?, payload = ? WHERE session_id = ? AND seq = ?`,
			string(next.Kind), string(payload), r.session, r.seq); err != nil {
			return 0, err
		}
		changed++
	}
	if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES (?, ?)`, "rewrite:"+name, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return 0, err
	}
	return changed, tx.Commit()
}

// Transcript returns a session's events, oldest first, from the given seq.
func (s *Store) Transcript(sessionID string, sinceSeq uint64, limit int) ([]protocol.TranscriptEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
SELECT payload FROM transcript
WHERE session_id = ? AND seq > ?
ORDER BY seq ASC LIMIT ?`, sessionID, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []protocol.TranscriptEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var e protocol.TranscriptEvent
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TranscriptBefore returns up to limit of a session's events with seq below
// before -- the newest ones when before is 0 -- in order, and whether older
// ones remain.
//
// A conversation is read from its end. Reading it from the start, a page at
// a time, gave a long session's first 500 events and stopped there: the
// newest messages, the ones anyone opens a session to see, never arrived.
func (s *Store) TranscriptBefore(sessionID string, before uint64, limit int) ([]protocol.TranscriptEvent, bool, error) {
	if limit <= 0 {
		limit = 500
	}
	if before == 0 {
		before = math.MaxInt64
	}
	rows, err := s.db.Query(`
SELECT payload FROM transcript
WHERE session_id = ? AND seq < ?
ORDER BY seq DESC LIMIT ?`, sessionID, int64(min(before, math.MaxInt64)), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []protocol.TranscriptEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, false, err
		}
		var e protocol.TranscriptEvent
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	slices.Reverse(out)
	return out, more, nil
}

// DeleteTranscript forgets a session's stored events, for reading its file
// again from the start.
func (s *Store) DeleteTranscript(sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM transcript WHERE session_id = ?`, sessionID)
	return err
}

// MaxSeq returns the highest seq stored for a session, so an ingester can
// resume where it left off rather than re-reading a 143 MB file.
func (s *Store) MaxSeq(sessionID string) (uint64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM transcript WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 0, nil
	}
	return uint64(seq.Int64), nil
}

// RecordApproval stores an approval request.
func (s *Store) RecordApproval(req *protocol.ApprovalRequest) error {
	payload, _ := json.Marshal(req)
	_, err := s.db.Exec(`
INSERT INTO approvals (id, session_id, subject, payload, asked_at)
VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		req.ID, req.SessionID, req.Subject, string(payload), req.AskedAt.UnixMilli())
	return err
}

// ResolveApproval records the answer.
func (s *Store) ResolveApproval(id string, d protocol.ApprovalDecision) error {
	_, err := s.db.Exec(`
UPDATE approvals SET resolved_at = ?, behavior = ?, decided_by = ? WHERE id = ?`,
		time.Now().UnixMilli(), d.Behavior, d.DecidedBy, id)
	return err
}

// Audit appends an audit record.
func (s *Store) Audit(actor, action, sessionID, detail string) error {
	_, err := s.db.Exec(`
INSERT INTO audit (at, actor, action, session_id, detail) VALUES (?,?,?,?,?)`,
		time.Now().UnixMilli(), actor, action, sessionID, detail)
	return err
}

// AuditEntry is one audit record.
type AuditEntry struct {
	At        time.Time `json:"at"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	SessionID string    `json:"session_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// AuditLog returns recent audit entries, newest first.
func (s *Store) AuditLog(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	// Tiebreak on the autoincrement id: several audit rows routinely land in the
	// same millisecond, and ordering by timestamp alone makes their order
	// arbitrary -- which reads as a scrambled log to anyone auditing an incident.
	rows, err := s.db.Query(`
SELECT at, actor, action, COALESCE(session_id,''), COALESCE(detail,'')
FROM audit ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&at, &e.Actor, &e.Action, &e.SessionID, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purge deletes sessions that ended before the cutoff, and their transcripts.
func (s *Store) Purge(before time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	cutoff := before.UnixMilli()
	if _, err := tx.Exec(`
DELETE FROM transcript WHERE session_id IN
  (SELECT id FROM sessions WHERE ended_at IS NOT NULL AND ended_at < ?)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM sessions WHERE ended_at IS NOT NULL AND ended_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

package hermes

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// buildFixture creates a database with the real Hermes v0.12.0 schema.
func buildFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT, model TEXT,
  model_config TEXT, system_prompt TEXT, parent_session_id TEXT,
  started_at REAL NOT NULL, ended_at REAL, end_reason TEXT,
  message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0,
  input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
  cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0,
  reasoning_tokens INTEGER DEFAULT 0, billing_provider TEXT, billing_base_url TEXT,
  billing_mode TEXT, estimated_cost_usd REAL, actual_cost_usd REAL,
  cost_status TEXT, cost_source TEXT, pricing_version TEXT, title TEXT,
  api_call_count INTEGER DEFAULT 0);
CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
  role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT,
  tool_name TEXT, timestamp REAL NOT NULL, token_count INTEGER,
  finish_reason TEXT, reasoning TEXT, reasoning_content TEXT,
  reasoning_details TEXT, codex_reasoning_items TEXT, codex_message_items TEXT);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	now := float64(time.Now().Unix())
	// A live session, a finished one, and a subagent child.
	if _, err := db.Exec(`
INSERT INTO sessions (id, source, model, parent_session_id, started_at, ended_at,
  end_reason, message_count, tool_call_count, input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens, actual_cost_usd, title) VALUES
  ('s-live','cli','hermes-4', NULL, ?, NULL, NULL, 4, 1, 1200, 340, 800, 60, 0.02, 'Live work'),
  ('s-done','cli','hermes-4', NULL, ?, ?, 'compression', 2, 0, 100, 20, 0, 0, 0.001, 'Older work'),
  ('s-child','cli','hermes-4','s-live', ?, NULL, NULL, 1, 0, 10, 5, 0, 0, 0, 'Subagent')`,
		now-600, now-9000, now-8000, now-300); err != nil {
		t.Fatal(err)
	}

	toolCalls := `[{"id":"call_1","call_id":"call_1","type":"function","function":{"name":"search_files","arguments":"{\"pattern\":\"*\",\"path\":\"~/dev/app\",\"limit\":100}"}}]`
	if _, err := db.Exec(`
INSERT INTO messages (session_id, role, content, tool_call_id, tool_calls, tool_name, timestamp, reasoning) VALUES
  ('s-live','user','find the config',NULL,NULL,NULL,?,NULL),
  ('s-live','assistant','I will search.',NULL,?,NULL,?,'Let me look at the layout first.'),
  ('s-live','tool','{"total_count": 3}','call_1',NULL,'search_files',?,NULL),
  ('s-live','system','internal machinery',NULL,NULL,NULL,?,NULL),
  ('s-done','user','older question',NULL,NULL,NULL,?,NULL)`,
		now-600, toolCalls, now-590, now-580, now-575, now-9000); err != nil {
		t.Fatal(err)
	}
	return path
}

func openFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(buildFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSessions(t *testing.T) {
	s := openFixture(t)
	sessions, err := s.Sessions(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}

	byID := map[string]Session{}
	for _, sess := range sessions {
		byID[sess.ID] = sess
	}

	live := byID["s-live"]
	if !live.IsLive() {
		t.Error("s-live should be live")
	}
	if live.Model != "hermes-4" || live.Title != "Live work" || live.Source != "cli" {
		t.Errorf("live session = %+v", live)
	}
	if live.Tokens.InputTokens != 1200 || live.Tokens.CacheReadTokens != 800 {
		t.Errorf("tokens = %+v", live.Tokens)
	}

	if byID["s-done"].IsLive() {
		t.Error("a session with an end_reason is not live")
	}
	if !byID["s-child"].IsChild() {
		t.Error("a session with parent_session_id is a child")
	}
	if byID["s-live"].IsChild() {
		t.Error("a root session was reported as a child")
	}
}

func TestLiveSessionsFiltersEnded(t *testing.T) {
	s := openFixture(t)
	live, err := s.LiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range live {
		if !sess.IsLive() {
			t.Errorf("%s is not live but was returned", sess.ID)
		}
	}
	if len(live) != 2 { // s-live and s-child
		t.Errorf("got %d live sessions, want 2", len(live))
	}
}

func TestTranscriptMapping(t *testing.T) {
	s := openFixture(t)
	events, maxID, err := s.Transcript("s-live", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if maxID == 0 {
		t.Error("maxID should advance so a tailer can resume")
	}

	counts := map[protocol.EventKind]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	want := map[protocol.EventKind]int{
		protocol.EventUserMessage:      1,
		protocol.EventAssistantMessage: 1,
		protocol.EventThinking:         1,
		protocol.EventToolCall:         1,
		protocol.EventToolResult:       1,
	}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("count[%s] = %d, want %d", k, counts[k], w)
		}
	}

	for i, e := range events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, e.Seq, i+1)
		}
		if e.SessionID != "s-live" {
			t.Errorf("event %d has session %q", i, e.SessionID)
		}
	}
}

// tool_calls carries arguments as a JSON string, the same shape Codex uses.
// Showing it raw would give an escaped blob instead of the arguments a human
// needs to judge the action.
func TestToolArgumentsAreParsed(t *testing.T) {
	s := openFixture(t)
	events, _, _ := s.Transcript("s-live", 0, 0)
	for _, e := range events {
		if e.Kind != protocol.EventToolCall {
			continue
		}
		if e.ToolName != "search_files" || e.ToolUseID != "call_1" {
			t.Errorf("tool call = %+v", e)
		}
		if e.ToolInput["path"] != "~/dev/app" {
			t.Errorf("arguments not parsed into a map: %v", e.ToolInput)
		}
		return
	}
	t.Fatal("no tool call event")
}

func TestSystemMessagesExcluded(t *testing.T) {
	s := openFixture(t)
	events, _, _ := s.Transcript("s-live", 0, 0)
	for _, e := range events {
		if e.Text == "internal machinery" {
			t.Error("a system message leaked into the transcript")
		}
	}
}

// Resuming by row id is what keeps a tailer cheap regardless of history size.
func TestTranscriptResumesByRowID(t *testing.T) {
	s := openFixture(t)
	first, maxID, err := s.Transcript("s-live", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("no events in the first page")
	}
	second, _, err := s.Transcript("s-live", maxID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range first {
		for _, b := range second {
			if a.Seq == b.Seq && a.Text == b.Text && a.Text != "" {
				t.Errorf("event repeated across pages: %q", a.Text)
			}
		}
	}
}

// end_reason is the one exact signal Hermes gives; everything else is inference
// and its confidence must say so.
func TestInferState(t *testing.T) {
	ended := Session{EndReason: "compression", EndedAt: time.Now()}
	g := InferState(ended, time.Now(), 6*time.Second)
	if g.State != protocol.StateExited || g.Confidence != 1.0 {
		t.Errorf("an ended session -> %s at %v, want EXITED at 1.0", g.State, g.Confidence)
	}

	live := Session{}
	recent := InferState(live, time.Now(), 6*time.Second)
	if recent.State != protocol.StateRunning {
		t.Errorf("recent activity -> %s, want RUNNING", recent.State)
	}
	if recent.Confidence >= 1.0 {
		t.Errorf("inference confidence %v must stay below the exact path", recent.Confidence)
	}

	quiet := InferState(live, time.Now().Add(-time.Hour), 6*time.Second)
	if quiet.State != protocol.StateWaitingInput {
		t.Errorf("quiet -> %s, want WAITING_INPUT", quiet.State)
	}

	empty := InferState(live, time.Time{}, 6*time.Second)
	if empty.State != protocol.StateUnknown {
		t.Errorf("no messages -> %s, want UNKNOWN", empty.State)
	}
	for _, g := range []protocol.StateGuess{recent, quiet, empty} {
		if g.Evidence == "" {
			t.Error("every guess needs evidence for `debug state`")
		}
	}
}

func TestFloatTimestampPrecision(t *testing.T) {
	got := floatTime(1785723682.149)
	if got.Unix() != 1785723682 {
		t.Errorf("seconds = %d", got.Unix())
	}
	if got.Nanosecond() == 0 {
		t.Error("sub-second precision was dropped")
	}
	if !floatTime(0).IsZero() {
		t.Error("a zero timestamp should be the zero time, not the epoch")
	}
}

func TestOpenIsReadOnly(t *testing.T) {
	path := buildFixture(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Hermes is actively writing this file. A monitoring tool that can take a
	// write lock on the agent's own database is exactly the interference
	// muxscale must avoid.
	if _, err := s.db.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('x','cli',1)`); err == nil {
		t.Error("the connection accepted a write; it must be read-only")
	}
}

func TestOpenMissingFileIsClear(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "nope.db"))
	if err == nil {
		t.Fatal("expected an error for a missing database")
	}
	if !contains(err.Error(), "not found") {
		t.Errorf("error should say the database is missing: %v", err)
	}
}

// Hermes records no working directory. Saying so beats rendering a blank field
// the user will read as broken.
func TestMissingCwdIsExplained(t *testing.T) {
	if NoWorkingDirectory == "" {
		t.Error("the missing-cwd gap needs an explanation string")
	}
}

// Every real Hermes session on this machine must read cleanly.
func TestAgainstRealDatabase(t *testing.T) {
	path, err := DBPath()
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no hermes database on this machine")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("opening the real database: %v", err)
	}
	defer s.Close()

	sessions, err := s.Sessions(0)
	if err != nil {
		t.Fatalf("reading real sessions: %v", err)
	}

	var totalEvents, live, children int
	for _, sess := range sessions {
		if sess.IsLive() {
			live++
		}
		if sess.IsChild() {
			children++
		}
		events, _, err := s.Transcript(sess.ID, 0, 100000)
		if err != nil {
			t.Errorf("transcript for %s: %v", sess.ID, err)
			continue
		}
		totalEvents += len(events)
	}
	t.Logf("read %d real sessions (%d live, %d children), %d transcript events",
		len(sessions), live, children, totalEvents)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// seedDB writes a database with OpenCode 1.18's schema (the columns this
// adapter reads) under a temporary XDG_DATA_HOME.
func seedDB(t *testing.T) *sql.DB {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	path := filepath.Join(data, "opencode", "opencode.db")
	if err := mkdirAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		"CREATE TABLE `session` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `parent_id` text, `slug` text NOT NULL, `directory` text NOT NULL, `title` text NOT NULL, `version` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_archived` integer, `model` text)",
		"CREATE TABLE `message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL)",
		"CREATE TABLE `part` (`id` text PRIMARY KEY, `message_id` text NOT NULL, `session_id` text NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseSessionsAndTranscript(t *testing.T) {
	db := seedDB(t)
	now := time.Now().UnixMilli()
	exec(t, db, `INSERT INTO session VALUES
	  ('ses_a','p','', 'a','/home/dev/app','New session - 2026-08-03T02:19:17.417Z','1.18.33',?,?,NULL,'{"id":"kimi-k3","providerID":"opencode-go"}'),
	  ('ses_child','p','ses_a','c','/home/dev/app','Subagent','1.18.33',?,?,NULL,NULL),
	  ('ses_old','p',NULL,'o','/home/dev/app','Archived','1.18.33',?,?,?,NULL)`,
		now-60000, now-1000, now, now, now, now, now)
	// parent_id '' is not NULL; make the root a real root.
	exec(t, db, `UPDATE session SET parent_id = NULL WHERE id = 'ses_a'`)

	exec(t, db, `INSERT INTO message VALUES
	  ('msg_1','ses_a',?,?,'{"role":"user","time":{"created":1}}'),
	  ('msg_2','ses_a',?,?,'{"role":"assistant","modelID":"kimi-k3","finish":"tool-calls","time":{"created":2,"completed":3}}'),
	  ('msg_3','ses_a',?,?,'{"role":"assistant","modelID":"kimi-k3","time":{"created":4}}')`,
		now-50000, now-50000, now-40000, now-40000, now-30000, now-30000)
	exec(t, db, `INSERT INTO part VALUES
	  ('prt_1','msg_1','ses_a',?,?,'{"type":"text","text":"add a health endpoint"}'),
	  ('prt_1b','msg_1','ses_a',?,?,'{"type":"text","text":"<file contents>","synthetic":true}'),
	  ('prt_2','msg_2','ses_a',?,?,'{"type":"step-start","snapshot":"x"}'),
	  ('prt_3','msg_2','ses_a',?,?,'{"type":"reasoning","text":"check the router","time":{"start":1,"end":2}}'),
	  ('prt_4','msg_2','ses_a',?,?,'{"type":"tool","tool":"read","callID":"call_1","state":{"status":"completed","input":{"path":"router.ts"},"output":"export const router"}}'),
	  ('prt_5','msg_3','ses_a',?,?,'{"type":"tool","tool":"bash","callID":"call_2","state":{"status":"running","input":{"command":"pnpm test"}}}'),
	  ('prt_6','msg_3','ses_a',?,?,'{"type":"text","text":"Tests are running."}')`,
		now, now, now, now, now, now, now, now, now, now, now, now, now, now)

	got, err := (&Adapter{}).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("discovered %+v; want only the unarchived root", got)
	}
	d := got[0]
	if d.NaturalKey != "ses_a" || d.Cwd != "/home/dev/app" || d.Model != "kimi-k3" || !d.Live {
		t.Errorf("discovered = %+v", d)
	}
	if d.Title != "add a health endpoint" {
		t.Errorf("an untitled session's title = %q, want its first prompt", d.Title)
	}

	ts, err := (&Adapter{}).OpenTranscript(context.Background(), "ses_a")
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close()
	first, err := ts.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := []protocol.EventKind{}
	for _, e := range first {
		kinds = append(kinds, e.Kind)
	}
	// The running tool's call goes out; its result, and the text after it,
	// wait until the tool is done.
	want := []protocol.EventKind{protocol.EventUserMessage, protocol.EventThinking,
		protocol.EventToolCall, protocol.EventToolResult, protocol.EventToolCall}
	if len(kinds) != len(want) {
		t.Fatalf("first read = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] || first[i].Seq != uint64(i+1) {
			t.Fatalf("event %d = %s seq %d, want %s seq %d", i, kinds[i], first[i].Seq, want[i], i+1)
		}
	}
	if first[3].Text != "export const router" || first[2].ToolInput["path"] != "router.ts" {
		t.Errorf("tool events = %+v / %+v", first[2], first[3])
	}
	er := ts.(adapter.EventReporter)
	if er.LastEvent() != lifeTool {
		t.Errorf("lifecycle = %q, want %q", er.LastEvent(), lifeTool)
	}

	// The tool finishes and the turn ends.
	exec(t, db, `UPDATE part SET data = '{"type":"tool","tool":"bash","callID":"call_2","state":{"status":"completed","input":{"command":"pnpm test"},"output":"ok"}}' WHERE id = 'prt_5'`)
	exec(t, db, `UPDATE message SET data = '{"role":"assistant","modelID":"kimi-k3","finish":"stop","time":{"created":4,"completed":5}}' WHERE id = 'msg_3'`)
	next, err := ts.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 || next[0].Kind != protocol.EventToolResult || next[0].Seq != 6 ||
		next[1].Kind != protocol.EventAssistantMessage || next[1].Seq != 7 {
		t.Fatalf("second read = %+v", next)
	}
	if again, _ := ts.Poll(context.Background()); len(again) != 0 {
		t.Fatalf("re-emitted %+v", again)
	}
	g, ok := (&Adapter{}).InferState(adapter.StateInput{Event: er.LastEvent(), Idle: time.Minute, Quiescence: 6 * time.Second})
	if !ok || g.State != protocol.StateWaitingInput {
		t.Fatalf("finished turn state = %+v", g)
	}

	// A reader resumed after a restart picks up where the store left off.
	resumed, _ := (&Adapter{}).OpenTranscript(context.Background(), "ses_a")
	defer resumed.Close()
	if err := resumed.(adapter.SeekableStream).SeekEnd(7); err != nil {
		t.Fatal(err)
	}
	if evs, _ := resumed.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("resumed reader repeated %d events", len(evs))
	}

	// The process scan's join: same directory, started with the session.
	started := time.UnixMilli(now - 60000)
	if id := (&Adapter{}).MatchTranscript("/home/dev/app", started, nil); id != "ses_a" {
		t.Errorf("join = %q", id)
	}
	if id := (&Adapter{}).MatchTranscript("/elsewhere", started, nil); id != "" {
		t.Errorf("joined another directory's session: %q", id)
	}
}

func TestAbortedAndFailedTurnsEnd(t *testing.T) {
	for _, tc := range []struct {
		data string
		life string
		kind protocol.EventKind
	}{
		{`{"role":"assistant","time":{"created":1,"completed":2},"error":{"name":"MessageAbortedError"}}`, lifeAborted, protocol.EventNotice},
		{`{"role":"assistant","time":{"created":1,"completed":2},"error":{"name":"APIError","data":{"message":"rate limited"}}}`, lifeError, protocol.EventError},
	} {
		db := seedDB(t)
		exec(t, db, `INSERT INTO session VALUES ('ses_a','p',NULL,'a','/w','t','1.18.33',1,1,NULL,NULL)`)
		exec(t, db, `INSERT INTO message VALUES ('msg_1','ses_a',1,1,'{"role":"user","time":{"created":1}}'), ('msg_2','ses_a',2,2,?)`, tc.data)
		exec(t, db, `INSERT INTO part VALUES ('prt_1','msg_1','ses_a',1,1,'{"type":"text","text":"go"}')`)
		ts, err := (&Adapter{}).OpenTranscript(context.Background(), "ses_a")
		if err != nil {
			t.Fatal(err)
		}
		evs, _ := ts.Poll(context.Background())
		if len(evs) != 2 || evs[1].Kind != tc.kind {
			t.Errorf("%s: events = %+v", tc.life, evs)
		}
		if got := ts.(adapter.EventReporter).LastEvent(); got != tc.life {
			t.Errorf("lifecycle = %q, want %q", got, tc.life)
		}
		g, _ := stateFromLifecycle(tc.life, time.Minute, 6*time.Second)
		if g.State != protocol.StateWaitingInput {
			t.Errorf("%s -> %s, want WAITING_INPUT", tc.life, g.State)
		}
		ts.Close()
	}
}

func TestNoDatabaseIsNotAnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	got, err := (&Adapter{}).Discover(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

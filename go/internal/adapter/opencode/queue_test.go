package opencode

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Both generations of OpenCode's queue table, created as its migrations do.
func TestQueueBothTables(t *testing.T) {
	for _, tc := range []struct {
		table, ddl string
		rows       []string
	}{
		{"session_inbox",
			"CREATE TABLE session_inbox (id text PRIMARY KEY, session_id text, type text, payload text, delivery text, enqueued_seq integer, time_created integer)",
			[]string{
				`INSERT INTO session_inbox VALUES ('m1','s','user','{"text":"run the tests"}','queue',1,1760000000000)`,
				`INSERT INTO session_inbox VALUES ('m2','s','compaction','{}','queue',2,1760000000001)`,
				`INSERT INTO session_inbox VALUES ('m3','s','user','{"text":"actually stop"}','steer',3,1760000000002)`,
				`INSERT INTO session_inbox VALUES ('m4','other','user','{"text":"not this one"}','queue',1,1760000000003)`,
			}},
		{"session_input",
			"CREATE TABLE session_input (id text PRIMARY KEY, session_id text, prompt text, delivery text, admitted_seq integer, promoted_seq integer, time_created integer)",
			[]string{
				`INSERT INTO session_input VALUES ('m0','s','{"text":"already taken"}','queue',0,5,1760000000000)`,
				`INSERT INTO session_input VALUES ('m1','s','{"text":"run the tests"}','queue',1,NULL,1760000000000)`,
				`INSERT INTO session_input VALUES ('m3','s','{"text":"actually stop"}','steer',3,NULL,1760000000002)`,
			}},
	} {
		t.Run(tc.table, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "o.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, q := range append([]string{tc.ddl}, tc.rows...) {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if got := queueTable(db); got != tc.table {
				t.Fatalf("table = %q", got)
			}
			q, err := readQueue(db, tc.table, "s")
			if err != nil || len(q) != 2 {
				t.Fatalf("queue = %+v, %v", q, err)
			}
			if q[0].Text != "run the tests" || q[0].Delivery != "queue" || q[1].Text != "actually stop" || q[1].Delivery != "steer" {
				t.Errorf("queue = %+v", q)
			}

			// The stream reports it once, then only when it changes.
			s := &dbStream{db: db, sessionID: "s"}
			s.pollQueue()
			if m := s.Meta(); m.Queue == nil || len(*m.Queue) != 2 {
				t.Fatalf("first = %+v", m.Queue)
			}
			s.pollQueue()
			if m := s.Meta(); m.Queue != nil {
				t.Error("an unchanged queue was reported again")
			}
		})
	}
}

// 1.18 keeps no queue table in use: a queued prompt is a user message after
// the unfinished reply that no reply answers yet (verified against 1.18.33).
func TestQueueFromMessages(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE session_input (id text PRIMARY KEY, session_id text, prompt text, delivery text, admitted_seq integer, promoted_seq integer, time_created integer)",
		"CREATE TABLE message (id text PRIMARY KEY, session_id text, time_created integer, time_updated integer, data text)",
		"CREATE TABLE part (id text PRIMARY KEY, message_id text, session_id text, time_created integer, time_updated integer, data text)",
		`INSERT INTO message VALUES ('m1','s',100,100,'{"role":"user","time":{"created":100}}')`,
		`INSERT INTO message VALUES ('m2','s',101,101,'{"role":"assistant","parentID":"m1","time":{"created":101}}')`,
		`INSERT INTO message VALUES ('m3','s',200,200,'{"role":"user","time":{"created":200}}')`,
		`INSERT INTO part VALUES ('p1','m1','s',100,100,'{"type":"text","text":"count to 200"}')`,
		`INSERT INTO part VALUES ('p3','m3','s',200,200,'{"type":"text","text":"then say done"}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	q, err := readQueue(db, queueTable(db), "s")
	if err != nil || len(q) != 1 || q[0].Text != "then say done" {
		t.Fatalf("queued = %+v, %v", q, err)
	}

	// Taken: the reply finished, and one answering m3 began.
	for _, x := range []string{
		`UPDATE message SET data='{"role":"assistant","parentID":"m1","time":{"created":101,"completed":150}}' WHERE id='m2'`,
		`INSERT INTO message VALUES ('m4','s',201,201,'{"role":"assistant","parentID":"m3","time":{"created":201}}')`,
	} {
		if _, err := db.Exec(x); err != nil {
			t.Fatal(err)
		}
	}
	if q, _ := readQueue(db, queueTable(db), "s"); len(q) != 0 {
		t.Fatalf("after it was taken = %+v", q)
	}
}

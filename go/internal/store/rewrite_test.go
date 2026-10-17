package store

import (
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

func TestRewriteEventsOnce(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	for _, s := range []protocol.Session{
		{ID: "cc", AgentID: "claude-code", NaturalKey: "a", State: protocol.StateIdle, StateSince: now, CreatedAt: now, LastEventAt: now},
		{ID: "cx", AgentID: "codex", NaturalKey: "b", State: protocol.StateIdle, StateSince: now, CreatedAt: now, LastEventAt: now},
	} {
		if err := st.SaveSession(&s); err != nil {
			t.Fatal(err)
		}
	}
	msg := func(session string, seq uint64, text string) protocol.TranscriptEvent {
		return protocol.TranscriptEvent{SessionID: session, Seq: seq, Kind: protocol.EventUserMessage, Text: text, At: now}
	}
	if err := st.AppendEvents([]protocol.TranscriptEvent{
		msg("cc", 1, "hello"),
		msg("cc", 2, "<cmd>/compact"),
		msg("cc", 3, "<drop>"),
		msg("cx", 1, "<cmd>/untouched"),
	}); err != nil {
		t.Fatal(err)
	}

	fn := func(e protocol.TranscriptEvent) (protocol.TranscriptEvent, bool) {
		switch {
		case strings.HasPrefix(e.Text, "<drop>"):
			return e, false
		case strings.HasPrefix(e.Text, "<cmd>"):
			e.Kind, e.Text = protocol.EventCommand, strings.TrimPrefix(e.Text, "<cmd>")
		}
		return e, true
	}
	n, err := st.RewriteEventsOnce("t1", "claude-code", protocol.EventUserMessage, fn)
	if err != nil || n != 2 {
		t.Fatalf("first run changed %d (err %v), want 2", n, err)
	}
	if n, _ := st.RewriteEventsOnce("t1", "claude-code", protocol.EventUserMessage, fn); n != 0 {
		t.Fatalf("second run changed %d, want 0: it runs once", n)
	}

	got, _ := st.Transcript("cc", 0, 0)
	if len(got) != 2 || got[0].Text != "hello" || got[1].Kind != protocol.EventCommand || got[1].Text != "/compact" {
		t.Fatalf("claude-code transcript = %+v", got)
	}
	other, _ := st.Transcript("cx", 0, 0)
	if len(other) != 1 || other[0].Kind != protocol.EventUserMessage {
		t.Fatalf("another agent's transcript was rewritten: %+v", other)
	}
}

func TestSessionNamePersistsAndOldDatabasesMigrate(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A database from before names: drop the column and reopen.
	if _, err := st.db.Exec(`ALTER TABLE sessions DROP COLUMN name`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if st, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	in := protocol.Session{ID: "s", AgentID: "claude-code", NaturalKey: "k", Title: "auto", Name: "mine",
		State: protocol.StateIdle, StateSince: now, CreatedAt: now, LastEventAt: now}
	if err := st.SaveSession(&in); err != nil {
		t.Fatal(err)
	}
	out, err := st.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "mine" || out[0].Title != "auto" {
		t.Fatalf("loaded %+v", out)
	}
}

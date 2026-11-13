package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/claudecode"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// A rename from a phone is written where Claude Code keeps its own, and read
// back from there, so a /rename in the terminal and one from the app are the
// same setting.
func TestRenameSyncsThroughTheTranscript(t *testing.T) {
	m, reg, _ := testManager(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "abc-123.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"ai-title","aiTitle":"Made up","sessionId":"abc-123"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := claudecode.NewTailer(path, time.Second)
	s := reg.Upsert("claude-code", "pid:1", func(s *protocol.Session) { s.TranscriptPath = path })

	poll := func() {
		t.Helper()
		_, meta, err := tl.Poll()
		if err != nil {
			t.Fatal(err)
		}
		m.applyMeta(s.ID, meta)
	}
	poll()

	got, err := m.Rename(s.ID, "  Ship   the\nrelease ", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ship the release" || got.Title != "Made up" {
		t.Fatalf("name %q title %q", got.Name, got.Title)
	}

	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var entry map[string]string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["type"] != "custom-title" || entry["customTitle"] != "Ship the release" || entry["sessionId"] != "abc-123" {
		t.Fatalf("appended %v", entry)
	}
	side, err := os.ReadFile(filepath.Join(dir, "abc-123", "custom-title.json"))
	if err != nil || !strings.Contains(string(side), "Ship the release") {
		t.Fatalf("sidecar %q, %v", side, err)
	}

	// A /rename in the terminal, followed by a fresh ai-title: the person's
	// name holds.
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"type":"custom-title","customTitle":"From the terminal","sessionId":"abc-123"}` + "\n")
	f.WriteString(`{"type":"ai-title","aiTitle":"Made up again","sessionId":"abc-123"}` + "\n")
	f.Close()
	poll()
	if got, _ := reg.Get(s.ID); got.Name != "From the terminal" || got.Title != "Made up again" {
		t.Fatalf("after terminal rename: name %q title %q", got.Name, got.Title)
	}

	// Clearing goes back to the agent's title and removes the sidecar.
	if _, err := m.Rename(s.ID, "", "phone"); err != nil {
		t.Fatal(err)
	}
	poll()
	if got, _ := reg.Get(s.ID); got.Name != "" {
		t.Fatalf("name %q after clearing", got.Name)
	}
	if _, err := os.Stat(filepath.Join(dir, "abc-123", "custom-title.json")); !os.IsNotExist(err) {
		t.Fatalf("sidecar still there: %v", err)
	}

	if _, err := m.Rename(s.ID, strings.Repeat("x", maxNameLen+1), "phone"); err == nil {
		t.Fatal("an over-long name was accepted")
	}
}

// After a restart only rows still running are back in memory, so a resumed
// conversation turns up as a new row. It takes the name its transcript was
// last known by, rather than coming back unnamed and overwriting the stored
// row that had the name.
func TestAResumedConversationKeepsItsName(t *testing.T) {
	m, reg, st := testManager(t)
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	now := time.Now()
	ended := protocol.Session{
		ID: "old", AgentID: "omp", NaturalKey: "pid:1", Name: "Ship it",
		State: protocol.StateExited, StateSince: now, CreatedAt: now, LastEventAt: now,
		TranscriptPath: path,
	}
	if err := st.SaveSession(&ended); err != nil {
		t.Fatal(err)
	}

	s := reg.Upsert("omp", "pid:2", func(s *protocol.Session) { s.TranscriptPath = path })
	if s.Name != "Ship it" {
		t.Fatalf("resumed row named %q", s.Name)
	}

	// Taken up later, by a hook, works the same way.
	later := reg.Upsert("omp", "pid:3", nil)
	got := reg.Update(later.ID, func(s *protocol.Session) { s.TranscriptPath = path })
	if got.Name != "Ship it" {
		t.Fatalf("row given the transcript later named %q", got.Name)
	}

	// A name cleared on purpose stays cleared.
	if _, err := m.Rename(s.ID, "", "phone"); err != nil {
		t.Fatal(err)
	}
	if again := reg.Update(s.ID, func(s *protocol.Session) { s.Title = "moved on" }); again.Name != "" {
		t.Fatalf("cleared name came back as %q", again.Name)
	}
}

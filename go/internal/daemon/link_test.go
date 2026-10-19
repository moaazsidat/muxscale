package daemon

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// linkServer is a hook server whose every request comes from process 4242.
func linkServer(t *testing.T) (*httptest.Server, *Registry) {
	t.Helper()
	reg := NewRegistry()
	h := NewHookServer(reg, hookAdapters(), "secret-token", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.peerPID = func(string) (int, error) { return 4242, nil }
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, reg
}

func live(reg *Registry) []protocol.Session {
	var out []protocol.Session
	for _, s := range reg.List() {
		if !s.State.Terminal() {
			out = append(out, s)
		}
	}
	return out
}

// Regression: a Claude Code process found by the scan and then heard from
// through its hooks showed as two sessions -- the scan's, which could be typed
// into, and the hooks', which knew the state exactly.
func TestHooksJoinTheSessionTheScanFound(t *testing.T) {
	srv, reg := linkServer(t)
	scanned := reg.Upsert("claude-code", "pid:4242", func(s *protocol.Session) {
		s.Attachment = protocol.AttachTermctl
		s.Name = "mine"
	})

	postHook(t, srv, "secret-token", map[string]any{
		"session_id": "abc", "hook_event_name": "Stop", "cwd": "/w",
	})
	postHook(t, srv, "secret-token", map[string]any{
		"session_id": "abc", "hook_event_name": "UserPromptSubmit", "cwd": "/w",
	})

	rows := live(reg)
	if len(rows) != 1 || rows[0].ID != scanned.ID {
		t.Fatalf("got %d sessions, want just the scanned one: %+v", len(rows), rows)
	}
	if rows[0].State != protocol.StateRunning || rows[0].Guess.Confidence != 1 || rows[0].Name != "mine" {
		t.Fatalf("hook state did not land on the scanned row: %+v", rows[0])
	}
}

// The other order: the hooks first, the scan finding the process after.
func TestScanJoinsTheSessionTheHooksFound(t *testing.T) {
	srv, reg := linkServer(t)
	postHook(t, srv, "secret-token", map[string]any{
		"session_id": "abc", "hook_event_name": "Stop", "cwd": "/w",
	})
	hooked, _ := reg.IDFor("claude-code", "abc")

	again := reg.Upsert("claude-code", "pid:4242", nil)
	if again.ID != hooked || len(live(reg)) != 1 {
		t.Fatalf("the scan made a second session (%s, hooks have %s)", again.ID, hooked)
	}
}

// Both rows already exist, as after a restart: they merge into the scanned
// one, and clients are told the other is gone.
func TestTwoRowsForOneProcessMerge(t *testing.T) {
	srv, reg := linkServer(t)
	scanned := reg.Upsert("claude-code", "pid:4242", nil)
	dup := reg.Upsert("claude-code", "abc", func(s *protocol.Session) { s.Attachment = protocol.AttachNative })
	ch, cancel := reg.Subscribe()
	defer cancel()

	postHook(t, srv, "secret-token", map[string]any{
		"session_id": "abc", "hook_event_name": "Stop", "cwd": "/w",
	})

	if rows := live(reg); len(rows) != 1 || rows[0].ID != scanned.ID {
		t.Fatalf("after merging: %+v", rows)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case c := <-ch:
			if c.Kind == ChangeSessionRemove && c.Session.ID == dup.ID {
				return
			}
		case <-deadline:
			t.Fatal("clients were never told the duplicate is gone")
		}
	}
}

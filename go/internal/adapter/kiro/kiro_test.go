package kiro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBothGenerations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessions := filepath.Join(home, ".kiro", "sessions")

	// CLI 3.0, the shape kiro-cli-history reads.
	v3 := filepath.Join(sessions, "a1b2c3", "sess_01")
	write(t, filepath.Join(v3, "session.json"),
		`{"id":"sess_01","title":"Fix the build","createdAt":"2026-10-19T10:00:00Z","lastModifiedAt":"2026-10-19T10:05:00Z","workspacePaths":["/home/dev/app"]}`)
	write(t, filepath.Join(v3, "messages.jsonl"), strings.Join([]string{
		`{"payload":{"type":"user","content":"fix the build"}}`,
		`{"payload":{"type":"tool_use","content":"ignored"}}`,
		`{"payload":{"type":"assistant","content":"The build is fixed."}}`,
		`{"payload":{"type":"usage_summary","promptTurnSummaries":[{"unit":"credit","usage":0.4}]}}`,
	}, "\n")+"\n")

	// CLI 2.x classic.
	write(t, filepath.Join(sessions, "cli", "0f0e.json"),
		`{"session_id":"0f0e","title":"Old one","cwd":"/home/dev/other","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:10:00Z"}`)
	write(t, filepath.Join(sessions, "cli", "0f0e.jsonl"), strings.Join([]string{
		`{"kind":"Prompt","data":{"content":[{"kind":"text","data":"hello"}]}}`,
		`{"kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"hi"}]}}`,
		`{"kind":"ToolResults","data":{}}`,
	}, "\n")+"\n")

	a := New()
	got, err := a.Discover(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("discovered %+v, %v", got, err)
	}
	byKey := map[string]adapter.Discovered{}
	for _, d := range got {
		byKey[d.NaturalKey] = d
	}
	if d := byKey["sess_01"]; d.Cwd != "/home/dev/app" || d.Title != "Fix the build" || !d.Live {
		t.Errorf("v3 = %+v", d)
	}
	if d := byKey["0f0e"]; d.Cwd != "/home/dev/other" || d.Title != "Old one" {
		t.Errorf("2.x = %+v", d)
	}

	for key, want := range map[string]string{"sess_01": "user_message,assistant_message", "0f0e": "user_message,assistant_message"} {
		ts, _ := a.OpenTranscript(context.Background(), byKey[key].Ref)
		evs, _ := ts.Poll(context.Background())
		var k []string
		for _, e := range evs {
			k = append(k, string(e.Kind))
		}
		if strings.Join(k, ",") != want {
			t.Errorf("%s events = %v", key, k)
		}
		g, _ := a.InferState(adapter.StateInput{Event: ts.(adapter.EventReporter).LastEvent(), Idle: time.Minute, Quiescence: 6 * time.Second})
		if g.State != protocol.StateWaitingInput {
			t.Errorf("%s state = %+v", key, g)
		}
	}

	started := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if m := a.MatchTranscript("/home/dev/app", started, nil); m != byKey["sess_01"].Ref {
		t.Errorf("join = %q", m)
	}
}

func TestDetectIgnoresTheIDE(t *testing.T) {
	a := New()
	if !a.Detect(adapter.Process{Argv: []string{"/usr/local/bin/kiro-cli", "chat"}}) {
		t.Error("missed kiro-cli")
	}
	if a.Detect(adapter.Process{Argv: []string{"kiro"}}) {
		t.Error("claimed the IDE launcher")
	}
}

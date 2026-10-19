package codex

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
)

func seedThreads(t *testing.T) (home, mainRollout string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	rolloutDir := filepath.Join(home, "sessions", "2026", "10", "18")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainRollout = filepath.Join(rolloutDir, "rollout-2026-10-18T22-40-11-01a15207-e400-7180-ad11-0bd4405a75e7.jsonl")
	header := `{"timestamp":"2026-10-19T02:40:11.809Z","type":"session_meta","payload":{"id":"01a15207-e400-7180-ad11-0bd4405a75e7","session_id":"01a15207-e400-7180-ad11-0bd4405a75e7","timestamp":"2026-10-19T02:40:11.705Z","cwd":"/Users/sam/dev/muxscale","cli_version":"0.163.0","source":"cli"}}` + "\n"
	if err := os.WriteFile(mainRollout, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	execRollout := filepath.Join(rolloutDir, "rollout-2026-10-18T23-50-00-01a15299-0000-7000-8000-000000000000.jsonl")
	if err := os.WriteFile(execRollout, []byte(strings.ReplaceAll(header, `"source":"cli"`, `"source":"exec"`)), 0o600); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`create table threads(id text primary key, rollout_path text, created_at integer,
		  updated_at integer, source text, model_provider text, cwd text, title text,
		  sandbox_policy text, approval_mode text, archived integer default 0,
		  cli_version text default '', model text, agent_role text, name text,
		  git_branch text)`,
		`create table thread_spawn_edges(parent_thread_id text, child_thread_id text primary key, status text)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	insert := `insert into threads(id,rollout_path,created_at,updated_at,source,model_provider,cwd,title,sandbox_policy,approval_mode,archived,cli_version,model,agent_role,name,git_branch) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	for _, row := range []struct {
		id, path, source, title, name, role string
		archived                            int
	}{
		{"01a15207-e400-7180-ad11-0bd4405a75e7", mainRollout, "cli", "Fix the flaky test", "focus-codex", "", 0},
		{"exec-run", execRollout, "exec", "Reply with exactly OK", "", "", 0},
		{"archived", mainRollout, "cli", "Old", "", "", 1},
		{"sub", mainRollout, "subagent", "Child", "", "review", 0},
	} {
		if _, err := db.Exec(insert, row.id, row.path, now, now, row.source, "openai",
			"/Users/sam/dev/muxscale", row.title, "workspace-write", "on-request",
			row.archived, "0.163.0", "gpt-5.5", row.role, row.name, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`insert into thread_spawn_edges values (?,?,?)`,
		"01a15207-e400-7180-ad11-0bd4405a75e7", "sub", "running"); err != nil {
		t.Fatal(err)
	}
	return home, mainRollout
}

func TestDiscoveryUsesTheThreadIndex(t *testing.T) {
	_, rollout := seedThreads(t)
	got, err := (&Adapter{}).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d threads: %+v", len(got), got)
	}
	var d, run adapter.Discovered
	for _, g := range got {
		if g.NaturalKey == "exec-run" {
			run = g
		} else {
			d = g
		}
	}
	// A `codex exec` run answered one prompt and exited; nobody can reply.
	if run.NaturalKey == "" || run.Live {
		t.Errorf("exec run = %+v, want discovered and not live", run)
	}
	if d.Ref != rollout || d.Title != "Fix the flaky test" || d.Name != "focus-codex" {
		t.Fatalf("discovered = %+v", d)
	}
	if d.Cwd != "/Users/sam/dev/muxscale" || d.Model != "gpt-5.5" {
		t.Errorf("metadata = %+v", d)
	}
	if !d.Live {
		t.Error("fresh thread reported not live")
	}
}

func TestTranscriptForProcessJoinsByStartTime(t *testing.T) {
	_, rollout := seedThreads(t)
	started := time.Date(2026, 10, 18, 22, 40, 11, 0, time.Local)
	p := adapter.Process{Cwd: "/Users/sam/dev/muxscale", Started: started}
	if got := (&Adapter{}).TranscriptForProcess(p); got != rollout {
		t.Fatalf("transcript = %q, want %q", got, rollout)
	}
	// A process two hours later is a different session, not this rollout.
	p.Started = started.Add(2 * time.Hour)
	if got := (&Adapter{}).TranscriptForProcess(p); got != "" {
		t.Fatalf("joined an unrelated rollout: %q", got)
	}
}

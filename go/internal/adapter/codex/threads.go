package codex

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	_ "modernc.org/sqlite"
)

// The thread index is Codex's own record of conversations, and it carries
// what a rollout does not: the human title, the person's rename, the working
// directory, the model and the git branch. Reading it avoids opening every
// rollout just to label a session.
//
// Schema suffix has changed before (state_5.sqlite here). The highest present
// file is used, and every query failure falls back to walking rollouts -- a
// Codex update must not make sessions disappear.

// Thread is one row of the index, the subset discovery needs.
type Thread struct {
	ID        string
	Rollout   string
	Cwd       string
	Title     string
	Name      string
	Model     string
	Version   string
	GitBranch string
	Source    string
	UpdatedAt time.Time
}

// ThreadsDBPath returns the newest ~/.codex/state_N.sqlite, or "".
func ThreadsDBPath() string {
	home, err := CodexHome()
	if err != nil {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	type cand struct {
		path string
		n    int
	}
	var cands []cand
	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".sqlite")
		n, err := strconv.Atoi(strings.TrimPrefix(base, "state_"))
		if err == nil {
			cands = append(cands, cand{m, n})
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].n > cands[j].n })
	return cands[0].path
}

// ReadThreads returns Codex's own thread list, newest first.
//
// Subagents are excluded twice: by source and by the spawn-edge table. A
// session Codex started for itself is activity inside a conversation, not a
// peer in the list.
func ReadThreads(limit int) ([]Thread, error) {
	path := ThreadsDBPath()
	if path == "" {
		return nil, fmt.Errorf("no Codex state database")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(1000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if limit <= 0 {
		limit = 200
	}
	rows, err := db.Query(`
SELECT id, rollout_path, cwd, coalesce(title,''), coalesce(name,''),
       coalesce(model,''), coalesce(cli_version,''), coalesce(git_branch,''),
       coalesce(source,''), updated_at
FROM threads t
WHERE archived = 0
  AND coalesce(agent_role,'') = ''
  AND coalesce(source,'') NOT LIKE '%subagent%'
  AND NOT EXISTS (SELECT 1 FROM thread_spawn_edges e WHERE e.child_thread_id = t.id)
ORDER BY updated_at DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Thread
	for rows.Next() {
		var t Thread
		var updated int64
		if err := rows.Scan(&t.ID, &t.Rollout, &t.Cwd, &t.Title, &t.Name,
			&t.Model, &t.Version, &t.GitBranch, &t.Source, &updated); err != nil {
			continue
		}
		if updated > 0 {
			t.UpdatedAt = time.Unix(updated, 0)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// threadsForCwd returns the index rows for one project.
func threadsForCwd(cwd string, limit int) ([]Thread, error) {
	all, err := ReadThreads(limit)
	if err != nil {
		return nil, err
	}
	want := filepath.Clean(cwd)
	var out []Thread
	for _, t := range all {
		if filepath.Clean(t.Cwd) == want {
			out = append(out, t)
		}
	}
	return out, nil
}

// rolloutStart parses the local start time Codex puts in a rollout filename:
// rollout-2026-10-18T22-40-11-<uuid>.jsonl.
func rolloutStart(path string) time.Time {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "rollout-") {
		return time.Time{}
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(base, "rollout-"), ".jsonl")
	if len(rest) < 19 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02T15-04-05", rest[:19], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// TranscriptForProcess joins a running Codex process to its rollout by the
// start time in the filename and the cwd in the thread index.
func (*Adapter) TranscriptForProcess(p adapter.Process) string {
	if p.Cwd == "" || p.Started.IsZero() {
		return ""
	}
	threads, err := threadsForCwd(p.Cwd, 200)
	if err != nil {
		return ""
	}
	const tolerance = 90 * time.Second
	var pick string
	var best time.Duration
	for _, t := range threads {
		if t.Rollout == "" {
			continue
		}
		started := rolloutStart(t.Rollout)
		if started.IsZero() {
			continue
		}
		d := absDuration(started.Sub(p.Started))
		if d <= tolerance && (pick == "" || d < best) {
			pick, best = t.Rollout, d
		}
	}
	return pick
}

// MatchTranscript picks a rollout when the process's own start time did not
// land on one -- typically a resumed session, whose file predates the process.
func (*Adapter) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	type cand struct {
		path    string
		started time.Time
		mod     time.Time
	}
	var cands []cand
	if threads, err := threadsForCwd(cwd, 200); err == nil {
		for _, t := range threads {
			if t.Rollout == "" || claimed[t.Rollout] {
				continue
			}
			info, err := os.Stat(t.Rollout)
			if err != nil {
				continue
			}
			cands = append(cands, cand{path: t.Rollout, started: rolloutStart(t.Rollout), mod: info.ModTime()})
		}
	}
	if len(cands) == 0 {
		// The index is unreadable or from a future schema. The rollouts are
		// still on disk; read their headers.
		paths, err := FindRollouts(200)
		if err != nil {
			return ""
		}
		for _, p := range paths {
			if claimed[p] {
				continue
			}
			gotCwd, _, err := RolloutCwd(p)
			if err != nil || filepath.Clean(gotCwd) != filepath.Clean(cwd) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			cands = append(cands, cand{path: p, started: rolloutStart(p), mod: info.ModTime()})
		}
	}
	if len(cands) == 0 {
		return ""
	}

	const tolerance = 90 * time.Second
	if !started.IsZero() {
		var pick string
		var best time.Duration
		for _, c := range cands {
			if c.started.IsZero() {
				continue
			}
			d := absDuration(c.started.Sub(started))
			if d <= tolerance && (pick == "" || d < best) {
				pick, best = c.path, d
			}
		}
		if pick != "" {
			return pick
		}
	}
	if len(cands) == 1 {
		return cands[0].path
	}
	if started.IsZero() {
		return ""
	}
	// Resumed: the file predates the process, so the newest file written
	// after the process began is the best available evidence.
	var pick string
	var pickMod time.Time
	for _, c := range cands {
		if c.mod.Before(started.Add(-5 * time.Second)) {
			continue
		}
		if c.mod.After(pickMod) {
			pick, pickMod = c.path, c.mod
		}
	}
	return pick
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// discoveredFromThreads turns index rows into adapter discoveries.
func discoveredFromThreads(limit int) ([]adapter.Discovered, bool) {
	threads, err := ReadThreads(limit)
	if err != nil || len(threads) == 0 {
		return nil, false
	}
	out := make([]adapter.Discovered, 0, len(threads))
	for _, t := range threads {
		if t.Rollout == "" {
			continue
		}
		if _, err := os.Stat(t.Rollout); err != nil {
			continue
		}
		live := time.Since(t.UpdatedAt) < liveWindow && t.Source != sourceExec
		out = append(out, adapter.Discovered{
			NaturalKey: t.ID,
			Cwd:        t.Cwd,
			Title:      t.Title,
			Name:       t.Name,
			Model:      t.Model,
			Ref:        t.Rollout,
			Live:       live,
		})
	}
	return out, true
}

var _ adapter.Locator = (*Adapter)(nil)

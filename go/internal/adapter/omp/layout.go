package omp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
)

// Layout is where a session-tree agent keeps its files.
//
// omp is a fork of pi, and the two still write the same session format: a
// JSONL tree of entries, one file per session, one directory per project. What
// differs is where: the agent directory, how a working directory becomes a
// project directory's name, and whether a writer lease marks a session live.
// The parsing, discovery and process-joining in this package are shared; the
// layout is the only thing the pi adapter supplies.
type Layout struct {
	// Agent names the agent in evidence strings ("omp transcript: ...").
	Agent string
	// AgentDir is the directory holding sessions/.
	AgentDir func() (string, error)
	// Bucket names the project directory a working directory's sessions go in.
	Bucket func(cwd string) (string, error)
	// Leases reports whether the agent holds a writer lease per session. omp
	// does; pi does not, and liveness falls back to how recently a file was
	// written.
	Leases bool
}

// OmpLayout is omp's: ~/.omp/agent, home-relative buckets, writer leases.
var OmpLayout = Layout{Agent: "omp", AgentDir: agentDir, Bucket: bucketName, Leases: true}

// recentWindow is how recently a lease-less session must have been written to
// count as live. Generous for the reason the Codex adapter gives: a session
// waiting on its person is quiet by definition.
const recentWindow = 12 * time.Hour

// SessionsRoot is the directory holding one bucket per project.
func (l Layout) SessionsRoot() (string, error) {
	d, err := l.AgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "sessions"), nil
}

// Live reports whether a session is live, and whether that is known at all.
func (l Layout) Live(path string) (bool, bool) {
	if !l.Leases {
		return false, false
	}
	return Live(path)
}

// Discover walks the sessions directory.
//
// Only direct children of a bucket are sessions. Subagent transcripts live in
// a directory named after their parent (<timestamp>_<id>/<name>.jsonl); they
// are activity inside a session, not peers of it.
func (l Layout) Discover() ([]adapter.Discovered, error) {
	root, err := l.SessionsRoot()
	if err != nil {
		return nil, err
	}
	buckets, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // not installed, or has never run.
		}
		return nil, err
	}

	// A copied session file keeps its id, so the same id can appear twice in a
	// bucket. One row per id, newest file wins: two rows for one conversation
	// would split its state and duplicate its transcript.
	best := map[string]adapter.Discovered{}
	bestMod := map[string]time.Time{}
	for _, b := range buckets {
		if !b.IsDir() {
			continue
		}
		dir := filepath.Join(root, b.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			id := SessionIDFromPath(path)
			if id == "" {
				continue // a subagent file placed beside its parent
			}
			h, ok := readHeader(path)
			if !ok {
				continue // not a session, or still being written
			}
			info, statErr := f.Info()
			mod := time.Time{}
			if statErr == nil {
				mod = info.ModTime()
			}
			if prev, ok := bestMod[id]; ok && !mod.After(prev) {
				continue
			}
			live := time.Since(mod) < recentWindow
			if held, known := l.Live(path); known {
				live = held
			}
			best[id] = adapter.Discovered{
				NaturalKey: id,
				Cwd:        h.Cwd,
				Title:      firstNonEmpty(h.Title, h.FirstPrompt),
				Name:       h.Name,
				Ref:        path,
				Live:       live,
			}
			bestMod[id] = mod
		}
	}

	out := make([]adapter.Discovered, 0, len(best))
	for _, d := range best {
		out = append(out, d)
	}
	// Deterministic order; the filesystem gives none.
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// MatchTranscript joins a process to a project's session files. Same rules as
// the Claude Code join: an exact start time wins, one unclaimed file is
// unambiguous, and a resumed session falls back to the file written after the
// process began. Never two equally plausible files.
func (l Layout) MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string {
	if cwd == "" {
		return ""
	}
	bucket, err := l.Bucket(cwd)
	if err != nil {
		return ""
	}
	root, err := l.SessionsRoot()
	if err != nil {
		return ""
	}
	dir := filepath.Join(root, bucket)
	files, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	type cand struct {
		path    string
		started time.Time
		mod     time.Time
	}
	var cands []cand
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		if claimed[path] || SessionIDFromPath(path) == "" {
			continue
		}
		h, ok := readHeader(path)
		if !ok {
			continue
		}
		if h.Cwd != "" && filepath.Clean(h.Cwd) != filepath.Clean(cwd) {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{path: path, started: h.Started, mod: info.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}

	// A fresh session writes its header moments after the process starts.
	const tolerance = 60 * time.Second
	if !started.IsZero() {
		for _, c := range cands {
			if !c.started.IsZero() && absDuration(c.started.Sub(started)) <= tolerance {
				return c.path
			}
		}
	}
	if len(cands) == 1 {
		return cands[0].path
	}
	if started.IsZero() {
		return ""
	}
	// Resumed: the session file predates the process, so the best available
	// signal is the newest file written after the process began.
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

// NewStream follows one session file of this format.
func NewStream(path string) adapter.TranscriptStream { return newStream(path) }

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

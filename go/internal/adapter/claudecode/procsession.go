package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// sessionsDir is where Claude Code records, for each running process, the
// session it is in: ~/.claude/sessions/<pid>.json.
func sessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "sessions"), nil
}

// procSlack is how far before a process's start its pid file may have been
// written and still be its own: ps reports start time to the second.
const procSlack = 5 * time.Second

// ProcessTranscript returns the transcript a running Claude Code process is
// writing, as that process itself recorded it, or "" if it has not.
//
// This is a fact where PickTranscript is a guess. The guess joins on start
// time, which a resumed session does not have -- its file began long before
// the process -- and falls back to the most recently written file, so two
// resumed sessions in one project both followed whichever wrote last. The
// file also follows a /resume or /clear inside a running process, which no
// join on start time could.
//
// A file older than the process is a previous holder of the pid's, and is
// ignored.
func ProcessTranscript(pid int, started time.Time, cwd string) string {
	if pid <= 0 || cwd == "" {
		return ""
	}
	dir, err := sessionsDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if !started.IsZero() && fi.ModTime().Before(started.Add(-procSlack)) {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var rec struct {
		PID       int    `json:"pid"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.PID != pid || rec.SessionID == "" {
		return ""
	}
	// The process may have moved; the transcript lives under the directory it
	// recorded, falling back to the one the scan saw.
	for _, d := range []string{rec.Cwd, cwd} {
		if d == "" {
			continue
		}
		proj, err := ProjectDir(d)
		if err != nil {
			continue
		}
		t := filepath.Join(proj, filepath.Base(rec.SessionID)+".jsonl")
		if _, err := os.Stat(t); err == nil {
			return t
		}
	}
	return ""
}

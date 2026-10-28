package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	name := filepath.Base(rec.SessionID) + ".jsonl"
	for _, d := range []string{rec.Cwd, cwd} {
		if d == "" {
			continue
		}
		proj, err := ProjectDir(d)
		if err != nil {
			continue
		}
		t := filepath.Join(proj, name)
		if _, err := os.Stat(t); err == nil {
			return t
		}
	}
	return transcriptAnywhere(name)
}

// transcriptAnywhere finds a session file in whichever project holds it.
//
// A conversation resumed from another directory goes on writing to the file
// it began in, under the project it started in, not the one the process now
// runs from. Looking only under the process's own directory found nothing:
// the process was a row with no conversation, and the conversation's row was
// ended because its old process was gone. Only reached when the record names
// a session and neither directory has it, so the walk is rare.
func transcriptAnywhere(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", name))
	best, bestMod := "", time.Time{}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && fi.ModTime().After(bestMod) {
			best, bestMod = m, fi.ModTime()
		}
	}
	return best
}

// liveConversations returns the conversations a running Claude Code process
// is in, from the record each one keeps in ~/.claude/sessions. known is false
// when the records cannot be read, so a permissions problem can never mark
// every session ended.
//
// This is how a conversation is known to have ended. Its transcript says
// nothing of the kind: one cut off by its terminal closing ends on an
// interrupted turn, which reads as waiting for you.
func liveConversations() (live map[string]bool, known bool) {
	dir, err := sessionsDir()
	if err != nil {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing too: a Claude Code from before it kept them may be running.
		return nil, false
	}
	live = map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil || pid <= 0 || !processAlive(pid) {
			continue // a record left by a process that did not clean up
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var rec struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(b, &rec) != nil || rec.PID != pid || rec.SessionID == "" {
			continue
		}
		live[rec.SessionID] = true
	}
	return live, true
}

// processAlive reports whether a process exists. EPERM means it does, owned
// by someone else.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Live reports whether a Claude Code process is in the conversation a
// transcript records (adapter.Liveness).
func (*Adapter) Live(ref string) (bool, bool) {
	live, known := liveConversations()
	if !known {
		return false, false
	}
	return live[strings.TrimSuffix(filepath.Base(ref), ".jsonl")], true
}

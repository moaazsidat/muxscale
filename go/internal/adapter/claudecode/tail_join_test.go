package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTranscript writes a minimal session file whose first event carries the
// given session id and time.
func writeTranscript(t *testing.T, path, sessionID string, first time.Time, mod time.Time) {
	t.Helper()
	line := `{"type":"user","sessionId":"` + sessionID + `","timestamp":"` +
		first.UTC().Format(time.RFC3339Nano) + `","cwd":"/tmp/proj"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func candidate(path string, started, mod time.Time) TranscriptCandidate {
	return TranscriptCandidate{Path: path, Started: started, Modified: mod}
}

// The bug this replaces: the newest file in the project directory won, so a
// second agent in the same project adopted the first one's conversation and
// the phone showed the wrong session -- silently, and only wrong some of the
// time, which is the worst kind.
func TestPickTranscriptPrefersTheStartTimeOverTheNewestFile(t *testing.T) {
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	old := candidate("/t/old.jsonl", start, start.Add(time.Hour))
	// Newer file, but it began at a different time: not this process's session.
	other := candidate("/t/other.jsonl", start.Add(-30*time.Minute), start.Add(2*time.Hour))

	got := PickTranscript([]TranscriptCandidate{other, old}, start, nil)
	if got != old.Path {
		t.Fatalf("picked %q, want %q -- the newest file won over the start-time match", got, old.Path)
	}
}

// A resumed session reuses a file whose first event predates the process, so
// there is no exact join. With one unclaimed candidate there is nothing to
// choose between and the answer is that file.
func TestPickTranscriptFallsBackToTheOnlyUnclaimedFile(t *testing.T) {
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	resumed := candidate("/t/resumed.jsonl", start.Add(-48*time.Hour), start.Add(time.Minute))

	if got := PickTranscript([]TranscriptCandidate{resumed}, start, nil); got != resumed.Path {
		t.Fatalf("picked %q, want the only unclaimed candidate %q", got, resumed.Path)
	}
}

// Two resumed sessions, no exact matches. The file written most recently after
// this process began is the discriminator -- the older process is the one whose
// transcript has been in use longer.
func TestPickTranscriptUsesTheClosestModifiedTimeWhenResumed(t *testing.T) {
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	longRunning := candidate("/t/long.jsonl", start.Add(-72*time.Hour), start.Add(30*time.Minute))
	justStarted := candidate("/t/just.jsonl", start.Add(-1*time.Hour), start.Add(5*time.Second))

	got := PickTranscript([]TranscriptCandidate{justStarted, longRunning}, start, nil)
	if got != justStarted.Path {
		t.Fatalf("picked %q, want %q -- the transcript closest to this process's start", got, justStarted.Path)
	}
}

// A transcript another live session already follows must never be handed to a
// second process: two sessions sharing one transcript is a worse failure than
// one session with none.
func TestPickTranscriptNeverReusesAClaimedFile(t *testing.T) {
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	taken := candidate("/t/taken.jsonl", start, start.Add(time.Minute))
	free := candidate("/t/free.jsonl", start.Add(-2*time.Hour), start.Add(time.Minute))

	claimed := map[string]bool{taken.Path: true}
	if got := PickTranscript([]TranscriptCandidate{taken, free}, start, claimed); got != free.Path {
		t.Fatalf("picked %q, want the unclaimed %q", got, free.Path)
	}
	if got := PickTranscript([]TranscriptCandidate{taken}, start, claimed); got != "" {
		t.Fatalf("picked %q, want no answer when every candidate is claimed", got)
	}
}

// A candidate whose first line is unreadable has no start time, so it cannot
// win the exact join. It is still a candidate for the fallbacks.
func TestPickTranscriptSkipsCandidatesWithNoStartTime(t *testing.T) {
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	broken := candidate("/t/broken.jsonl", time.Time{}, start.Add(time.Minute))
	good := candidate("/t/good.jsonl", start.Add(3*time.Second), start.Add(time.Minute))

	if got := PickTranscript([]TranscriptCandidate{broken, good}, start, nil); got != good.Path {
		t.Fatalf("picked %q, want the candidate with a real start time %q", got, good.Path)
	}
}

// Reading the candidates has to survive a file that is not JSON, a file with
// no events, and a missing project directory -- the discovery loop runs every
// few seconds against whatever is on disk.
func TestTranscriptCandidatesReadsTheFirstEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/tmp/proj"
	dir := filepath.Join(home, ".claude", "projects", ProjectSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	writeTranscript(t, filepath.Join(dir, "a.jsonl"), "sess-a", start, time.Time{})
	if err := os.WriteFile(filepath.Join(dir, "junk.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cands, err := TranscriptCandidates(cwd)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]TranscriptCandidate{}
	for _, c := range cands {
		byName[filepath.Base(c.Path)] = c
	}
	a, ok := byName["a.jsonl"]
	if !ok {
		t.Fatalf("a.jsonl is missing from %v", byName)
	}
	if a.SessionID != "sess-a" || !a.Started.Equal(start) {
		t.Errorf("a.jsonl = (%q, %s), want (sess-a, %s)", a.SessionID, a.Started, start)
	}
	// Unreadable is not fatal: the file is still listed, just without a start
	// time, so it can only ever win the fallbacks.
	if j, ok := byName["junk.jsonl"]; !ok || !j.Started.IsZero() {
		t.Errorf("junk.jsonl = %+v, want present with no start time", j)
	}

	// A project with no transcript directory yields no candidates and no error.
	if got, err := TranscriptCandidates("/tmp/nowhere-at-all"); err != nil || len(got) != 0 {
		t.Errorf("TranscriptCandidates on an unknown project = (%v, %v), want none", got, err)
	}
}

package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two resumed sessions in one project: neither file began when its process
// did, so the start-time join has nothing to go on and both processes had
// followed whichever file was written last. Each process's own record says
// which is its.
func TestProcessTranscriptIsTheFileTheProcessRecorded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/work/proj"
	proj := filepath.Join(home, ".claude", "projects", ProjectSlug(cwd))
	sessions := filepath.Join(home, ".claude", "sessions")
	for _, d := range []string{proj, sessions} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now().Add(-time.Hour)
	writeTranscript(t, filepath.Join(proj, "aaa.jsonl"), "aaa", started.Add(-48*time.Hour), time.Now())
	writeTranscript(t, filepath.Join(proj, "bbb.jsonl"), "bbb", started.Add(-24*time.Hour), started)
	write := func(pid, sid string, mod time.Time) {
		p := filepath.Join(sessions, pid+".json")
		body := `{"pid":` + pid + `,"sessionId":"` + sid + `","cwd":"` + cwd + `"}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	write("101", "bbb", time.Now())

	if got, want := ProcessTranscript(101, started, cwd), filepath.Join(proj, "bbb.jsonl"); got != want {
		t.Fatalf("got %q, want %q: the process's own record was not followed", got, want)
	}

	// A record left by an earlier process that had the same pid.
	write("102", "aaa", started.Add(-time.Hour))
	if got := ProcessTranscript(102, started, cwd); got != "" {
		t.Fatalf("got %q from a record older than the process", got)
	}

	// No record, or one naming a file that is not there.
	if got := ProcessTranscript(103, started, cwd); got != "" {
		t.Fatalf("got %q with no record", got)
	}
	write("104", "missing", time.Now())
	if got := ProcessTranscript(104, started, cwd); got != "" {
		t.Fatalf("got %q for a transcript that does not exist", got)
	}
}

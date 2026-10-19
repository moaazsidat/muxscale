package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/adapter/omp"
	"github.com/muxscale/muxscale/go/internal/store"
)

// Adapter discovery is the path that finds a session whose process the scan
// did not see. It must adopt a live one with its transcript, and must not
// resurrect the history the agent left behind.
func TestAdapterDiscoveryAdoptsOnlyLiveSessions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(root, "agent"))

	liveID := "01a151d4-146e-7571-a994-f30d585b56fa"
	deadID := "01a0db4a-11c7-7619-9d4e-5168ffa43653"
	dir := filepath.Join(root, "agent", "sessions", "-w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession := func(id string) string {
		path := filepath.Join(dir, "2026-10-18T21-00-00-000Z_"+id+".jsonl")
		body := `{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-10-18T21:00:00Z","cwd":"/w","title":"A task"}` + "\n" +
			`{"type":"message","id":"m1","parentId":null,"timestamp":"2026-10-18T21:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	livePath := writeSession(liveID)
	writeSession(deadID)

	// The writer lease: a held flock is what makes a session live.
	lockDir := filepath.Join(root, "run", "session-owners")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, liveID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := NewRegistry()
	adapters := adapter.NewRegistry()
	adapters.MustRegister(omp.New())
	m := NewManager(reg, adapters, st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	m.lastAdapterScan = time.Time{}
	m.discoverAdapters()

	id, ok := reg.IDFor("omp", liveID)
	if !ok {
		t.Fatal("live session was not adopted")
	}
	if _, ok := reg.IDFor("omp", deadID); ok {
		t.Fatal("historical session was adopted as a live row")
	}
	s, _ := reg.Get(id)
	if s.TranscriptPath != livePath || s.Cwd != "/w" || s.Title != "A task" {
		t.Fatalf("adopted session = %+v", s)
	}

	// The tailer is asynchronous; the transcript is the point of adopting it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if seq, err := st.MaxSeq(id); err == nil && seq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transcript was never read")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

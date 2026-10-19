package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateLockIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	unlock, err := acquireStateLock(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := acquireStateLock(dir); err == nil {
		t.Fatal("a second daemon acquired the same state directory")
	}

	unlock()
	again, err := acquireStateLock(dir)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	again()

	// The file stays behind on purpose; the flock is the lock.
	if _, err := os.Stat(filepath.Join(dir, "daemon.lock")); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

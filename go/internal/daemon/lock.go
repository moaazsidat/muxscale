package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireStateLock makes one daemon the owner of a state directory.
//
// Binding the loopback port is not enough on its own. A second daemon opens
// the same SQLite database, runs Restore -- marking every live session as
// detached and persisting it -- and only then tries to listen and fails. The
// first daemon is still running, so the user's live sessions vanish into the
// app's Detached section for no reason they can see. This lock is taken before
// the store is opened, so a duplicate exits without touching state.
//
// The lock file is deliberately left behind: unlinking it would let two
// processes lock different inodes. The flock, not the file's existence, is the
// lock.
func acquireStateLock(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another muxscale daemon is already using %s", stateDir)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

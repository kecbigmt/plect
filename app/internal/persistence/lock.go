package persistence

import (
	"fmt"
	"os"
	"syscall"
)

// withFileLock runs fn while holding an exclusive flock on a file next to
// the database (created if absent). It is not a substitute for the design's
// full migration access gate (a later slice); it exists only to close the
// two narrow races a brand-new database file exposes before that gate
// exists: two processes opening the same not-yet-existent file for the
// first time, and two processes migrating the same not-yet-populated file.
func withFileLock(lockPath string, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}

package persistence

import (
	"fmt"
	"os"
	"syscall"
)

// withFileLock runs fn while holding an exclusive flock on a file next to
// the database (created if absent). Its scope is narrow and interim: it
// covers only the two first-touch/migrate races a brand-new database file
// exposes — two processes opening the same not-yet-existent file for the
// first time (Open's ping), and two processes migrating the same
// not-yet-populated file (Migrate) — nothing else in this package takes it.
// It is not the design's coordination/access gate (the advisory lock files,
// diagnostics marker, and bounded wait-or-refuse around every normal
// operation), which is being implemented separately and will replace this
// helper once it lands.
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

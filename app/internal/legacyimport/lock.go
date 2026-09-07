package legacyimport

import (
	"fmt"
	"os"
	"syscall"
)

// checkNotLocked confirms a live writer does not hold path's flock. It takes
// the lock non-blockingly and releases it immediately, so a slow-but-live
// holder reads the same as a stuck one; the cutover procedure's own
// "stop every writer" prerequisite is what makes that acceptable here.
func checkNotLocked(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("%s is still held by a live writer; stop every plect process before importing", path)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return nil
}

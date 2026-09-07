package legacyimport

import (
	"fmt"
	"os"
	"syscall"
)

// checkNotLocked confirms path is not currently flock-held (a live writer
// still holding it), without copying or otherwise touching it — the access
// gate and this package's own database transactions replace every one of
// these locks after cutover. A path that does not exist is trivially
// unlocked. This never blocks: it takes the lock non-blockingly and
// immediately releases it, so a merely-slow holder that will release
// momentarily is indistinguishable from a genuinely stuck one — the cutover
// procedure's own "stop every writer" prerequisite is what makes that
// acceptable here.
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

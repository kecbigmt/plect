package persistence

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/flocktest"
)

// flockPath is shared by an exclusive holder (a migration) and a shared
// holder (a normal access), so it must open the descriptor writable: the
// Linux NFS client rejects LOCK_EX on an O_RDONLY descriptor with EBADF,
// even though local filesystems tolerate it. This test inspects the lock
// file descriptor's own open flags via /proc, so it catches the regression
// even on a local (non-NFS) test filesystem. Mirrors
// eventlog.TestFlockOpensLockFileWritable.
func TestFlockPathOpensLockFileWritable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lock fd flags are inspected via /proc, which is Linux-specific")
	}

	lockPath := filepath.Join(t.TempDir(), ".lock")
	unlock, err := flockPath(lockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	accMode, err := flocktest.AccessMode(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if accMode == os.O_RDONLY {
		t.Error("lock file opened O_RDONLY; exclusive lock (LOCK_EX) requires a writable descriptor on NFS")
	}
}

func TestAccessGate_AccessExclusiveBlocksAccessShared(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	unlockExclusive, err := gate.accessExclusive()
	if err != nil {
		t.Fatalf("accessExclusive: %v", err)
	}

	_, ok, err := tryFlockPath(gate.accessLockPath, syscall.LOCK_SH)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if ok {
		t.Error("a shared access probe succeeded while accessExclusive was held, want it blocked")
	}

	unlockExclusive()

	unlockShared, ok, err := tryFlockPath(gate.accessLockPath, syscall.LOCK_SH)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if !ok {
		t.Fatal("a shared access probe failed after accessExclusive was released, want it to succeed")
	}
	unlockShared()
}

func TestAccessGate_AccessSharedDoesNotBlockAnotherAccessShared(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	unlockFirst, err := gate.accessShared()
	if err != nil {
		t.Fatalf("accessShared: %v", err)
	}
	defer unlockFirst()

	unlockSecond, ok, err := tryFlockPath(gate.accessLockPath, syscall.LOCK_SH)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if !ok {
		t.Fatal("a second shared access probe was blocked by the first, want two shared holders to coexist")
	}
	unlockSecond()
}

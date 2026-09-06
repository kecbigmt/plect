package persistence

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

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

// flock grants a fresh shared request the instant nothing currently holds
// the lock exclusively, regardless of a queued exclusive waiter, so
// accessShared alone would let a new operation race a migrator that has
// already recorded intent by holding the coordination lock.
func TestAccessGate_EnterSharedWaitsForCoordinationLockBeforeTakingAccessShared(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	unlockCoord, ok, err := tryFlockPath(gate.coordinationLockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if !ok {
		t.Fatal("tryFlockPath did not acquire the uncontended coordination lock")
	}

	type outcome struct {
		unlock func()
		err    error
	}
	result := make(chan outcome, 1)
	go func() {
		unlock, err := gate.enterShared(context.Background())
		result <- outcome{unlock, err}
	}()

	select {
	case r := <-result:
		if r.unlock != nil {
			r.unlock()
		}
		t.Fatalf("enterShared returned (err=%v) while a migrator held the coordination lock (recorded intent); a new operation must wait, not race in via accessShared", r.err)
	case <-time.After(300 * time.Millisecond):
		// expected: still waiting behind the coordination lock
	}

	unlockCoord()

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("enterShared errored after the coordination lock cleared: %v", r.err)
		}
		r.unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("enterShared never proceeded after the coordination lock was released")
	}
}

// TestAccessGate_EnterSharedHoldsCoordinationLockUntilAccessSharedIsAcquired
// is the deterministic regression test for the handoff gap a release-then-
// acquire enterShared would have: it forces enterShared to block on
// accessShared (by holding accessExclusive externally) and, while it is
// stuck there, probes the coordination lock exclusively from outside. That
// probe must fail — if enterShared had already released its
// coordination-shared hold before accessShared succeeded, a migrator could
// acquire the coordination lock and record intent in exactly this window,
// while this operation is still on its way in.
func TestAccessGate_EnterSharedHoldsCoordinationLockUntilAccessSharedIsAcquired(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	unlockAccessExclusive, err := gate.accessExclusive()
	if err != nil {
		t.Fatalf("accessExclusive: %v", err)
	}

	type outcome struct {
		unlock func()
		err    error
	}
	result := make(chan outcome, 1)
	go func() {
		unlock, err := gate.enterShared(context.Background())
		result <- outcome{unlock, err}
	}()

	// Give enterShared time to acquire the coordination lock and reach (and
	// block on) accessShared.
	time.Sleep(200 * time.Millisecond)

	_, ok, err := tryFlockPath(gate.coordinationLockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if ok {
		t.Fatal("acquired the coordination lock exclusively while enterShared was still blocked on accessShared — the coordination-to-access handoff is not atomic")
	}

	unlockAccessExclusive()

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("enterShared: %v", r.err)
		}
		r.unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("enterShared never completed after accessExclusive was released")
	}
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

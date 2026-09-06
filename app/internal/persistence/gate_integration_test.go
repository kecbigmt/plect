//go:build integration

package persistence

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAccessGate_RefusesAfterTheFullMigrationWaitBound is the standing
// proof that a held coordination lock is waited out for the whole fixed
// migrationWait bound (30s per docs/design/sqlite-persistence.md) and then
// actually refuses, rather than waiting forever or refusing early. It is
// integration-tagged because migrationWait is a genuine fixed constant, not
// a configuration knob a fast unit test can shorten — see gate.go.
func TestAccessGate_RefusesAfterTheFullMigrationWaitBound(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	if err := writeMarker(gate.markerPath, migrationMarker{
		PID:       424242,
		Stage:     "migrating",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	unlock, ok, err := tryFlockPath(gate.coordinationLockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if !ok {
		t.Fatal("tryFlockPath did not acquire the uncontended coordination lock")
	}
	defer unlock()

	start := time.Now()
	err = gate.waitUntilNoMigrationInProgress(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitUntilNoMigrationInProgress unexpectedly succeeded while the coordination lock was held for the whole wait")
	}
	if elapsed < migrationWait {
		t.Errorf("waitUntilNoMigrationInProgress returned after %s, want it to wait out the full %s bound before refusing", elapsed, migrationWait)
	}
	if !strings.Contains(err.Error(), "424242") {
		t.Errorf("error = %q, want it to mention the live marker's pid for diagnosis", err)
	}
}

//go:build integration

package persistence

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Integration-tagged, not a fast unit test: migrationWait is a genuine
// fixed constant (see gate.go), not a configuration knob this test can
// shorten.
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
	_, err = gate.enterShared(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("enterShared unexpectedly succeeded while the coordination lock was held for the whole wait")
	}
	if elapsed < migrationWait {
		t.Errorf("enterShared returned after %s, want it to wait out the full %s bound before refusing", elapsed, migrationWait)
	}
	if !strings.Contains(err.Error(), "424242") {
		t.Errorf("error = %q, want it to mention the live marker's pid for diagnosis", err)
	}
}

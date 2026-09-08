package persistence

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
)

func TestImportSidecars_MovesRowsByIncarnationAndRetiresEventsTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := ensureCurrent(ctx, PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("ensure current: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: "live", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	liveID, err := db.EventStreamID(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(ctx, &domain.Session{Name: "gone", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	goneID, err := db.EventStreamID(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DestroySession(ctx, "gone", now); err != nil {
		t.Fatal(err)
	}

	eventsDir := filepath.Join(dir, "events", "live")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chain := `{"work\u0000review\u0000` + liveID + `":"cap|target"}`
	if err := os.WriteFile(filepath.Join(eventsDir, "chain_attempts.json"), []byte(chain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "events", "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events", "gone", "tombstone.json"), []byte(`{"ignored":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := `{"subscribe":{"live":["resource://live"]},"unsubscribe":{"gone":["resource://gone"]}}`
	if err := os.WriteFile(filepath.Join(dir, "pending_delivery.json"), []byte(pending), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := db.ImportSidecars(ctx, dir); err != nil {
		t.Fatalf("import sidecars: %v", err)
	}
	previous, won, err := db.SwapChainAttempt(ctx, "live", "work", "review", "cap|target")
	if err != nil || won || previous != "cap|target" {
		t.Fatalf("imported chain attempt = previous %q won %v err %v", previous, won, err)
	}
	destroyed, err := db.DestroyedSubscriptionRetries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(destroyed) != 1 || destroyed[0].SessionID != goneID || destroyed[0].Action != subscriptionRetryUnsubscribe {
		t.Fatalf("destroyed subscription retries = %+v, want gone unsubscribe", destroyed)
	}
	live, err := db.SubscriptionRetriesForLiveSession(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].SessionID != liveID || live[0].Action != subscriptionRetrySubscribe {
		t.Fatalf("live subscription retries = %+v, want live subscribe", live)
	}
	if _, err := os.Stat(filepath.Join(dir, "pending_delivery.json")); !os.IsNotExist(err) {
		t.Fatalf("pending sidecar still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "events")); !os.IsNotExist(err) {
		t.Fatalf("events tree still exists: %v", err)
	}
}

func TestImportSidecars_CleanDirectoryDoesNotWaitForExclusiveGate(t *testing.T) {
	dir := t.TempDir()
	db, err := ensureCurrent(context.Background(), PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("ensure current: %v", err)
	}
	defer db.Close()
	unlock, ok, err := tryFlockPath(db.gate.coordinationLockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("acquire coordination lock")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := db.ImportSidecars(ctx, dir); err != nil {
		t.Fatalf("clean import waited for the exclusive gate: %v", err)
	}
}

func TestImportSidecars_RejectsUnmappedSessionWithoutRemovingSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := ensureCurrent(ctx, PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("ensure current: %v", err)
	}
	defer db.Close()
	path := filepath.Join(dir, "pending_delivery.json")
	if err := os.WriteFile(path, []byte(`{"subscribe":{"missing":["resource://missing"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.ImportSidecars(ctx, dir); err == nil {
		t.Fatal("import sidecars unexpectedly accepted an unmapped session")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unmapped source was removed: %v", err)
	}
}

func TestImportSidecars_RejectsConflictingLegacyChainGenerations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := ensureCurrent(ctx, PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("ensure current: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: "live", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events", "live", "chain_attempts.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"work\u0000review\u0000old":"cap|old","work\u0000review\u0000new":"cap|new"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := db.ImportSidecars(ctx, dir); err == nil {
		t.Fatal("import sidecars unexpectedly accepted conflicting chain attempts")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("conflicting source was removed: %v", err)
	}
}

func TestEnsureCurrentImportsPostCutoverSidecars(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := PathIn(dir)
	db, err := ensureCurrent(ctx, path, migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("initial ensure current: %v", err)
	}
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: "live", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "pending_delivery.json")
	if err := os.WriteFile(path, []byte(`{"subscribe":{"live":["resource://live"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err = ensureCurrent(ctx, PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("ensure current with sidecar: %v", err)
	}
	defer db.Close()
	retries, err := db.SubscriptionRetriesForLiveSession(ctx, "live")
	if err != nil || len(retries) != 1 || retries[0].Action != subscriptionRetrySubscribe {
		t.Fatalf("subscription retries = %+v, err=%v", retries, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sidecar still exists: %v", err)
	}
}

func TestEnsureCurrentRetiresEventsLeftByDurableStorageCutover(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := ensureCurrent(ctx, PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("initial ensure current: %v", err)
	}
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: "live", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(dir, "events", "live", "log.jsonl"),
		filepath.Join(dir, "events", "live", ".gen"),
		filepath.Join(dir, "events", "live", ".cursor.dispatcher"),
		filepath.Join(dir, "events", "gone", "tombstone.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err = ensureCurrent(ctx, PathIn(dir), migrationsSourceFS(), false, false)
	if err != nil {
		t.Fatalf("ensure current after durable storage cutover: %v", err)
	}
	defer db.Close()
	if _, err := os.Stat(filepath.Join(dir, "events")); !os.IsNotExist(err) {
		t.Fatalf("retired events tree still exists: %v", err)
	}
}

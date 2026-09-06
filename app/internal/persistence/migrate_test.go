package persistence

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMigrate_SerializesConcurrentMigrationsOfAFreshDatabase proves Migrate
// is safe to call from several processes racing to create the same
// brand-new database: without lockMigration, each provider lists the
// ledger as empty before any of them commits, so more than one attempts
// the same CREATE TABLE and collides.
func TestMigrate_SerializesConcurrentMigrationsOfAFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	const n = 8

	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			db, err := Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			errs[i] = db.Migrate(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("migrate %d: %v", i, err)
		}
	}
}

func TestMigrate_RejectsDatabaseNewerThanEmbeddedMigrations(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Simulate a database a newer binary already migrated further: goose's
	// own ledger table is the applied-version authority, so planting a row
	// there is enough without any corresponding migration file.
	if _, err := db.write.ExecContext(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999999999, 1)"); err != nil {
		t.Fatalf("plant future version row: %v", err)
	}

	err := db.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate over a database with a newer applied version must fail, not proceed")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("error = %q, want it to name the newer-than-supported condition", err.Error())
	}
}

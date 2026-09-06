package webui

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// plect-web has no cobra parent chain of its own, unlike the `plect` CLI.
func TestNewLiveService_RefusesADatabaseNewerThanSupported(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	dbPath := filepath.Join(dataHome, "plect", "store.db")
	seed, err := persistence.EnsureCurrent(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("seed EnsureCurrent: %v", err)
	}
	// Fabricate a ledger entry ahead of every migration this binary embeds
	// by inserting directly into goose's own ledger table, the same
	// unsupported-newer-version scenario EnsureCurrent must refuse.
	// Goose migration versions are 14-digit timestamps (YYYYMMDDHHMMSS), so
	// this literal must exceed that magnitude to read as "newer", not just
	// as a different number.
	if err := seed.WithImmediateTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999999999, 1)")
		return err
	}); err != nil {
		t.Fatalf("seed a newer-than-supported ledger row: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = NewLiveService()
	if err == nil {
		t.Fatal("NewLiveService() over a database newer than this binary supports must fail")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("error = %q, want an actionable newer-than-supported message", err)
	}
}

func TestLiveServiceListSurfacesStateVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	writeDatabaseNewerThanBinarySupports(t, dir)

	rec := get(t, newLiveService(nil, state.NewStore(dir)), "/sessions")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "No sessions") {
		t.Fatalf("mismatched database rendered empty-state placeholder: %q", body)
	}
	if !strings.Contains(body, "newer than this binary supports") {
		t.Fatalf("body = %q, want the newer-than-supported condition", body)
	}
}

// writeDatabaseNewerThanBinarySupports creates and migrates a real database
// in dir, then plants a goose ledger row past anything this binary embeds,
// so any later open/read/write path must refuse it — the SQLite
// counterpart of the retired state.json version-mismatch fixture.
func writeDatabaseNewerThanBinarySupports(t *testing.T, dir string) {
	t.Helper()
	db, err := persistence.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999999999, 1)")
		return err
	}); err != nil {
		t.Fatalf("plant future version row: %v", err)
	}
}

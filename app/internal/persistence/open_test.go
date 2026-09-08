package persistence

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := PathIn(t.TempDir())
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// Both pools are asserted on directly (rather than only through the public
// API) because DB deliberately opens two separate connection pools against
// the same file (see DB's doc comment) — a regression that applies the
// production pragmas to only one of them would not show up if only one
// pool were ever queried.
func TestOpen_BothHandlesHaveProductionPragmas(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for name, handle := range map[string]*sql.DB{"read": db.read, "write": db.write} {
		t.Run(name, func(t *testing.T) {
			assertPragma(t, ctx, handle, "journal_mode", "wal")
			assertPragma(t, ctx, handle, "foreign_keys", "1")
			assertBusyTimeoutIsBoundedAndNonZero(t, ctx, handle)
		})
	}
}

func assertPragma(t *testing.T, ctx context.Context, handle *sql.DB, pragma, want string) {
	t.Helper()
	var got string
	if err := handle.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
	}
}

func assertBusyTimeoutIsBoundedAndNonZero(t *testing.T, ctx context.Context, handle *sql.DB) {
	t.Helper()
	var ms int
	if err := handle.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if ms <= 0 {
		t.Errorf("PRAGMA busy_timeout = %d, want a positive (bounded, non-zero) value", ms)
	}
}

func TestJournalMode_DefaultsToWALWhenUnset(t *testing.T) {
	t.Setenv(JournalModeEnvVar, "")
	mode, err := journalMode()
	if err != nil {
		t.Fatalf("journalMode: %v", err)
	}
	if mode != "WAL" {
		t.Errorf("journalMode() = %q, want %q", mode, "WAL")
	}
}

func TestJournalMode_AcceptsDeleteAndTruncateCaseInsensitively(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"DELETE", "DELETE"},
		{"delete", "DELETE"},
		{"TRUNCATE", "TRUNCATE"},
		{"truncate", "TRUNCATE"},
		{"wal", "WAL"},
	} {
		t.Run(tc.set, func(t *testing.T) {
			t.Setenv(JournalModeEnvVar, tc.set)
			mode, err := journalMode()
			if err != nil {
				t.Fatalf("journalMode: %v", err)
			}
			if mode != tc.want {
				t.Errorf("journalMode() with %s=%q = %q, want %q", JournalModeEnvVar, tc.set, mode, tc.want)
			}
		})
	}
}

func TestJournalMode_RejectsUnknownValueNamingTheValidSet(t *testing.T) {
	t.Setenv(JournalModeEnvVar, "MEMORY")
	_, err := journalMode()
	if err == nil {
		t.Fatal("journalMode() with an unsupported value must error")
	}
	for _, want := range []string{"WAL", "DELETE", "TRUNCATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("journalMode() error = %q, want it to name %q", err, want)
		}
	}
}

func TestOpen_RejectsUnknownJournalModeAtOpenTime(t *testing.T) {
	t.Setenv(JournalModeEnvVar, "MEMORY")
	path := PathIn(t.TempDir())
	if _, err := Open(path); err == nil {
		t.Fatal("Open must refuse an unsupported PLECT_SQLITE_JOURNAL_MODE rather than silently falling back")
	}
}

func TestOpen_JournalModeEnvVarSwitchesAnExistingWALDatabase(t *testing.T) {
	dir := t.TempDir()
	path := PathIn(dir)

	t.Setenv(JournalModeEnvVar, "")
	walDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open (WAL): %v", err)
	}
	if err := walDB.Close(); err != nil {
		t.Fatalf("Close (WAL): %v", err)
	}
	assertNoWALSidecars(t, path)

	t.Setenv(JournalModeEnvVar, "DELETE")
	deleteDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open (DELETE): %v", err)
	}
	defer deleteDB.Close()

	ctx := context.Background()
	for name, handle := range map[string]*sql.DB{"read": deleteDB.read, "write": deleteDB.write} {
		t.Run(name, func(t *testing.T) {
			assertPragma(t, ctx, handle, "journal_mode", "delete")
		})
	}
	assertNoWALSidecars(t, path)
}

func assertNoWALSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("%s%s exists, want no WAL-mode sidecar", path, suffix)
		} else if !os.IsNotExist(err) {
			t.Errorf("stat %s%s: %v", path, suffix, err)
		}
	}
}

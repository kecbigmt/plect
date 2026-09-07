package persistence

import (
	"context"
	"database/sql"
	"fmt"
)

// IntegrityCheck runs SQLite's own consistency check, returning an error
// naming every problem `PRAGMA integrity_check` reports (it can report more
// than one row). The one-time legacy importer runs this against its freshly
// built temporary database before promoting it.
func (db *DB) IntegrityCheck(ctx context.Context) error {
	var problems []string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, qerr := tx.QueryContext(ctx, "PRAGMA integrity_check")
		if qerr != nil {
			return fmt.Errorf("run integrity_check: %w", qerr)
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if serr := rows.Scan(&line); serr != nil {
				return fmt.Errorf("scan integrity_check row: %w", serr)
			}
			if line != "ok" {
				problems = append(problems, line)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("database integrity check failed: %v", problems)
	}
	return nil
}

// Checkpoint merges the WAL file's content back into the main database file
// and truncates it, so a caller about to move the database file alone (the
// one-time legacy importer's atomic promotion step) does not leave pending
// writes stranded in a sidecar `-wal` file the destination path never gets.
// It runs in autocommit (outside any explicit transaction): SQLite's own
// TRUNCATE checkpoint needs to reach exclusive access to the WAL itself,
// which a caller-held BEGIN IMMEDIATE would only get in its own way of.
func (db *DB) Checkpoint(ctx context.Context) error {
	unlock, err := db.enterNormalAccess(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := db.write.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

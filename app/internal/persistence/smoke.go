package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

// SmokeRecord is persistence's own translation of the generated
// sqlcgen.PersistenceSmoke row; callers outside this package never see
// generated database types, per the design's persistence-boundary rule.
type SmokeRecord struct {
	ID        int64
	Note      string
	CreatedAt time.Time
}

// InsertSmoke inserts one persistence_smoke row inside a BEGIN IMMEDIATE
// write transaction and returns it, exercising the schema.sql -> sqlc ->
// generated Go -> database/sql -> SQLite driver pipeline end to end.
func (db *DB) InsertSmoke(ctx context.Context, note string) (SmokeRecord, error) {
	var record SmokeRecord
	err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).InsertPersistenceSmoke(ctx, sqlcgen.InsertPersistenceSmokeParams{
			Note:      note,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return fmt.Errorf("insert persistence_smoke: %w", err)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
		if err != nil {
			return fmt.Errorf("parse persistence_smoke.created_at: %w", err)
		}
		record = SmokeRecord{ID: row.ID, Note: row.Note, CreatedAt: createdAt}
		return nil
	})
	if err != nil {
		return SmokeRecord{}, err
	}
	return record, nil
}

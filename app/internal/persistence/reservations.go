package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

// ReserveUpSlot locks the whole reservation table (via the write
// transaction) and every session, unlike UpdateSession's single session, so
// fn can weigh every session and reservation together. It never overwrites
// a still-live reservation for childName itself (domain.ErrUpAlreadyReserved).
func (db *DB) ReserveUpSlot(ctx context.Context, childName, parentName string, fn func(sessions map[string]*domain.Session, reservations map[string]domain.UpReservation) bool) (bool, error) {
	approved := false
	err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)

		rows, err := q.ListUpReservations(ctx)
		if err != nil {
			return fmt.Errorf("list up-slot reservations: %w", err)
		}
		live := make(map[string]domain.UpReservation, len(rows))
		for _, row := range rows {
			if !processAlive(int(row.Pid)) {
				if err := q.DeleteUpReservation(ctx, row.ChildSessionName); err != nil {
					return fmt.Errorf("delete dead reservation %q: %w", row.ChildSessionName, err)
				}
				continue
			}
			reservedAt, err := parseTime(row.ReservedAt)
			if err != nil {
				return fmt.Errorf("parse reservation %q reserved_at: %w", row.ChildSessionName, err)
			}
			live[row.ChildSessionName] = domain.UpReservation{Parent: row.ParentName, At: reservedAt, PID: int(row.Pid)}
		}
		if _, held := live[childName]; held {
			return domain.ErrUpAlreadyReserved
		}

		sessionRows, err := q.ListSessions(ctx)
		if err != nil {
			return fmt.Errorf("list sessions: %w", err)
		}
		sessions := make(map[string]*domain.Session, len(sessionRows))
		for _, row := range sessionRows {
			s, err := sessionFromRow(row)
			if err != nil {
				return err
			}
			if err := db.loadSessionExtras(ctx, tx, s); err != nil {
				return err
			}
			sessions[s.Name] = s
		}

		approved = fn(sessions, live)
		if !approved {
			return nil
		}

		if err := q.UpsertUpReservation(ctx, sqlcgen.UpsertUpReservationParams{
			ChildSessionName: childName,
			ParentName:       parentName,
			Pid:              int64(os.Getpid()),
			ReservedAt:       formatTime(time.Now()),
		}); err != nil {
			return fmt.Errorf("upsert up-slot reservation %q: %w", childName, err)
		}
		return nil
	})
	return approved, err
}

// ReleaseUpSlot drops childName's reservation. It is idempotent: releasing
// an unreserved child is a no-op.
func (db *DB) ReleaseUpSlot(ctx context.Context, childName string) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		if err := sqlcgen.New(tx).DeleteUpReservation(ctx, childName); err != nil {
			return fmt.Errorf("release up-slot reservation %q: %w", childName, err)
		}
		return nil
	})
}

// ListUpReservations returns every current up-slot reservation, keyed by
// child session name, without pruning dead ones — callers needing pruning
// semantics use ReserveUpSlot.
func (db *DB) ListUpReservations(ctx context.Context) (map[string]domain.UpReservation, error) {
	rows, err := sqlcgen.New(db.read).ListUpReservations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list up-slot reservations: %w", err)
	}
	result := make(map[string]domain.UpReservation, len(rows))
	for _, row := range rows {
		reservedAt, err := parseTime(row.ReservedAt)
		if err != nil {
			return nil, fmt.Errorf("parse reservation %q reserved_at: %w", row.ChildSessionName, err)
		}
		result[row.ChildSessionName] = domain.UpReservation{Parent: row.ParentName, At: reservedAt, PID: int(row.Pid)}
	}
	return result, nil
}

// processAlive treats PID reuse (a crashed holder's PID reassigned to an
// unrelated live process) as an accepted false negative — a stuck
// reservation stays recoverable via retry or Destroy either way.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM // EPERM: exists, just unsignalable
}

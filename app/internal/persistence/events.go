package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	"github.com/kecbigmt/plecture/contracts/event"
)

// AppendEvent errors on a name with no live session rather than starting one.
func (db *DB) AppendEvent(ctx context.Context, ev event.Event) (sequence int64, err error) {
	err = db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, err := currentSessionID(ctx, q, ev.SessionName)
		if err != nil {
			return err
		}

		next, err := q.NextEventSequence(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("next sequence for %q: %w", ev.SessionName, err)
		}

		metadataJSON, err := marshalEventMetadata(ev.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata for event %q: %w", ev.ID, err)
		}

		if err := q.InsertEvent(ctx, sqlcgen.InsertEventParams{
			ID:           ev.ID,
			SessionID:    sessionID,
			Sequence:     next,
			Time:         formatTime(ev.Time),
			Type:         ev.Type,
			Source:       ev.Source,
			Direction:    string(ev.Direction),
			Summary:      ev.Summary,
			Body:         ev.Body,
			MetadataJson: metadataJSON,
		}); err != nil {
			return fmt.Errorf("insert event %q: %w", ev.ID, err)
		}
		sequence = next
		return nil
	})
	return sequence, err
}

// EventStreamID is session's live row id, event.Cursor's v2 stream_id.
func (db *DB) EventStreamID(ctx context.Context, session string) (string, error) {
	var id string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		got, err := sqlcgen.New(tx).SessionIDByLiveName(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get session id for %q: %w", session, err)
		}
		id = got
		return nil
	})
	return id, err
}

func (db *DB) EventStreamSessions(ctx context.Context) ([]string, error) {
	var names []string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListEverSessionNames(ctx)
		if err != nil {
			return fmt.Errorf("list session names: %w", err)
		}
		names = rows
		return nil
	})
	return names, err
}

// ReadSessionNames is EventStreamSessions' own query (every session name
// ever, live or destroyed), run through a bare read-only connection instead
// of Open's access gate -- for a caller that wants to preview a database
// without migrating it or creating gate-lock sidecars next to it. SQLite
// itself may still create or update path's own -wal/-shm as an ordinary
// side effect of reading a WAL-mode database; that is the engine's doing,
// not a write this function performs. It requests no journal mode on this
// connection: a read-only connection can't change the on-disk mode, and
// go-sqlite3's _journal_mode DSN parameter fails outright (not a no-op)
// when the requested mode differs from what's on disk. It still validates
// JournalModeEnvVar, so an unsupported value fails the same way Open would.
func ReadSessionNames(ctx context.Context, path string) ([]string, error) {
	if _, err := journalMode(); err != nil {
		return nil, err
	}
	raw, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d", path, busyTimeoutMillis))
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	defer raw.Close()
	names, err := sqlcgen.New(raw).ListEverSessionNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("list session names: %w", err)
	}
	return names, nil
}

func (db *DB) EventStreamIDsBySession(ctx context.Context, session string) ([]string, error) {
	var ids []string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListSessionIDsByName(ctx, session)
		if err != nil {
			return fmt.Errorf("list session ids for %q: %w", session, err)
		}
		ids = rows
		return nil
	})
	return ids, err
}

// ListEventsFrom returns session's events at or after `since`, ascending,
// with their sequences (parallel slices). No live row returns empty.
func (db *DB) ListEventsFrom(ctx context.Context, session string, since int64) ([]event.Event, []int64, error) {
	evs, seqs, _, err := db.listCurrentEventsFrom(ctx, session, since)
	return evs, seqs, err
}

// ListCurrentEventsFrom is ListEventsFrom plus the resolved incarnation id.
func (db *DB) ListCurrentEventsFrom(ctx context.Context, session string, since int64) (evs []event.Event, seqs []int64, sessionID string, err error) {
	return db.listCurrentEventsFrom(ctx, session, since)
}

func (db *DB) listCurrentEventsFrom(ctx context.Context, session string, since int64) (evs []event.Event, seqs []int64, sessionID string, err error) {
	err = db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		id, gerr := q.SessionIDByLiveName(ctx, session)
		if gerr != nil {
			if errors.Is(gerr, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get session id for %q: %w", session, gerr)
		}
		sessionID = id
		rows, lerr := q.ListEventsFromBySession(ctx, sqlcgen.ListEventsFromBySessionParams{
			SessionID: sessionID,
			Sequence:  since,
		})
		if lerr != nil {
			return fmt.Errorf("list events for %q from %d: %w", session, since, lerr)
		}
		for _, row := range rows {
			ev, eerr := eventFromRow(row, session)
			if eerr != nil {
				return eerr
			}
			evs = append(evs, ev)
			seqs = append(seqs, row.Sequence)
		}
		return nil
	})
	return evs, seqs, sessionID, err
}

// ListEventsFromStreamID bypasses name resolution to reach a superseded incarnation.
func (db *DB) ListEventsFromStreamID(ctx context.Context, sessionID, session string, since int64) ([]event.Event, []int64, error) {
	var evs []event.Event
	var seqs []int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		// sessionID rides in from a caller-controlled resume token, so its
		// ownership is checked rather than trusted.
		owner, oerr := eventStreamSessionName(ctx, q, sessionID)
		if oerr != nil {
			return oerr
		}
		if owner != session {
			return fmt.Errorf("session id %q does not belong to session %q", sessionID, session)
		}
		rows, err := q.ListEventsFromBySession(ctx, sqlcgen.ListEventsFromBySessionParams{
			SessionID: sessionID,
			Sequence:  since,
		})
		if err != nil {
			return fmt.Errorf("list events for session id %q from %d: %w", sessionID, since, err)
		}
		for _, row := range rows {
			ev, err := eventFromRow(row, session)
			if err != nil {
				return err
			}
			evs = append(evs, ev)
			seqs = append(seqs, row.Sequence)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return evs, seqs, nil
}

func (db *DB) EventStreamSessionName(ctx context.Context, sessionID string) (string, error) {
	var owner string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		var qerr error
		owner, qerr = eventStreamSessionName(ctx, sqlcgen.New(tx), sessionID)
		return qerr
	})
	return owner, err
}

func eventStreamSessionName(ctx context.Context, q *sqlcgen.Queries, sessionID string) (string, error) {
	owner, err := q.SessionNameByID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get owner of session id %q: %w", sessionID, err)
	}
	return owner, nil
}

// HasEventCursor distinguishes "never committed" from a committed 0.
func (db *DB) HasEventCursor(ctx context.Context, session, cursorName string) (bool, error) {
	var has bool
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, err := q.SessionIDByLiveName(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get session id for %q: %w", session, err)
		}
		count, err := q.HasEventCursor(ctx, sqlcgen.HasEventCursorParams{
			SessionID: sessionID,
			Kind:      cursorName,
		})
		if err != nil {
			return fmt.Errorf("check cursor %q/%q: %w", session, cursorName, err)
		}
		has = count > 0
		return nil
	})
	return has, err
}

func (db *DB) EventCursor(ctx context.Context, session, cursorName string) (int64, error) {
	var pos int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, err := q.SessionIDByLiveName(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get session id for %q: %w", session, err)
		}
		p, err := q.GetEventCursor(ctx, sqlcgen.GetEventCursorParams{
			SessionID: sessionID,
			Kind:      cursorName,
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get cursor %q/%q: %w", session, cursorName, err)
		}
		pos = p
		return nil
	})
	return pos, err
}

// SetEventCursor requires session's row to already exist, same as AppendEvent.
func (db *DB) SetEventCursor(ctx context.Context, session, cursorName string, next int64) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, err := currentSessionID(ctx, q, session)
		if err != nil {
			return err
		}
		if err := q.UpsertEventCursor(ctx, sqlcgen.UpsertEventCursorParams{
			SessionID:    sessionID,
			Kind:         cursorName,
			NextSequence: next,
		}); err != nil {
			return fmt.Errorf("set cursor %q/%q: %w", session, cursorName, err)
		}
		return nil
	})
}

func (db *DB) LatestSessionEventByType(ctx context.Context, session, eventType string) (ev event.Event, ok bool, err error) {
	err = db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, gerr := q.SessionIDByLiveName(ctx, session)
		if gerr != nil {
			if errors.Is(gerr, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get session id for %q: %w", session, gerr)
		}
		row, lerr := q.LatestEventByType(ctx, sqlcgen.LatestEventByTypeParams{SessionID: sessionID, Type: eventType})
		if lerr != nil {
			if errors.Is(lerr, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get latest %q event for %q: %w", eventType, session, lerr)
		}
		parsed, perr := eventFromRow(sqlcgen.ListEventsFromBySessionRow(row), session)
		if perr != nil {
			return perr
		}
		ev = parsed
		ok = true
		return nil
	})
	return ev, ok, err
}

// currentSessionID errors on a missing session rather than silently minting one.
func currentSessionID(ctx context.Context, q *sqlcgen.Queries, session string) (string, error) {
	id, err := q.SessionIDByLiveName(ctx, session)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no session named %q", session)
		}
		return "", fmt.Errorf("get session id for %q: %w", session, err)
	}
	return id, nil
}

func marshalEventMetadata(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalEventMetadata(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// eventFromRow's session is caller-supplied: events no longer carries it.
func eventFromRow(row sqlcgen.ListEventsFromBySessionRow, session string) (event.Event, error) {
	t, err := parseTime(row.Time)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q time: %w", row.ID, err)
	}
	metadata, err := unmarshalEventMetadata(row.MetadataJson)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q metadata: %w", row.ID, err)
	}
	return event.Event{
		ID:          row.ID,
		SessionName: session,
		Time:        t,
		Type:        row.Type,
		Source:      row.Source,
		Direction:   event.Direction(row.Direction),
		Summary:     row.Summary,
		Body:        row.Body,
		Metadata:    metadata,
	}, nil
}

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

// AppendEvent inserts one fully-formed event (id, time, and every other
// field already assigned by the caller) into its session's live row in one
// write transaction. It returns the assigned sequence, a positive,
// per-session append position; ev.ID remains the event's own global dedup
// identity, unrelated to this number. A name with no live session (never
// created, a typo, or a session already destroyed) is a caller error, not
// silently started here — a session row is minted only by session
// creation itself (see writeSessionTx), since a row now is one incarnation.
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

// EventStreamID returns session's live row's id, or "" if it has none (never
// created, or already destroyed). event.Cursor's v2 stream_id is this
// value: a session create mints a new id, so a cursor issued for a
// since-destroyed and recreated incarnation fails validation against the
// current one.
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

// EventStreamSessions returns the names of every session that has ever
// existed, live or destroyed, sorted for deterministic iteration.
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

// EventStreamIDsBySession returns every incarnation's id for a name, oldest
// first (live or destroyed), so a caller draining a superseded incarnation
// can find the very next one even when more than one rotation happened
// since it last read.
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

// ListEventsFrom returns every event of session's live incarnation at or
// after sequence `since` (inclusive), in ascending sequence order, alongside
// each event's own sequence (parallel slices, index-aligned). A session
// with no live row returns empty, not an error: a read has nothing to
// reject a missing session against, unlike an append or a cursor commit.
func (db *DB) ListEventsFrom(ctx context.Context, session string, since int64) ([]event.Event, []int64, error) {
	evs, seqs, _, err := db.listCurrentEventsFrom(ctx, session, since)
	return evs, seqs, err
}

// ListCurrentEventsFrom is ListEventsFrom's counterpart that also returns the
// current incarnation's id, resolved atomically with the rows in the same
// read transaction rather than a separate, racing id-then-List.
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

// ListEventsFromStreamID returns every event of the given session id at or
// after sequence `since`, bypassing name resolution — the only way to reach
// a superseded incarnation's rows once a recreate has a newer one live.
// session names the returned events, since events itself does not.
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

// EventStreamSessionName returns the session name that owns sessionID, or ""
// if no session has that id — for validating a resume token before ever
// trusting it, independent of reading that incarnation's rows.
func (db *DB) EventStreamSessionName(ctx context.Context, sessionID string) (string, error) {
	var owner string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		var qerr error
		owner, qerr = eventStreamSessionName(ctx, sqlcgen.New(tx), sessionID)
		return qerr
	})
	return owner, err
}

// eventStreamSessionName is EventStreamSessionName's transaction-scoped
// primitive, shared with ListEventsFromStreamID's own ownership check.
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

// HasEventCursor reports whether a consumer has ever committed an offset
// for session's live incarnation, distinguishing "never started" from a
// committed position of 0. A session with no live row reports false, not
// an error, matching the "never started" case it cannot distinguish from.
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

// EventCursor returns cursorName's committed next-sequence position for
// session's live incarnation (0 if never committed, including when the
// session has no live row).
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

// SetEventCursor durably records cursorName's next-sequence position for
// session's live incarnation (a cursor may be seeded before that
// incarnation's first event, but the session row itself must already
// exist — created at session creation, same as for AppendEvent).
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

// LatestSessionEventByType returns the most recent event of eventType for
// session's live incarnation, and whether one exists — the status-message
// reader's primitive (see events_session_id_type_sequence_idx).
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

// currentSessionID resolves session's live row's id for a write that
// requires one to exist already (an append, or a cursor commit): unlike a
// read, it has nothing else to fall back to, so a missing session is an
// error naming it, not a silent mint — session creation is the only place
// that starts a new incarnation.
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

// eventFromRow decodes an events row into a domain event.Event. session is
// supplied by the caller (already resolved to find the row's incarnation)
// rather than read from a column, since events no longer carries it.
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

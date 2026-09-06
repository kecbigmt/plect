package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	"github.com/kecbigmt/plecture/contracts/event"
)

// CreateEventStream mints a new event_streams row for session — a new
// incarnation's log — and returns its id. It always inserts, never looks up
// first: session_name is not unique, so a name with an existing (now
// superseded) row still gets a fresh one, becoming the new current
// incarnation that GetEventStreamIDBySession resolves to.
func (db *DB) CreateEventStream(ctx context.Context, session string) (string, error) {
	var id string
	err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		id = newULID()
		if err := q.InsertEventStream(ctx, sqlcgen.InsertEventStreamParams{
			ID:          id,
			SessionName: session,
			CreatedAt:   formatTime(time.Now()),
		}); err != nil {
			return fmt.Errorf("create event stream for %q: %w", session, err)
		}
		return nil
	})
	return id, err
}

// AppendEvent inserts one fully-formed event (id, time, and every other
// field already assigned by the caller) into its session's current stream
// in one write transaction. It returns the assigned sequence, a positive,
// per-stream append position; ev.ID remains the event's own global dedup
// identity, unrelated to this number. A session with no current stream
// (never created, or an identifier typo) is a caller error, not silently
// started here — every stream begins at session creation.
func (db *DB) AppendEvent(ctx context.Context, ev event.Event) (sequence int64, err error) {
	err = db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := currentEventStreamID(ctx, q, ev.SessionName)
		if err != nil {
			return err
		}

		next, err := q.NextEventSequence(ctx, streamID)
		if err != nil {
			return fmt.Errorf("next sequence for %q: %w", ev.SessionName, err)
		}

		metadataJSON, err := marshalEventMetadata(ev.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata for event %q: %w", ev.ID, err)
		}

		if err := q.InsertEvent(ctx, sqlcgen.InsertEventParams{
			ID:           ev.ID,
			StreamID:     streamID,
			Sequence:     next,
			Time:         formatTime(ev.Time),
			Type:         ev.Type,
			Source:       ev.Source,
			Direction:    string(ev.Direction),
			Summary:      ev.Summary,
			Body:         ev.Body,
			MetadataJson: metadataJSON,
			DeliveryMode: string(ev.DeliveryMode),
		}); err != nil {
			return fmt.Errorf("insert event %q: %w", ev.ID, err)
		}
		sequence = next
		return nil
	})
	return sequence, err
}

// EventStreamID returns session's current incarnation's stream id, or "" if
// it has never had one created. event.Cursor's v2 stream_id is this value:
// a session create mints a new id, so a cursor issued for a since-destroyed
// and recreated incarnation fails validation against the current one.
func (db *DB) EventStreamID(ctx context.Context, session string) (string, error) {
	var id string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		got, err := sqlcgen.New(tx).GetEventStreamIDBySession(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get event stream id for %q: %w", session, err)
		}
		id = got
		return nil
	})
	return id, err
}

// EventStreamSessions returns the distinct names of every session with at
// least one event stream, sorted for deterministic iteration.
func (db *DB) EventStreamSessions(ctx context.Context) ([]string, error) {
	var names []string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListEventStreamSessions(ctx)
		if err != nil {
			return fmt.Errorf("list event stream sessions: %w", err)
		}
		names = rows
		return nil
	})
	return names, err
}

// EventStreamIDsBySession returns every incarnation's stream id for session, oldest first, so a caller draining a superseded stream can find the very next incarnation even when more than one rotation happened since it last read.
func (db *DB) EventStreamIDsBySession(ctx context.Context, session string) ([]string, error) {
	var ids []string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListEventStreamIDsBySession(ctx, session)
		if err != nil {
			return fmt.Errorf("list event stream ids for %q: %w", session, err)
		}
		ids = rows
		return nil
	})
	return ids, err
}

// ListEventsFrom returns every event of session's current stream at or
// after sequence `since` (inclusive), in ascending sequence order, alongside
// each event's own sequence (parallel slices, index-aligned). A session
// with no current stream returns empty, not an error: a read has nothing to
// reject a missing stream against, unlike an append or a cursor commit.
func (db *DB) ListEventsFrom(ctx context.Context, session string, since int64) ([]event.Event, []int64, error) {
	evs, seqs, _, err := db.listCurrentEventsFrom(ctx, session, since)
	return evs, seqs, err
}

// ListCurrentEventsFrom is ListEventsFrom's counterpart that also returns the current stream id, resolved atomically with the rows in the same read transaction rather than a separate, racing StreamID-then-List.
func (db *DB) ListCurrentEventsFrom(ctx context.Context, session string, since int64) (evs []event.Event, seqs []int64, streamID string, err error) {
	return db.listCurrentEventsFrom(ctx, session, since)
}

func (db *DB) listCurrentEventsFrom(ctx context.Context, session string, since int64) (evs []event.Event, seqs []int64, streamID string, err error) {
	err = db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		id, gerr := q.GetEventStreamIDBySession(ctx, session)
		if gerr != nil {
			if errors.Is(gerr, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get event stream id for %q: %w", session, gerr)
		}
		streamID = id
		rows, lerr := q.ListEventsFromByStream(ctx, sqlcgen.ListEventsFromByStreamParams{
			StreamID: streamID,
			Sequence: since,
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
	return evs, seqs, streamID, err
}

// ListEventsFromStreamID returns every event of the given stream id at or
// after sequence `since`, bypassing session-name resolution — the only way
// to reach a superseded stream's rows once a recreate has a newer one
// current. session names the returned events, since events itself does not.
func (db *DB) ListEventsFromStreamID(ctx context.Context, streamID, session string, since int64) ([]event.Event, []int64, error) {
	var evs []event.Event
	var seqs []int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		// streamID rides in from a caller-controlled resume token, so its ownership is checked rather than trusted.
		owner, oerr := eventStreamSessionName(ctx, q, streamID)
		if oerr != nil {
			return oerr
		}
		if owner != session {
			return fmt.Errorf("stream %q does not belong to session %q", streamID, session)
		}
		rows, err := q.ListEventsFromByStream(ctx, sqlcgen.ListEventsFromByStreamParams{
			StreamID: streamID,
			Sequence: since,
		})
		if err != nil {
			return fmt.Errorf("list events for stream %q from %d: %w", streamID, since, err)
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

// EventStreamSessionName returns the session that owns streamID, or "" if no stream has that id — for validating a resume token before trusting it, independent of reading that stream's rows.
func (db *DB) EventStreamSessionName(ctx context.Context, streamID string) (string, error) {
	var owner string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		var qerr error
		owner, qerr = eventStreamSessionName(ctx, sqlcgen.New(tx), streamID)
		return qerr
	})
	return owner, err
}

// eventStreamSessionName is EventStreamSessionName's transaction-scoped
// primitive, shared with ListEventsFromStreamID's own ownership check.
func eventStreamSessionName(ctx context.Context, q *sqlcgen.Queries, streamID string) (string, error) {
	owner, err := q.GetEventStreamSessionName(ctx, streamID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get owner of stream %q: %w", streamID, err)
	}
	return owner, nil
}

// HasEventCursor reports whether cursorName has ever been committed for
// session's current stream, distinguishing "never started" from a
// committed position of 0. A session with no current stream reports false,
// not an error, matching the "never started" case it cannot distinguish
// from.
func (db *DB) HasEventCursor(ctx context.Context, session, cursorName string) (bool, error) {
	var has bool
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := q.GetEventStreamIDBySession(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get event stream id for %q: %w", session, err)
		}
		count, err := q.HasEventCursor(ctx, sqlcgen.HasEventCursorParams{
			StreamID: streamID,
			Kind:     cursorName,
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
// session's current stream (0 if never committed, including when the
// session has no current stream).
func (db *DB) EventCursor(ctx context.Context, session, cursorName string) (int64, error) {
	var pos int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := q.GetEventStreamIDBySession(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get event stream id for %q: %w", session, err)
		}
		p, err := q.GetEventCursor(ctx, sqlcgen.GetEventCursorParams{
			StreamID: streamID,
			Kind:     cursorName,
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
// session's current stream (a cursor may be seeded before that stream's
// first event, but the stream itself must already exist — created at
// session creation, same as for AppendEvent).
func (db *DB) SetEventCursor(ctx context.Context, session, cursorName string, next int64) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := currentEventStreamID(ctx, q, session)
		if err != nil {
			return err
		}
		if err := q.UpsertEventCursor(ctx, sqlcgen.UpsertEventCursorParams{
			StreamID:     streamID,
			Kind:         cursorName,
			NextSequence: next,
		}); err != nil {
			return fmt.Errorf("set cursor %q/%q: %w", session, cursorName, err)
		}
		return nil
	})
}

// currentEventStreamID resolves session's current incarnation's stream id
// for a write that requires one to exist already (an append, or a cursor
// commit): unlike a read, it has nothing else to fall back to, so a missing
// stream is an error naming the session, not a silent mint — CreateEventStream
// is the only place that starts one.
func currentEventStreamID(ctx context.Context, q *sqlcgen.Queries, session string) (string, error) {
	id, err := q.GetEventStreamIDBySession(ctx, session)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("no current event stream for session %q", session)
		}
		return "", fmt.Errorf("get event stream id for %q: %w", session, err)
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
// supplied by the caller (already resolved to find the row's stream)
// rather than read from a column, since events no longer carries it.
func eventFromRow(row sqlcgen.ListEventsFromByStreamRow, session string) (event.Event, error) {
	t, err := parseTime(row.Time)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q time: %w", row.ID, err)
	}
	metadata, err := unmarshalEventMetadata(row.MetadataJson)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q metadata: %w", row.ID, err)
	}
	return event.Event{
		ID:           row.ID,
		SessionName:  session,
		Time:         t,
		Type:         row.Type,
		Source:       row.Source,
		Direction:    event.Direction(row.Direction),
		Summary:      row.Summary,
		Body:         row.Body,
		Metadata:     metadata,
		DeliveryMode: event.DeliveryMode(row.DeliveryMode),
	}, nil
}

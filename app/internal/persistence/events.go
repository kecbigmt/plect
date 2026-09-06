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
// field already assigned by the caller) into its session's stream in one
// write transaction, creating the stream first if this is the session's
// first touch. It returns the assigned sequence, a positive, per-stream
// append position; ev.ID remains the event's own global dedup identity,
// unrelated to this number.
func (db *DB) AppendEvent(ctx context.Context, ev event.Event) (sequence int64, err error) {
	err = db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := ensureEventStream(ctx, q, ev.SessionName)
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
			EventID:      ev.ID,
			StreamID:     streamID,
			Sequence:     next,
			RecordedAt:   formatTime(ev.Time),
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

// EventStreamID returns a session's event-stream id, or "" if the stream
// has never been touched (no event appended, no consumer position ever
// committed). It is minted once, at stream creation, and never changes for
// that session name — event.Cursor's v2 stream_id is this value.
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

// EventStreamSessions returns the names of every session whose event
// stream has been touched (an event appended, or a consumer position ever
// committed), sorted for deterministic iteration.
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

// ListEventsFrom returns every event of session at or after sequence
// `since` (inclusive), in ascending sequence order, alongside each event's
// own sequence (parallel slices, index-aligned). A session whose stream
// has never been touched returns empty, not an error.
func (db *DB) ListEventsFrom(ctx context.Context, session string, since int64) ([]event.Event, []int64, error) {
	var evs []event.Event
	var seqs []int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := q.GetEventStreamIDBySession(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get event stream id for %q: %w", session, err)
		}
		rows, err := q.ListEventsFromByStream(ctx, sqlcgen.ListEventsFromByStreamParams{
			StreamID: streamID,
			Sequence: since,
		})
		if err != nil {
			return fmt.Errorf("list events for %q from %d: %w", session, since, err)
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

// HasEventCursor reports whether cursorName has ever been committed for
// session, distinguishing "never started" from a committed position of 0.
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
			StreamID:   streamID,
			CursorName: cursorName,
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
// session (0 if never committed).
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
			StreamID:   streamID,
			CursorName: cursorName,
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
// session, creating the stream first if this is the first thing ever to
// touch it (a cursor may be seeded before any event exists).
func (db *DB) SetEventCursor(ctx context.Context, session, cursorName string, next int64) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		streamID, err := ensureEventStream(ctx, q, session)
		if err != nil {
			return err
		}
		if err := q.UpsertEventCursor(ctx, sqlcgen.UpsertEventCursorParams{
			StreamID:     streamID,
			CursorName:   cursorName,
			NextSequence: next,
		}); err != nil {
			return fmt.Errorf("set cursor %q/%q: %w", session, cursorName, err)
		}
		return nil
	})
}

// ensureEventStream returns session's event_streams id, creating the row
// with a freshly minted id if it does not already exist. Every write that
// touches a session's event data — an append, or a consumer position
// committed ahead of the session's first event — goes through this, so the
// events/event_cursors tables' foreign key is always
// satisfiable.
func ensureEventStream(ctx context.Context, q *sqlcgen.Queries, session string) (string, error) {
	id, err := q.GetEventStreamIDBySession(ctx, session)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("get event stream id for %q: %w", session, err)
	}
	id = newULID()
	if err := q.InsertEventStream(ctx, sqlcgen.InsertEventStreamParams{ID: id, SessionName: session}); err != nil {
		return "", fmt.Errorf("create event stream for %q: %w", session, err)
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
// DeliveryMode is not a stored column, so it always comes back as the zero
// value here: nothing reads it back from persistence, and it is derivable
// from the event's type prefix when a reader needs it.
func eventFromRow(row sqlcgen.ListEventsFromByStreamRow, session string) (event.Event, error) {
	t, err := parseTime(row.RecordedAt)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q recorded_at: %w", row.EventID, err)
	}
	metadata, err := unmarshalEventMetadata(row.MetadataJson)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q metadata: %w", row.EventID, err)
	}
	return event.Event{
		ID:          row.EventID,
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

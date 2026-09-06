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
// write transaction, creating the stream — and assigning its generation —
// first if this is the session's first touch. It returns the assigned
// sequence, a positive, per-stream append position; ev.ID remains the
// event's own global dedup identity, unrelated to this number.
func (db *DB) AppendEvent(ctx context.Context, ev event.Event) (sequence int64, err error) {
	err = db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		if err := ensureEventStream(ctx, q, ev.SessionName); err != nil {
			return err
		}

		next, err := q.NextEventSequence(ctx, ev.SessionName)
		if err != nil {
			return fmt.Errorf("next sequence for %q: %w", ev.SessionName, err)
		}

		metadataJSON, err := marshalEventMetadata(ev.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata for event %q: %w", ev.ID, err)
		}

		if err := q.InsertEvent(ctx, sqlcgen.InsertEventParams{
			EventID:      ev.ID,
			SessionName:  ev.SessionName,
			Sequence:     next,
			RecordedAt:   formatTime(ev.Time),
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

// EventStreamGeneration returns a session's event-stream generation, or ""
// if the stream has never been touched (no event appended, no consumer
// position ever committed).
func (db *DB) EventStreamGeneration(ctx context.Context, session string) (string, error) {
	var gen string
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		g, err := sqlcgen.New(tx).GetEventStreamGeneration(ctx, session)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get event stream generation for %q: %w", session, err)
		}
		gen = g
		return nil
	})
	return gen, err
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
// own sequence (parallel slices, index-aligned).
func (db *DB) ListEventsFrom(ctx context.Context, session string, since int64) ([]event.Event, []int64, error) {
	var evs []event.Event
	var seqs []int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListEventsFrom(ctx, sqlcgen.ListEventsFromParams{
			SessionName: session,
			Sequence:    since,
		})
		if err != nil {
			return fmt.Errorf("list events for %q from %d: %w", session, since, err)
		}
		for _, row := range rows {
			ev, err := eventFromRow(row)
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

// HasEventConsumerPosition reports whether consumer has ever committed a
// position for session, distinguishing "never started" from a committed
// position of 0.
func (db *DB) HasEventConsumerPosition(ctx context.Context, session, consumer string) (bool, error) {
	var has bool
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		count, err := sqlcgen.New(tx).HasEventConsumerPosition(ctx, sqlcgen.HasEventConsumerPositionParams{
			SessionName:  session,
			ConsumerName: consumer,
		})
		if err != nil {
			return fmt.Errorf("check consumer position %q/%q: %w", session, consumer, err)
		}
		has = count > 0
		return nil
	})
	return has, err
}

// EventConsumerPosition returns consumer's committed next-sequence position
// for session (0 if never committed).
func (db *DB) EventConsumerPosition(ctx context.Context, session, consumer string) (int64, error) {
	var pos int64
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		p, err := sqlcgen.New(tx).GetEventConsumerPosition(ctx, sqlcgen.GetEventConsumerPositionParams{
			SessionName:  session,
			ConsumerName: consumer,
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get consumer position %q/%q: %w", session, consumer, err)
		}
		pos = p
		return nil
	})
	return pos, err
}

// SetEventConsumerPosition durably records consumer's next-sequence
// position for session, creating the stream first if this consumer is the
// first thing ever to touch it (a cursor may be seeded before any event
// exists).
func (db *DB) SetEventConsumerPosition(ctx context.Context, session, consumer string, next int64) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		if err := ensureEventStream(ctx, q, session); err != nil {
			return err
		}
		if err := q.UpsertEventConsumerPosition(ctx, sqlcgen.UpsertEventConsumerPositionParams{
			SessionName:  session,
			ConsumerName: consumer,
			NextSequence: next,
		}); err != nil {
			return fmt.Errorf("set consumer position %q/%q: %w", session, consumer, err)
		}
		return nil
	})
}

// ensureEventStream creates session's event_streams row (with a freshly
// minted generation) if it does not already exist. Every write that
// touches a session's event data — an append, or a consumer position
// committed ahead of the session's first event — goes through this, so
// event_consumer_positions' foreign key to event_streams is always
// satisfiable.
func ensureEventStream(ctx context.Context, q *sqlcgen.Queries, session string) error {
	if err := q.InsertEventStream(ctx, sqlcgen.InsertEventStreamParams{
		SessionName: session,
		Generation:  newULID(),
	}); err != nil {
		return fmt.Errorf("ensure event stream for %q: %w", session, err)
	}
	return nil
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

func eventFromRow(row sqlcgen.Event) (event.Event, error) {
	t, err := parseTime(row.RecordedAt)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q recorded_at: %w", row.EventID, err)
	}
	metadata, err := unmarshalEventMetadata(row.MetadataJson)
	if err != nil {
		return event.Event{}, fmt.Errorf("parse event %q metadata: %w", row.EventID, err)
	}
	return event.Event{
		ID:           row.EventID,
		SessionName:  row.SessionName,
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

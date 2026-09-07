package persistence

import (
	"context"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// writeChannelHealthTx replaces sessionID's two channel-health streak rows:
// a nil streak deletes its row (no open failure streak to record), a
// non-nil one upserts it. The two kinds are independent (see
// Session.ChannelValidationHealth/ChannelDeliveryHealth's own doc comment),
// so each is written on its own regardless of the other's outcome.
func writeChannelHealthTx(ctx context.Context, q *sqlcgen.Queries, sessionID string, validation, delivery *contract.ChannelHealth) error {
	if err := writeOneChannelHealthTx(ctx, q, sessionID, contract.ChannelFailureKindValidation, validation); err != nil {
		return err
	}
	return writeOneChannelHealthTx(ctx, q, sessionID, contract.ChannelFailureKindDelivery, delivery)
}

func writeOneChannelHealthTx(ctx context.Context, q *sqlcgen.Queries, sessionID, kind string, ch *contract.ChannelHealth) error {
	if ch == nil {
		if err := q.DeleteSessionChannelHealth(ctx, sqlcgen.DeleteSessionChannelHealthParams{SessionID: sessionID, Kind: kind}); err != nil {
			return fmt.Errorf("clear channel health %q/%q: %w", sessionID, kind, err)
		}
		return nil
	}
	if err := q.UpsertSessionChannelHealth(ctx, sqlcgen.UpsertSessionChannelHealthParams{
		SessionID:           sessionID,
		Kind:                kind,
		ConsecutiveFailures: int64(ch.ConsecutiveFailures),
		FirstFailureAt:      formatTime(ch.FirstFailureAt),
		LastFailureAt:       formatTime(ch.LastFailureAt),
		LastChannel:         nullString(ch.LastChannel),
		LastError:           nullString(ch.LastError),
		EscalatedAt:         formatTimeNull(ch.EscalatedAt),
	}); err != nil {
		return fmt.Errorf("upsert channel health %q/%q: %w", sessionID, kind, err)
	}
	return nil
}

// loadChannelHealth reads sessionID's channel-health rows and splits them
// back into the two independent streaks by kind; a kind with no row is nil
// (no open failure streak), matching writeChannelHealthTx's own convention.
func loadChannelHealth(ctx context.Context, q *sqlcgen.Queries, sessionID string) (validation, delivery *contract.ChannelHealth, err error) {
	rows, err := q.ListSessionChannelHealth(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list channel health for %q: %w", sessionID, err)
	}
	for _, row := range rows {
		firstFailureAt, err := parseTime(row.FirstFailureAt)
		if err != nil {
			return nil, nil, fmt.Errorf("parse channel health %q/%q first_failure_at: %w", sessionID, row.Kind, err)
		}
		lastFailureAt, err := parseTime(row.LastFailureAt)
		if err != nil {
			return nil, nil, fmt.Errorf("parse channel health %q/%q last_failure_at: %w", sessionID, row.Kind, err)
		}
		escalatedAt, err := parseTimeNull(row.EscalatedAt)
		if err != nil {
			return nil, nil, fmt.Errorf("parse channel health %q/%q escalated_at: %w", sessionID, row.Kind, err)
		}
		ch := &contract.ChannelHealth{
			ConsecutiveFailures: int(row.ConsecutiveFailures),
			FirstFailureAt:      firstFailureAt,
			LastFailureAt:       lastFailureAt,
			LastChannel:         row.LastChannel.String,
			LastError:           row.LastError.String,
			EscalatedAt:         escalatedAt,
		}
		switch row.Kind {
		case contract.ChannelFailureKindValidation:
			validation = ch
		case contract.ChannelFailureKindDelivery:
			delivery = ch
		}
	}
	return validation, delivery, nil
}

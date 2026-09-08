package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

const (
	subscriptionRetrySubscribe   = "subscribe"
	subscriptionRetryUnsubscribe = "unsubscribe"
)

// SubscriptionRetry is one provider operation that did not complete and is
// retried by ordinary session activity.
type SubscriptionRetry struct {
	SessionID  string
	Session    string
	Action     string
	ResourceID string
}

// Tombstone returns name's most recently destroyed incarnation. The retained
// session row and its children are the one authority for a tombstone.
func (db *DB) Tombstone(ctx context.Context, name string) (*contract.Tombstone, error) {
	var tombstone *contract.Tombstone
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetLatestDestroyedSession(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get destroyed session %q: %w", name, err)
		}
		session, err := sessionFromRow(row)
		if err != nil {
			return err
		}
		if err := db.loadSessionExtras(ctx, tx, session); err != nil {
			return err
		}
		tombstone = &contract.Tombstone{Session: *session, DestroyedAt: session.DestroyedAt}
		return nil
	})
	return tombstone, err
}

// SwapChainAttempt atomically replaces one incarnation-scoped fingerprint.
func (db *DB) SwapChainAttempt(ctx context.Context, session, instance, chainID, fingerprint string) (previous string, won bool, err error) {
	err = db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, err := currentSessionID(ctx, q, session)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM chain_attempts WHERE session_id = ? AND instance = ? AND chain_id = ?`, sessionID, instance, chainID).Scan(&previous); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("read chain attempt: %w", err)
			}
			previous = ""
		}
		if previous == fingerprint {
			return nil
		}
		won = true
		if fingerprint == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM chain_attempts WHERE session_id = ? AND instance = ? AND chain_id = ?`, sessionID, instance, chainID)
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO chain_attempts (session_id, instance, chain_id, fingerprint) VALUES (?, ?, ?, ?)
			ON CONFLICT(session_id, instance, chain_id) DO UPDATE SET fingerprint = excluded.fingerprint`, sessionID, instance, chainID, fingerprint)
		return err
	})
	return previous, won, err
}

// RevertChainAttempt restores previous only while claimed remains current.
func (db *DB) RevertChainAttempt(ctx context.Context, session, instance, chainID, claimed, previous string) (reverted bool, err error) {
	err = db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		sessionID, err := currentSessionID(ctx, q, session)
		if err != nil {
			return err
		}
		if previous == "" {
			result, err := tx.ExecContext(ctx, `DELETE FROM chain_attempts WHERE session_id = ? AND instance = ? AND chain_id = ? AND fingerprint = ?`, sessionID, instance, chainID, claimed)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			reverted = n == 1
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE chain_attempts SET fingerprint = ? WHERE session_id = ? AND instance = ? AND chain_id = ? AND fingerprint = ?`, previous, sessionID, instance, chainID, claimed)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		reverted = n == 1
		return nil
	})
	return reverted, err
}

// ClearChainAttempts removes all markers for one session incarnation.
func (db *DB) ClearChainAttempts(ctx context.Context, sessionID string) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM chain_attempts WHERE session_id = ?`, sessionID)
		return err
	})
}

// QueueSubscriptionRetry queues an operation against the live incarnation.
func (db *DB) QueueSubscriptionRetry(ctx context.Context, session, action, resourceID string) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		id, err := currentSessionID(ctx, sqlcgen.New(tx), session)
		if err != nil {
			return err
		}
		return queueSubscriptionRetryTx(ctx, tx, id, action, resourceID)
	})
}

// QueueSubscriptionRetryByID records a teardown retry against the destroyed
// incarnation that owned the original provider registration.
func (db *DB) QueueSubscriptionRetryByID(ctx context.Context, sessionID, action, resourceID string) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		return queueSubscriptionRetryTx(ctx, tx, sessionID, action, resourceID)
	})
}

func queueSubscriptionRetryTx(ctx context.Context, tx *sql.Tx, sessionID, action, resourceID string) error {
	if action != subscriptionRetrySubscribe && action != subscriptionRetryUnsubscribe {
		return fmt.Errorf("invalid subscription retry action %q", action)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO subscription_retries (session_id, action, resource_id) VALUES (?, ?, ?) ON CONFLICT(session_id, action, resource_id) DO NOTHING`, sessionID, action, resourceID)
	return err
}

// SubscriptionRetriesForLiveSession returns retries for name's live row.
func (db *DB) SubscriptionRetriesForLiveSession(ctx context.Context, name string) ([]SubscriptionRetry, error) {
	return db.subscriptionRetries(ctx, `SELECT r.session_id, s.name, r.action, r.resource_id FROM subscription_retries r JOIN sessions s ON s.id = r.session_id WHERE s.name = ? AND s.status <> 'destroyed' ORDER BY r.action, r.resource_id`, name)
}

// DestroyedSubscriptionRetries returns retries whose retained session row is
// destroyed. Callers drop subscribe attempts and retry unsubscribe attempts.
func (db *DB) DestroyedSubscriptionRetries(ctx context.Context) ([]SubscriptionRetry, error) {
	return db.subscriptionRetries(ctx, `SELECT r.session_id, s.name, r.action, r.resource_id FROM subscription_retries r JOIN sessions s ON s.id = r.session_id WHERE s.status = 'destroyed' ORDER BY s.name, r.action, r.resource_id`)
}

func (db *DB) subscriptionRetries(ctx context.Context, query string, args ...any) ([]SubscriptionRetry, error) {
	var retries []SubscriptionRetry
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var retry SubscriptionRetry
			if err := rows.Scan(&retry.SessionID, &retry.Session, &retry.Action, &retry.ResourceID); err != nil {
				return err
			}
			retries = append(retries, retry)
		}
		return rows.Err()
	})
	return retries, err
}

// DeleteSubscriptionRetry removes a retry only after its outcome is known.
func (db *DB) DeleteSubscriptionRetry(ctx context.Context, sessionID, action, resourceID string) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM subscription_retries WHERE session_id = ? AND action = ? AND resource_id = ?`, sessionID, action, resourceID)
		return err
	})
}

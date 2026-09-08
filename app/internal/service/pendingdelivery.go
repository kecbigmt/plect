package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/app/internal/state"
)

const (
	retrySubscribe   = "subscribe"
	retryUnsubscribe = "unsubscribe"
)

func retryStore(store *state.Store) (*persistence.DB, error) {
	return persistence.EnsureCurrentShared(context.Background(), persistence.PathIn(store.Dir()))
}

func queuePendingSubscribe(store *state.Store, sessionName, resource string) error {
	db, err := retryStore(store)
	if err != nil {
		return err
	}
	return db.QueueSubscriptionRetry(context.Background(), sessionName, retrySubscribe, resource)
}

func queuePendingUnsubscribe(store *state.Store, sessionName, resource string) error {
	db, err := retryStore(store)
	if err != nil {
		return err
	}
	return db.QueueSubscriptionRetry(context.Background(), sessionName, retryUnsubscribe, resource)
}

func queuePendingUnsubscribeForSessionID(store *state.Store, sessionID, resource string) error {
	db, err := retryStore(store)
	if err != nil {
		return err
	}
	return db.QueueSubscriptionRetryByID(context.Background(), sessionID, retryUnsubscribe, resource)
}

func dequeuePendingDelivery(store *state.Store, retry persistence.SubscriptionRetry) error {
	db, err := retryStore(store)
	if err != nil {
		return err
	}
	return db.DeleteSubscriptionRetry(context.Background(), retry.SessionID, retry.Action, retry.Resource)
}

// flushPendingDeliveryLogged is TaskSetup's/TaskCleanup's/Destroy's own call
// site for flushPendingDelivery, logging what it could not resolve since
// none of them has a result field for an unrelated resource's retry failure.
func flushPendingDeliveryLogged(cfg *config.Config, store *state.Store, sessionName string) {
	for _, err := range flushPendingDelivery(cfg, store, sessionName) {
		slog.Default().Warn("subscription retry flush failed", "session", sessionName, "error", err)
	}
	sweepOrphanedPendingDeliveries(cfg, store)
}

// sweepOrphanedPendingDeliveries handles retries owned by retained destroyed
// rows. Subscribe attempts are stale; unsubscribe attempts retain the source
// incarnation id so a same-name replacement can never consume them.
func sweepOrphanedPendingDeliveries(cfg *config.Config, store *state.Store) {
	db, err := retryStore(store)
	if err != nil {
		slog.Default().Warn("subscription retry sweep: open database", "error", err)
		return
	}
	retries, err := db.DestroyedSubscriptionRetries(context.Background())
	if err != nil {
		slog.Default().Warn("subscription retry sweep: list destroyed sessions", "error", err)
		return
	}
	for _, retry := range retries {
		if retry.Action == retrySubscribe {
			if err := dequeuePendingDelivery(store, retry); err != nil {
				slog.Default().Warn("subscription retry sweep: drop stale subscribe", "session", retry.Session, "error", err)
			}
			continue
		}
		unsubscribed, err := unsubscribeIfWired(cfg, retry.Session, retry.Resource)
		if err != nil {
			slog.Default().Warn("subscription retry flush failed", "session", retry.Session, "error", err)
			continue
		}
		if unsubscribed {
			if err := dequeuePendingDelivery(store, retry); err != nil {
				slog.Default().Warn("subscription retry sweep: dequeue unsubscribe", "session", retry.Session, "error", err)
			}
		}
	}
}

// flushPendingDelivery retries every operation owned by sessionName's live
// incarnation under its delivery lock. Ordinary activity, not a background
// worker, drains live retries.
func flushPendingDelivery(cfg *config.Config, store *state.Store, sessionName string) []error {
	var errs []error
	if lockErr := withDeliveryLock(store, sessionName, func() {
		db, err := retryStore(store)
		if err != nil {
			errs = append(errs, err)
			return
		}
		retries, err := db.SubscriptionRetriesForLiveSession(context.Background(), sessionName)
		if err != nil {
			errs = append(errs, err)
			return
		}
		for _, retry := range retries {
			if err := flushOnePendingDelivery(cfg, store, retry); err != nil {
				errs = append(errs, err)
			}
		}
	}); lockErr != nil {
		errs = append(errs, lockErr)
	}
	return errs
}

func flushOnePendingDelivery(cfg *config.Config, store *state.Store, retry persistence.SubscriptionRetry) error {
	fresh, err := store.GetE(retry.Session)
	if err != nil {
		return err
	}
	switch retry.Action {
	case retrySubscribe:
		if fresh == nil || !resourceStillNeededBySession(fresh, retry.Resource) {
			return dequeuePendingDelivery(store, retry)
		}
		subscribed, err := subscribeIfWired(cfg, retry.Session, retry.Resource, domain.SessionBranch(fresh))
		if err != nil || !subscribed {
			return err
		}
		return dequeuePendingDelivery(store, retry)
	case retryUnsubscribe:
		if fresh != nil && resourceStillNeededBySession(fresh, retry.Resource) {
			return dequeuePendingDelivery(store, retry)
		}
		unsubscribed, err := unsubscribeIfWired(cfg, retry.Session, retry.Resource)
		if err != nil || !unsubscribed {
			return err
		}
		return dequeuePendingDelivery(store, retry)
	default:
		return fmt.Errorf("unknown subscription retry action %q", retry.Action)
	}
}

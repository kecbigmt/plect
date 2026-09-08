package service

import (
	"context"
	"path/filepath"

	"github.com/kecbigmt/plecture/app/internal/persistence"
)

// pendingDeliveryFile preserves the old tests' queue assertions while the
// production representation is relational.
type pendingDeliveryFile struct {
	Subscribe   map[string][]string
	Unsubscribe map[string][]string
}

func pendingDeliveryPath(store interface{ Dir() string }) string {
	return filepath.Join(store.Dir(), "pending_delivery.json")
}

func loadPendingDelivery(path string) (*pendingDeliveryFile, error) {
	db, err := persistence.EnsureCurrentShared(context.Background(), persistence.PathIn(filepath.Dir(path)))
	if err != nil {
		return nil, err
	}
	f := &pendingDeliveryFile{Subscribe: map[string][]string{}, Unsubscribe: map[string][]string{}}
	retries, err := db.SubscriptionRetries(context.Background())
	if err != nil {
		return nil, err
	}
	for _, retry := range retries {
		if retry.Action == retrySubscribe {
			f.Subscribe[retry.Session] = append(f.Subscribe[retry.Session], retry.Resource)
		} else {
			f.Unsubscribe[retry.Session] = append(f.Unsubscribe[retry.Session], retry.Resource)
		}
	}
	return f, nil
}

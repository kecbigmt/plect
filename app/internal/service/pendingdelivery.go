package service

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
)

// pendingDeliveryFile durably queues, per session, resources a subscribe or
// unsubscribe attempt could not carry out — a failed hook, a failed
// delivery-lock acquisition, or (unsubscribe only) a failed freshness
// re-check — so the failure is a retry candidate on the session's next
// activity rather than a lost one-shot attempt. Kept out of
// contracts/state.Session (a separately pseudo-version-pinned module this
// change must not touch) and rooted next to state.json instead, as a pure
// implementation detail of this package's own retry behavior.
type pendingDeliveryFile struct {
	Subscribe   map[string][]string `json:"subscribe,omitempty"`
	Unsubscribe map[string][]string `json:"unsubscribe,omitempty"`
}

func pendingDeliveryPath(store *state.Store) string {
	return filepath.Join(store.Dir(), "pending_delivery.json")
}

// loadPendingDelivery reports a read/parse failure to the caller rather than
// returning an empty queue for it: silently treating a genuine read error as
// "nothing queued" would be exactly the kind of durability loss this queue
// exists to prevent. An absent file is not a failure — it means nothing has
// ever been queued yet.
func loadPendingDelivery(path string) (*pendingDeliveryFile, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &pendingDeliveryFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	f := &pendingDeliveryFile{}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, err
	}
	return f, nil
}

// updatePendingDelivery runs fn under an exclusive file lock shared with
// every other process touching this queue — the same locked
// read-modify-write shape state.Store already uses for a JSON file several
// processes may touch concurrently.
func updatePendingDelivery(path string, fn func(*pendingDeliveryFile)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// LOCK_EX requires a descriptor opened for writing: the Linux NFS client
	// enforces this and returns EBADF for an O_RDONLY fd, even though local
	// filesystems tolerate it. The descriptor is never read or written to;
	// only its lock is used.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	f, err := loadPendingDelivery(path)
	if err != nil {
		return err
	}
	fn(f)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, data)
}

func queuePendingSubscribe(store *state.Store, sessionName, resource string) error {
	return updatePendingDelivery(pendingDeliveryPath(store), func(f *pendingDeliveryFile) {
		if f.Subscribe == nil {
			f.Subscribe = map[string][]string{}
		}
		if !slices.Contains(f.Subscribe[sessionName], resource) {
			f.Subscribe[sessionName] = append(f.Subscribe[sessionName], resource)
		}
	})
}

func queuePendingUnsubscribe(store *state.Store, sessionName, resource string) error {
	return updatePendingDelivery(pendingDeliveryPath(store), func(f *pendingDeliveryFile) {
		if f.Unsubscribe == nil {
			f.Unsubscribe = map[string][]string{}
		}
		if !slices.Contains(f.Unsubscribe[sessionName], resource) {
			f.Unsubscribe[sessionName] = append(f.Unsubscribe[sessionName], resource)
		}
	})
}

func dequeuePendingSubscribe(store *state.Store, sessionName, resource string) error {
	return updatePendingDelivery(pendingDeliveryPath(store), func(f *pendingDeliveryFile) {
		f.Subscribe[sessionName] = slices.DeleteFunc(f.Subscribe[sessionName], func(r string) bool { return r == resource })
		if len(f.Subscribe[sessionName]) == 0 {
			delete(f.Subscribe, sessionName)
		}
	})
}

func dequeuePendingUnsubscribe(store *state.Store, sessionName, resource string) error {
	return updatePendingDelivery(pendingDeliveryPath(store), func(f *pendingDeliveryFile) {
		f.Unsubscribe[sessionName] = slices.DeleteFunc(f.Unsubscribe[sessionName], func(r string) bool { return r == resource })
		if len(f.Unsubscribe[sessionName]) == 0 {
			delete(f.Unsubscribe, sessionName)
		}
	})
}

// flushPendingDeliveryLogged is TaskSetup's/TaskCleanup's/Destroy's own call
// site for flushPendingDelivery, logging what it could not resolve since
// none of them has a result field for an unrelated resource's retry
// failure. Also sweeps other sessions' orphaned entries; see
// sweepOrphanedPendingDeliveries.
func flushPendingDeliveryLogged(cfg *config.Config, store *state.Store, sessionName string) {
	for _, err := range flushPendingDelivery(cfg, store, sessionName) {
		slog.Default().Warn("pending delivery flush failed", "session", sessionName, "error", err)
	}
	sweepOrphanedPendingDeliveries(cfg, store, sessionName)
}

// sweepOrphanedPendingDeliveries retries every other session's queued entry
// whose own session no longer exists — the post-destroy case resolveSession
// can never again resolve for that session to retry itself. A still-live
// other session is left to drain its own queue as usual.
func sweepOrphanedPendingDeliveries(cfg *config.Config, store *state.Store, sessionName string) {
	f, err := loadPendingDelivery(pendingDeliveryPath(store))
	if err != nil {
		slog.Default().Warn("pending delivery sweep: failed to load queue", "error", err)
		return
	}
	orphaned := map[string]bool{}
	for name := range f.Subscribe {
		orphaned[name] = true
	}
	for name := range f.Unsubscribe {
		orphaned[name] = true
	}
	delete(orphaned, sessionName)
	for name := range orphaned {
		if store.Get(name) != nil {
			continue
		}
		for _, err := range flushPendingDelivery(cfg, store, name) {
			slog.Default().Warn("pending delivery flush failed", "session", name, "error", err)
		}
	}
}

// flushPendingDelivery retries every subscribe/unsubscribe queued for
// sessionName, under the same per-session lock a fresh subscribe/unsubscribe
// decision runs under (withDeliveryLock) — so a flush can never interleave
// with one either. Called opportunistically at the top of
// TaskSetup/TaskCleanup: ordinary session activity is what drains the
// queue, there is no background process.
func flushPendingDelivery(cfg *config.Config, store *state.Store, sessionName string) []error {
	var errs []error
	if lockErr := withDeliveryLock(store, sessionName, func() {
		f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
		if loadErr != nil {
			errs = append(errs, loadErr)
			return
		}
		for _, resource := range slices.Clone(f.Subscribe[sessionName]) {
			if err := flushOnePendingSubscribe(cfg, store, sessionName, resource); err != nil {
				errs = append(errs, err)
			}
		}
		for _, resource := range slices.Clone(f.Unsubscribe[sessionName]) {
			if err := flushOnePendingUnsubscribe(cfg, store, sessionName, resource); err != nil {
				errs = append(errs, err)
			}
		}
	}); lockErr != nil {
		errs = append(errs, lockErr)
	}
	return errs
}

// flushOnePendingSubscribe retries one queued subscribe. A resource nothing
// needs any more (every instance that once bound it is gone) is dropped
// without retrying — the failure that queued it is moot once nothing is
// left to deliver to. A resolved (false, nil) outcome — no provider
// matches, or the matched one declares no subscribe hook — is left queued
// rather than dequeued: subscribeIfWired only ever queues a resource after
// an ambiguous match or a hook execution failure, both of which presuppose
// a real, hooked provider existed at queue time, so a later "nothing to
// wire" answer means the config changed underneath the queue rather than
// confirming the original failure is resolved, and this queue never treats
// silence as success.
func flushOnePendingSubscribe(cfg *config.Config, store *state.Store, sessionName, resource string) error {
	fresh, freshErr := store.GetE(sessionName)
	if freshErr != nil {
		return freshErr
	}
	if fresh == nil || !resourceStillNeededBySession(fresh, resource) {
		return dequeuePendingSubscribe(store, sessionName, resource)
	}
	subscribed, subErr := subscribeIfWired(cfg, sessionName, resource, fresh.Branch)
	if subErr != nil {
		return subErr
	}
	if !subscribed {
		return nil
	}
	return dequeuePendingSubscribe(store, sessionName, resource)
}

// flushOnePendingUnsubscribe mirrors flushOnePendingSubscribe: a resource
// something needs again is dropped from the queue without running the
// hook, a confirmed unsubscribe dequeues, and anything else (an error, or a
// resolved-but-unconfirmed (false, nil)) stays queued.
func flushOnePendingUnsubscribe(cfg *config.Config, store *state.Store, sessionName, resource string) error {
	fresh, freshErr := store.GetE(sessionName)
	if freshErr != nil {
		return freshErr
	}
	if fresh != nil && resourceStillNeededBySession(fresh, resource) {
		return dequeuePendingUnsubscribe(store, sessionName, resource)
	}
	unsubscribed, unsubErr := unsubscribeIfWired(cfg, sessionName, resource)
	if unsubErr != nil {
		return unsubErr
	}
	if !unsubscribed {
		return nil
	}
	return dequeuePendingUnsubscribe(store, sessionName, resource)
}

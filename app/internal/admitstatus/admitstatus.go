// Package admitstatus reads a population member's admit outcome back from
// its event log; shared by population and service, which cannot import each other.
package admitstatus

import (
	"sync"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
)

// admitReasons excludes poll/subscribe/down/destroy failures against the same resource: not this member's own admit outcome.
var admitReasons = map[string]bool{
	"capacity":   true,
	"up":         true,
	"input":      true,
	"task_setup": true,
}

var scannedTypes = []string{
	event.TypeWorkflowPopulationFailure,
	event.TypeWorkflowPopulationConflict,
	event.TypeWorkflowPopulationAdmitOK,
}

type Status struct {
	LastReason  string // "" once an admit_ok is newer than any admit failure
	LastError   string
	Consecutive int
}

// Member is for the out-of-process status surface, which has no running
// Cache to read from. event.Filter has no resource-scoped variant, so a
// Tail limit narrower than "everything" risks dropping the one event that
// mattered before the resource/reason filtering below ever runs.
func Member(log *eventlog.Store, session, resource string) Status {
	if session == "" {
		return Status{}
	}
	return scan(log, session, resource)
}

func scan(log *eventlog.Store, session, resource string) Status {
	events, err := log.Tail(session, event.Filter{Types: scannedTypes}, 0)
	if err != nil {
		return Status{}
	}
	var status Status
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Metadata["resource"] != resource {
			continue
		}
		reason := ev.Metadata["reason"]
		switch {
		case ev.Type == event.TypeWorkflowPopulationAdmitOK:
			return status
		case ev.Type == event.TypeWorkflowPopulationFailure && !admitReasons[reason]:
			continue
		case ev.Type != event.TypeWorkflowPopulationFailure && ev.Type != event.TypeWorkflowPopulationConflict:
			continue
		}
		if status.LastReason == "" {
			status.LastReason = reason
			status.LastError = ev.Summary
		}
		status.Consecutive++
	}
	return status
}

type cacheKey struct{ session, resource string }

// Cache holds one Status per member in memory: Get seeds an entry with one
// scan on first access, Record then keeps it exact without scanning again —
// the capacity gate's hot path never scans a log itself.
type Cache struct {
	log *eventlog.Store
	mu  sync.Mutex
	m   map[cacheKey]Status
}

func NewCache(log *eventlog.Store) *Cache {
	return &Cache{log: log, m: make(map[cacheKey]Status)}
}

func (c *Cache) Get(session, resource string) Status {
	if session == "" {
		return Status{}
	}
	k := cacheKey{session, resource}
	c.mu.Lock()
	defer c.mu.Unlock()
	if status, ok := c.m[k]; ok {
		return status
	}
	status := scan(c.log, session, resource)
	c.m[k] = status
	return status
}

// Record applies one already-classified admit outcome in place: ok resets
// session/resource to eligible, otherwise reason/errMsg extend its streak.
// Callers use this only where the event itself is already known to qualify
// (an admit attempt's own reason, or a conflict) — Get's own scan is what
// filters historical noise the first time a member is seen.
func (c *Cache) Record(session, resource string, ok bool, reason, errMsg string) {
	if session == "" {
		return
	}
	k := cacheKey{session, resource}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.m[k] = Status{}
		return
	}
	status := c.m[k]
	if status.LastReason == "" {
		status.LastReason = reason
		status.LastError = errMsg
	}
	status.Consecutive++
	c.m[k] = status
}

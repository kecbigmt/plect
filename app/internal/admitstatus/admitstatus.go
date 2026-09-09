// Package admitstatus reads a population member's admit outcome back from
// its event log; shared by population and service, which cannot import each other.
package admitstatus

import (
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
)

// latestLookback bounds LatestReason's own ring; Member scans unbounded.
const latestLookback = 20

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

// LatestReason is for the capacity gate's hot path, held under
// capacityCoordinator's mutex: it stops at the first qualifying event
// instead of Member's unbounded, exact scan.
func LatestReason(log *eventlog.Store, session, resource string) string {
	if session == "" {
		return ""
	}
	events, err := log.Tail(session, event.Filter{Types: scannedTypes}, latestLookback)
	if err != nil {
		return ""
	}
	return classify(events, resource, false).LastReason
}

// Member is for the unlocked, on-demand status surface, which needs an
// exact count; limit 0 costs nothing extra since Tail scans the full
// session regardless.
func Member(log *eventlog.Store, session, resource string) Status {
	if session == "" {
		return Status{}
	}
	events, err := log.Tail(session, event.Filter{Types: scannedTypes}, 0)
	if err != nil {
		return Status{}
	}
	return classify(events, resource, true)
}

// classify walks events newest-first for resource, stopping at the first
// admit_ok or, unless all is set, the first qualifying failure.
func classify(events []event.Event, resource string, all bool) Status {
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
		if !all {
			return status
		}
	}
	return status
}

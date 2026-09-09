// Package admitstatus reads a population member's admit outcome back from
// its event log; shared by population and service, which cannot import
// each other.
package admitstatus

import (
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
)

// admitReasons excludes poll/subscribe/down/destroy: those also record a
// plect.workflow_population.failure against a member's resource but are
// not that member's own admit outcome.
var admitReasons = map[string]bool{
	"capacity":   true,
	"up":         true,
	"input":      true,
	"task_setup": true,
}

type Status struct {
	LastReason  string // "" once an admit_ok is newer than any admit failure
	LastError   string
	Consecutive int
}

// limit 0: Tail reads a session's full history per call regardless, so an
// unbounded scan costs nothing extra and keeps the count exact.
func Member(log *eventlog.Store, session, resource string) Status {
	if session == "" {
		return Status{}
	}
	events, err := log.Tail(session, event.Filter{
		Types: []string{event.TypeWorkflowPopulationFailure, event.TypeWorkflowPopulationAdmitOK},
	}, 0)
	if err != nil {
		return Status{}
	}
	var status Status
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Metadata["resource"] != resource {
			continue
		}
		if ev.Type == event.TypeWorkflowPopulationAdmitOK {
			break
		}
		reason := ev.Metadata["reason"]
		if !admitReasons[reason] {
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

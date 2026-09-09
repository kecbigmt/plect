package population

import (
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// admitStatusWindow bounds the event-log scan to a fixed cost, not a
// member's entire history.
const admitStatusWindow = 50

type admitStatus struct {
	LastReason  string // "" once an admit_ok is newer than any failure
	LastError   string
	Consecutive int
}

// memberAdmitStatus keys off admit_ok rather than the presence-only up
// event: a retry that finds the session already up fires no up event, which
// would otherwise leave a stale failure looking current.
func memberAdmitStatus(log *eventlog.Store, member *state.PopulationMember) admitStatus {
	if member.SessionName == "" {
		return admitStatus{}
	}
	events, err := log.Tail(member.SessionName, event.Filter{
		Types: []string{event.TypeWorkflowPopulationFailure, event.TypeWorkflowPopulationAdmitOK},
	}, admitStatusWindow)
	if err != nil {
		return admitStatus{}
	}
	var status admitStatus
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Metadata["resource"] != member.ResourceID {
			continue
		}
		if ev.Type == event.TypeWorkflowPopulationAdmitOK {
			break
		}
		if status.LastReason == "" {
			status.LastReason = ev.Metadata["reason"]
			status.LastError = ev.Summary
		}
		status.Consecutive++
	}
	return status
}

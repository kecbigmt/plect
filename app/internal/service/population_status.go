package service

import (
	"fmt"
	"sort"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// admitStatusWindow bounds the event-log scan to a fixed cost.
const admitStatusWindow = 50

type PopulationMemberStatus struct {
	Resource                 string `json:"resource"`
	Session                  string `json:"session,omitempty"`
	PendingUp                bool   `json:"pending_up"`
	Tombstoned               bool   `json:"tombstoned"`
	LastAdmitReason          string `json:"last_admit_reason,omitempty"`
	LastAdmitError           string `json:"last_admit_error,omitempty"`
	ConsecutiveAdmitFailures int    `json:"consecutive_admit_failures,omitempty"`
}

// PopulationStatus reads runtime state directly rather than resolving
// workflow/population against current config, so it still shows a
// population whose config was just changed or removed out from under it.
func PopulationStatus(store *state.Store, workflow, population string) ([]PopulationMemberStatus, error) {
	key := workflow + "/" + population
	pop, err := store.Population(key)
	if err != nil {
		return nil, fmt.Errorf("read population %q: %w", key, err)
	}
	if pop == nil {
		return nil, nil
	}
	log := eventlog.NewStore(store.Dir())
	resources := make([]string, 0, len(pop.Members))
	for resource := range pop.Members {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	out := make([]PopulationMemberStatus, 0, len(resources))
	for _, resource := range resources {
		member := pop.Members[resource]
		st := PopulationMemberStatus{
			Resource: resource, Session: member.SessionName,
			PendingUp: member.PendingUp, Tombstoned: member.Tombstoned,
		}
		if member.SessionName != "" {
			st.LastAdmitReason, st.LastAdmitError, st.ConsecutiveAdmitFailures =
				memberAdmitOutcome(log, member.SessionName, resource)
		}
		out = append(out, st)
	}
	return out, nil
}

// memberAdmitOutcome mirrors population.memberAdmitStatus: that package
// cannot be imported here, since it already depends on this one.
func memberAdmitOutcome(log *eventlog.Store, session, resource string) (reason, lastError string, consecutive int) {
	events, err := log.Tail(session, event.Filter{
		Types: []string{event.TypeWorkflowPopulationFailure, event.TypeWorkflowPopulationAdmitOK},
	}, admitStatusWindow)
	if err != nil {
		return "", "", 0
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Metadata["resource"] != resource {
			continue
		}
		if ev.Type == event.TypeWorkflowPopulationAdmitOK {
			break
		}
		if reason == "" {
			reason = ev.Metadata["reason"]
			lastError = ev.Summary
		}
		consecutive++
	}
	return reason, lastError, consecutive
}

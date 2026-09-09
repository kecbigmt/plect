package service

import (
	"fmt"
	"sort"

	"github.com/kecbigmt/plecture/app/internal/admitstatus"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
)

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
		status := admitstatus.Member(log, member.SessionName, resource)
		st.LastAdmitReason, st.LastAdmitError, st.ConsecutiveAdmitFailures =
			status.LastReason, status.LastError, status.Consecutive
		out = append(out, st)
	}
	return out, nil
}

package service

import (
	"fmt"
	"slices"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// InstanceObservation is one instance's resource observation outcome.
type InstanceObservation struct {
	Instance string    `json:"instance"`
	Observer string    `json:"observer"`
	At       time.Time `json:"at,omitzero"`
	Error    string    `json:"error,omitempty"`
}

// ObserveSessionResources observes the resource of every live instance whose
// task document declares an observer, and persists the pass's observations in
// one write.
//
// A pass that is going to act observes first, once, and everything that pass
// evaluates then reads the persisted snapshot — so a completion predicate and
// the chain conditions beside it cannot disagree about what the resource
// says. A failed observation leaves the prior snapshot in place and is
// reported: a failed reading makes the facts old, which a display says
// out loud, rather than making them absent.
func ObserveSessionResources(cfg *config.Config, store *state.Store, sessionName string) ([]InstanceObservation, error) {
	return observeResources(cfg, store, sessionName, "")
}

// ObserveInstanceResource observes one instance's resource. It is the pass a
// single-instance decision makes — `plect task finalize` reconfirms at the
// current revision, so it reads what the resource says now rather than what
// it said when something last looked.
func ObserveInstanceResource(cfg *config.Config, store *state.Store, sessionName, instanceKey string) (*InstanceObservation, error) {
	out, err := observeResources(cfg, store, sessionName, instanceKey)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// observeResources observes every live document-backed instance, or the one
// named by only.
func observeResources(cfg *config.Config, store *state.Store, sessionName, only string) ([]InstanceObservation, error) {
	resolvedName, session, err := resolveSession(cfg, store, sessionName)
	if err != nil {
		return nil, err
	}
	docs, _, err := loadTaskDeclarations(cfg, session)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	observers, err := cfg.LoadResourceDefs()
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load resource observers: %v", err)}
	}
	var out []InstanceObservation
	taken := map[string]*contract.ResourceObservation{}
	for _, key := range sortedInstanceKeys(session.Tasks) {
		st := session.Tasks[key]
		if st == nil || st.Status != contract.TaskStatusProduced {
			continue
		}
		if only != "" && key != only {
			continue
		}
		doc, ok := docs[taskIDForInstance(key, st)]
		if !ok {
			continue
		}
		observed, result := observeInstanceResource(cfg, session, doc, observers, key, st)
		out = append(out, result)
		if observed != nil {
			taken[key] = observed
		}
	}
	if err := persistObservations(store, resolvedName, taken); err != nil {
		return out, err
	}
	return out, nil
}

// observeInstanceResource runs one instance's observation. A nil observation
// with a reported error is the failed-reading case: the caller keeps whatever
// was observed last rather than replacing it with nothing.
func observeInstanceResource(cfg *config.Config, session *domain.Session, doc config.TaskDocument, observers map[string]config.ResourceDef, key string, st *contract.TaskState) (*contract.ResourceObservation, InstanceObservation) {
	result := InstanceObservation{Instance: key, Observer: doc.ResourceObserver}
	observer, ok := observers[doc.ResourceObserver]
	if !ok {
		result.Error = fmt.Sprintf("task %s is written for resource observer %q, which no config layer declares", doc.ID, doc.ResourceObserver)
		return nil, result
	}
	resourceID := st.Resource
	if resourceID == "" {
		resourceID = session.ResourceID
	}
	if resourceID == "" {
		result.Error = fmt.Sprintf("instance %q has no resource to observe", key)
		return nil, result
	}
	observed, err := task.ObserveResource(observer, resourceID, domain.SessionBranch(session), session.WorkspaceDirPath, cfg.Plugins)
	if err != nil {
		result.Error = err.Error()
		return nil, result
	}
	result.At = time.Now()
	return &contract.ResourceObservation{State: observed, At: result.At}, result
}

// persistObservations writes one pass's observations under the state lock,
// touching only the instances it observed.
func persistObservations(store *state.Store, resolvedName string, taken map[string]*contract.ResourceObservation) error {
	if len(taken) == 0 {
		return nil
	}
	err := store.Update(resolvedName, func(s *domain.Session) error {
		for key, observed := range taken {
			cur := s.Tasks[key]
			// A concurrent teardown during the unlocked observation window
			// leaves nothing to attach the reading to; dropping it is right,
			// because a cleaned instance decides nothing.
			if cur == nil || cur.Status != contract.TaskStatusProduced {
				continue
			}
			cur.Observed = observed
		}
		s.UpdatedAt = time.Now()
		return nil
	})
	if err != nil {
		return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to persist resource observations: %v", err)}
	}
	return nil
}

func sortedInstanceKeys(tasks map[string]*contract.TaskState) []string {
	keys := make([]string, 0, len(tasks))
	for key := range tasks {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

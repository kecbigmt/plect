package population

import (
	"context"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

type populationConflictError struct {
	session string
	reason  string
}

func (e *populationConflictError) Error() string { return e.reason }

func serviceHooks(cfg func() *config.Config, store *state.Store, def Definition, coordinator *capacityCoordinator) Hooks {
	provenance := &contract.PopulationProvenance{Workflow: def.Workflow.Address, Name: def.Population.Name}
	return Hooks{
		Up: func(_ context.Context, resource string, inputs map[string]any) (UpOutcome, error) {
			if coordinator != nil {
				return coordinator.up(context.Background(), def, resource, inputs)
			}
			return upPopulation(cfg, store, def, provenance, resource, inputs)
		},
		Destroy: func(_ context.Context, session, resource string, force bool) error {
			current := store.Get(session)
			if current == nil || current.Population == nil || *current.Population != *provenance || current.ResourceID != resource {
				return &populationConflictError{session: session, reason: fmt.Sprintf("session %q no longer has matching workflow-population provenance", session)}
			}
			_, err := service.Destroy(cfg(), store, service.DestroyParams{Identifier: session, Force: force})
			return err
		},
		EnsureInitial: func(_ context.Context, session, taskID, resource string) error {
			current := store.Get(session)
			if current == nil {
				return fmt.Errorf("session %q disappeared before initial task setup", session)
			}
			if existing := current.Tasks["initial"]; existing != nil {
				if !existing.Dynamic || existing.Name != "initial" || existing.TaskID != taskID || existing.Resource != resource {
					return fmt.Errorf("session %q already has a conflicting initial task instance", session)
				}
				if existing.Status == contract.TaskStatusProduced {
					return nil
				}
				return fmt.Errorf("session %q initial task is %q; clean it before population setup can retry", session, existing.Status)
			}
			_, err := service.TaskSetup(cfg(), store, service.TaskSetupParams{
				TaskID: taskID, SessionName: session, Name: "initial", Resource: resource,
			})
			return err
		},
		Blockers: func(_ context.Context, session string) ([]string, error) {
			return service.PopulationTaskBlockers(cfg(), store, session)
		},
	}
}

func upPopulation(cfg func() *config.Config, store *state.Store, def Definition, provenance *contract.PopulationProvenance, resource string, inputs map[string]any) (UpOutcome, error) {
	name, err := service.ResolvePopulationSessionName(cfg(), def.Workflow.Address, resource)
	if err != nil {
		return UpOutcome{}, err
	}
	if current := store.Get(name); current != nil {
		if current.Population == nil || *current.Population != *provenance || current.ResourceID != resource {
			return UpOutcome{}, &populationConflictError{session: name, reason: fmt.Sprintf("session %q is owned by another lifecycle authority", name)}
		}
		alreadyUp := cfg().RunScopeUp(current)
		result, err := service.Up(cfg(), store, service.UpParams{Identifier: name})
		if err != nil {
			return UpOutcome{}, err
		}
		return UpOutcome{SessionName: result.SessionName, AlreadyUp: alreadyUp}, nil
	}
	result, err := service.Up(cfg(), store, service.UpParams{
		Identifier: resource,
		Workflow:   def.Workflow.Address,
		Inputs:     inputs,
		Population: provenance,
	})
	if err != nil {
		return UpOutcome{}, err
	}
	return UpOutcome{SessionName: result.SessionName}, nil
}

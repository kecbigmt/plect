package population

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

type capacityCoordinator struct {
	cfg         func() *config.Config
	state       *state.Store
	log         *eventlog.Store
	mu          sync.Mutex
	definitions map[string]Definition
}

func newCapacityCoordinator(cfg func() *config.Config, stateStore *state.Store, logStore *eventlog.Store) *capacityCoordinator {
	return &capacityCoordinator{cfg: cfg, state: stateStore, log: logStore, definitions: make(map[string]Definition)}
}

func (c *capacityCoordinator) setDefinitions(definitions []Definition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.definitions = make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		c.definitions[populationKey(definition)] = definition
	}
}

func (c *capacityCoordinator) up(_ context.Context, def Definition, resource string, inputs map[string]any) (UpOutcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	provenance := &contract.PopulationProvenance{Workflow: def.Workflow.Address, Name: def.Population.Name}
	outcome, err := upPopulation(c.cfg, c.state, def, provenance, resource, inputs)
	if !isCapError(err) {
		return outcome, err
	}
	if c.pendingExistingAhead(def, resource) {
		return UpOutcome{}, fmt.Errorf("an existing population member has a pending up request and takes priority")
	}
	candidates, err := c.idleCandidates()
	if err != nil {
		return UpOutcome{}, fmt.Errorf("find idle candidates to free capacity: %w", err)
	}
	for _, candidate := range candidates {
		if _, downErr := service.Down(c.cfg(), c.state, service.DownParams{Identifier: candidate.session}); downErr != nil {
			c.record(candidate, event.TypeWorkflowPopulationFailure, "down", downErr.Error())
			continue
		}
		c.record(candidate, event.TypeWorkflowPopulationDown, "capacity", "population member brought down for virtual-root capacity")
		outcome, err = upPopulation(c.cfg, c.state, def, provenance, resource, inputs)
		if !isCapError(err) {
			return outcome, err
		}
	}
	if target, resolveErr := service.ResolvePopulationSessionName(c.cfg(), def.Workflow.Address, resource); resolveErr == nil {
		c.record(idleCandidate{session: target, resource: resource, key: populationKey(def)}, event.TypeWorkflowPopulationDown,
			"capacity", "virtual-root capacity remains full with no eligible population member to bring down")
	}
	return UpOutcome{}, err
}

func isCapError(err error) bool {
	var serviceErr *service.Error
	return errors.As(err, &serviceErr) && serviceErr.Code == service.ErrChildCapExceeded
}

func (c *capacityCoordinator) pendingExistingAhead(current Definition, resource string) bool {
	currentState, _ := c.state.Population(populationKey(current))
	if currentState == nil || currentState.Members[resource] == nil || currentState.Members[resource].SessionName != "" {
		return false
	}
	for key := range c.definitions {
		population, err := c.state.Population(key)
		if err != nil || population == nil {
			continue
		}
		for _, member := range population.Members {
			if member != nil && member.PendingUp && member.SessionName != "" && !member.Tombstoned {
				return true
			}
		}
	}
	return false
}

type idleCandidate struct {
	session  string
	resource string
	key      string
	last     time.Time
}

func (c *capacityCoordinator) idleCandidates() ([]idleCandidate, error) {
	sessions, err := c.state.AllE()
	if err != nil {
		return nil, fmt.Errorf("read session state: %w", err)
	}
	var candidates []idleCandidate
	for sessionName, session := range sessions {
		if session == nil || session.Population == nil || !logicalVirtualRootChild(session) || !runIsUp(session) {
			continue
		}
		key := session.Population.Workflow + "/" + session.Population.Name
		definition, ok := c.definitions[key]
		if !ok || !definition.Population.AutoDown {
			continue
		}
		population, err := c.state.Population(key)
		if err != nil {
			// One session's population lookup failing does not invalidate
			// the whole candidate sweep; it is simply not idle-eligible
			// this pass.
			continue
		}
		if population == nil {
			continue
		}
		member := population.Members[session.ResourceID]
		if member == nil || member.SessionName != sessionName || member.Tombstoned {
			continue
		}
		status, ok := c.latestStatus(sessionName)
		if !ok || status.Metadata["cleared"] != "true" {
			continue
		}
		activation := session.CreatedAt
		for _, at := range []time.Time{member.LastAppearance, member.LastInbound} {
			if at.After(activation) {
				activation = at
			}
		}
		inbound, inboundErr := c.latestInbound(sessionName)
		if inboundErr != nil {
			continue
		}
		if inbound.After(activation) {
			activation = inbound
		}
		if !status.Time.After(activation) && !status.Time.Equal(activation) {
			continue
		}
		last := activation
		if status.Time.After(last) {
			last = status.Time
		}
		candidates = append(candidates, idleCandidate{session: sessionName, resource: session.ResourceID, key: key, last: last})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].last.Equal(candidates[j].last) {
			return candidates[i].last.Before(candidates[j].last)
		}
		return candidates[i].session < candidates[j].session
	})
	return candidates, nil
}

func (c *capacityCoordinator) latestInbound(session string) (time.Time, error) {
	events, err := c.log.Tail(session, event.Filter{Direction: event.Inbound}, 1)
	if err != nil || len(events) == 0 {
		return time.Time{}, err
	}
	return events[0].Time, nil
}

func logicalVirtualRootChild(session *domain.Session) bool {
	return session.ParentSession == "" || strings.HasPrefix(session.ParentSession, "root:")
}

func runIsUp(session *domain.Session) bool {
	for _, taskState := range session.Tasks {
		if taskState != nil && taskState.Scope == contract.TaskScopeRun && taskState.Status == contract.TaskStatusProduced {
			return true
		}
	}
	return false
}

func (c *capacityCoordinator) latestStatus(session string) (event.Event, bool) {
	events, err := c.log.Tail(session, event.Filter{Types: []string{event.TypeStatusMessage}}, 1)
	if err != nil || len(events) == 0 {
		return event.Event{}, false
	}
	return events[0], true
}

func (c *capacityCoordinator) record(candidate idleCandidate, typ, reason, summary string) {
	definition := c.definitions[candidate.key]
	_, _, _, _ = c.log.Append(event.Event{
		SessionName: candidate.session,
		Type:        typ,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     summary,
		Metadata: map[string]string{
			"workflow":   definition.Workflow.Address,
			"population": definition.Population.Name,
			"resource":   candidate.resource,
			"reason":     reason,
		},
	})
}

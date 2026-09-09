package population

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// UpOutcome reports what one Up hook call did. AlreadyUp is captured before
// the hook runs, because the hook is idempotent and cannot say afterwards
// whether it rebuilt anything: it separates a genuine presence change from a
// re-admission of a member that never went down.
type UpOutcome struct {
	SessionName string
	AlreadyUp   bool
}

type Hooks struct {
	Up            func(context.Context, string, map[string]any) (UpOutcome, error)
	Destroy       func(context.Context, string, string, bool) error
	EnsureInitial func(context.Context, string, string, string) error
	Blockers      func(context.Context, string) ([]string, error)
}

type Engine struct {
	definition Definition
	state      *state.Store
	log        *eventlog.Store
	logger     *slog.Logger
	hooks      Hooks
	key        string
	now        func() time.Time
}

func populationKey(def Definition) string {
	return def.Workflow.Address + "/" + def.Population.Name
}

func NewEngine(def Definition, stateStore *state.Store, logStore *eventlog.Store, hooks Hooks) *Engine {
	return &Engine{definition: def, state: stateStore, log: logStore, logger: slog.Default(), hooks: hooks, key: populationKey(def), now: time.Now}
}

func (e *Engine) ApplyPoll(ctx context.Context, items []map[string]any) error {
	validated, err := e.validateItems(items, true)
	if err != nil {
		return err
	}
	present := make(map[string]map[string]any, len(validated))
	for _, item := range validated {
		present[item["resource"].(string)] = item
	}
	if err := e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		population.Workflow = e.definition.Workflow.Address
		population.Name = e.definition.Population.Name
		for resource, item := range present {
			member := population.Members[resource]
			if member == nil {
				member = &state.PopulationMember{ResourceID: resource, Generation: 1, PendingUp: true}
				population.Members[resource] = member
			} else if member.Tombstoned {
				member.Generation++
				member.Tombstoned = false
				member.PendingUp = true
				member.AcceptedAt = time.Time{}
				member.LastAppearance = time.Time{}
				member.LastInbound = time.Time{}
				member.LastDecision = ""
				member.LastBlockers = nil
			}
			member.Item = item
			if member.SessionName == "" {
				member.PendingUp = true
			}
		}
		for resource, member := range population.Members {
			if present[resource] != nil || member.Tombstoned {
				continue
			}
			member.Tombstoned = true
			member.PendingUp = false
			member.LastDecision = ""
			member.LastBlockers = nil
		}
		return nil
	}); err != nil {
		return err
	}
	return e.Reconcile(ctx)
}

func (e *Engine) ApplyAppearance(ctx context.Context, item map[string]any) error {
	validated, err := e.validateItems([]map[string]any{item}, false)
	if err != nil {
		return err
	}
	item = validated[0]
	resource := item["resource"].(string)
	now := e.now()
	suppressed := false
	if err := e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		population.Workflow = e.definition.Workflow.Address
		population.Name = e.definition.Population.Name
		member := population.Members[resource]
		if member != nil && member.Tombstoned && e.definition.Population.UsesPoll() {
			suppressed = true
			return nil
		}
		if member == nil || member.Tombstoned {
			generation := uint64(1)
			if member != nil {
				generation = member.Generation + 1
			}
			member = &state.PopulationMember{ResourceID: resource, Generation: generation}
			population.Members[resource] = member
		}
		member.Item = item
		member.LastAppearance = now
		member.PendingUp = true
		member.Tombstoned = false
		member.LastDecision = ""
		member.LastBlockers = nil
		return nil
	}); err != nil {
		return err
	}
	if suppressed {
		e.logger.Info("subscribe appearance suppressed by poll tombstone", "population", e.key, "resource", resource)
		return nil
	}
	return e.Reconcile(ctx)
}

func (e *Engine) Reconcile(ctx context.Context) error {
	population, err := e.state.Population(e.key)
	if err != nil || population == nil {
		return err
	}
	resources := make([]string, 0, len(population.Members))
	for resource := range population.Members {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	var firstErr error
	for _, resource := range resources {
		member := population.Members[resource]
		if member.Tombstoned && member.SessionName != "" {
			if err := e.decideDestroy(ctx, member, "poll_absent"); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	sort.SliceStable(resources, func(i, j int) bool {
		left, right := population.Members[resources[i]], population.Members[resources[j]]
		if (left.SessionName != "") != (right.SessionName != "") {
			return left.SessionName != ""
		}
		return resources[i] < resources[j]
	})
	for _, resource := range resources {
		member := population.Members[resource]
		if member.Tombstoned || !member.PendingUp {
			continue
		}
		if err := e.admit(ctx, member); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (e *Engine) SweepExpiry(ctx context.Context) error {
	if err := e.processInbound(); err != nil {
		return err
	}
	if e.definition.Population.UsesPoll() {
		return e.Reconcile(ctx)
	}
	population, err := e.state.Population(e.key)
	if err != nil || population == nil {
		return err
	}
	now := e.now()
	var firstErr error
	for _, resource := range sortedMembers(population.Members) {
		member := population.Members[resource]
		if member.Tombstoned || member.SessionName == "" || member.AcceptedAt.IsZero() {
			continue
		}
		last := member.AcceptedAt
		if member.LastAppearance.After(last) {
			last = member.LastAppearance
		}
		inbound, inErr := e.latestInbound(member.SessionName)
		if inErr != nil {
			if firstErr == nil {
				firstErr = inErr
			}
			continue
		}
		if inbound.After(last) {
			last = inbound
		}
		if now.Sub(last) < e.definition.Population.ExpireAfter.Duration {
			continue
		}
		if err := e.decideDestroy(ctx, member, "subscribe_expired"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := e.Reconcile(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (e *Engine) processInbound() error {
	population, err := e.state.Population(e.key)
	if err != nil || population == nil {
		return err
	}
	updates := make(map[string]time.Time)
	for _, resource := range sortedMembers(population.Members) {
		member := population.Members[resource]
		if member.Tombstoned || member.SessionName == "" {
			continue
		}
		inbound, err := e.latestInbound(member.SessionName)
		if err != nil {
			return err
		}
		if inbound.After(member.LastInbound) {
			updates[resource] = inbound
		}
	}
	if len(updates) == 0 {
		return nil
	}
	return e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		for resource, inbound := range updates {
			member := population.Members[resource]
			if member == nil || member.Tombstoned {
				continue
			}
			member.LastInbound = inbound
			member.PendingUp = true
		}
		return nil
	})
}

func (e *Engine) admit(ctx context.Context, member *state.PopulationMember) error {
	inputs, err := e.sessionInputs(member.ResourceID, member.Item)
	if err != nil {
		e.record(member.SessionName, event.TypeWorkflowPopulationFailure, "input", err.Error(), member.ResourceID)
		return err
	}
	if e.hooks.Up == nil {
		return fmt.Errorf("population admission has no lifecycle hook")
	}
	outcome, err := e.hooks.Up(ctx, member.ResourceID, inputs)
	if err != nil {
		var conflict *populationConflictError
		if errors.As(err, &conflict) {
			e.record(conflict.session, event.TypeWorkflowPopulationConflict, "provenance", err.Error(), member.ResourceID)
			return err
		}
		// pendingExistingAhead reads this tag back to decide whether the
		// member still deserves head-of-line priority next time.
		reason := "up"
		if isCapacityRefusal(err) {
			reason = "capacity"
		}
		e.record(member.SessionName, event.TypeWorkflowPopulationFailure, reason, err.Error(), member.ResourceID)
		return err
	}
	session := outcome.SessionName
	now := e.now()
	if err := e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		current := population.Members[member.ResourceID]
		if current == nil || current.Generation != member.Generation || current.Tombstoned {
			return nil
		}
		if current.SessionName == "" || current.AcceptedAt.IsZero() {
			current.AcceptedAt = now
		}
		current.SessionName = session
		return nil
	}); err != nil {
		return err
	}
	// The record has to land here rather than after a completed admission:
	// initial-task setup runs against an already-up session, and its failure
	// leaves a retry whose up hook reports no transition, so a record deferred
	// past it would be lost for good instead of merely delayed.
	if !outcome.AlreadyUp {
		e.record(session, event.TypeWorkflowPopulationUp, "up", "population member is up", member.ResourceID)
	}
	if e.definition.Population.Session.Task != "" && e.hooks.EnsureInitial != nil {
		if err := e.hooks.EnsureInitial(ctx, session, e.definition.Population.Session.Task, member.ResourceID); err != nil {
			e.record(session, event.TypeWorkflowPopulationFailure, "task_setup", err.Error(), member.ResourceID)
			return err
		}
	}
	if err := e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		current := population.Members[member.ResourceID]
		if current == nil || current.Generation != member.Generation || current.Tombstoned {
			return nil
		}
		current.SessionName = session
		current.PendingUp = false
		return nil
	}); err != nil {
		return err
	}
	// Unlike TypeWorkflowPopulationUp above, this fires even when outcome.
	// AlreadyUp is true: pendingExistingAhead needs a "the last attempt
	// succeeded" signal that isn't gated on presence changing.
	e.record(session, event.TypeWorkflowPopulationAdmitOK, "admit", "population member admission succeeded", member.ResourceID)
	return nil
}

func (e *Engine) decideDestroy(ctx context.Context, member *state.PopulationMember, reason string) error {
	var blockers []string
	var err error
	if e.hooks.Blockers != nil {
		blockers, err = e.hooks.Blockers(ctx, member.SessionName)
		if err != nil {
			blockers = []string{"completion evaluation failed: " + err.Error()}
		}
	}
	sort.Strings(blockers)
	if len(blockers) > 0 {
		return e.recordDecision(member, event.TypeWorkflowPopulationDestroyDeferred, reason, blockers)
	}
	if !e.definition.Population.AutoDestroy {
		return e.recordDecision(member, event.TypeWorkflowPopulationDestroyDryRun, reason, nil)
	}
	if e.hooks.Destroy == nil {
		return fmt.Errorf("population destruction has no lifecycle hook")
	}
	if err := e.hooks.Destroy(ctx, member.SessionName, member.ResourceID, e.definition.Population.Session.Destroy.Force); err != nil {
		var conflict *populationConflictError
		if errors.As(err, &conflict) {
			e.record(conflict.session, event.TypeWorkflowPopulationConflict, "provenance", err.Error(), member.ResourceID)
			return err
		}
		e.record(member.SessionName, event.TypeWorkflowPopulationFailure, "destroy", err.Error(), member.ResourceID)
		return err
	}
	e.record(member.SessionName, event.TypeWorkflowPopulationDestroy, reason, "population member destroyed", member.ResourceID)
	return e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		current := population.Members[member.ResourceID]
		if current == nil || current.Generation != member.Generation {
			return nil
		}
		current.Tombstoned = true
		current.SessionName = ""
		current.PendingUp = false
		current.LastDecision = event.TypeWorkflowPopulationDestroy
		current.LastBlockers = nil
		return nil
	})
}

func (e *Engine) recordDecision(member *state.PopulationMember, typ, reason string, blockers []string) error {
	if member.LastDecision == typ+":"+reason && reflect.DeepEqual(member.LastBlockers, blockers) {
		return nil
	}
	e.record(member.SessionName, typ, reason, strings.Join(blockers, ", "), member.ResourceID,
		map[string]string{"blockers": strings.Join(blockers, ",")})
	return e.state.UpdatePopulation(e.key, func(population *state.PopulationState) error {
		current := population.Members[member.ResourceID]
		if current == nil || current.Generation != member.Generation {
			return nil
		}
		current.LastDecision = typ + ":" + reason
		current.LastBlockers = append([]string(nil), blockers...)
		return nil
	})
}

func (e *Engine) RecordFailure(reason string, failure error) {
	population, err := e.state.Population(e.key)
	if err != nil || population == nil {
		return
	}
	for _, resource := range sortedMembers(population.Members) {
		member := population.Members[resource]
		if member != nil && member.SessionName != "" {
			e.record(member.SessionName, event.TypeWorkflowPopulationFailure, reason, failure.Error(), resource)
		}
	}
}

func (e *Engine) record(session, typ, reason, summary, resource string, extra ...map[string]string) {
	if session == "" {
		return
	}
	metadata := map[string]string{
		"workflow":   e.definition.Workflow.Address,
		"population": e.definition.Population.Name,
		"resource":   resource,
		"reason":     reason,
	}
	for _, fields := range extra {
		for key, value := range fields {
			metadata[key] = value
		}
	}
	_, _, _, _ = e.log.Append(event.Event{
		SessionName: session,
		Type:        typ,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     summary,
		Metadata:    metadata,
	})
}

func (e *Engine) validateItems(items []map[string]any, complete bool) ([]map[string]any, error) {
	seen := make(map[string]bool, len(items))
	out := make([]map[string]any, 0, len(items))
	observerMatch := regexp.MustCompile(e.definition.Observer.Match)
	providerMatch := regexp.MustCompile(e.definition.Provider.Match)
	for i, item := range items {
		if err := e.definition.ItemSchema.Validate(item); err != nil {
			return nil, fmt.Errorf("query item %d: %w", i, err)
		}
		resource := item["resource"].(string)
		if seen[resource] {
			return nil, fmt.Errorf("query item %d duplicates resource %q", i, resource)
		}
		seen[resource] = true
		if !observerMatch.MatchString(resource) {
			return nil, fmt.Errorf("query item %d resource %q is not recognized by observer %q", i, resource, e.definition.Observer.ID)
		}
		if !providerMatch.MatchString(resource) {
			return nil, fmt.Errorf("query item %d resource %q is not recognized by workspace provider %q", i, resource, e.definition.Provider.ID)
		}
		if _, err := e.sessionInputs(resource, item); err != nil {
			if complete {
				return nil, fmt.Errorf("query item %d: %w", i, err)
			}
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (e *Engine) sessionInputs(resource string, item map[string]any) (map[string]any, error) {
	roots := lang.Roots{"resource": map[string]any{"id": resource}, "item": item}
	eval := lang.Eval{Roots: roots}
	inputs := make(map[string]any, len(e.definition.Population.Session.Inputs))
	keys := make([]string, 0, len(e.definition.Population.Session.Inputs))
	for key := range e.definition.Population.Session.Inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, absent, err := eval.Value(e.definition.Population.Session.Inputs[key])
		if err != nil {
			return nil, fmt.Errorf("session input %q: %w", key, err)
		}
		if !absent {
			inputs[key] = value
		}
	}
	if e.definition.SessionSchema != nil {
		if err := e.definition.SessionSchema.Validate(inputs); err != nil {
			return nil, fmt.Errorf("session inputs: %w", err)
		}
	}
	return inputs, nil
}

func (e *Engine) latestInbound(session string) (time.Time, error) {
	events, err := e.log.Tail(session, event.Filter{Direction: event.Inbound}, 1)
	if err != nil || len(events) == 0 {
		return time.Time{}, err
	}
	return events[0].Time, nil
}

func sortedMembers(members map[string]*state.PopulationMember) []string {
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

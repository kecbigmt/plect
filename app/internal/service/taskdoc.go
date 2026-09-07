package service

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// loadTaskDeclarations loads everything the `tasks/` root declares and runs
// the contract checks that need the rest of the layer. Every caller that
// reads a completion predicate goes through here: an id resolves to one
// declaration of one kind, and a document whose observer reference or
// completion key no longer resolves is reported once, as a load error, rather
// than evaluating against a contract nothing checked.
func loadTaskDeclarations(cfg *config.Config, session *domain.Session) (map[string]config.TaskDocument, map[string]config.TaskDefinition, error) {
	docs, effects, err := cfg.LoadTaskDeclarations(session.WorkspaceDirPath)
	if err != nil {
		return nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load task declarations: %v", err)}
	}
	if len(docs) == 0 {
		return docs, effects, nil
	}
	if err := validateTaskDocuments(cfg, session, docs); err != nil {
		return nil, nil, err
	}
	return docs, effects, nil
}

func validateTaskDocuments(cfg *config.Config, session *domain.Session, docs map[string]config.TaskDocument) error {
	observers, err := cfg.LoadResourceDefs()
	if err != nil {
		return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load resource observers: %v", err)}
	}
	workflows, err := cfg.LoadWorkflows(session.WorkspaceDirPath)
	if err != nil {
		return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load workflows: %v", err)}
	}
	if err := cfg.ValidateTaskDocuments(docs, observers, workflows); err != nil {
		return &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	return nil
}

// SetTaskStateParams addresses one live instance's own state.
type SetTaskStateParams struct {
	Identifier string
	// Instance is the instance key, e.g. "review#1".
	Instance string
	// State is the merge payload: only the keys present are written.
	State map[string]any
}

// SetTaskStateResult reports what was recorded.
type SetTaskStateResult struct {
	SessionName string   `json:"session_name"`
	Instance    string   `json:"instance"`
	Keys        []string `json:"keys"`
}

// SetTaskState records facts into a live instance's own state — the keys a
// reviewer or another session writes and the instance's completion predicate
// reads as `self.state.*`.
//
// There is no mutability annotation to consult: state is mutable by
// definition, and what bounds a write is the document's `state_schema`. An
// instance no task document declares has no state to hold, so a write to one
// is refused rather than landing where nothing reads it.
func SetTaskState(cfg *config.Config, store *state.Store, params SetTaskStateParams) (*SetTaskStateResult, error) {
	if params.Instance == "" {
		return nil, &Error{Code: ErrInvalidInput, Message: "an instance is required"}
	}
	if len(params.State) == 0 {
		return nil, &Error{Code: ErrInvalidInput, Message: "payload must be a non-empty JSON object"}
	}
	sessionName, session, err := resolveSession(cfg, store, params.Identifier)
	if err != nil {
		return nil, err
	}
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return nil, guardErr
	}
	st := session.Tasks[params.Instance]
	if st == nil {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q not found in session %s", params.Instance, sessionName)}
	}
	docs, _, err := loadTaskDeclarations(cfg, session)
	if err != nil {
		return nil, err
	}
	doc, ok := docs[taskIDForInstance(params.Instance, st)]
	if !ok {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q is not declared by a task document, so it holds no state of its own", params.Instance)}
	}
	schema, serr := lang.CompileSchema(doc.StateSchema, doc.ResolvedStateSchemaPath(), "task:"+doc.ID)
	if serr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s: state_schema: %v", doc.ID, serr)}
	}
	if schema == nil {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %s declares no state_schema, so it keeps no state; declare the keys it holds to record them", doc.ID)}
	}
	declared, derr := task.SchemaPropertyNames(doc.StateSchema, doc.ResolvedStateSchemaPath())
	if derr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s: state_schema: %v", doc.ID, derr)}
	}
	keys := make([]string, 0, len(params.State))
	for k := range params.State {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	// A key the schema does not name is rejected here rather than left to
	// `additionalProperties`, so a misspelling is refused whether or not the
	// author closed the schema.
	for _, k := range keys {
		if !slices.Contains(declared, k) {
			return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %s declares no state key %q; it keeps: %s", doc.ID, k, strings.Join(declared, ", "))}
		}
	}

	updateErr := store.Update(sessionName, func(s *domain.Session) error {
		cur := s.Tasks[params.Instance]
		if cur == nil {
			return &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q not found in session %s", params.Instance, sessionName)}
		}
		if cur.Status != contract.TaskStatusProduced {
			return &Error{Code: ErrNotProduced, Message: fmt.Sprintf("instance %q is %q, not %q; an instance that is no longer live holds no state", params.Instance, cur.Status, contract.TaskStatusProduced)}
		}
		merged := make(map[string]any, len(cur.State)+len(params.State))
		maps.Copy(merged, cur.State)
		maps.Copy(merged, params.State)
		if verr := schema.Validate(merged); verr != nil {
			return &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("recorded state does not match task %s's state_schema: %s", doc.ID, task.DescribeValidationError(schema, verr))}
		}
		cur.State = merged
		s.UpdatedAt = time.Now()
		return nil
	})
	if updateErr != nil {
		if svcErr, ok := updateErr.(*Error); ok {
			return nil, svcErr
		}
		return nil, &Error{Code: ErrExecutionFailed, Message: updateErr.Error()}
	}
	return &SetTaskStateResult{SessionName: sessionName, Instance: params.Instance, Keys: keys}, nil
}

// setupTaskDocument instantiates a task document against a live session. It
// owns no lifecycle: nothing is brought up, so instantiation is binding the
// document to a resource, observing that resource once, and recording the
// instruction the session is asked to carry out.
func setupTaskDocument(cfg *config.Config, store *state.Store, resolvedName string, session *domain.Session, doc config.TaskDocument, params TaskSetupParams) (*TaskSetupResult, error) {
	observers, err := cfg.LoadResourceDefs()
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load resource observers: %v", err)}
	}
	observer, ok := observers[doc.ResourceObserver]
	if !ok {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s is written for resource observer %q, which no config layer declares", doc.ID, doc.ResourceObserver)}
	}
	resourceID := params.Resource
	if resourceID == "" {
		resourceID = session.ResourceID
	}
	if resourceID == "" {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %s is written for resource observer %q, so an instance needs a resource; pass --resource", doc.ID, doc.ResourceObserver)}
	}
	// The document states a type, so compatibility is checked up front: an
	// instance bound to a resource the declared observer does not claim can
	// never satisfy, and is refused instead of created.
	matchedAddress, _, ok, merr := task.MatchResourceDef(observers, resourceID)
	if merr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: merr.Error()}
	}
	if !ok || matchedAddress != doc.ResourceObserver {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %s is written for resource observer %q, which does not claim resource %q", doc.ID, doc.ResourceObserver, resourceID)}
	}

	inputs, bindErr := bindDocumentInputs(doc, params.Inputs, session)
	if bindErr != nil {
		return nil, bindErr
	}
	// Instantiation observes once, and a failed first observation rejects it:
	// an instance whose predicate has nothing to read is worse than none.
	observed, oerr := task.ObserveResource(observer, resourceID, domain.SessionBranch(session), session.WorkspaceDirPath, cfg.Plugins)
	if oerr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s: observing %s failed, so no instance was created: %v", doc.ID, resourceID, oerr)}
	}
	now := time.Now()
	observation := &contract.ResourceObservation{State: observed, At: now}

	instruction, ierr := renderInstruction(cfg, session, doc, inputs, resourceID, observed)
	if ierr != nil {
		return nil, ierr
	}

	var key string
	var collision bool
	reserveErr := store.Update(resolvedName, func(s *domain.Session) error {
		if s.Tasks == nil {
			s.Tasks = make(map[string]*contract.TaskState)
		}
		if params.Name != "" {
			key = params.Name
			// A `--name` collides against either collection's existing state:
			// an uninstantiated node leaves its id free for a dynamic instance
			// to take, but a live node under this name must still be refused.
			if domain.TaskState(s, key) != nil {
				collision = true
				return nil
			}
		} else {
			key = task.InstanceKey(doc.ID, strconv.Itoa(task.NextInstanceNumber(doc.ID, s.Tasks)))
		}
		s.Tasks[key] = &contract.TaskState{
			Scope: contract.TaskScopeSession,
			// The stored task id is the address the reference selected, which
			// is what lets a later lookup find this document again when two
			// plugins declare the id. The instance key stays the bare id, the
			// same way a workflow node's defaulted id does.
			TaskID:   params.TaskID,
			Status:   contract.TaskStatusProduced,
			Inputs:   inputs,
			Resource: params.Resource,
			Name:     params.Name,
			Observed: observation,
			Seq:      task.NextSeq(s.Nodes, s.Tasks),
			SetupAt:  now,
		}
		s.UpdatedAt = now
		return nil
	})
	if reserveErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to record instance: %v", reserveErr)}
	}
	if collision {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q already exists in session %s; run `plect task cleanup %s` first to recreate it", key, resolvedName, key)}
	}
	recordLifecycle(store, resolvedName, "task_setup", fmt.Sprintf("instantiated %s", key))
	appendInstruction(store, resolvedName, key, params.Resource, instruction)

	subscribed, subscribeErrMsg := wireDeliveryOnSetup(cfg, store, resolvedName, params.Resource, domain.SessionBranch(session))

	return &TaskSetupResult{
		SessionName: resolvedName,
		Instance:    key,
		// The address the reference selected, so a caller echoing this back at
		// another command names the same declaration.
		TaskID:         params.TaskID,
		Scope:          contract.TaskScopeSession,
		Name:           params.Name,
		Resource:       params.Resource,
		Subscribed:     subscribed,
		SubscribeError: subscribeErrMsg,
		Instruction:    instruction,
	}, nil
}

// bindDocumentInputs resolves the document's declared inputs from the
// explicit bindings, then the session's own inputs, then the schema's own
// declared default. An input the document does not declare is a caller
// error, not a value quietly carried along. The schema default is what lets
// an instruction body project an optional input unconditionally: a
// {{ inputs.<key> }} projection has no syntax of its own for "absent renders
// as this", so a property that wants that behavior states it once, in its
// own schema, rather than in prose.
func bindDocumentInputs(doc config.TaskDocument, cliInputs map[string]string, session *domain.Session) (map[string]any, *Error) {
	names, err := task.SchemaPropertyNames(doc.InputsSchema, doc.ResolvedInputsSchemaPath())
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s: inputs schema: %v", doc.ID, err)}
	}
	declared := make(map[string]bool, len(names))
	for _, n := range names {
		declared[n] = true
	}
	for k := range cliInputs {
		if !declared[k] {
			return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %s does not declare input %q (declared: %s)", doc.ID, k, strings.Join(names, ", "))}
		}
	}
	props, err := config.SchemaProperties(doc.InputsSchema, doc.ResolvedInputsSchemaPath())
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s: inputs schema: %v", doc.ID, err)}
	}
	out := make(map[string]any, len(names))
	for _, name := range names {
		switch {
		case cliInputs[name] != "":
			out[name] = cliInputs[name]
		case session.Inputs[name] != nil:
			out[name] = session.Inputs[name]
		default:
			if prop, ok := props[name].(map[string]any); ok {
				if def, hasDefault := prop["default"]; hasDefault {
					out[name] = def
				}
			}
		}
	}
	return out, nil
}

// renderInstruction resolves the document body's projections against the
// roots the instruction surface observes.
func renderInstruction(cfg *config.Config, session *domain.Session, doc config.TaskDocument, inputs map[string]any, resourceID string, observed map[string]any) (string, *Error) {
	workflowOutputs := map[string]any{}
	if w := session.Nodes[contract.WorkflowPseudoNodeID]; w != nil && w.Outputs != nil {
		workflowOutputs = w.Outputs
	}
	env := lang.Roots{
		"resource": map[string]any{"id": resourceID, "state": observed},
		"self":     map[string]any{"state": map[string]any{}},
		"inputs":   inputs,
		"session": map[string]any{
			"name":   session.Name,
			"inputs": session.Inputs,
		},
		"workflow": map[string]any{"outputs": workflowOutputs},
	}
	rendered, err := lang.RenderInstruction(doc.Instruction, env)
	if err != nil {
		return "", &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %s: %v", doc.ID, err)}
	}
	return strings.TrimSpace(rendered), nil
}

// evaluateDocumentInstance evaluates one task-document instance's completion
// predicate against the pair of live roots it reads. A document owns no
// lifecycle, so there are no layers to compose and no per-layer patience to
// account for: one document, one predicate, one budget.
func evaluateDocumentInstance(cfg *config.Config, store *state.Store, doc config.TaskDocument, resolvedName string, session *domain.Session, key string, st *contract.TaskState, allSessions map[string]*domain.Session, trigger TickTrigger) (*computedAction, []ChainSpawn, *Error) {
	dw, err := effectiveDoneWhen(doc.DoneWhen, st)
	if err != nil {
		return nil, nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	live := documentCompletionState(st)
	var computed *computedAction
	var eval task.DoneWhenResult
	if dw != nil {
		lastAction, lastFingerprint := "", ""
		if st.DoneWhen != nil {
			lastAction, lastFingerprint = st.DoneWhen.LastAction, st.DoneWhen.LastFingerprint
		}
		eval = task.EvaluateTaskDoneWhenWithContext(dw, live, doneWhenEvalContext(resolvedName, st, allSessions))
		action := checkActionForResult(resolvedName, key, sessionResourceForCheck(session, st), dw, st, eval, trigger)
		if action.Action != "" {
			computed = &computedAction{instance: key, action: action, lastAction: lastAction, lastFingerprint: lastFingerprint, result: eval}
		}
	}
	if len(doc.Chains) == 0 {
		return computed, nil, nil
	}
	facts := buildChainFacts(live, eval)
	resource := sessionResourceForCheck(session, st)
	plan := make([]ChainSpawn, 0, len(doc.Chains))
	for _, ch := range doc.Chains {
		sp := evalDocumentChain(cfg, store, ch, resolvedName, session, key, resource, facts)
		sp.Task = doc.ID
		plan = append(plan, sp)
	}
	return computed, plan, nil
}

// documentCompletionState is the pair of live roots a document instance's
// predicate reads: the last observation of its resource, and what was
// recorded into the instance. A document declares no outputs, so what an
// instance produced is not part of `self.state` — a value a predecessor
// effect instance left in outputs has to be recorded before it reads as
// recorded.
func documentCompletionState(st *contract.TaskState) task.CompletionState {
	state := task.CompletionState{Self: st.State}
	if st.Observed != nil {
		state.Resource = st.Observed.State
	}
	return state
}

// taskDeclarations pairs the two kinds an id can resolve to, so a caller
// asking what an instance's declaration says does not have to know which kind
// said it. Ids share one namespace, so at most one of the two answers.
type taskDeclarations struct {
	docs    map[string]config.TaskDocument
	effects map[string]config.TaskDefinition
}

func loadDeclarations(cfg *config.Config, session *domain.Session) (taskDeclarations, error) {
	docs, effects, err := loadTaskDeclarations(cfg, session)
	if err != nil {
		return taskDeclarations{}, err
	}
	return taskDeclarations{docs: docs, effects: effects}, nil
}

// gate resolves one instance's completion predicate and the live roots it
// reads. Only a task document declares one: an effect brings something up and
// takes it down, and answers for nothing beyond that.
func (d taskDeclarations) gate(key string, st *contract.TaskState) (*config.DoneWhen, task.CompletionState, error) {
	// A declaration-less instance still answers for the leaves it was set up
	// with: `--done-when-json` adds conditions to one instance, and nothing
	// about them depends on a document existing.
	doc := d.docs[taskIDForInstance(key, st)]
	dw, err := effectiveDoneWhen(doc.DoneWhen, st)
	if err != nil {
		return nil, task.CompletionState{}, err
	}
	return dw, documentCompletionState(st), nil
}

// instanceRevision is the revision a verdict is recorded against, and the one
// a completion record cites. A document reads it from the last observation of
// its resource, which is the only thing that reports it; an effect still reads
// the `revision` output a carried `[[outputs]]` entry copied it into.
func instanceRevision(st *contract.TaskState) string {
	if st == nil {
		return ""
	}
	if st.Observed != nil {
		if rev := currentRevision(st.Observed.State); rev != "" {
			return rev
		}
	}
	return currentRevision(st.Outputs)
}

// declares reports whether either kind claims this id.
func (d taskDeclarations) declares(taskID string) bool {
	if _, ok := d.docs[taskID]; ok {
		return true
	}
	_, ok := d.effects[taskID]
	return ok
}

package service

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SetOutputParams holds parameters for SetOutput.
type SetOutputParams struct {
	Identifier string
	// Node targets a workflow node's outputs by node id. Mutually exclusive
	// with Workflow / Task; exactly one target must be set.
	Node string
	// Workflow targets the workflow pseudo-node's outputs.
	Workflow bool
	// Task targets a runtime task's outputs by its displayed handle, e.g.
	// "review#1". Dynamic output refresh is the normal DoD path; this remains
	// for explicit trusted updates.
	Task string
	// Outputs is the merge payload: only the keys present are written.
	Outputs map[string]any
}

// SetOutputResult reports what was written.
type SetOutputResult struct {
	SessionName string   `json:"session_name"`
	Target      string   `json:"target"` // Tasks map key that was updated
	Keys        []string `json:"keys"`   // payload keys, sorted
}

// SetOutput merges the payload into a produced task's persisted outputs.
//
// Write policy (safe by default):
//   - merge only — keys absent from the payload are left untouched; there is
//     no full-replace mode
//   - only keys declared `mutable = true` in the target's outputs schema are
//     writable; with no such declaration nothing is writable
//   - the reserved `workspace_dir` key is always immutable (schemas declaring
//     it mutable fail at load, before this check is ever reached)
//   - the merged result must still satisfy the outputs schema
//
// The read-modify-write runs under the state file lock so concurrent explicit
// updates and lifecycle commands cannot lose writes.
func SetOutput(cfg *config.Config, store *state.Store, params SetOutputParams) (*SetOutputResult, error) {
	selected := 0
	if params.Node != "" {
		selected++
	}
	if params.Workflow {
		selected++
	}
	if params.Task != "" {
		selected++
	}
	if selected != 1 {
		return nil, &Error{Code: ErrInvalidInput, Message: "exactly one of --node, --workflow, or --task is required"}
	}
	if len(params.Outputs) == 0 {
		return nil, &Error{Code: ErrInvalidInput, Message: "payload must be a non-empty JSON object"}
	}
	if _, ok := params.Outputs[contract.OutputKeyWorkspaceDir]; ok {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("output key %q is reserved and always immutable", contract.OutputKeyWorkspaceDir)}
	}

	sessionName, session, err := resolveSession(cfg, store, params.Identifier)
	if err != nil {
		return nil, err
	}
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return nil, guardErr
	}

	target, mutable, schema, layers, resolveErr := resolveSetOutputTarget(cfg, session, params)
	if resolveErr != nil {
		return nil, resolveErr
	}

	keys := make([]string, 0, len(params.Outputs))
	for k := range params.Outputs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(mutable, k) {
			if len(mutable) == 0 {
				return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("output key %q is immutable: %s declares no mutable output keys (annotate outputs_schema properties with `mutable = true` to allow external updates)", k, target)}
			}
			return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("output key %q is immutable; mutable keys for %s: %s", k, target, strings.Join(mutable, ", "))}
		}
	}

	// Re-resolve inside the lock: the snapshot read above only served to
	// derive the schema/mutable-set, which depends on config, not state.
	updateErr := store.Update(sessionName, func(s *domain.Session) error {
		st := domain.TaskState(s, target)
		if st == nil {
			return &Error{Code: ErrNotProduced, Message: fmt.Sprintf("%s has no recorded state for session %q", target, sessionName)}
		}
		if st.Status != contract.TaskStatusProduced {
			return &Error{Code: ErrNotProduced, Message: fmt.Sprintf("%s is %q, not %q; outputs of non-live tasks are immutable", target, st.Status, contract.TaskStatusProduced)}
		}
		// A nested task's public contract is a projection, not a stored
		// object: the write addresses the inner output its direct binding
		// publishes, and the contract is re-read from there.
		if len(layers) > 0 {
			if applyErr := effect.ApplyMutableOutputs(layers, st, params.Outputs); applyErr != nil {
				return &Error{Code: ErrInvalidInput, Message: applyErr.Error()}
			}
			if schema != nil {
				if vErr := schema.Validate(st.Outputs); vErr != nil {
					return &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("merged outputs violate schema: %v", vErr)}
				}
			}
			s.UpdatedAt = time.Now()
			return nil
		}
		merged := make(map[string]any, len(st.Outputs)+len(params.Outputs))
		maps.Copy(merged, st.Outputs)
		maps.Copy(merged, params.Outputs)
		if schema != nil {
			if vErr := schema.Validate(merged); vErr != nil {
				return &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("merged outputs violate schema: %v", vErr)}
			}
		}
		st.Outputs = merged
		s.UpdatedAt = time.Now()
		return nil
	})
	if updateErr != nil {
		if svcErr, ok := updateErr.(*Error); ok {
			return nil, svcErr
		}
		return nil, &Error{Code: ErrExecutionFailed, Message: updateErr.Error()}
	}

	return &SetOutputResult{SessionName: sessionName, Target: target, Keys: keys}, nil
}

// resolveSetOutputTarget maps the selected target to the Tasks map key plus
// the mutable-key set, compiled outputs schema, and — for a nested task — the
// layer chain a write routes through.
func resolveSetOutputTarget(cfg *config.Config, session *domain.Session, params SetOutputParams) (target string, mutable []string, schema *jsonschema.Schema, layers []effect.Layer, err *Error) {
	if params.Workflow {
		workflows, loadErr := cfg.LoadWorkflows(session.WorkspaceDirPath)
		if loadErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load workflows: %v", loadErr)}
		}
		wf, ok := workflows[session.Workflow]
		if !ok {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("workflow %q not found in .plect/workflows or global config", session.Workflow)}
		}
		workspaceProviders, loadErr := cfg.LoadWorkspaceProviders()
		if loadErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load workspace providers: %v", loadErr)}
		}
		prov, ok, provErr := workspaceProviderFor(wf, workspaceProviders)
		if provErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: provErr.Error()}
		}
		if !ok {
			return "", nil, nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("workflow %q declares no workspace provider; the @workflow pseudo-node has no outputs contract", session.Workflow)}
		}
		mutableKeys, mErr := task.MutableOutputKeys(prov.OutputsSchema, prov.ResolvedOutputsSchemaPath())
		if mErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("workspace provider %q: outputs schema: %v", prov.ID, mErr)}
		}
		compiled, cErr := lang.CompileSchema(prov.OutputsSchema, prov.ResolvedOutputsSchemaPath(), "plect:workspace_provider:"+prov.ID+":outputs")
		if cErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("workspace provider %q: outputs schema: %v", prov.ID, cErr)}
		}
		return contract.WorkflowPseudoNodeID, mutableKeys, compiled, nil, nil
	}

	if params.Task != "" {
		handle := params.Task
		st, ok := session.Tasks[handle]
		if !ok || st == nil {
			if nodeSt := session.Nodes[handle]; nodeSt != nil {
				return "", nil, nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("%q is a static workflow node, not a runtime task; use --node", handle)}
			}
			return "", nil, nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task handle %q not found in session %q", handle, session.Name)}
		}
		taskID := instanceDefinitionAddress(handle, st, true, nodeAddresses(cfg, session))
		// Both kinds are loaded together, so which one this id names is the
		// loader's answer rather than this branch's guess — and a document
		// that will not load is reported as that rather than as a missing
		// declaration.
		docs, defs, loadErr := cfg.LoadTaskDeclarations(session.WorkspaceDirPath)
		if loadErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load task declarations: %v", loadErr)}
		}
		if _, isDoc := docs[taskID]; isDoc {
			return "", nil, nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %q is a task document, which produces no outputs; record what it holds about itself with `plect state set --instance %s` instead", taskID, handle)}
		}
		def, ok := defs[taskID]
		if !ok {
			return "", nil, nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("task %q for handle %q not found in any config layer", taskID, handle)}
		}
		mutableKeys, mErr := task.MutableOutputKeys(def.OutputsSchema, def.ResolvedOutputsSchemaPath())
		if mErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %q: outputs schema: %v", taskID, mErr)}
		}
		compiled, cErr := lang.CompileSchema(def.OutputsSchema, def.ResolvedOutputsSchemaPath(), "plect:task:"+taskID+":outputs")
		if cErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %q: outputs schema: %v", taskID, cErr)}
		}
		resolvedLayers, lErr := effect.ResolveLayers(def)
		if lErr != nil {
			return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("task %q: %v", taskID, lErr)}
		}
		return handle, mutableKeys, compiled, resolvedLayers, nil
	}

	plan, planErr := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if planErr != nil {
		return "", nil, nil, nil, &Error{Code: ErrExecutionFailed, Message: planErr.Error()}
	}
	for _, list := range [][]task.Resolved{plan.Session, plan.Run} {
		for i := range list {
			if list[i].NodeID == params.Node {
				return params.Node, list[i].MutableOutputs, list[i].OutputsSchema, list[i].Layers, nil
			}
		}
	}
	return "", nil, nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("node %q not found in workflow %q", params.Node, session.Workflow)}
}

package task

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// InstanceKey derives the session.Tasks key (a dynamic instance always lives
// there, never in session.Nodes) for the numbered form of a dynamic
// task instance: "<taskID>#<instanceID>", where instanceID is a
// per-task sequential number (see NextInstanceNumber). This is the `--name`-
// less form; a `--name` instance keys on the name alone (ValidInstanceName), so
// the two namespaces are disjoint (the named form carries no `#`). A blank
// instanceID degrades to the bare task id (used only in tests that key tasks
// directly).
func InstanceKey(taskID, instanceID string) string {
	if strings.TrimSpace(instanceID) == "" {
		return taskID
	}
	return taskID + "#" + instanceID
}

// instanceNameRE is the definition-id grammar, which a `--name` shares: no
// `#`, so a named key can never collide with the numbered "<task>#<n>"
// namespace nor interfere with NextInstanceNumber's suffix scan.
var instanceNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidInstanceName reports whether a `--name` is a usable instance key.
func ValidInstanceName(name string) bool {
	return instanceNameRE.MatchString(name)
}

// NextInstanceNumber returns the next per-task instance number for taskID:
// one past the highest integer suffix among existing "<taskID>#<n>" keys in
// the session. Numbering is per-task and 1-based ("review#1", "review#2", …;
// "build" numbers independently). Cleaned instances remain in state, so numbers
// are monotonic and never reused — a re-instantiation always gets a fresh,
// higher number. Keys whose suffix is not an integer are ignored.
//
// The session is single-machine and the key is session-local (not a capability,
// so it need not be unguessable); the caller allocates the number under the
// state lock so concurrent `plect task setup`s cannot collide.
func NextInstanceNumber(taskID string, tasks map[string]*contract.TaskState) int {
	prefix := taskID + "#"
	max := 0
	for key := range tasks {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		n, err := strconv.Atoi(key[len(prefix):])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1
}

// InstanceSetup is what one dynamic instance's setup produced. Layers is the
// per-layer record of a nested task's chain and is populated on failure too:
// the layers that did produce still have to be unwound by the next cleanup,
// so the caller persists it either way.
type InstanceSetup struct {
	Outputs map[string]any
	Layers  []contract.LayerState
	Stderr  []byte
}

// ExecuteTaskSetup runs a dynamic instance's setup — input-schema validation,
// value resolution, execution, output parse, and output-schema validation — and
// returns the parsed outputs. It does NOT touch session state: the caller
// reserves the instance key under the state lock, runs this WITHOUT the lock
// (setup may shell out for a while), then merges the result back under the lock.
// Stderr is returned so the caller's observer can surface diagnostic output.
//
// inputs are the already-bound input values (the caller applies the
// --input > workspace-provider/workflow outputs > session vars precedence).
// nodes is the session's Nodes map, read only to expose the @workflow
// pseudo-node's outputs to the setup template.
func ExecuteTaskSetup(goCtx context.Context, r Resolved, inputs map[string]any, session SessionVars, nodes map[string]*contract.TaskState) (InstanceSetup, error) {
	if inputs == nil {
		inputs = map[string]any{}
	}
	if r.InputsSchema != nil {
		if vErr := r.InputsSchema.Validate(toJSONShape(inputs)); vErr != nil {
			return InstanceSetup{}, fmt.Errorf("input schema: %w", vErr)
		}
	}
	ctx := RenderContext{
		Self:       map[string]any{},
		Inputs:     inputs,
		Workflow:   workflowOutputs(nodes),
		Session:    session,
		SourcePath: r.SourcePath,
	}
	if len(r.Layers) > 0 {
		layers, stderr, err := effect.RunLayers(goCtx, r.Layers, chainHost(ctx, nil, r.Scope, r.NodeID), session.WorkspaceDirPath, inputs)
		if err != nil {
			return InstanceSetup{Layers: layers, Stderr: stderr}, err
		}
		outputs, projErr := projectNestedOutputs(r, layers, session)
		return InstanceSetup{Outputs: outputs, Layers: layers, Stderr: stderr}, projErr
	}
	outputs := map[string]any{}
	var stderr []byte
	if r.Setup != nil {
		resolved, resolveErr := resolveEffect(r.Setup, setupRoots(ctx), ctx, r.From, nil)
		if resolveErr != nil {
			return InstanceSetup{}, fmt.Errorf("setup: %w", resolveErr)
		}
		stdout, capturedStderr, runErr := resolved.Run(goCtx, session.WorkspaceDirPath)
		resolved.Close()
		stderr = capturedStderr
		if runErr != nil {
			return InstanceSetup{Stderr: stderr}, fmt.Errorf("setup: %w", runErr)
		}
		parsed, parseErr := lang.ParseOutputs(stdout)
		if parseErr != nil {
			return InstanceSetup{Stderr: stderr}, fmt.Errorf("setup: %w", parseErr)
		}
		outputs = parsed
	}
	if r.OutputsSchema != nil {
		if vErr := r.OutputsSchema.Validate(outputs); vErr != nil {
			return InstanceSetup{Stderr: stderr}, fmt.Errorf("setup: outputs schema: %w", vErr)
		}
	}
	return InstanceSetup{Outputs: outputs, Stderr: stderr}, nil
}

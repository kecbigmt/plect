package effect

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// ChainHost is what a nesting chain's execution needs from its caller beyond
// what a Layer already declares about itself: per-layer root-building and
// capability resolution, plus a report for a cleanup step the chain chose
// not to run. The caller owns its own session/task-DAG context; this package
// never sees that context, only the closures derived from it — the same
// one-way rule Capabilities already draws for a single action.
type ChainHost struct {
	// SetupRoots builds the roots one layer's setup action resolves
	// against, given the inputs this layer receives.
	SetupRoots func(layer Layer, inputs map[string]any) lang.Roots
	// CleanupRoots builds the roots one layer's cleanup action resolves
	// against, given its own recorded inputs and its projected public
	// contract at cleanup time.
	CleanupRoots func(layer Layer, self, inputs map[string]any) lang.Roots
	// InnerRoots builds the roots an `[id.inner]` joint's values resolve
	// against: the layer's own inputs plus what its setup just emitted as
	// locals.
	InnerRoots func(layer Layer, inputs, locals map[string]any) lang.Roots
	// Caps resolves one layer's execution capabilities (bin refs, terminal
	// verbs) from its own declaration.
	Caps func(layer Layer) Capabilities
	// OnSkip reports a cleanup step the chain chose not to run: a layer
	// already cleaned, or one the current definition no longer declares.
	OnSkip func(msg string)
}

// RunLayers runs a nesting chain's setup outside-in, returning one state
// record per layer that reached an outcome. The records are returned on
// failure too: a layer that produced must be unwound by the next cleanup, so
// the caller persists them either way.
func RunLayers(goCtx context.Context, layers []Layer, host ChainHost, workDir string, nodeInputs map[string]any) ([]contract.LayerState, []byte, error) {
	states := make([]contract.LayerState, 0, len(layers))
	inputs := orEmpty(nodeInputs)
	var env []string
	var lastStderr []byte

	for i, layer := range layers {
		now := time.Now()
		innermost := i == len(layers)-1
		// The outermost layer's inputs are the node's own, already validated
		// against the composed task's inputs schema by the caller; every
		// layer inward answers to its own schema for what the joint bound.
		if i > 0 && layer.InputsSchema != nil {
			if err := layer.InputsSchema.Validate(orEmpty(inputs)); err != nil {
				return states, lastStderr, fmt.Errorf("layer %q: bound inputs: %w", layer.EffectID, err)
			}
		}
		state := contract.LayerState{EffectID: layer.EffectID, Inputs: inputs, SetupAt: now}

		emitted := map[string]any{}
		if layer.Setup != nil {
			resolved, resolveErr := Resolve(layer.Setup, host.SetupRoots(layer, inputs), host.Caps(layer), nil)
			if resolveErr != nil {
				state.Status = contract.TaskStatusFailed
				state.FailedAt = now
				state.Error = resolveErr.Error()
				return append(states, state), lastStderr, fmt.Errorf("layer %q setup: %w", layer.EffectID, resolveErr)
			}
			stdout, stderr, runErr := resolved.Run(goCtx, workDir, env...)
			resolved.Close()
			lastStderr = stderr
			if runErr != nil {
				state.Status = contract.TaskStatusFailed
				state.FailedAt = now
				state.Error = runErr.Error()
				return append(states, state), stderr, fmt.Errorf("layer %q setup: %w", layer.EffectID, runErr)
			}
			parsed, parseErr := lang.ParseOutputs(stdout)
			if parseErr != nil {
				state.Status = contract.TaskStatusFailed
				state.FailedAt = now
				state.Error = parseErr.Error()
				return append(states, state), stderr, fmt.Errorf("layer %q setup: %w", layer.EffectID, parseErr)
			}
			emitted = parsed
		}
		if innermost {
			if layer.OutputsSchema != nil {
				if err := layer.OutputsSchema.Validate(emitted); err != nil {
					state.Status = contract.TaskStatusFailed
					state.FailedAt = now
					state.Error = err.Error()
					return append(states, state), lastStderr, fmt.Errorf("layer %q setup: outputs schema: %w", layer.EffectID, err)
				}
			}
			state.Outputs = emitted
		} else {
			if layer.LocalsSchema != nil {
				if err := layer.LocalsSchema.Validate(emitted); err != nil {
					state.Status = contract.TaskStatusFailed
					state.FailedAt = now
					state.Error = err.Error()
					return append(states, state), lastStderr, fmt.Errorf("layer %q setup: locals schema: %w", layer.EffectID, err)
				}
			}
			state.Locals = emitted
		}
		state.Status = contract.TaskStatusProduced

		if !innermost {
			// A joint that fails to render leaves this layer produced, not
			// failed: its setup ran and took effect, so the unwind still owes
			// it a cleanup. The node-level state carries why the chain stopped.
			jointEnv := host.InnerRoots(layer, inputs, emitted)
			caps := host.Caps(layer)
			injected, err := ResolveValues(layer.InnerEnv, jointEnv, caps)
			if err != nil {
				return append(states, state), lastStderr, fmt.Errorf("layer %q inner.env: %w", layer.EffectID, err)
			}
			bound, err := ResolveValues(layer.InnerInputs, jointEnv, caps)
			if err != nil {
				return append(states, state), lastStderr, fmt.Errorf("layer %q inner.inputs: %w", layer.EffectID, err)
			}
			state.Env = injected
			env = append(env, envAssignments(injected)...)
			inputs = make(map[string]any, len(bound))
			for k, v := range bound {
				inputs[k] = v
			}
		}
		states = append(states, state)
	}
	return states, lastStderr, nil
}

// RunLayerCleanup unwinds a chain inside-out. A layer that never reached
// setup has no record here at all, which is what "cleanup skips layers that
// never reached setup" amounts to; every layer that does have a record is
// owed an attempt until it is cleaned, exactly as a plain task whose cleanup
// failed is re-attempted rather than written off. It keeps going past a
// failure — an inner layer that refuses to release must not strand the outer
// layers that wrap it — and returns the first error.
//
// A layer left uncleaned is always an error, even when nothing was run for
// it: the node's status is what `plect status` reads long after the
// observer's line has scrolled away, so `cleaned` must never be reachable
// while a record still owes a release.
func RunLayerCleanup(goCtx context.Context, layers []Layer, states []contract.LayerState, host ChainHost, workDir string) ([]byte, error) {
	var firstErr error
	var lastStderr []byte
	for i := len(states) - 1; i >= 0; i-- {
		state := &states[i]
		if state.Status == contract.TaskStatusCleaned {
			host.OnSkip(fmt.Sprintf("layer %q: already cleaned", state.EffectID))
			continue
		}
		if i >= len(layers) || layers[i].EffectID != state.EffectID {
			// The definition's chain no longer matches what was set up.
			// Running a script from a layer that is not the one recorded
			// would release the wrong thing, so the record stands and the
			// debt stays visible.
			host.OnSkip(fmt.Sprintf("layer %q: definition no longer declares this layer", state.EffectID))
			continue
		}
		now := time.Now()
		if layers[i].Cleanup == nil {
			state.Status = contract.TaskStatusCleaned
			state.CleanedAt = now
			continue
		}
		// A layer's cleanup reads its own public contract, the same view its
		// probes and completion conditions are written against — which is
		// how a layer releases what its setup produced as a private local:
		// by projecting it. The cleanup surface observes no locals root of
		// its own.
		self := layerSelf(layers, states, i)
		resolved, resolveErr := Resolve(layers[i].Cleanup, host.CleanupRoots(layers[i], self, state.Inputs), host.Caps(layers[i]), nil)
		if resolveErr != nil {
			state.Status = contract.TaskStatusFailed
			state.FailedAt = now
			state.Error = resolveErr.Error()
			if firstErr == nil {
				firstErr = fmt.Errorf("layer %q cleanup: %w", state.EffectID, resolveErr)
			}
			continue
		}
		_, stderr, runErr := resolved.Run(goCtx, workDir, EnclosingEnv(states, i)...)
		resolved.Close()
		lastStderr = stderr
		if runErr != nil {
			state.Status = contract.TaskStatusFailed
			state.FailedAt = now
			state.Error = runErr.Error()
			if firstErr == nil {
				firstErr = fmt.Errorf("layer %q cleanup: %w", state.EffectID, runErr)
			}
			continue
		}
		state.Status = contract.TaskStatusCleaned
		state.CleanedAt = now
		state.Error = ""
	}
	for i := range states {
		if states[i].Status != contract.TaskStatusCleaned && firstErr == nil {
			firstErr = fmt.Errorf("layer %q is not released (%s)", states[i].EffectID, states[i].Status)
		}
	}
	return lastStderr, firstErr
}

// layerSelf is one layer's own public contract as of the recorded state. A
// chain whose records no longer line up with the declaration cannot be
// projected, so that layer falls back to what it recorded directly.
func layerSelf(layers []Layer, states []contract.LayerState, i int) map[string]any {
	if len(states) != len(layers) {
		return states[i].Outputs
	}
	views, err := ProjectLayerOutputs(layers, states, zeroCaps)
	if err != nil {
		return states[i].Outputs
	}
	return views[i]
}

// EnclosingEnv is the process environment the layers outside index i inject
// into its executions: its setup and cleanup, and equally its probes, output
// scripts, and terminal operation commands.
func EnclosingEnv(states []contract.LayerState, i int) []string {
	var env []string
	for _, outer := range states[:i] {
		env = append(env, envAssignments(outer.Env)...)
	}
	return env
}

// envAssignments renders a bind.env map as sorted KEY=VALUE assignments;
// sorted so a recorded execution is reproducible rather than map-ordered.
func envAssignments(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// orEmpty normalizes a nil map to an empty one so a rendered root is
// absent-but-present rather than a nil that a template would panic on.
func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

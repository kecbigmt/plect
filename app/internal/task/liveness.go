package task

import (
	"context"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/effect"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// verifyLiveness runs a produced node's declared `[health.alive]` probe(s)
// and reports whether its record may still be reused. A plain node runs its
// own probe against its stored outputs; a nested node composes every layer's
// probe by AND, since liveness is a chain of necessary resources — the first
// failing layer's error, named by its own effect id, is what invalidates the
// whole node. A layer or plain node declaring no probe at all is vacuous: it
// never blocks reuse.
//
// A value the probe reads that fails to resolve (e.g. an
// `self.outputs.<key>` no longer present) is returned as an error exactly
// like a failing exit code: the node is invalid, not skipped.
func verifyLiveness(goCtx context.Context, r Resolved, session SessionVars, existing *contract.TaskState) error {
	if len(r.Layers) == 0 {
		action := r.Health.AliveProbe()
		if action == nil {
			return nil
		}
		return RunAliveProbe(goCtx, Probe{
			Action:     action,
			Self:       existing.Outputs,
			Inputs:     existing.Inputs,
			SourcePath: r.SourcePath,
			From:       r.From,
		}, session)
	}
	if len(existing.Layers) != len(r.Layers) {
		return fmt.Errorf("nesting chain has %d layers but %d layer records", len(r.Layers), len(existing.Layers))
	}
	views, err := ProjectLayerOutputs(r.Layers, existing.Layers, session)
	if err != nil {
		return err
	}
	for i, layer := range r.Layers {
		action := layer.Health.AliveProbe()
		if action == nil {
			continue
		}
		probe := Probe{
			Action:     action,
			Self:       views[i],
			Inputs:     existing.Layers[i].Inputs,
			SourcePath: layer.SourcePath,
			From:       layer.From,
			Env:        effect.EnclosingEnv(existing.Layers, i),
		}
		if err := RunAliveProbe(goCtx, probe, session); err != nil {
			return fmt.Errorf("layer %q: %w", layer.EffectID, err)
		}
	}
	return nil
}

// invalidateProducedNode marks r failed with the liveness error that
// disqualified it from reuse, then cleans r and every node in ordered that
// transitively depends on it, in reverse dependency order — the same
// direction a teardown unwinds in. The walk that called this resumes at r's
// own position once cleanup returns, and finds r (and any cleaned dependent
// reached later in ordered) no longer "produced", so the ordinary setup path
// rebuilds them in dependency order.
func invalidateProducedNode(goCtx context.Context, r Resolved, ordered []Resolved, aliveErr error, session SessionVars, tasks map[string]*contract.TaskState, obs Observer) error {
	if existing := tasks[r.NodeID]; existing != nil {
		existing.Status = contract.TaskStatusFailed
		existing.Error = aliveErr.Error()
		existing.FailedAt = time.Now()
	}
	toClean := transitiveDependents(r.NodeID, ordered)
	if err := RunCleanup(goCtx, toClean, session, tasks, obs); err != nil {
		return fmt.Errorf("node %q: liveness check failed (%v), cleanup: %w", r.NodeID, aliveErr, err)
	}
	return nil
}

// transitiveDependents returns nodeID's own entry in ordered followed by
// every node that transitively depends on it (directly, or through another
// dependent), in ordered's own dependency-respecting order. ordered is
// already topologically sorted, so a dependent always sorts after what it
// depends on — filtering ordered by set membership therefore needs no
// re-sort.
func transitiveDependents(nodeID string, ordered []Resolved) []Resolved {
	children := make(map[string][]string, len(ordered))
	for _, r := range ordered {
		for _, dep := range r.DependsOn {
			children[dep] = append(children[dep], r.NodeID)
		}
	}
	seen := map[string]bool{nodeID: true}
	queue := []string{nodeID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range children[cur] {
			if !seen[child] {
				seen[child] = true
				queue = append(queue, child)
			}
		}
	}
	out := make([]Resolved, 0, len(seen))
	for _, r := range ordered {
		if seen[r.NodeID] {
			out = append(out, r)
		}
	}
	return out
}

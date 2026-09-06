package task

import (
	"context"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/effect"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// verifyLiveness composes a nesting chain's layers by AND, since liveness is
// a chain of necessary resources, and treats a probe value that fails to
// resolve the same as a failing exit code: either way the node cannot be
// verified, so it must not be skipped.
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

// invalidateProducedNode stamps the liveness error onto r before cleanup
// runs, so a cleanup failure midway still leaves that reason on disk rather
// than only in a return value neither `plect status` nor a retry can see.
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

// transitiveDependents filters ordered down to nodeID and whatever
// transitively depends on it. ordered is already topologically sorted, so
// membership filtering alone preserves dependency order — no re-sort needed.
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

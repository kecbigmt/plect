// Package dispatch is the session dispatcher: one per active session, hosted in
// the resident `plect serve` process. It reads a session's event log from a
// durable read cursor (so it survives session down/up) and fans each event out
// to the workflow's [[event.channel]] workers whose include filter matches,
// delivering via app/internal/channel and recording a plect.channel.error when a
// channel exhausts its retries.
package dispatch

import (
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// channelInputs resolves an [[event.channel]]'s input wiring against the
// session's persisted node outputs — the same surface a node's inputs read,
// so a projection of a node output resolves to the live value. Resolved fresh
// per batch because a down/up re-creates run-scoped outputs. The definition's
// own [input_schema] defaults fill whatever this wiring left unset, so an
// author-declared optional parameter needs no per-workflow line.
func channelInputs(s *domain.Session, ch config.EventChannel, def config.ChannelDefinition) (map[string]any, error) {
	merged := domain.MergedTasks(s)
	deps := make(map[string]map[string]any, len(merged))
	for id, st := range merged {
		if st == nil {
			continue
		}
		out := st.Outputs
		if out == nil {
			out = map[string]any{}
		}
		deps[id] = out
	}
	var wfOutputs map[string]any
	if st := s.Nodes[contract.WorkflowPseudoNodeID]; st != nil {
		wfOutputs = st.Outputs
	}
	resolved, err := task.ResolveNodeInputs(ch.Inputs, deps, wfOutputs, task.SessionVars{
		Name:             s.Name,
		ResourceID:       s.ResourceID,
		WorkspaceDirPath: s.WorkspaceDirPath,
		Branch:           domain.SessionBranch(s),
		Inputs:           s.Inputs,
	})
	if err != nil {
		return nil, err
	}
	return def.ApplyInputDefaults(resolved), nil
}

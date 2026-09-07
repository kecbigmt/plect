package service

import (
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// nodeAddresses maps a session's node ids to the addresses the effects behind
// them answer to, read off the workflow the session runs.
//
// A workflow node's instance stores no task id when the two agree, so the node
// id is all a later lookup has — and a node id is not an address, because it
// defaults to a reference's last segment rather than the whole reference. The
// workflow that wrote the reference is the only thing that still knows which
// declaration the node runs.
func nodeAddresses(cfg *config.Config, session *domain.Session) map[string]string {
	if session == nil {
		return nil
	}
	wf := sessionWorkflowConfig(cfg, session.Workflow, session.WorkspaceDirPath)
	if wf == nil {
		return nil
	}
	out := make(map[string]string, len(wf.Nodes))
	for _, node := range wf.Nodes {
		if node.Uses != "" {
			out[node.ID] = node.Uses
		}
	}
	return out
}

// instanceDefinitionAddress answers which declaration an instance runs.
// dynamic reports which of the session's two collections st came from
// (session.Tasks when true, session.Nodes when false) — a caller already
// knows this structurally from where it read st, so it states it directly
// rather than this function trying to infer it from st alone.
//
// A dynamic instance recorded the address its own reference selected, and that
// is authoritative however it compares to the instance key — a `--name` equal
// to the id it instantiated must not be mistaken for a workflow node of that
// name. A workflow node is the other way round: what it stores is the
// definition's bare id, which is not an address, so the workflow that named
// the effect answers for it whether or not the instance stored anything.
func instanceDefinitionAddress(key string, st *contract.TaskState, dynamic bool, nodes map[string]string) string {
	if dynamic && st != nil && st.TaskID != "" {
		return st.TaskID
	}
	if address, ok := nodes[key]; ok {
		return address
	}
	return taskIDForInstance(key, st)
}

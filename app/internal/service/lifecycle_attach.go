package service

import (
	"fmt"
	"os"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// AttachParams holds parameters for Attach.
type AttachParams struct {
	Identifier string
}

// AttachResult is the resolved attach plan. The caller (CLI) is expected to
// hand control to the rendered command via syscall.Exec.
type AttachResult struct {
	SessionName string `json:"session_name"`
	TaskID      string `json:"task_id"`
	Command     string `json:"command"`
}

// Attach resolves the session, locates the effect declaring the attach verb,
// and resolves it against that effect's own outputs. It does not exec
// anything — the CLI handles syscall.Exec so this stays testable.
func Attach(cfg *config.Config, store *state.Store, params AttachParams) (*AttachResult, error) {
	sessionName, session, err := resolveSession(cfg, store, params.Identifier)
	if err != nil {
		return nil, err
	}

	plan, err := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}

	target := plan.TerminalTask()
	if target == nil {
		return nil, &Error{Code: ErrNotAttachable, Message: "this workflow has no attach target"}
	}

	// Resolved.Config is empty for nodes synthesized from workflow files (only
	// legacy inline `[[tasks]]` populates it). Reach for NodeID/Terminal
	// directly so the workflow path renders the right command instead of
	// looking up `session.Nodes[""]`.
	st, ok := session.Nodes[target.NodeID]
	if !ok || st == nil || st.Status != contract.TaskStatusProduced {
		return nil, &Error{
			Code:    ErrNotProduced,
			Message: fmt.Sprintf("task %q is not produced; run 'plect up %s' first", target.NodeID, sessionName),
		}
	}

	// The run directory a shell attach verb materializes into is deliberately
	// left behind: the caller replaces its own process with this command, so
	// nothing of ours runs afterwards to remove it.
	dir, err := os.MkdirTemp("", "plect-attach-")
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	cmdStr, err := task.TerminalCommand(terminalBinding(plan, session), "attach", sessionVars(cfg, session, plan), dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("attach: %v", err)}
	}

	return &AttachResult{
		SessionName: sessionName,
		TaskID:      target.NodeID,
		Command:     cmdStr,
	}, nil
}

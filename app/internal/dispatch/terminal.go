package dispatch

import (
	"log/slog"

	"github.com/kecbigmt/plecture/app/internal/channel"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// terminalOwner is the workflow's terminal-declaring effect, resolved once
// per dispatcher build — the same static answer buildDispatcher already
// resolves other workflow-level facts (channels, environment executor) at.
type terminalOwner struct {
	NodeID     string
	Ops        *config.TerminalConfig
	Layers     []effect.Layer
	SourcePath string
	From       lang.Ownership
}

// resolveTerminalOwner finds it. A load or compile failure is logged and
// treated as "no terminal effect": every other channel still delivers; only
// one that actually consumes a terminal verb would then fail per-delivery
// with a clear error, the same as a channel referencing an unwired input.
func resolveTerminalOwner(logger *slog.Logger, cfg *config.Config, s *domain.Session, wf config.WorkflowFile) *terminalOwner {
	defs, err := cfg.LoadTaskDefinitions(s.WorkspaceDirPath)
	if err != nil {
		logger.Warn("event channel terminal capability unavailable; load effect declarations failed",
			"session", s.Name, "workflow", wf.ID, "error", err)
		return nil
	}
	plan, err := task.CompileWorkflow(wf, defs)
	if err != nil {
		logger.Warn("event channel terminal capability unavailable; compile plan failed",
			"session", s.Name, "workflow", wf.ID, "error", err)
		return nil
	}
	t := plan.TerminalTask()
	if t == nil {
		return nil
	}
	return &terminalOwner{NodeID: t.NodeID, Ops: t.Terminal, Layers: t.Layers, SourcePath: t.SourcePath, From: t.From}
}

// terminalResolver builds the terminal-capability closure for one event
// delivery: it resolves the declaring effect's verbs against that effect's
// own CURRENT outputs, re-read from s.Nodes fresh (not cached at
// dispatcher-build time) since a down/up recreates them — the same reason
// channelInputs re-reads the session's outputs per drain instead of once.
// Returns nil when the workflow declares no such effect, so a channel that
// consumes no terminal verb is unaffected.
func terminalResolver(s *domain.Session, owner *terminalOwner, session task.SessionVars) channel.TerminalResolver {
	if owner == nil {
		return nil
	}
	outputs := map[string]any{}
	if st, ok := s.Nodes[owner.NodeID]; ok && st != nil {
		if self := effect.TerminalSelf(owner.Layers, st); self != nil {
			outputs = self
		}
	}
	binding := &task.TerminalBinding{Ops: owner.Ops, Outputs: outputs, SourcePath: owner.SourcePath, From: owner.From}
	return func(verb, dir string) (string, error) {
		return task.TerminalCommand(binding, verb, session, dir)
	}
}

package effect

import (
	"fmt"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// SubscribeHookVars is the context a workspace provider's
// `subscribe` hook. Like WorkflowHookVars it is deliberately minimal —
// subscribe is a runtime verb that runs from an arbitrary cwd with no
// workspace or task DAG in scope. It forwards only the current session and
// the opaque resource being subscribed, plus the mounted plugin list so the
// hook can name its own plugin's executables through `bin`.
type SubscribeHookVars struct {
	ResourceID  string
	SessionName string
	Plugins     []plugins.Mounted
	// SourcePath is the workspace provider definition's own file path
	// (config.WorkspaceProviderConfig.SourcePath), threaded through so a
	// `bin = "<name>"` in Subscribe can resolve against the workspace
	// provider's containing plugin.
	SourcePath string
	// Branch is the session's workspace branch, when the workspace provider
	// that set up the session produced one. Neutral session context: core
	// has no opinion on what a workspace provider's subscribe hook does with
	// it. Empty when the workspace provider produced no branch, or when the
	// caller has no session workspace in scope. Only Subscribe projects it
	// into the hook's env; a subscribe hook with no use for it is
	// unaffected.
	Branch string
}

// RunWorkspaceProviderSubscribe renders and runs the workspace provider's
// `subscribe` hook. The hook is fire-and-forget: it has no outputs contract
// and no persisted state — it binds the current session to the resource in
// whatever delivery mechanism the workspace provider owns (a resident
// watcher's subscription registry, say). stderr is folded into the error on
// failure for diagnosis.
func RunWorkspaceProviderSubscribe(prov config.WorkspaceProviderConfig, vars SubscribeHookVars) error {
	if prov.Subscribe == nil {
		return fmt.Errorf("workspace provider %q declares no subscribe hook", prov.ID)
	}
	env := lang.Roots{
		"session":  map[string]any{"name": vars.SessionName, "branch": vars.Branch},
		"resource": map[string]any{"id": vars.ResourceID},
	}
	_, stderr, runErr := RunProviderAction(prov.Subscribe, ProviderEval(env, vars.Plugins, vars.SourcePath, prov.Ownership()))
	if runErr != nil {
		if msg := strings.TrimSpace(string(stderr)); msg != "" {
			return fmt.Errorf("workspace provider %q subscribe: %w: %s", prov.ID, runErr, msg)
		}
		return fmt.Errorf("workspace provider %q subscribe: %w", prov.ID, runErr)
	}
	return nil
}

// RunWorkspaceProviderUnsubscribe renders and runs the workspace provider's
// `unsubscribe` hook — the counterpart to RunWorkspaceProviderSubscribe,
// dropping a session's binding to a resource rather than creating one. Same
// fire-and-forget contract: no outputs, no persisted core state.
func RunWorkspaceProviderUnsubscribe(prov config.WorkspaceProviderConfig, vars SubscribeHookVars) error {
	if prov.Unsubscribe == nil {
		return fmt.Errorf("workspace provider %q declares no unsubscribe hook", prov.ID)
	}
	env := lang.Roots{
		"session":  map[string]any{"name": vars.SessionName},
		"resource": map[string]any{"id": vars.ResourceID},
	}
	_, stderr, runErr := RunProviderAction(prov.Unsubscribe, ProviderEval(env, vars.Plugins, vars.SourcePath, prov.Ownership()))
	if runErr != nil {
		if msg := strings.TrimSpace(string(stderr)); msg != "" {
			return fmt.Errorf("workspace provider %q unsubscribe: %w: %s", prov.ID, runErr, msg)
		}
		return fmt.Errorf("workspace provider %q unsubscribe: %w", prov.ID, runErr)
	}
	return nil
}

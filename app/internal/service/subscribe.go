package service

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// SubscribeParams are the inputs to Subscribe. ResourceID is the opaque
// resource the caller wants this session to receive events from. SessionName
// defaults to the ambient pane env ($PLECT_SESSION_NAME, exported by the
// interactive-agent task) when left empty, so a running agent can `plect
// subscribe <url>` without naming itself.
type SubscribeParams struct {
	ResourceID  string
	SessionName string
}

// Subscribe binds the current session to an opaque resource so the session
// starts receiving that resource's events. It is the runtime counterpart to
// the dispatch-time auto-subscribe task: additive, never
// replacing — subscribing a resource another session already watches does not
// take it over.
//
// core stays workspace-provider-agnostic: it matches the resource against
// each workspace provider's resolver to pick the owning workspace provider,
// fills in the current session from the ambient env, and runs that workspace
// provider's `subscribe` hook. Everything resource-specific (registering with
// a resident watcher, say) lives in the hook — core never parses the
// resource.
func Subscribe(cfg *config.Config, store *state.Store, params SubscribeParams) error {
	if strings.TrimSpace(params.ResourceID) == "" {
		return &Error{Code: ErrInvalidInput, Message: "resource id is required"}
	}

	sessionName := params.SessionName
	if sessionName == "" {
		sessionName = os.Getenv("PLECT_SESSION_NAME")
	}
	if sessionName == "" {
		return &Error{Code: ErrInvalidInput, Message: "no session in scope: run inside a plect session pane (PLECT_SESSION_NAME) or pass --session"}
	}
	if err := validateSessionName(sessionName); err != nil {
		return err
	}
	// The subscriber must be a real session: the watcher delivers events into
	// its event log, so a typo'd --session would create a ghost subscription
	// publishing to a name nothing reads. The env-default path always names
	// the caller's own (existing) session; this guards the explicit override.
	session, err := store.GetE(sessionName)
	if err != nil {
		return err
	}
	if session == nil {
		return &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q does not exist", sessionName)}
	}

	prov, err := workspaceProviderForResource(cfg, params.ResourceID)
	if err != nil {
		return err
	}

	if hookErr := effect.RunWorkspaceProviderSubscribe(prov, effect.SubscribeHookVars{
		ResourceID:  params.ResourceID,
		SessionName: sessionName,
		Plugins:     cfg.Plugins,
		SourcePath:  prov.SourcePath,
		Branch:      session.Branch,
	}); hookErr != nil {
		return &Error{Code: ErrExecutionFailed, Message: hookErr.Error()}
	}
	return nil
}

// matchWorkspaceProviders returns every workspace provider whose resolver
// matches the resource, in deterministic (id-sorted) order. It does not
// check which hooks a match declares — callers that need a particular hook
// (subscribe, unsubscribe) check that themselves, since a resource with no
// match, or an unhooked match, means different things to different callers
// (an explicit `plect subscribe` fails outright; the implicit wiring a
// dynamic task instance's `--resource` gets is left unwired instead).
func matchWorkspaceProviders(cfg *config.Config, resource string) ([]config.WorkspaceProviderConfig, error) {
	workspaceProviders, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load workspace providers: %v", err)}
	}
	ids := make([]string, 0, len(workspaceProviders))
	for id := range workspaceProviders {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var matched []config.WorkspaceProviderConfig
	for _, id := range ids {
		prov := workspaceProviders[id]
		if !prov.HasResolver() {
			continue
		}
		re, reErr := regexp.Compile(prov.Match)
		if reErr != nil {
			return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("workspace provider %q resolver match: %v", prov.ID, reErr)}
		}
		if re.MatchString(resource) {
			matched = append(matched, prov)
		}
	}
	return matched, nil
}

// workspaceProviderForResource selects the single workspace provider whose
// resolver matches the resource. Mirrors dispatchResource's no-flag branch
// but keys on the workspace provider's `match` directly (subscribe has no
// workflow): zero matches is an "unknown resource" error, several is
// ambiguity. The chosen workspace provider must declare a `subscribe` hook.
func workspaceProviderForResource(cfg *config.Config, resource string) (config.WorkspaceProviderConfig, error) {
	matched, err := matchWorkspaceProviders(cfg, resource)
	if err != nil {
		return config.WorkspaceProviderConfig{}, err
	}
	switch len(matched) {
	case 0:
		return config.WorkspaceProviderConfig{}, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("no workspace provider matches resource %q", resource)}
	case 1:
		prov := matched[0]
		if prov.Subscribe == nil {
			return config.WorkspaceProviderConfig{}, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("workspace provider %q does not support subscribe", prov.ID)}
		}
		return prov, nil
	default:
		names := make([]string, len(matched))
		for i, p := range matched {
			names[i] = p.ID
		}
		return config.WorkspaceProviderConfig{}, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("resource %q matches multiple workspace providers (%s)", resource, strings.Join(names, ", "))}
	}
}

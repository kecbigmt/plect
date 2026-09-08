package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// lifecycleConfigurationDigest hashes the canonical JSON projection of
// parsed declarations and unresolved binding expressions, never resolved
// values or credentials. outstanding is the caller's own already-resolved
// teardown list -- the same one it goes on to execute -- taken as a
// parameter rather than re-resolved here, so comparison and execution
// always read one shared snapshot.
func lifecycleConfigurationDigest(cfg *config.Config, session *domain.Session, plan *task.Plan, outstanding []task.Resolved) (string, error) {
	upOrder := plan.UpOrder()
	nodes := make(map[string]any, len(upOrder))
	planTaskID := make(map[string]string, len(upOrder))
	for _, r := range upOrder {
		nodes[r.NodeID] = projectResolved(r)
		planTaskID[r.NodeID] = r.TaskID
	}

	for _, r := range outstanding {
		// An unresolved definition contributes nothing; its later repair
		// is what changes the digest, not its current absence.
		if r.Unresolved {
			continue
		}
		if taskID, inPlan := planTaskID[r.NodeID]; inPlan {
			if taskID == r.TaskID {
				// Same node, same retained declaration: projectResolved
				// above already covers it in full.
				continue
			}
			// The desired workflow reused this node id for a different
			// declaration; the outstanding execution still retains the
			// old one by its own identity, so its cleanup is folded in
			// alongside rather than lost to the node-id collision.
			node, _ := nodes[r.NodeID].(map[string]any)
			node["retained_cleanup"] = projectCleanupOnly(r)
			continue
		}
		nodes[r.NodeID] = projectCleanupOnly(r)
	}

	projection := map[string]any{"nodes": nodes}
	workspaceProvider, err := projectWorkspaceProvider(cfg, session)
	if err != nil {
		return "", err
	}
	if workspaceProvider != nil {
		projection["workspace_provider"] = workspaceProvider
	}

	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("encode lifecycle configuration projection: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// projectWorkspaceProvider renders the workflow's own reference to its
// workspace provider, and that provider's declared inputs and
// setup/cleanup actions -- none of which task.Resolved carries, since the
// provider runs once per session via the @workflow pseudo-node, outside
// the plan's ordinary node list. nil, nil when the session's workflow
// declares none.
func projectWorkspaceProvider(cfg *config.Config, session *domain.Session) (map[string]any, error) {
	wf, err := loadSessionWorkflow(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return nil, fmt.Errorf("load session workflow for lifecycle configuration digest: %w", err)
	}
	if wf.WorkspaceProvider == "" {
		return nil, nil
	}
	workspaceProviders, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		return nil, fmt.Errorf("load workspace providers for lifecycle configuration digest: %w", err)
	}
	prov, ok, err := workspaceProviderFor(wf, workspaceProviders)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	m := map[string]any{"reference": wf.WorkspaceProvider, "provider_id": prov.ID}
	if len(wf.WorkspaceProviderInputs) > 0 {
		m["inputs"] = projectLiteral(wf.WorkspaceProviderInputs)
	}
	if prov.Setup != nil {
		m["setup"] = projectAction(prov.Setup)
	}
	if prov.Cleanup != nil {
		m["cleanup"] = projectAction(prov.Cleanup)
	}
	return m, nil
}

// projectResolved deliberately has no working-directory field: every
// action in this codebase runs at the session's one workspace directory,
// so there is no per-node cwd selection to compare.
func projectResolved(r task.Resolved) map[string]any {
	m := map[string]any{
		"task_id": r.TaskID,
		"scope":   r.Scope,
	}
	if len(r.DependsOn) > 0 {
		m["depends_on"] = r.DependsOn
	}
	if r.Setup != nil {
		m["setup"] = projectAction(r.Setup)
	}
	if r.Cleanup != nil {
		m["cleanup"] = projectAction(r.Cleanup)
	}
	if r.Health != nil {
		m["health"] = projectHealth(r.Health)
	}
	if r.Terminal != nil {
		m["terminal"] = projectTerminal(r.Terminal)
	}
	if len(r.Inputs) > 0 {
		m["inputs"] = projectValues(r.Inputs)
	}
	if len(r.Layers) > 0 {
		layers := make([]map[string]any, len(r.Layers))
		for i, l := range r.Layers {
			layers[i] = projectLayer(l)
		}
		m["layers"] = layers
	}
	return m
}

// projectCleanupOnly omits setup/liveness/terminal: an outstanding
// execution no longer runs them, so they carry no change signal.
func projectCleanupOnly(r task.Resolved) map[string]any {
	m := map[string]any{"task_id": r.TaskID}
	if r.Cleanup != nil {
		m["cleanup"] = projectAction(r.Cleanup)
	}
	if len(r.Layers) > 0 {
		layers := make([]map[string]any, len(r.Layers))
		for i, l := range r.Layers {
			layers[i] = projectLayerCleanupOnly(l)
		}
		m["layers"] = layers
	}
	return m
}

func projectLayer(l effect.Layer) map[string]any {
	m := map[string]any{"effect_id": l.EffectID}
	if l.Setup != nil {
		m["setup"] = projectAction(l.Setup)
	}
	if l.Cleanup != nil {
		m["cleanup"] = projectAction(l.Cleanup)
	}
	if len(l.InnerInputs) > 0 {
		m["inner_inputs"] = projectValues(l.InnerInputs)
	}
	if len(l.InnerEnv) > 0 {
		m["inner_env"] = projectValues(l.InnerEnv)
	}
	if l.Health != nil {
		m["health"] = projectHealth(l.Health)
	}
	if l.Terminal != nil {
		m["terminal"] = projectTerminal(l.Terminal)
	}
	if binds := projectOutputBinds(l.BindOutputs); binds != nil {
		m["output_binds"] = binds
	}
	return m
}

// projectLayerCleanupOnly still includes output_binds even though the
// layer's own setup no longer runs: a dependent already bound to this
// layer's public output reads it through the binding, so a change to the
// binding changes what that dependent's cleanup observes just as much as a
// change to the cleanup action itself would.
func projectLayerCleanupOnly(l effect.Layer) map[string]any {
	m := map[string]any{"effect_id": l.EffectID}
	if l.Cleanup != nil {
		m["cleanup"] = projectAction(l.Cleanup)
	}
	if binds := projectOutputBinds(l.BindOutputs); binds != nil {
		m["output_binds"] = binds
	}
	return m
}

func projectOutputBinds(bindings []config.OutputBinding) []map[string]any {
	if len(bindings) == 0 {
		return nil
	}
	binds := make([]map[string]any, len(bindings))
	for i, b := range bindings {
		binds[i] = map[string]any{"key": b.Key, "value": projectValue(b.Value)}
	}
	return binds
}

func projectHealth(h *config.HealthConfig) map[string]any {
	m := map[string]any{}
	if h.Alive != nil {
		m["alive"] = projectAction(h.Alive)
	}
	if h.Activity != nil {
		m["activity"] = projectAction(h.Activity)
	}
	return m
}

func projectTerminal(t *config.TerminalConfig) map[string]any {
	m := map[string]any{}
	for _, verb := range []struct {
		name   string
		action *lang.Action
	}{
		{"attach", t.Attach},
		{"capture", t.Capture},
		{"send_text", t.SendText},
		{"send_keys", t.SendKeys},
		{"pid", t.PID},
	} {
		if verb.action != nil {
			m[verb.name] = projectAction(verb.action)
		}
	}
	return m
}

func projectAction(a *lang.Action) map[string]any {
	m := map[string]any{"type": a.Type}
	if a.Bin != "" {
		m["bin"] = a.Bin
	}
	if a.Command != "" {
		m["command"] = a.Command
	}
	if len(a.Args) > 0 {
		args := make([]any, len(a.Args))
		for i, v := range a.Args {
			args[i] = projectValue(v)
		}
		m["args"] = args
	}
	if a.Stdin != nil {
		m["stdin"] = projectValue(a.Stdin)
	}
	if a.Script != "" {
		m["script"] = a.Script
	}
	if len(a.Bind) > 0 {
		m["bind"] = projectValues(a.Bind)
	}
	return m
}

func projectValues(vals map[string]*lang.Value) map[string]any {
	out := make(map[string]any, len(vals))
	for k, v := range vals {
		out[k] = projectValue(v)
	}
	return out
}

// projectValue never uses lang.Value.Source(): its %v literal formatting
// collapses distinct literals to the same text (e.g. the int 1 and the
// float 1.0 both render "1"), and FormJSON's Source() collapses to a
// constant placeholder regardless of content. Each form gets its own
// type-tagged, JSON-safe projection instead.
func projectValue(v *lang.Value) any {
	if v == nil {
		return nil
	}
	switch v.Form {
	case lang.FormFrom:
		m := map[string]any{"form": "from", "from": v.From}
		if v.HasDefault {
			m["default"] = projectLiteral(v.Default)
		}
		if v.Optional {
			m["optional"] = true
		}
		return m
	case lang.FormExpr:
		return map[string]any{"form": "expr", "expr": v.Expr}
	case lang.FormTerminal:
		return map[string]any{"form": "terminal", "terminal": v.Terminal}
	case lang.FormBin:
		return map[string]any{"form": "bin", "bin": v.Bin}
	case lang.FormJSON:
		return map[string]any{"form": "json", "json": projectJSONOperand(v.JSON)}
	default:
		return projectLiteral(v.Literal)
	}
}

// projectLiteral tags a literal leaf with its Go type -- recursing through
// a map or slice without tagging the container itself -- so two literals
// that render identically as text but hold different values (or types)
// never collide in the projection.
func projectLiteral(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = projectLiteral(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = projectLiteral(val)
		}
		return out
	default:
		return map[string]any{"type": fmt.Sprintf("%T", v), "value": v}
	}
}

func projectJSONOperand(op *lang.JSONOperand) any {
	if op == nil {
		return nil
	}
	switch {
	case op.Leaf != nil:
		return projectValue(op.Leaf)
	case op.Object != nil:
		out := make(map[string]any, len(op.Object))
		for k, v := range op.Object {
			out[k] = projectJSONOperand(v)
		}
		return out
	case op.Array != nil:
		out := make([]any, len(op.Array))
		for i, v := range op.Array {
			out[i] = projectJSONOperand(v)
		}
		return out
	default:
		return nil
	}
}

// lifecycleConfigurationNotice only decides whether to warn; the current
// trusted configuration executes regardless. An empty baseline (first or
// legacy execution) never warns.
func lifecycleConfigurationNotice(cfg *config.Config, session *domain.Session, plan *task.Plan, outstanding []task.Resolved) (digest, warning string, err error) {
	digest, err = lifecycleConfigurationDigest(cfg, session, plan, outstanding)
	if err != nil {
		return "", "", err
	}
	if session.LifecycleConfigurationDigest != "" && session.LifecycleConfigurationDigest != digest {
		warning = "lifecycle configuration has changed since this session's last up/down/destroy; executing the current trusted configuration"
	}
	return digest, warning, nil
}

// recordLifecycleConfigurationDigest is called before setup/cleanup starts,
// not after, so a failed execution still leaves the baseline advanced.
func recordLifecycleConfigurationDigest(store *state.Store, sessionName, digest string) error {
	return store.Update(sessionName, func(s *domain.Session) error {
		s.LifecycleConfigurationDigest = digest
		return nil
	})
}

// noticeAndAdvanceBaseline is Up/Down/Destroy's shared call-site logic.
// teardown is the caller's own already-resolved, operation-scoped list --
// the same one it goes on to execute, never re-resolved here -- so
// comparison and execution always share one snapshot. An unresolved
// definition in it blocks the notice and baseline entirely, as a
// precondition failure rather than a configuration change.
func noticeAndAdvanceBaseline(cfg *config.Config, store *state.Store, sessionName string, session *domain.Session, plan *task.Plan, teardown []task.Resolved) (string, error) {
	if hasUnresolvedCleanup(teardown) {
		return "", nil
	}
	digest, warning, err := lifecycleConfigurationNotice(cfg, session, plan, teardown)
	if err != nil {
		return "", err
	}
	if warning != "" {
		slog.Warn(warning, "session", sessionName)
	}
	if err := recordLifecycleConfigurationDigest(store, sessionName, digest); err != nil {
		return "", fmt.Errorf("failed to record lifecycle configuration baseline: %w", err)
	}
	session.LifecycleConfigurationDigest = digest
	return warning, nil
}

// workspaceProviderInputsPrecondition reports whether the workflow's
// declared workspace_provider_inputs validate against that provider's own
// inputs schema, without running any hook. A workflow with no workspace
// provider, or a reference this trusted configuration cannot resolve at
// all, has nothing to precheck here: that absence is reported later, by
// whichever step actually needs the provider to exist.
func workspaceProviderInputsPrecondition(cfg *config.Config, session *domain.Session) error {
	wf, err := loadSessionWorkflow(cfg, session.WorkspaceDirPath, session)
	if err != nil || wf.WorkspaceProvider == "" {
		return nil
	}
	workspaceProviders, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		return nil
	}
	prov, ok, err := workspaceProviderFor(wf, workspaceProviders)
	if err != nil || !ok {
		return nil
	}
	_, err = resolveWorkspaceProviderInputs(prov, wf)
	return err
}

// attachWarning threads warning onto err so a caller that only sees an
// error from Up/Down/Destroy still learns of a lifecycle-configuration
// change noticed before that error occurred. A no-op when warning is
// empty; call it from a deferred function keyed to the named error return,
// so it covers every return point in the caller uniformly.
func attachWarning(err error, warning string) error {
	if err == nil || warning == "" {
		return err
	}
	if svcErr, ok := err.(*Error); ok {
		svcErr.LifecycleConfigurationWarning = warning
		return svcErr
	}
	return &Error{Code: ErrExecutionFailed, Message: err.Error(), LifecycleConfigurationWarning: warning}
}

func hasUnresolvedCleanup(items []task.Resolved) bool {
	for _, r := range items {
		if r.Unresolved {
			return true
		}
	}
	return false
}

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
// parsed declarations and unresolved binding expressions -- never resolved
// values, outputs, or credentials -- so it can run before either exists.
// encoding/json's own key-sorting and whitespace-free output already give
// a canonical encoding. An outstanding execution whose node has left the
// desired workflow is still projected, via its own retained declaration
// identity, so a later repair to its cleanup definition still changes the
// digest.
func lifecycleConfigurationDigest(cfg *config.Config, session *domain.Session, plan *task.Plan) (string, error) {
	upOrder := plan.UpOrder()
	nodes := make(map[string]any, len(upOrder))
	inPlan := make(map[string]bool, len(upOrder))
	for _, r := range upOrder {
		nodes[r.NodeID] = projectResolved(r)
		inPlan[r.NodeID] = true
	}

	teardown, err := unifiedTeardownList(cfg, session, false)
	if err != nil {
		return "", fmt.Errorf("resolve outstanding cleanup for lifecycle configuration digest: %w", err)
	}
	for _, r := range teardown {
		// An unresolved definition contributes nothing; its later repair
		// is what changes the digest, not its current absence.
		if inPlan[r.NodeID] || r.Unresolved {
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

// projectWorkspaceProvider renders the workflow's own workspace-acquisition
// setup/cleanup, which task.Resolved never carries: it runs once per
// session via the @workflow pseudo-node, outside the plan's ordinary node
// list. nil, nil when the session's workflow declares none.
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
	m := map[string]any{}
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
	if len(l.BindOutputs) > 0 {
		binds := make([]map[string]any, len(l.BindOutputs))
		for i, b := range l.BindOutputs {
			binds[i] = map[string]any{"key": b.Key, "value": projectValue(b.Value)}
		}
		m["output_binds"] = binds
	}
	return m
}

func projectLayerCleanupOnly(l effect.Layer) map[string]any {
	m := map[string]any{"effect_id": l.EffectID}
	if l.Cleanup != nil {
		m["cleanup"] = projectAction(l.Cleanup)
	}
	return m
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

// projectLiteral tags a literal with its Go type so two literals that
// render identically as text but hold different values (or types) never
// collide in the projection.
func projectLiteral(v any) map[string]any {
	return map[string]any{"type": fmt.Sprintf("%T", v), "value": v}
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
func lifecycleConfigurationNotice(cfg *config.Config, session *domain.Session, plan *task.Plan) (digest, warning string, err error) {
	digest, err = lifecycleConfigurationDigest(cfg, session, plan)
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

// noticeAndAdvanceBaseline is the shared call-site logic for Up/Down/Destroy.
// It refuses to notice or advance the baseline when operationTeardown holds
// an unresolved definition: that is an execution-precondition failure, not
// a configuration change, and the caller's own cleanup call surfaces it the
// same way regardless of this function ever having run. A produced warning
// is logged immediately (not only returned) because the caller's own
// execution can still fail afterward, and its result is not the only place
// this warning needs to reach an operator.
func noticeAndAdvanceBaseline(cfg *config.Config, store *state.Store, sessionName string, session *domain.Session, plan *task.Plan, operationTeardown []task.Resolved) (string, error) {
	if hasUnresolvedCleanup(operationTeardown) {
		return "", nil
	}
	digest, warning, err := lifecycleConfigurationNotice(cfg, session, plan)
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

func hasUnresolvedCleanup(items []task.Resolved) bool {
	for _, r := range items {
		if r.Unresolved {
			return true
		}
	}
	return false
}

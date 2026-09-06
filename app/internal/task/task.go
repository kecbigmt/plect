// Package task is the completion engine for task documents: compiling a
// workflow's nodes into a Plan, instantiating them, evaluating done_when,
// and tracking each node's persisted contracts/state.TaskState. It compiles
// each setup/cleanup pair declared in config.toml, scoped to either the
// session lifecycle ("session") or the run lifecycle ("run"); tasks can
// depend on each other, and outputs from a setup command (parsed as JSON
// from stdout) are exposed to dependents and the task's own cleanup as
// values over the surface's declared roots.
//
// It is intentionally minimal: sequential execution, no reactivity, no
// dynamic DAG. Scope-aware topological sort, one resolution pass per
// execution. Actually running a resolved action — including walking a task's
// nesting chain, when it declares one — is app/internal/effect's job, not
// this package's: task builds the roots and the per-layer closures a chain
// walk needs, and calls into effect to resolve and run.
package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Resolved represents a workflow node after compilation: a task definition
// instantiated under a node id, with its input bindings, depends_on edges
// derived from those bindings, and the compiled outputs schema.
//
// NodeID is the state key (the field used to index session.Tasks). TaskID
// is the `uses` target — preserved for traceability and to render `.TaskID`
// in templates when a node id has been customized.
type Resolved struct {
	NodeID  string
	TaskID  string
	Scope   string // canonical scope ("session" | "run")
	Setup   *lang.Action
	Cleanup *lang.Action
	// Health is the task's own `[health]` table for a plain (non-nested)
	// node. A nested node's liveness composes from Layers instead, so this
	// is nil whenever len(Layers) > 0.
	Health *config.HealthConfig
	// Terminal is the task's declared `[terminal]` table, or nil for a task
	// that owns no interactive endpoint. See config.TerminalConfig.
	Terminal *config.TerminalConfig
	// Inputs is the node's input wiring: one value per key, resolved at setup
	// time against the node-inputs surface and persisted as the instance's
	// own inputs.
	Inputs        map[string]*lang.Value
	InputsSchema  *jsonschema.Schema // optional: validated against resolved inputs
	OutputsSchema *jsonschema.Schema
	// MutableOutputs lists output keys declared `mutable = true` in the
	// outputs schema. Only these may be updated post-setup via
	// `plect state set-output`; everything else is immutable (safe by default).
	MutableOutputs []string
	DependsOn      []string
	// SourcePath is the effect declaration's own file path
	// (config.TaskDefinition.SourcePath), threaded through so a bare
	// `bin = "<name>"` in an action resolves against the declaring plugin.
	SourcePath string
	// From names the layer that wrote the declaration, which is what decides
	// whether its bin references may name another plugin.
	From lang.Ownership
	// Layers is the node's nesting chain, outermost first, when its task
	// definition declares `inner`. Empty for a plain task, which is the one
	// distinction the lifecycle runners make between the two.
	Layers []effect.Layer
}

// Plan groups tasks by scope, in topo-sorted order.
type Plan struct {
	Session []Resolved // session-scoped nodes, setup order
	Run     []Resolved // run-scoped nodes, setup order
}

// UpOrder is session-scoped nodes followed by run-scoped nodes: a re-up
// must walk both, not just Run, so a workflow-definition upgrade that adds a
// session-scoped node still gets produced against a session created under
// the older definition, instead of staying silently absent forever. Safe to
// call unconditionally — RunSetup skips whatever a prior create/up already
// produced.
func (p *Plan) UpOrder() []Resolved {
	ordered := make([]Resolved, 0, len(p.Session)+len(p.Run))
	ordered = append(ordered, p.Session...)
	ordered = append(ordered, p.Run...)
	return ordered
}

// TerminalTask returns the resolved node that declares a `[terminal]` table,
// or nil if none does. assemblePlan has already enforced at most one such
// declaration per plan, so — unlike the pre-[terminal] Attach/Capture split,
// where only attach carried that compile-time guarantee — this lookup can
// never be ambiguous. `plect attach` / `plect capture` and a
// `{ terminal = "..." }` binding all resolve through this one node.
func (p *Plan) TerminalTask() *Resolved {
	for i, r := range p.Session {
		if r.Terminal != nil {
			return &p.Session[i]
		}
	}
	for i, r := range p.Run {
		if r.Terminal != nil {
			return &p.Run[i]
		}
	}
	return nil
}

// Probe is one health probe and the instance context it resolves against:
// the instance's own outputs, the inputs persisted at setup (needed when a
// probe re-derives a mutable output from an input rather than from its own
// outputs), the file the probe was declared in, and the environment the
// enclosing layers of a nesting chain inject.
//
// SourcePath is one layer's own declaration for a nesting chain and not the
// outermost one: a bare `bin = "<name>"` resolves against the plugin that
// ships the file, so the wrong layer's path would look in the wrong plugin.
type Probe struct {
	Action     *lang.Action
	Self       map[string]any
	Inputs     map[string]any
	SourcePath string
	From       lang.Ownership
	Env        []string
}

func (p Probe) context(session SessionVars) RenderContext {
	return RenderContext{Self: p.Self, Inputs: p.Inputs, Session: session, SourcePath: p.SourcePath}
}

// RunAliveProbe runs one liveness probe. A non-zero exit or a resolution
// failure is returned as an error carrying stderr; nil means the execution
// surface is present. A noop action runs nothing and always reports nil: it
// is a declared non-observation, not a check that happens to pass.
func RunAliveProbe(goCtx context.Context, p Probe, session SessionVars) error {
	if p.Action.Type == lang.ActionNoop {
		return nil
	}
	ctx := p.context(session)
	resolved, err := resolveEffect(p.Action, healthRoots(ctx), ctx, p.From, nil)
	if err != nil {
		return err
	}
	defer resolved.Close()
	_, stderr, err := resolved.Run(goCtx, session.WorkspaceDirPath, p.Env...)
	if err != nil {
		if len(stderr) > 0 {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return err
	}
	return nil
}

// RunCapture runs the plan's capture verb and returns its stdout as-is —
// this is a read-only "raw view", not an outputs contract, so stdout is
// never JSON-parsed. A non-zero exit or a resolution failure is returned as
// an error carrying stderr (e.g. an endpoint that no longer exists), so an
// orphaned consumer never succeeds with empty output.
func RunCapture(goCtx context.Context, binding *TerminalBinding, session SessionVars, env ...string) (string, error) {
	stdout, stderr, err := runTerminalVerb(goCtx, binding, "capture", session, nil, env)
	if err != nil {
		if len(stderr) > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return "", err
	}
	return string(stdout), nil
}

// runTerminalVerb resolves one terminal verb against its declaring effect's
// own contract and runs it, handing operands to the action as its positional
// arguments.
func runTerminalVerb(goCtx context.Context, binding *TerminalBinding, verb string, session SessionVars, operands, env []string) (stdout, stderr []byte, err error) {
	if binding == nil {
		return nil, nil, fmt.Errorf("terminal %q: no effect in this workflow's plan declares an interactive endpoint", verb)
	}
	action, err := binding.Ops.Verb(verb)
	if err != nil {
		return nil, nil, err
	}
	ctx := RenderContext{Self: binding.Outputs, Session: session, SourcePath: binding.SourcePath}
	resolved, err := resolveEffect(action, terminalRoots(binding.Outputs, session), ctx, binding.From, operands)
	if err != nil {
		return nil, nil, err
	}
	defer resolved.Close()
	return resolved.Run(goCtx, session.WorkspaceDirPath, env...)
}

// CompileWorkflow turns a workflow file plus its referenced task definitions
// into an executable Plan. The DAG is derived from `.Nodes.<id>.outputs.<key>`
// references in node input bindings (no explicit depends_on).
//
// Errors are surfaced for:
//   - duplicate node ids
//   - `uses` referencing an unknown task definition
//   - input bindings that reference a missing node
//   - input bindings that reference a non-existent output key (when the
//     upstream task declares an outputs schema)
//   - cycles in the derived DAG
//   - session-scoped nodes depending on run-scoped nodes
//   - more than one node declaring `attach`
func CompileWorkflow(wf config.WorkflowFile, defs map[string]config.TaskDefinition) (*Plan, error) {
	if len(wf.Nodes) == 0 {
		// An empty plan would silently no-op `plect up --workflow foo`, which
		// is more confusing than helpful. Force the author to either declare
		// nodes or delete the file.
		return nil, fmt.Errorf("workflow %q declares no nodes", wf.ID)
	}
	resolved, err := resolveWorkflowNodes(wf, defs)
	if err != nil {
		return nil, err
	}
	return assemblePlan(resolved)
}

func resolveWorkflowNodes(wf config.WorkflowFile, defs map[string]config.TaskDefinition) ([]Resolved, error) {
	out := make([]Resolved, 0, len(wf.Nodes))
	for _, node := range wf.Nodes {
		def, ok := defs[node.Uses]
		if !ok {
			return nil, fmt.Errorf("workflow %q: node %q references unknown effect %q%s", wf.ID, node.ID, node.Uses, config.AddressHint(config.Addresses(defs), node.Uses))
		}
		resolved, err := ResolveDefinition(def, node.ID)
		if err != nil {
			return nil, err
		}
		resolved.Inputs = node.Inputs
		out = append(out, resolved)
	}
	if err := deriveDependsOn(out); err != nil {
		return nil, err
	}
	if err := applyBlocks(out, wf.Nodes); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveDefinition compiles a single task definition into a Resolved under
// the given node id: scope validation, input/output schema compilation, mutable
// key extraction, and done_when template validation. It does not populate
// workflow-specific fields (Inputs / DependsOn) — the workflow compiler sets
// those, and the dynamic `plect task setup` path leaves them empty (it binds
// input values directly rather than via node templates).
func ResolveDefinition(def config.TaskDefinition, nodeID string) (Resolved, error) {
	scope := def.EffectiveScope()
	if scope != config.TaskScopeSession && scope != config.TaskScopeRun {
		return Resolved{}, fmt.Errorf("task %q: invalid scope %q (want %q or %q)",
			def.ID, def.Scope, config.TaskScopeSession, config.TaskScopeRun)
	}
	outputsSchema, err := lang.CompileSchema(def.OutputsSchema, def.ResolvedOutputsSchemaPath(), "plect:task:"+def.ID+":outputs")
	if err != nil {
		return Resolved{}, fmt.Errorf("task %q: outputs schema: %w", def.ID, err)
	}
	mutableOutputs, err := MutableOutputKeys(def.OutputsSchema, def.ResolvedOutputsSchemaPath())
	if err != nil {
		return Resolved{}, fmt.Errorf("task %q: outputs schema: %w", def.ID, err)
	}
	inputsSchema, err := lang.CompileSchema(def.InputsSchema, def.ResolvedInputsSchemaPath(), "plect:task:"+def.ID+":inputs")
	if err != nil {
		return Resolved{}, fmt.Errorf("task %q: input schema: %w", def.ID, err)
	}
	layers, err := effect.ResolveLayers(def)
	if err != nil {
		return Resolved{}, fmt.Errorf("task %q: %w", def.ID, err)
	}
	terminal := def.Terminal
	if len(layers) > 0 {
		// A nested task presents one [terminal], whichever layer declared it:
		// from the outside it is exactly an effect, so nothing downstream
		// asks which layer answered.
		if t := effect.TerminalLayer(layers); t >= 0 {
			terminal = layers[t].Terminal
		}
	}
	return Resolved{
		NodeID:         nodeID,
		TaskID:         def.ID,
		Scope:          scope,
		Setup:          def.Setup,
		Cleanup:        def.Cleanup,
		Health:         def.Health,
		SourcePath:     def.SourcePath,
		From:           def.Ownership(),
		Terminal:       terminal,
		InputsSchema:   inputsSchema,
		OutputsSchema:  outputsSchema,
		MutableOutputs: mutableOutputs,
		Layers:         layers,
	}, nil
}

// applyBlocks turns each WorkflowNode.Blocks entry into a reverse dependency
// edge: if node A lists B in Blocks, B gains A as a dependency, forcing A to
// set up before B (and cleanup after B in reverse order). Scope violations and
// cycles are caught later by assemblePlan's existing checks.
func applyBlocks(nodes []Resolved, wfNodes []config.WorkflowNode) error {
	byID := make(map[string]*Resolved, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	if len(wfNodes) != len(nodes) {
		return fmt.Errorf("internal: applyBlocks length mismatch (%d wfNodes vs %d resolved)", len(wfNodes), len(nodes))
	}
	for i, wfn := range wfNodes {
		if len(wfn.Blocks) == 0 {
			continue
		}
		srcID := nodes[i].NodeID
		for _, target := range wfn.Blocks {
			if target == srcID {
				return fmt.Errorf("node %q: blocks itself", srcID)
			}
			t, ok := byID[target]
			if !ok {
				return fmt.Errorf("node %q: blocks unknown node %q", srcID, target)
			}
			if hasDep(t.DependsOn, srcID) {
				continue
			}
			t.DependsOn = append(t.DependsOn, srcID)
		}
	}
	return nil
}

func hasDep(deps []string, id string) bool {
	for _, d := range deps {
		if d == id {
			return true
		}
	}
	return false
}

// deriveDependsOn fills each node's DependsOn from the node ids the language
// reads out of its input wiring, and validates that those nodes exist and that
// scope ordering holds (a session-scoped node must not depend on a run-scoped
// one, which would outlive it).
func deriveDependsOn(nodes []Resolved) error {
	byID := make(map[string]*Resolved, len(nodes))
	for i := range nodes {
		byID[nodes[i].NodeID] = &nodes[i]
	}
	for i := range nodes {
		n := &nodes[i]
		for _, ref := range lang.NodeReads(n.Inputs) {
			if ref == n.NodeID {
				return fmt.Errorf("node %q reads its own outputs", n.NodeID)
			}
			dep, ok := byID[ref]
			if !ok {
				return fmt.Errorf("node %q reads unknown node %q", n.NodeID, ref)
			}
			if n.Scope == config.TaskScopeSession && dep.Scope == config.TaskScopeRun {
				return fmt.Errorf("node %q (session) depends on %q (run): session-scoped nodes must not depend on run-scoped nodes", n.NodeID, ref)
			}
			n.DependsOn = append(n.DependsOn, ref)
		}
	}
	return nil
}

func assemblePlan(resolved []Resolved) (*Plan, error) {
	byID := make(map[string]Resolved, len(resolved))
	for _, r := range resolved {
		if _, dup := byID[r.NodeID]; dup {
			return nil, fmt.Errorf("duplicate node id %q", r.NodeID)
		}
		byID[r.NodeID] = r
	}

	// At most one [terminal] declaration per plan — the plan has at most one
	// interactive endpoint for plect attach/capture and a terminal binding
	// to resolve against.
	var terminalID string
	for _, r := range resolved {
		if r.Terminal == nil {
			continue
		}
		if terminalID != "" {
			return nil, fmt.Errorf("more than one node declares [terminal]: %q and %q (at most one allowed)", terminalID, r.NodeID)
		}
		terminalID = r.NodeID
	}

	// Validate DependsOn references and scope rule.
	for _, r := range resolved {
		for _, dep := range r.DependsOn {
			d, ok := byID[dep]
			if !ok {
				return nil, fmt.Errorf("node %q depends on unknown node %q", r.NodeID, dep)
			}
			if r.Scope == config.TaskScopeSession && d.Scope == config.TaskScopeRun {
				return nil, fmt.Errorf("node %q (session) depends on %q (run): session-scoped nodes must not depend on run-scoped nodes", r.NodeID, dep)
			}
		}
	}

	sessionList := filterByScope(resolved, config.TaskScopeSession)
	runList := filterByScope(resolved, config.TaskScopeRun)

	sessionSorted, err := topoSortNodes(sessionList)
	if err != nil {
		return nil, err
	}
	runSorted, err := topoSortNodes(runList)
	if err != nil {
		return nil, err
	}

	return &Plan{Session: sessionSorted, Run: runSorted}, nil
}

func filterByScope(resolved []Resolved, scope string) []Resolved {
	var out []Resolved
	for _, r := range resolved {
		if r.Scope == scope {
			out = append(out, r)
		}
	}
	return out
}

// topoSortNodes returns nodes in dependency-respecting order using Kahn's
// algorithm. Dependencies pointing outside `list` are treated as already
// satisfied — run-scoped nodes can depend on session-scoped nodes that live
// in a separate plan.
func topoSortNodes(list []Resolved) ([]Resolved, error) {
	indeg := make(map[string]int, len(list))
	inScope := make(map[string]bool, len(list))
	for _, r := range list {
		inScope[r.NodeID] = true
	}
	for _, r := range list {
		for _, dep := range r.DependsOn {
			if !inScope[dep] {
				continue
			}
			indeg[r.NodeID]++
		}
	}

	// Preserve declaration order among equally-ready nodes for stable output.
	var ready []Resolved
	for _, r := range list {
		if indeg[r.NodeID] == 0 {
			ready = append(ready, r)
		}
	}

	var out []Resolved
	for len(ready) > 0 {
		cur := ready[0]
		ready = ready[1:]
		out = append(out, cur)
		for _, r := range list {
			for _, dep := range r.DependsOn {
				if dep != cur.NodeID {
					continue
				}
				indeg[r.NodeID]--
				if indeg[r.NodeID] == 0 {
					ready = append(ready, r)
				}
			}
		}
	}

	if len(out) != len(list) {
		for _, r := range list {
			if indeg[r.NodeID] > 0 {
				return nil, fmt.Errorf("task dependency cycle detected (involves %q)", r.NodeID)
			}
		}
		return nil, fmt.Errorf("task dependency cycle detected")
	}
	return out, nil
}

// RenderContext is the variable bundle for task templates.
//
//	Self     — current node's outputs (cleanup only).
//	Prev     — previous run's outputs (setup; empty on first run / post-destroy).
//	Tasks  — outputs of upstream nodes, indexed by node id.
//	Inputs   — node's resolved input bindings (post-render), exposed as `.Inputs.<key>`.
//	           Also reachable via `.Nodes.<self>.inputs.<key>` for symmetry.
//	Workflow — the workflow pseudo-node's outputs, exposed as
//	           `.Workflow.outputs.<key>` (workspace_dir, branch, ...). Empty for
//	           sessions whose workflow declares no setup hook.
//
// `.Nodes.<id>.outputs.<key>` is the canonical reference for upstream node
// outputs in setup/cleanup templates; `.Tasks.<id>.<key>` is a shorter
// alias that resolves to the same value.
type RenderContext struct {
	Self     map[string]any
	Prev     map[string]any
	Tasks    map[string]map[string]any
	Inputs   map[string]any
	Workflow map[string]any
	Session  SessionVars
	// Locals are the private intermediates one layer of a nesting chain
	// emitted from its own setup, visible to that layer's cleanup and to its
	// own binding templates and nowhere else. Empty for a plain task.
	Locals map[string]any
	// SourcePath is the absolute path of the file the rendered template
	// (Setup/Cleanup/...) came from. It feeds the bare-name `bin = "<name>"`
	// reading only — plugins.ResolveBin uses it to find the containing plugin — so a
	// caller with no file origin (a test-built template, an attach/probe
	// template not tied to one definition) may safely leave it empty; only
	// bare-name `bin` references stop resolving without it.
	SourcePath string
}

type SessionVars struct {
	Name             string
	ResourceID       string
	ParentSession    string
	WorkspaceDirPath string
	Branch           string
	Inputs           map[string]any
	// Plugins are the resolved, catalog-qualified plugins
	// (config.Config.Plugins) in declaration order, against which a `bin`
	// reference resolves. Threaded through SessionVars rather than added as
	// a separate parameter to every render-driving function, since a
	// SessionVars is already built and passed at every one of those call
	// sites.
	Plugins []plugins.Mounted
	// Terminal is the session plan's [terminal]-declaring task, if any,
	// supplying the terminal capability the same way Plugins supplies the
	// executable one. Nil when the plan declares no such task, or the
	// caller has no full plan in scope (e.g. a dynamic instance's own
	// cleanup) — a terminal binding then fails with a clear error instead
	// of silently resolving to an empty command.
	Terminal *TerminalBinding
}

// TerminalBinding pairs the plan's terminal-declaring effect's verbs with
// that effect's own current outputs — what a verb's own values are resolved
// against — and with the identity a `bin` inside a verb resolves through.
type TerminalBinding struct {
	Ops        *config.TerminalConfig
	Outputs    map[string]any
	SourcePath string
	From       lang.Ownership
}

// normalizeOutputs applies lang.NormalizeNumbers to a whole outputs map,
// named locally so every task-package call site reads as "prepare this
// outputs map for a root" rather than naming the shared helper directly.
func normalizeOutputs(m map[string]any) map[string]any {
	return lang.NormalizeOutputs(m)
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// workflowOutputs extracts the workflow pseudo-node's outputs from the
// persisted tasks map (empty when the workflow declares no setup hook).
func workflowOutputs(tasks map[string]*contract.TaskState) map[string]any {
	if st, ok := tasks[contract.WorkflowPseudoNodeID]; ok && st != nil {
		return st.Outputs
	}
	return nil
}

// dependencyOutputs builds the .Tasks.<id> / .Nodes.<id>.outputs map for the
// given node from the persisted TaskState entries, restricted to declared
// dependencies.
func dependencyOutputs(deps []string, tasks map[string]*contract.TaskState) map[string]map[string]any {
	out := make(map[string]map[string]any, len(deps))
	for _, dep := range deps {
		if state, ok := tasks[dep]; ok && state != nil {
			if state.Outputs != nil {
				out[dep] = state.Outputs
			} else {
				out[dep] = map[string]any{}
			}
		}
	}
	return out
}

// Observer receives lifecycle events from RunSetup / RunCleanup. The runner
// only emits "what happened"; observers decide what to do with the events —
// render a progress UI, ship to telemetry, write a structured log, etc.
// A nil observer is treated as no-op.
//
// Events fire once per task, in execution order. OnStart precedes either
// OnSuccess or OnFailure; OnSkip is the alternative when an already-produced
// task is short-circuited (no OnStart for skipped tasks).
//
// OnSuccess and OnFailure receive the script's captured stderr so observers
// can surface diagnostic output (warnings, error context) without it racing
// against an animated spinner mid-run. The slice is nil when no script ran
// (e.g. empty setup body or template-render failures before exec).
type Observer interface {
	OnStart(scope, id string)
	OnSkip(scope, id, reason string)
	OnSuccess(scope, id string, elapsed time.Duration, stderr []byte)
	OnFailure(scope, id string, elapsed time.Duration, err error, stderr []byte)
}

type nopObserver struct{}

func (nopObserver) OnStart(string, string)                                 {}
func (nopObserver) OnSkip(string, string, string)                          {}
func (nopObserver) OnSuccess(string, string, time.Duration, []byte)        {}
func (nopObserver) OnFailure(string, string, time.Duration, error, []byte) {}

func observerOr(o Observer) Observer {
	if o == nil {
		return nopObserver{}
	}
	return o
}

// RunSetup executes the setup commands for the provided ordered task list
// against the given session. Outputs are persisted into session.Tasks.
// Stops at the first failure; subsequent tasks in the slice are not run.
//
// RunSetup is idempotent: a task whose persisted state is already
// "produced" is verified before it is reused, not blindly skipped — see
// verifyLiveness and invalidateProducedNode. Tasks in any other state
// (absent, "failed", "cleaned") are re-run with a fresh setup attempt. Task
// authors must make their setup scripts cope with this by verifying the
// desired state rather than blindly recreating; see README "Task model"
// section.
func RunSetup(goCtx context.Context, ordered []Resolved, session SessionVars, tasks map[string]*contract.TaskState, observer Observer) error {
	obs := observerOr(observer)
	terminalOwner := terminalOwnerIn(ordered)
	for _, r := range ordered {
		session = withFreshTerminalOutputs(session, terminalOwner, tasks)
		if existing, ok := tasks[r.NodeID]; ok && existing != nil && existing.Status == contract.TaskStatusProduced {
			if aliveErr := verifyLiveness(goCtx, r, session, existing); aliveErr == nil {
				obs.OnSkip(r.Scope, r.NodeID, "already produced")
				continue
			} else if invalidateErr := invalidateProducedNode(goCtx, r, ordered, aliveErr, session, tasks, obs); invalidateErr != nil {
				return invalidateErr
			}
		}
		obs.OnStart(r.Scope, r.NodeID)
		now := time.Now()
		// Cleanup flips Status without touching Outputs, so the prior run's
		// outputs survive into the next setup as .Prev. The same invariant must
		// hold across setup retries: a failed run preserves the prior outputs
		// so the next attempt can read .Prev — otherwise scripts that resume
		// from a previous run (an agent's own --resume flag, say) lose their
		// handle on retry.
		var prev map[string]any
		if existing, ok := tasks[r.NodeID]; ok && existing != nil {
			prev = existing.Outputs
		}
		deps := dependencyOutputs(r.DependsOn, tasks)
		resolvedInputs, inputErr := ResolveNodeInputs(r.Inputs, deps, workflowOutputs(tasks), session)
		if inputErr != nil {
			tasks[r.NodeID] = failedState(r, now, inputErr.Error(), prev, nil)
			wrapped := fmt.Errorf("node %q input: %w", r.NodeID, inputErr)
			obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, nil)
			return wrapped
		}
		if r.InputsSchema != nil {
			if vErr := r.InputsSchema.Validate(toJSONShape(resolvedInputs)); vErr != nil {
				tasks[r.NodeID] = failedState(r, now, vErr.Error(), prev, resolvedInputs)
				wrapped := fmt.Errorf("node %q input schema: %w", r.NodeID, vErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, nil)
				return wrapped
			}
		}
		ctx := RenderContext{
			Self:       map[string]any{},
			Prev:       prev,
			Tasks:      deps,
			Inputs:     resolvedInputs,
			Workflow:   workflowOutputs(tasks),
			Session:    session,
			SourcePath: r.SourcePath,
		}
		if len(r.Layers) > 0 {
			layers, stderr, nestErr := effect.RunLayers(goCtx, r.Layers, chainHost(ctx, obs, r.Scope, r.NodeID), ctx.Session.WorkspaceDirPath, resolvedInputs)
			if nestErr != nil {
				// The layers that did produce are persisted with the
				// failure: the next cleanup has to unwind exactly those.
				failed := failedState(r, now, nestErr.Error(), prev, resolvedInputs)
				failed.Layers = layers
				tasks[r.NodeID] = failed
				wrapped := fmt.Errorf("task %q: %w", r.NodeID, nestErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderr)
				return wrapped
			}
			outputs, projErr := projectNestedOutputs(r, layers, session)
			if projErr != nil {
				failed := failedState(r, now, projErr.Error(), prev, resolvedInputs)
				failed.Layers = layers
				tasks[r.NodeID] = failed
				wrapped := fmt.Errorf("task %q: %w", r.NodeID, projErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderr)
				return wrapped
			}
			tasks[r.NodeID] = &contract.TaskState{
				Scope:   r.Scope,
				TaskID:  taskIDFor(r),
				Status:  contract.TaskStatusProduced,
				Inputs:  resolvedInputs,
				Outputs: outputs,
				Layers:  layers,
				Seq:     nextSeq(tasks),
				SetupAt: now,
			}
			obs.OnSuccess(r.Scope, r.NodeID, time.Since(now), stderr)
			continue
		}
		outputs := map[string]any{}
		var stderrCaptured []byte
		if r.Setup != nil {
			resolved, resolveErr := resolveEffect(r.Setup, setupRoots(ctx), ctx, r.From, nil)
			if resolveErr != nil {
				tasks[r.NodeID] = failedState(r, now, resolveErr.Error(), prev, resolvedInputs)
				wrapped := fmt.Errorf("effect %q setup: %w", r.NodeID, resolveErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, nil)
				return wrapped
			}
			stdout, stderr, runErr := resolved.Run(goCtx, session.WorkspaceDirPath)
			resolved.Close()
			stderrCaptured = stderr
			if runErr != nil {
				tasks[r.NodeID] = failedState(r, now, runErr.Error(), prev, resolvedInputs)
				wrapped := fmt.Errorf("task %q setup: %w", r.NodeID, runErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderr)
				return wrapped
			}
			var parseErr error
			outputs, parseErr = lang.ParseOutputs(stdout)
			if parseErr != nil {
				tasks[r.NodeID] = failedState(r, now, parseErr.Error(), prev, resolvedInputs)
				wrapped := fmt.Errorf("task %q setup: %w", r.NodeID, parseErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderr)
				return wrapped
			}
		}
		if r.OutputsSchema != nil {
			if vErr := r.OutputsSchema.Validate(outputs); vErr != nil {
				tasks[r.NodeID] = failedState(r, now, vErr.Error(), prev, resolvedInputs)
				wrapped := fmt.Errorf("task %q setup: outputs schema: %w", r.NodeID, vErr)
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderrCaptured)
				return wrapped
			}
		}
		tasks[r.NodeID] = &contract.TaskState{
			Scope:   r.Scope,
			TaskID:  taskIDFor(r),
			Status:  contract.TaskStatusProduced,
			Inputs:  resolvedInputs,
			Outputs: outputs,
			Seq:     nextSeq(tasks),
			SetupAt: now,
		}
		obs.OnSuccess(r.Scope, r.NodeID, time.Since(now), stderrCaptured)
	}
	return nil
}

// terminalOwnerIn returns the node in this run list that declares
// `[terminal]`, or nil when the plan's declaring node runs elsewhere (a
// different scope, or an earlier pass).
func terminalOwnerIn(ordered []Resolved) *Resolved {
	for i, r := range ordered {
		if r.Terminal != nil {
			return &ordered[i]
		}
	}
	return nil
}

// withFreshTerminalOutputs re-resolves the terminal binding's
// .Self from the live state map. The caller builds the binding once, before
// the pass starts, so a downstream node would otherwise render the verb
// templates against whatever the declaring node's state held back then —
// nothing at all on a fresh create or a --force-recreate, which resets the
// map before this pass repopulates it.
func withFreshTerminalOutputs(session SessionVars, owner *Resolved, tasks map[string]*contract.TaskState) SessionVars {
	if owner == nil || session.Terminal == nil {
		return session
	}
	st, ok := tasks[owner.NodeID]
	if !ok || st == nil {
		return session
	}
	self := effect.TerminalSelf(owner.Layers, st)
	if self == nil {
		return session
	}
	refreshed := *session.Terminal
	refreshed.Outputs = self
	session.Terminal = &refreshed
	return session
}

// NextSeq is the exported form of nextSeq: the next instantiation sequence
// number for a tasks map. Callers persisting a dynamic instance re-stamp Seq
// with this under the state lock, so the value reflects the freshly-read map
// rather than the snapshot the setup ran against (atomic read-modify-write).
func NextSeq(tasks map[string]*contract.TaskState) int {
	return nextSeq(tasks)
}

// nextSeq returns the next instantiation sequence number for the tasks map:
// one past the highest Seq currently recorded. Seq is stamped when a task
// reaches "produced" (workflow pseudo-node, static node, or dynamic instance),
// so teardown can reclaim tasks in reverse-instantiation order regardless of
// scope or origin. Legacy state with no Seq (all zero) leaves later assignments
// starting at 1; the teardown path falls back to plan order in that case.
func nextSeq(tasks map[string]*contract.TaskState) int {
	max := 0
	for _, st := range tasks {
		if st != nil && st.Seq > max {
			max = st.Seq
		}
	}
	return max + 1
}

// taskIDFor returns the task id when it differs from the node id;
// otherwise empty so legacy state files (where node id == task id) round-trip
// without an `task_id` field appearing in JSON.
func taskIDFor(r Resolved) string {
	if r.TaskID == "" || r.TaskID == r.NodeID {
		return ""
	}
	return r.TaskID
}

func failedState(r Resolved, now time.Time, errMsg string, prev, inputs map[string]any) *contract.TaskState {
	return &contract.TaskState{
		Scope:    r.Scope,
		TaskID:   taskIDFor(r),
		Status:   contract.TaskStatusFailed,
		Inputs:   inputs,
		Outputs:  prev,
		FailedAt: now,
		Error:    errMsg,
	}
}

// toJSONShape normalizes a map[string]any (with string-typed leaves from
// RenderInputs) so JSON Schema validation sees the same shape it would after
// a JSON round-trip. Current implementation only stores strings, so this is a
// no-op pass-through; kept as a single seam in case node inputs grow non-string
// support.
func toJSONShape(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// RunCleanup executes cleanup commands for the given ordered slice IN REVERSE.
// Each node's cleanup is run with .Self bound to its persisted outputs and
// .Input bound to the inputs persisted at setup time — so cleanup never
// depends on the original CLI invocation.
// Cleanup errors are collected but do not stop the loop — all cleanups attempt.
func RunCleanup(goCtx context.Context, ordered []Resolved, session SessionVars, tasks map[string]*contract.TaskState, observer Observer) error {
	obs := observerOr(observer)
	var firstErr error
	for i := len(ordered) - 1; i >= 0; i-- {
		r := ordered[i]
		now := time.Now()
		state, ok := tasks[r.NodeID]
		if !ok || state == nil {
			// Nothing to clean — task never ran setup successfully.
			obs.OnSkip(r.Scope, r.NodeID, "no setup state")
			continue
		}
		if state.Status == contract.TaskStatusCleaned {
			obs.OnSkip(r.Scope, r.NodeID, "already cleaned")
			continue
		}
		if len(state.Layers) > 0 {
			obs.OnStart(r.Scope, r.NodeID)
			sess := session
			if state.Resource != "" {
				sess.ResourceID = state.Resource
			}
			base := RenderContext{
				Tasks:    dependencyOutputs(r.DependsOn, tasks),
				Workflow: workflowOutputs(tasks),
				Session:  sess,
			}
			stderr, nestErr := effect.RunLayerCleanup(goCtx, r.Layers, state.Layers, chainHost(base, obs, r.Scope, r.NodeID), base.Session.WorkspaceDirPath)
			if nestErr != nil {
				state.Status = contract.TaskStatusFailed
				state.Error = nestErr.Error()
				state.FailedAt = now
				wrapped := fmt.Errorf("task %q cleanup: %w", r.NodeID, nestErr)
				if firstErr == nil {
					firstErr = wrapped
				}
				obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderr)
				continue
			}
			state.Status = contract.TaskStatusCleaned
			state.CleanedAt = now
			state.Error = ""
			obs.OnSuccess(r.Scope, r.NodeID, time.Since(now), stderr)
			continue
		}
		if r.Cleanup == nil {
			// No cleanup action means the effect ends as soon as we say it
			// does — the entity has been "released" with no work required.
			// Surface this as success rather than skip so the progress UI
			// matches the new status semantics (cleaned ≡ gone).
			state.Status = contract.TaskStatusCleaned
			state.CleanedAt = now
			obs.OnSuccess(r.Scope, r.NodeID, time.Since(now), nil)
			continue
		}
		obs.OnStart(r.Scope, r.NodeID)
		// A dynamic instance's cleanup must see its OWN resource as .ResourceID,
		// the same one its setup bound — not the session's. SessionVars
		// is a value, so copying and overriding is local to this task.
		sess := session
		if state.Resource != "" {
			sess.ResourceID = state.Resource
		}
		ctx := RenderContext{
			Self:       state.Outputs,
			Tasks:      dependencyOutputs(r.DependsOn, tasks),
			Inputs:     state.Inputs,
			Workflow:   workflowOutputs(tasks),
			Session:    sess,
			SourcePath: r.SourcePath,
		}
		resolved, resolveErr := resolveEffect(r.Cleanup, cleanupRoots(ctx), ctx, r.From, nil)
		if resolveErr != nil {
			state.Status = contract.TaskStatusFailed
			state.Error = resolveErr.Error()
			state.FailedAt = now
			wrapped := fmt.Errorf("effect %q cleanup: %w", r.NodeID, resolveErr)
			if firstErr == nil {
				firstErr = wrapped
			}
			obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, nil)
			continue
		}
		_, stderr, runErr := resolved.Run(goCtx, session.WorkspaceDirPath)
		resolved.Close()
		if runErr != nil {
			state.Status = contract.TaskStatusFailed
			state.Error = runErr.Error()
			state.FailedAt = now
			wrapped := fmt.Errorf("task %q cleanup: %w", r.NodeID, runErr)
			if firstErr == nil {
				firstErr = wrapped
			}
			obs.OnFailure(r.Scope, r.NodeID, time.Since(now), wrapped, stderr)
			continue
		}
		state.Status = contract.TaskStatusCleaned
		state.CleanedAt = now
		state.Error = ""
		obs.OnSuccess(r.Scope, r.NodeID, time.Since(now), stderr)
	}
	return firstErr
}

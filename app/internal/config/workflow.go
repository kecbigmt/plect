package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/lang"
)

// WorkflowFile is one `kind = "workflow"` declaration, loaded from a
// definition document under a `workflows/` directory in any cascade layer.
//
// A workflow is a named bundle of nodes plus the event channels, display
// values and clocks for the sessions it produces. Each node selects an effect
// through `uses` and binds that effect's inputs; the setup and cleanup graph
// is derived from those bindings, so there is no `depends_on`.
//
// The id is the definition table's name, and `name` is the human-readable
// display label separate from it. `description` is the short summary
// `plect workflow list/show` and the MCP discovery tools present.
type WorkflowFile struct {
	ID string
	// Address is the name this workflow answers to across the cascade — its id
	// alone when the user owns it, its catalog address when a plugin declares
	// it. A session freezes this rather than the id, because the id is what
	// names the session and cannot say which declaration produced it.
	Address string
	// Definition is the declaration this file was decoded from, kept so a
	// reference naming this workflow resolves against what was parsed rather
	// than against a reconstruction of it.
	Definition  *lang.Definition
	Name        string
	Description string
	// WorkspaceProvider names the workspace provider this workflow runs on.
	// The workspace provider owns the resource-kind knowledge — resolver,
	// workspace setup/cleanup, the @workflow outputs contract; the workflow
	// owns the effect shape on top. A workflow without one cannot acquire a
	// workspace, so it cannot back a session.
	WorkspaceProvider string
	// WorkspaceProviderInputs sets the workspace provider's author-declared
	// parameters (its `[inputs_schema]`). The values are literal data: the
	// provider's hooks run before any workspace or node output exists, so
	// there is nothing for a projection to read.
	WorkspaceProviderInputs map[string]any
	// Display declares the values `plect ls` / `show` and the web UI render.
	// They read persisted outputs and the session's inputs only — no network
	// — so their freshness follows whatever cadence updates those outputs.
	Display    map[string]*lang.Value
	AutoSelect *bool
	// MaxUpChildren optionally caps concurrently-up children under a session
	// this workflow produces (see service.reserveChildCapSlot); nil means no cap.
	MaxUpChildren *int
	Nodes         []WorkflowNode
	Populations   []WorkflowPopulation
	Event         WorkflowEvent
	// Tick declares the machine-driven conditions that tick the sessions this
	// workflow produces. Unlike the other WorkflowFile fields, a deeper
	// cascade layer's `[tick]` replaces a shallower layer's wholesale — the
	// deeper-wins policy the ratified language states for runtime tuning
	// tables, not the additive/no-redeclare policy the rest of this struct
	// uses. Nil means no declaration: the session advances only via manual
	// `plect tick` and the judge builtin.
	Tick *TickConfig
	// Healthcheck declares the dedicated health sampling clock for sessions
	// produced by this workflow. Like `[tick]`, it is a deeper-wins
	// whole-table runtime tuning declaration. It names the sampling cycle,
	// not what health means: what each probe observes is an effect-level
	// `[health]` declaration.
	Healthcheck      *HealthcheckConfig
	InputsSchema     map[string]any
	InputsSchemaFile string
	// BaseDir anchors InputsSchemaFile so the resolved location is
	// independent of where the workflow document lives.
	BaseDir    string
	SourcePath string
}

// TickConfig declares when the tick reactor (internal/reactor) ticks a
// session produced by this workflow, on top of the judge builtin trigger
// (docs/wiki/verification-gate.md). Both fields are optional and independent:
// On alone is pure reactive tick; Heartbeat alone is pure periodic refresh;
// both together means notification-driven with a heartbeat backstop; neither
// means manual `plect tick` and the judge builtin are the only drivers.
//
//	[tick]
//	on        = ["resource.*"] # event type globs; a match ticks the session
//	heartbeat = "15m"          # tick if this long has passed since the last tick
//
// On deliberately carries no provider-shaped default in core (any one
// watcher's event types are just one possible value): which event namespaces
// mean "an external resource changed" is a workflow-configuration concern,
// the same way wiring that watcher's channel is.
type TickConfig struct {
	On        []string
	Heartbeat Duration
	// MaxHeartbeat caps the quiet-tick exponential backoff interval
	// (heartbeat * 2^n) the reactor applies when consecutive heartbeat sweeps
	// see no fingerprint change and no inbound event. Zero means the
	// reactor's default (4h) applies — declaring it is optional.
	MaxHeartbeat Duration
}

// DefaultMaxHeartbeat caps the quiet-tick backoff interval when a workflow's
// `[tick]` declares no `max_heartbeat`: standing goals must keep observing
// at some ceiling even after many quiet sweeps, so backoff never fully
// suppresses ticking.
const DefaultMaxHeartbeat = 8 * time.Hour

// MaxHeartbeatOrDefault returns t's declared `max_heartbeat` cap, falling
// back to DefaultMaxHeartbeat when the workflow declares none.
func (t TickConfig) MaxHeartbeatOrDefault() time.Duration {
	if t.MaxHeartbeat.Duration > 0 {
		return t.MaxHeartbeat.Duration
	}
	return DefaultMaxHeartbeat
}

// BackoffInterval computes heartbeat * 2^n capped at max. Doubling
// iteratively (rather than shifting n) keeps this well-defined for large n
// without overflowing time.Duration. Shared by the tick reactor's own
// quiet-tick backoff (internal/reactor) and the heartbeat deadman check
// (internal/service): both must judge a session against the same effective
// interval the reactor is actually honoring right now, not the bare
// declared `heartbeat` value, or a session legitimately backed off from
// quiet reads as a false stall.
func BackoffInterval(base, max time.Duration, n int) time.Duration {
	interval := base
	for i := 0; i < n && interval < max; i++ {
		interval *= 2
	}
	if interval > max {
		interval = max
	}
	return interval
}

// HealthcheckConfig declares the dedicated healthcheck cycle for a workflow.
// Unlike `[tick].heartbeat`, this clock has no quiet backoff: it is the
// health side's stall accelerator, so it keeps sampling even when the
// done_when brake has backed off.
type HealthcheckConfig struct {
	Period         Duration
	StallThreshold Duration
	RenotifyEvery  int
}

// DefaultHealthcheckConfig returns the workflow healthcheck defaults.
func DefaultHealthcheckConfig() HealthcheckConfig {
	period := 5 * time.Minute
	return HealthcheckConfig{
		Period:         Duration{Duration: period},
		StallThreshold: Duration{Duration: 3 * period},
		RenotifyEvery:  3,
	}
}

// NormalizeHealthcheckConfig applies defaults to an optional workflow
// healthcheck declaration.
func NormalizeHealthcheckConfig(in *HealthcheckConfig) HealthcheckConfig {
	out := DefaultHealthcheckConfig()
	if in == nil {
		return out
	}
	if in.Period.Duration > 0 {
		out.Period = in.Period
	}
	if in.StallThreshold.Duration > 0 {
		out.StallThreshold = in.StallThreshold
	} else {
		out.StallThreshold = Duration{Duration: 3 * out.Period.Duration}
	}
	if in.RenotifyEvery > 0 {
		out.RenotifyEvery = in.RenotifyEvery
	}
	return out
}

// WorkflowNode is a single instantiation of an effect within a workflow. ID
// is the state key (independent of the referenced effect's id) and Inputs is
// the value table bound to that effect's inputs.
//
// Blocks declares reverse dependency edges: each listed node id becomes a
// dependent of this one, equivalent to writing the inverse dependency in the
// listed node. It exists for a cascade overlay that must insert itself ahead
// of base nodes it cannot modify (e.g. a workspace-dir-process killer that
// must clean up after the runtime and agent effects).
type WorkflowNode struct {
	ID     string
	Uses   string
	Inputs map[string]*lang.Value
	Blocks []string
}

// WorkflowPopulation declares one desired set of sessions owned by its
// containing workflow and identified below it by Name.
type WorkflowPopulation struct {
	Name             string
	ResourceObserver string
	Query            map[string]any
	Session          PopulationSession
	Uses             []string
	PollEvery        Duration
	ExpireAfter      Duration
	AutoDown         bool
	AutoDestroy      bool
}

func (p WorkflowPopulation) UsesPoll() bool {
	return usesContains(p.Uses, "poll")
}

func (p WorkflowPopulation) UsesSubscribe() bool {
	return usesContains(p.Uses, "subscribe")
}

func usesContains(uses []string, means string) bool {
	for _, u := range uses {
		if u == means {
			return true
		}
	}
	return false
}

type PopulationSession struct {
	Task    string
	Inputs  map[string]*lang.Value
	Destroy PopulationDestroy
}

type PopulationDestroy struct {
	Force bool
}

// WorkflowEvent wraps [[event.channel]] in an [event] table so event-level
// settings can be added later without reshaping the workflow surface.
type WorkflowEvent struct {
	Channel []EventChannel
}

// EventChannel selects a channel definition instead of an effect and adds an
// `include` allowlist of event-type globs, matched by the contracts/event
// MatchType rules. Its inputs are values over the same roots a node's inputs
// read, evaluated at delivery.
//
// Name identifies the channel binding within the workflow; two bindings may
// select the same channel definition under different names and includes.
type EventChannel struct {
	Name    string
	Uses    string
	Inputs  map[string]*lang.Value
	Include []string
}

// TaskDefinition is one `kind = "effect"` declaration, loaded from a
// definition document under a `tasks/` directory in the trusted base layers.
// Effects are reusable — several workflows may reference the same declaration
// via `uses`, and one workflow may instantiate it under different node ids.
//
// The id is the definition table's name. Effects intentionally have no
// `depends_on`: wiring is the workflow's job.
type TaskDefinition struct {
	ID      string
	Scope   string
	Setup   *lang.Action
	Cleanup *lang.Action
	// Inner names the next effect inward, making this declaration the outer
	// layer of a nesting chain: either a plain id (resolved in the merged
	// namespace, or in the declaring plugin's own namespace when the
	// declaration is plugin-authored) or a catalog-qualified reference.
	Inner string
	// InnerInputs and InnerEnv are the joint inward: the input object passed
	// to the inner effect, and the environment added to its executions.
	InnerInputs map[string]*lang.Value
	InnerEnv    map[string]*lang.Value
	// OutputsBind is the joint outward: this layer's public outputs, each
	// projected or computed from an inner output, a local, or an input.
	OutputsBind map[string]*lang.Value
	// LocalsSchema / LocalsSchemaFile is the contract for the private
	// intermediates this layer's setup emits.
	LocalsSchema     map[string]any
	LocalsSchemaFile string
	// InnerChain is the nesting chain inward from this effect, next-inner
	// first and innermost last, stamped by load-time resolution. Empty for a
	// plain effect.
	InnerChain []TaskDefinition
	// Health declares the effect's `[health]` table — the alive and activity
	// probes that determine its contribution to session health. Nil for an
	// effect that declares neither.
	Health *HealthConfig
	// Terminal declares the effect's `[terminal]` table: the effect owns an
	// interactive endpoint and offers attach/capture/send_text/send_keys/pid
	// against it.
	Terminal          *TerminalConfig
	OutputsSchema     map[string]any
	OutputsSchemaFile string
	InputsSchema      map[string]any
	InputsSchemaFile  string
	BaseDir           string
	SourcePath        string
	// FromPlugin says a plugin layer wrote this declaration, which is what
	// decides whether its bin references may name another plugin.
	FromPlugin bool
}

// Ownership names the layer that wrote this declaration, for the reference
// rules that differ between shipped and user-authored config.
func (d TaskDefinition) Ownership() lang.Ownership {
	return lang.Ownership{IsPlugin: d.FromPlugin}
}

// DoneWhen is the structured completion predicate a task definition may
// declare. It is a conjunction of leaves (All) plus an optional Budget.
//
// done_when has two leaf kinds: check leaves compare observed outputs, while
// judge leaves wait for independent reviewer input recorded by `plect judge`.
type DoneWhen struct {
	All    []DoneWhenLeaf `toml:"all" json:"all"`
	Budget map[string]any `toml:"budget" json:"budget,omitempty"`
}

// DoneWhenLeaf is one conjunct of a done_when. It is either a check leaf or a
// judge leaf, exclusively.
//
//		all = [
//		  { check = "checks_status", eq = "SUCCESS" },
//		  { check = "coverage", gte = 80 },
//		  { check = "pr_state", in = ["merged", "closed"] },
//		  { judge = "AC satisfied with reasoning", id = "ac-met" },
//		]
//
//	  - A check leaf names an output (Check) and applies exactly one comparison
//	    operator to that output's value: Eq / Ne (string equality), In
//	    (membership), or Gte / Lte (numeric). Operators are typed in TOML, so a
//	    non-numeric gte/lte is a decode error.
//	  - A judge leaf carries a free-text criterion (Judge) and an optional id.
//	    Relation is the reviewer→work tree relations the leaf accepts a verdict
//	    from (default sibling/parent); self is always rejected, so independence is
//	    structural. Which reviewer workflow runs is a chaining concern, not a leaf
//	    field.
type DoneWhenLeaf struct {
	// Check names one completion key by its root path — `resource.state.<key>`
	// for what the declared observer publishes, `self.state.<key>` for what
	// this task holds.
	Check string   `toml:"check" json:"check,omitempty"`
	Eq    *string  `toml:"eq" json:"eq,omitempty"`
	Ne    *string  `toml:"ne" json:"ne,omitempty"`
	In    []any    `toml:"in" json:"in,omitempty"`
	Gte   *float64 `toml:"gte" json:"gte,omitempty"`
	Lte   *float64 `toml:"lte" json:"lte,omitempty"`
	// Expr states a predicate computed over those same roots, which is how a
	// recorded value is compared against a live one — a comparison with no key
	// of its own to hang on.
	Expr     string   `toml:"expr" json:"expr,omitempty"`
	Judge    string   `toml:"judge" json:"judge,omitempty"`
	ID       string   `toml:"id" json:"id,omitempty"`
	Relation []string `toml:"relation" json:"relation,omitempty"`
}

// IsCheck / IsJudge / IsExpr name which of the three leaf kinds this is.
// Exactly one holds for a valid leaf.
func (l DoneWhenLeaf) IsCheck() bool { return strings.TrimSpace(l.Check) != "" }
func (l DoneWhenLeaf) IsJudge() bool { return strings.TrimSpace(l.Judge) != "" }
func (l DoneWhenLeaf) IsExpr() bool  { return strings.TrimSpace(l.Expr) != "" }

// operatorCount returns how many comparison operators the leaf sets.
func (l DoneWhenLeaf) operatorCount() int {
	n := 0
	if l.Eq != nil {
		n++
	}
	if l.Ne != nil {
		n++
	}
	if l.In != nil {
		n++
	}
	if l.Gte != nil {
		n++
	}
	if l.Lte != nil {
		n++
	}
	return n
}

// Validate checks the structural invariants of a done_when: it declares at
// least one leaf, and each leaf is exactly one of the three kinds — a check
// naming one key and setting one operator, an expression, or a judge.
// Operator value typing (e.g. gte must be numeric) is enforced at TOML decode
// by the field types; which roots a key or an expression may read is the
// language's own check.
func (d *DoneWhen) Validate() error {
	if d == nil {
		return nil
	}
	if len(d.All) == 0 {
		return fmt.Errorf("done_when declares no leaves; add at least one `all` entry or remove the table")
	}
	for i, leaf := range d.All {
		kinds := 0
		for _, is := range []bool{leaf.IsCheck(), leaf.IsExpr(), leaf.IsJudge()} {
			if is {
				kinds++
			}
		}
		ops := leaf.operatorCount()
		switch {
		case kinds > 1:
			return fmt.Errorf("done_when.all[%d] sets more than one of `check`, `expr`, and `judge`; exactly one is allowed", i)
		case leaf.IsJudge():
			if ops > 0 {
				return fmt.Errorf("done_when.all[%d] is a judge leaf and must not set comparison operators (eq/ne/in/gte/lte)", i)
			}
			for _, r := range leaf.Relation {
				if !domain.AssignableJudgeRelation(domain.SessionRelation(r)) {
					return fmt.Errorf("done_when.all[%d] judge relation %q is not assignable; use sibling/parent/child (self is always rejected)", i, r)
				}
			}
		case leaf.IsExpr():
			if ops > 0 {
				return fmt.Errorf("done_when.all[%d] is an expression leaf and must not set comparison operators (eq/ne/in/gte/lte); the expression states the whole predicate", i)
			}
			if len(leaf.Relation) > 0 {
				return fmt.Errorf("done_when.all[%d] expression leaf must not set judge relation policy", i)
			}
		case leaf.IsCheck():
			if ops != 1 {
				return fmt.Errorf("done_when.all[%d] check %q must set exactly one operator (eq/ne/in/gte/lte), got %d", i, leaf.Check, ops)
			}
			if len(leaf.Relation) > 0 {
				return fmt.Errorf("done_when.all[%d] check leaf must not set judge relation policy", i)
			}
		default:
			if ops > 0 {
				return fmt.Errorf("done_when.all[%d] sets a comparison operator without a `check` key", i)
			}
			return fmt.Errorf("done_when.all[%d] sets none of `check`, `expr`, and `judge`; exactly one is required", i)
		}
	}
	return nil
}

// EffectiveScope returns the scope, defaulting to "run" when unset. A nested
// task that declares none takes the innermost task's scope rather than the
// plain-task default: the chain runs as one task, so the layer that owns the
// actual resource decides the lifecycle it belongs to.
func (d TaskDefinition) EffectiveScope() string {
	if d.Scope != "" {
		return d.Scope
	}
	if n := len(d.InnerChain); n > 0 {
		return d.InnerChain[n-1].EffectiveScope()
	}
	return TaskScopeRun
}

// ResolvedOutputsSchemaPath joins OutputsSchemaFile with BaseDir.
func (d TaskDefinition) ResolvedOutputsSchemaPath() string {
	return resolveSchemaPath(d.OutputsSchemaFile, d.BaseDir)
}

// ResolvedInputsSchemaPath joins InputsSchemaFile with BaseDir.
func (d TaskDefinition) ResolvedInputsSchemaPath() string {
	return resolveSchemaPath(d.InputsSchemaFile, d.BaseDir)
}

// ResolvedInputsSchemaPath joins the workflow file's InputsSchemaFile with BaseDir.
func (w WorkflowFile) ResolvedInputsSchemaPath() string {
	return resolveSchemaPath(w.InputsSchemaFile, w.BaseDir)
}

func resolveSchemaPath(file, baseDir string) string {
	if file == "" {
		return ""
	}
	if filepath.IsAbs(file) {
		return file
	}
	if baseDir == "" {
		return file
	}
	return filepath.Join(baseDir, file)
}

// layerDir pairs a cascade search directory with its trust classification.
// The workspace-dir layer (`.plect/` inside the workspace directory itself)
// is clone content — an attacker-controlled repository must not be able to
// introduce shell that plect would execute. Every other layer (plugin,
// global, ancestor overlays above the workspace dir) is machine-owned and
// trusted.
//
// plugin marks a layer sourced from c.PluginDirs specifically (as opposed to
// the global config dir or an ancestor overlay), which is what decides the
// address its declarations answer to: a mounted plugin's carry its catalog
// address, and everything else is addressed by id alone.
type layerDir struct {
	dir          string
	workspaceDir bool
	plugin       bool
	// global marks the machine's own config root, which is trusted the way a
	// plugin layer is but is not one, so a same-id collision with a plugin
	// layer is a replacement rather than an error.
	global bool
	// pluginID is the catalog-qualified identity of the plugin this layer
	// was mounted from, empty for a hand-authored plugin_dirs entry (which
	// carries no catalog identity) and for the non-plugin layers.
	pluginID string
}

// LoadWorkflows loads every workflow declaration the cascade layers hold:
// plugins (base), the global config, then each ancestor overlay ending at the
// workspace directory. A deeper layer merges into a same-id shallower
// workflow by the cascade rules — it may set a field the shallower layer left
// unset, its nodes append, and its `[tick]` / `[healthcheck]` replace the
// shallower table wholesale — so a project extends a shared workflow instead
// of forking it.
//
// Two plugins declaring one id land on separate addresses and both load; a
// user-owned workflow sharing an id with a plugin's does not extend it either,
// for the same reason. Only a deeper user-owned layer merges into a shallower
// one, which is what lets a project extend a workflow it owns.
//
// Trust restriction: a workflow document in the workspace-dir layer is clone
// content and may only add nodes.
func (c *Config) LoadWorkflows(workspaceDirPath string) (map[string]WorkflowFile, error) {
	layers, err := c.discoverLayers(workspaceDirPath)
	if err != nil {
		return nil, err
	}
	// Validation runs per layer rather than once over the merged definition so
	// a diagnostic names the layer that wrote the offending value; the merged
	// definition's own topology is checked when it is decoded.
	for _, discovered := range layers {
		for _, def := range discovered.ofKind(lang.KindWorkflow) {
			if err := c.checkWorkflowDefinition(def, discovered.layer); err != nil {
				return nil, err
			}
		}
	}
	resolved, err := c.resolveNamespace(layers, lang.KindWorkflow)
	if err != nil {
		return nil, err
	}
	out := make(map[string]WorkflowFile)
	for _, entry := range resolved {
		wf, err := workflowFrom(entry.def, entry.source)
		if err != nil {
			return nil, fmt.Errorf("workflow %s in %s: %w", entry.def.ID, entry.source, err)
		}
		wf.Address = entry.address
		if err := canonicalizeWorkflowRefs(&wf, entry.prefix); err != nil {
			return nil, err
		}
		out[entry.address] = wf
	}
	if !workflowsHavePopulations(out) {
		return out, nil
	}
	observers, err := c.LoadResourceDefs()
	if err != nil {
		return nil, err
	}
	docs, err := c.LoadTaskDocuments(workspaceDirPath)
	if err != nil {
		return nil, err
	}
	registry := c.taskReferenceRegistry(docs, observers, out)
	for _, address := range Addresses(out) {
		wf := out[address]
		if len(wf.Populations) == 0 {
			continue
		}
		validation := lang.Validation{From: registry.OwnerOf(wf.Definition), Executables: c.binResolver(wf.SourcePath)}
		if err := validation.ValidatePopulationContracts(wf.Definition, registry); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func workflowsHavePopulations(workflows map[string]WorkflowFile) bool {
	for _, workflow := range workflows {
		if len(workflow.Populations) > 0 {
			return true
		}
	}
	return false
}

// LoadTaskDefinitions merges plugin + global + ancestor `.plect/tasks/`
// layers. Same-id deeper (user-owned) layer wins because setup/cleanup is
// atomic — appending two shell scripts doesn't have a sensible meaning the
// way appending nodes does. Two plugins declaring one id land on separate
// addresses and both load; only a layer with no catalog identity to name it by
// still collides, since there declaration order would be deciding between two
// plugins' shell.
//
// Trust restriction: task definitions are arbitrary shell, so the
// workspace-dir layer (clone content) must not contribute any. A
// `.plect/tasks/*.toml` inside the workspace directory is a load error
// rather than a silent skip — silently ignoring it would make the author
// think the task is active.
func (c *Config) LoadTaskDefinitions(workspaceDirPath string) (map[string]TaskDefinition, error) {
	layers, err := c.discoverLayers(workspaceDirPath)
	if err != nil {
		return nil, err
	}
	resolved, err := c.resolveNamespace(layers, lang.KindEffect)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TaskDefinition)
	for _, entry := range resolved {
		def, err := c.effectFromDefinition(entry.def, entry.fromPlugin)
		if err != nil {
			return nil, err
		}
		inner, err := referenceAddress(entry.prefix, def.Inner)
		if err != nil {
			return nil, fmt.Errorf("effect %q inner: %w", def.ID, err)
		}
		def.Inner = inner
		out[entry.address] = def
	}
	if err := resolveNestedDefinitions(out, c.describeMissingDefinitionPlugin); err != nil {
		return nil, err
	}
	return out, nil
}

// definitionRoots orders the cascade: plugins (base) → global → ancestor
// overlays, outermost-first, ending at the workspace directory itself — the
// only untrusted layer. Each entry is a whole definition root rather than one
// kind's directory, because a directory under a root is author organization.
//
// The workspace-dir layer is walked whole too, but it is not a definition
// root: a declaration it must not carry has to be found in order to be
// refused, and only `.plect/workflows/` contributes (see discoverLayer).
func (c *Config) definitionRoots(workspaceDirPath string) []layerDir {
	pluginIDs := make(map[string]string, len(c.Plugins))
	for _, m := range c.Plugins {
		pluginIDs[m.Dir] = m.ID
	}
	var dirs []layerDir
	for _, plugin := range c.PluginDirs {
		dirs = append(dirs, layerDir{dir: filepath.Join(plugin, "config"), plugin: true, pluginID: pluginIDs[plugin]})
	}
	if c.BaseDir != "" {
		dirs = append(dirs, layerDir{dir: c.BaseDir, global: true})
	}
	cleanWorkspaceDir := ""
	if workspaceDirPath != "" {
		cleanWorkspaceDir = filepath.Clean(workspaceDirPath)
	}
	for _, anc := range cascadeAncestors(workspaceDirPath) {
		dirs = append(dirs, layerDir{dir: filepath.Join(anc, ".plect"), workspaceDir: anc == cleanWorkspaceDir})
	}
	return dirs
}

// cascadeAncestors walks up from workspaceDirPath, ordered outermost-first.
// $HOME is the exclusive upper bound because the user's global config lives
// at `~/.config/plect/` and `$HOME/.plect/` would collide with that.
func cascadeAncestors(workspaceDirPath string) []string {
	if workspaceDirPath == "" {
		return nil
	}
	var cleanHome string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cleanHome = filepath.Clean(home)
	}
	var chain []string
	cur := filepath.Clean(workspaceDirPath)
	for {
		if cur == cleanHome || cur == string(filepath.Separator) {
			break
		}
		chain = append(chain, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// listTOMLFiles returns sorted *.toml entries in dir. A missing dir returns an
// empty list (not an error) so missing repo/global directories are normal.
func listTOMLFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files, nil
}

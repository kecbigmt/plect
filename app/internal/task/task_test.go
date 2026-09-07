package task

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// recordingObserver captures observer events for assertions.
type recordingObserver struct {
	successes []string
	failures  []string
	skips     []string
}

func (r *recordingObserver) OnStart(string, string) {}
func (r *recordingObserver) OnSuccess(_, id string, _ time.Duration, _ []byte) {
	r.successes = append(r.successes, id)
}
func (r *recordingObserver) OnFailure(_, id string, _ time.Duration, _ error, _ []byte) {
	r.failures = append(r.failures, id)
}
func (r *recordingObserver) OnSkip(_, id, _ string) { r.skips = append(r.skips, id) }

// depInput wires a node to its upstream by projecting the upstream's outputs
// in an input binding. CompileWorkflow derives the DAG from these
// projections, so a scenario that used to thread `DependsOn` threads a
// synthetic input instead; the edge follows from the path, so the upstream key
// need not exist.
func depInput(upstream string) map[string]*lang.Value {
	return map[string]*lang.Value{
		"link": fromValue("nodes." + upstream + ".outputs.k"),
	}
}

func TestPlan_DuplicateNodeID(t *testing.T) {
	_, err := tryBuildPlan(
		[]taskStub{{id: "a", scope: "run", setup: "true"}},
		[]nodeStub{{id: "a"}, {id: "a"}},
	)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate id error, got %v", err)
	}
}

func TestPlan_RunMayDependOnSession(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{
			{id: "envfile", scope: "session", setup: "true"},
			{id: "tmux", scope: "run", setup: "true"},
		},
		[]nodeStub{
			{id: "envfile"},
			{id: "tmux", inputs: depInput("envfile")},
		},
	)
	if len(plan.Session) != 1 || plan.Session[0].NodeID != "envfile" {
		t.Fatalf("expected envfile in session plan, got %+v", plan.Session)
	}
	if len(plan.Run) != 1 || plan.Run[0].NodeID != "tmux" {
		t.Fatalf("expected tmux in run plan, got %+v", plan.Run)
	}
}

func TestPlan_TopoSort(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{
			{id: "claude", scope: "run", setup: "true"},
			{id: "tmux", scope: "run", setup: "true"},
		},
		[]nodeStub{
			{id: "claude", inputs: depInput("tmux")},
			{id: "tmux"},
		},
	)
	if got := ids(plan.Run); got[0] != "tmux" || got[1] != "claude" {
		t.Fatalf("expected [tmux claude], got %v", got)
	}
}

func TestPlan_TopoSort_Diamond(t *testing.T) {
	// d depends on b,c; b,c depend on a. Order must be: a first, d last,
	// b,c somewhere between. Catches off-by-one bugs in indegree updates.
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: "true"},
			{id: "b", scope: "run", setup: "true"},
			{id: "c", scope: "run", setup: "true"},
			{id: "d", scope: "run", setup: "true"},
		},
		[]nodeStub{
			{id: "a"},
			{id: "d", inputs: map[string]*lang.Value{
				"b": fromValue("nodes.b.outputs.k"),
				"c": fromValue("nodes.c.outputs.k"),
			}},
			{id: "b", inputs: depInput("a")},
			{id: "c", inputs: depInput("a")},
		},
	)
	order := ids(plan.Run)
	if len(order) != 4 {
		t.Fatalf("expected 4 nodes, got %v", order)
	}
	if order[0] != "a" {
		t.Fatalf("expected a first, got %v", order)
	}
	if order[3] != "d" {
		t.Fatalf("expected d last, got %v", order)
	}
	mid := map[string]bool{order[1]: true, order[2]: true}
	if !mid["b"] || !mid["c"] {
		t.Fatalf("expected {b,c} between a and d, got %v", order)
	}
}

func TestPlan_TopoSort_SiblingDeclarationOrder(t *testing.T) {
	// Independent tasks keep declaration order — users rely on this for
	// "envfile before dotenv" type ergonomics without writing depends_on.
	plan := buildPlan(t,
		[]taskStub{
			{id: "envfile", scope: "run", setup: "true"},
			{id: "dotenv", scope: "run", setup: "true"},
		},
		[]nodeStub{{id: "envfile"}, {id: "dotenv"}},
	)
	if got := ids(plan.Run); got[0] != "envfile" || got[1] != "dotenv" {
		t.Fatalf("expected [envfile dotenv], got %v", got)
	}
	plan2 := buildPlan(t,
		[]taskStub{
			{id: "envfile", scope: "run", setup: "true"},
			{id: "dotenv", scope: "run", setup: "true"},
		},
		[]nodeStub{{id: "dotenv"}, {id: "envfile"}},
	)
	if got := ids(plan2.Run); got[0] != "dotenv" || got[1] != "envfile" {
		t.Fatalf("expected [dotenv envfile], got %v", got)
	}
}

func TestPlan_TopoSort_CrossScope(t *testing.T) {
	// claude depends on tmux (run) AND envfile (session). The run plan must
	// only topo-sort within run scope — envfile must not contribute to claude's
	// indegree, otherwise claude would never become ready in the run plan.
	plan := buildPlan(t,
		[]taskStub{
			{id: "envfile", scope: "session", setup: "true"},
			{id: "claude", scope: "run", setup: "true"},
			{id: "tmux", scope: "run", setup: "true"},
		},
		[]nodeStub{
			{id: "envfile"},
			{id: "claude", inputs: map[string]*lang.Value{
				"tmux":    fromValue("nodes.tmux.outputs.k"),
				"envfile": fromValue("nodes.envfile.outputs.k"),
			}},
			{id: "tmux"},
		},
	)
	if got := ids(plan.Session); len(got) != 1 || got[0] != "envfile" {
		t.Fatalf("session plan: expected [envfile], got %v", got)
	}
	if got := ids(plan.Run); len(got) != 2 || got[0] != "tmux" || got[1] != "claude" {
		t.Fatalf("run plan: expected [tmux claude], got %v", got)
	}
}

func TestPlan_UpOrder(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{
			{id: "envfile", scope: "session", setup: "true"},
			{id: "gh_guard", scope: "session", setup: "true"},
			{id: "tmux", scope: "run", setup: "true"},
			{id: "claude", scope: "run", setup: "true"},
		},
		[]nodeStub{
			{id: "envfile"},
			{id: "gh_guard"},
			{id: "tmux"},
			{id: "claude", inputs: depInput("tmux")},
		},
	)
	got := ids(plan.UpOrder())
	want := []string{"envfile", "gh_guard", "tmux", "claude"}
	if len(got) != len(want) {
		t.Fatalf("UpOrder() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("UpOrder() = %v, want %v", got, want)
		}
	}
}

func ids(rs []Resolved) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.NodeID
	}
	return out
}

func TestPlan_InvalidScope(t *testing.T) {
	_, err := tryBuildPlan(
		[]taskStub{{id: "a", scope: "bogus", setup: "true"}},
		[]nodeStub{{id: "a"}},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid scope") {
		t.Fatalf("expected invalid scope error, got %v", err)
	}
}

// Cleanup after a setup that produced nothing: an output the instance never
// recorded is absent, not empty, so a cleanup that has to tolerate it says
// so. `optional` propagates the absence into the shell as an unset
// variable, which a shell-defensive script tells from a value.
func TestRunCleanup_OptionalSelfOutputPropagatesAbsence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	marker := filepath.Join(t.TempDir(), "seen")
	r := Resolved{NodeID: "a", Scope: config.TaskScopeRun, Cleanup: &lang.Action{
		Type:   lang.ActionShell,
		Script: `printf '%s' "${pid-unset}" > "$marker"`,
		Bind: map[string]*lang.Value{
			"pid":    {Form: lang.FormFrom, From: "self.outputs.pid", Optional: true},
			"marker": {Form: lang.FormLiteral, Literal: marker},
		},
	}}
	tasks := map[string]*contract.TaskState{
		"a": {Scope: config.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunCleanup(context.Background(), []Resolved{r}, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("RunCleanup: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if string(data) != "unset" {
		t.Errorf("cleanup saw %q, want the variable left unset", data)
	}
}

// The other half of the same rule: a cleanup binding that declares neither
// default nor optional is a claim that the output exists, so its absence is
// a failure that leaves the release owed rather than a silently empty value
// the script acts on.
func TestRunCleanup_RequiredSelfOutputAbsenceFailsTheRelease(t *testing.T) {
	r := Resolved{NodeID: "a", Scope: config.TaskScopeRun, Cleanup: &lang.Action{
		Type:   lang.ActionShell,
		Script: `true`,
		Bind:   map[string]*lang.Value{"pid": {Form: lang.FormFrom, From: "self.outputs.pid"}},
	}}
	tasks := map[string]*contract.TaskState{
		"a": {Scope: config.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	err := RunCleanup(context.Background(), []Resolved{r}, SessionVars{}, tasks, nil)
	if err == nil {
		t.Fatal("RunCleanup: want the unresolvable binding surfaced")
	}
	if tasks["a"].Status != contract.TaskStatusFailed {
		t.Errorf("status = %q, want %q", tasks["a"].Status, contract.TaskStatusFailed)
	}
}

// TestRunSetup_RetainsCleanupContractAndExecutionDir proves a plain node's
// setup snapshots its own cleanup action, execution directory, and plugin
// reference onto TaskState -- see issue #496's node_executions.cleanup_json/
// execution_dir/plugin_ref. persistence.writeTasksTx persists Cleanup
// opaquely; this test only proves the task package's own producer side.
func TestRunSetup_RetainsCleanupContractAndExecutionDir(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: `echo '{}'`, cleanup: `true`}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{WorkspaceDirPath: "/tmp/x"}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := tasks["a"]
	if got.ExecutionDir != "/tmp/x" {
		t.Fatalf("ExecutionDir = %q, want %q", got.ExecutionDir, "/tmp/x")
	}
	if len(got.Cleanup) == 0 {
		t.Fatal("Cleanup not retained")
	}
	var decoded struct {
		Action struct {
			Type   string `json:"Type"`
			Script string `json:"Script"`
		} `json:"action"`
	}
	if err := json.Unmarshal(got.Cleanup, &decoded); err != nil {
		t.Fatalf("Cleanup did not decode as JSON: %v (%s)", err, got.Cleanup)
	}
	if decoded.Action.Type != lang.ActionShell || decoded.Action.Script != "true" {
		t.Fatalf("decoded retained cleanup action = %+v, want the declared shell action", decoded.Action)
	}
}

// TestRunSetup_NestedNodeRetainsCleanupPerLayerNotOnTheComposedState proves
// a nested (layered) node's own, composed TaskState.Cleanup is left nil
// (the plain-node retained-contract shape does not fit a layered chain),
// while EACH layer's own cleanup is retained separately on
// LayerState.Cleanup instead (effect.RetainLayerCleanup, wired into
// effect.RunLayers) -- schema-free, since cleanup never needs the compiled
// input/locals/outputs schemas a layer's setup answers to (see
// effect.CleanupLayers, which never sets them either).
// TestRunSetup_RefusesUnreleasedNodeWhenNestingChainShapeChanges is issue
// #496's acceptance case 3: a node whose task id and scope are unchanged
// but whose nesting chain shape was revised (a different inner effect) is
// still a different declaration -- the old chain's own retained per-layer
// release recipe must survive untouched until something explicitly
// releases it, not be silently discarded because the composed identity
// looked the same.
func TestRunSetup_RefusesUnreleasedNodeWhenNestingChainShapeChanges(t *testing.T) {
	withScriptedExecutor(t, &scriptedExecutor{stdout: map[string]string{"inner-a-setup": `{"pid":1}`}})
	outer := config.TaskDefinition{ID: "outer", Scope: "run", Cleanup: shellStub("outer-cleanup")}
	innerA := config.TaskDefinition{ID: "inner-a", Scope: "run", Setup: shellStub("inner-a-setup"), Cleanup: shellStub("inner-a-cleanup")}
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), nestedPlan(t, outer, innerA), SessionVars{Name: "s"}, tasks, nil); err != nil {
		t.Fatalf("initial setup: %v", err)
	}
	if tasks["outer"].Status != contract.TaskStatusProduced || len(tasks["outer"].Layers) != 2 {
		t.Fatalf("initial state = %+v", tasks["outer"])
	}

	innerB := config.TaskDefinition{ID: "inner-b", Scope: "run", Setup: shellStub("inner-b-setup"), Cleanup: shellStub("inner-b-cleanup")}
	err := RunSetup(context.Background(), nestedPlan(t, outer, innerB), SessionVars{Name: "s"}, tasks, nil)
	if err == nil {
		t.Fatal("RunSetup: want refusal for a revised nesting chain, got nil error")
	}
	if got := tasks["outer"].Layers; len(got) != 2 || got[1].EffectID != "inner-a" {
		t.Fatalf("retained layers = %+v, want the old chain (inner-a) left untouched", got)
	}
}

func TestRunSetup_NestedNodeRetainsCleanupPerLayerNotOnTheComposedState(t *testing.T) {
	withScriptedExecutor(t, &scriptedExecutor{stdout: map[string]string{"inner-setup": `{"pid":42}`}})
	outer := config.TaskDefinition{ID: "outer", Scope: "run", Cleanup: shellStub("outer-cleanup")}
	inner := config.TaskDefinition{ID: "inner", Scope: "run", Setup: shellStub("inner-setup"), Cleanup: shellStub("inner-cleanup")}
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), nestedPlan(t, outer, inner), SessionVars{Name: "s"}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := tasks["outer"].Cleanup; got != nil {
		t.Fatalf("Cleanup = %s, want nil on the composed state for a nested node", got)
	}
	if len(tasks["outer"].Layers) != 2 {
		t.Fatalf("Layers = %+v, want 2", tasks["outer"].Layers)
	}
	for i, name := range []string{"outer-cleanup", "inner-cleanup"} {
		rc, ok, err := effect.DecodeRetainedLayerCleanup(tasks["outer"].Layers[i].Cleanup)
		if err != nil {
			t.Fatalf("layer %d: decode retained cleanup: %v", i, err)
		}
		if !ok {
			t.Fatalf("layer %d: no retained cleanup contract", i)
		}
		if rc.Cleanup == nil || rc.Cleanup.Script != name {
			t.Fatalf("layer %d retained cleanup = %+v, want Script %q", i, rc.Cleanup, name)
		}
	}
}

// TestRunSetup_RetainsPluginRefWithRevision proves a plugin-owned node's
// PluginRef names both the mounted plugin's catalog-qualified id and its
// locked content revision, resolved via plugins.ContainingPlugin against
// the declaration's own SourcePath -- the same lookup plugins.ResolveBin
// trusts for the identical path.
func TestRunSetup_RetainsPluginRefWithRevision(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	pluginDir := t.TempDir()
	mount := plugins.Mounted{ID: "acme/tools", Dir: pluginDir, Revision: "deadbeef"}
	r := Resolved{
		NodeID:     "a",
		Scope:      "run",
		Setup:      &lang.Action{Type: lang.ActionShell, Script: `echo '{}'`},
		SourcePath: pluginDir + "/tasks/a.toml",
		From:       lang.Ownership{IsPlugin: true},
	}
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), []Resolved{r}, SessionVars{Plugins: []plugins.Mounted{mount}}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got, want := tasks["a"].PluginRef, "acme/tools@deadbeef"; got != want {
		t.Fatalf("PluginRef = %q, want %q", got, want)
	}
}

func TestRunSetup_CapturesOutputsAndRespectsDeps(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: `echo '{"value":"first"}'`},
			{id: "b", scope: "run", setupAction: &lang.Action{
				Type:   lang.ActionShell,
				Script: `jq -nc --arg saw "$saw" '{saw:$saw}'`,
				Bind:   map[string]*lang.Value{"saw": {Form: lang.FormFrom, From: "nodes.a.outputs.value"}},
			}},
		},
		[]nodeStub{
			{id: "a"},
			{id: "b", inputs: map[string]*lang.Value{"a_dep": fromValue("nodes.a.outputs.value")}},
		},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{Name: "x"}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if tasks["a"].Outputs["value"] != "first" {
		t.Fatalf("a.value = %v", tasks["a"].Outputs["value"])
	}
	if tasks["b"].Outputs["saw"] != "first" {
		t.Fatalf("b.saw = %v", tasks["b"].Outputs["saw"])
	}
	if tasks["a"].Status != contract.TaskStatusProduced {
		t.Fatalf("a.Status = %q", tasks["a"].Status)
	}
}

// TestRunSetup_StampsDependsOnFromResolvedNode proves each node's setup
// records its own resolved dependency edges onto TaskState.DependsOn --
// persistence.writeTasksTx snapshots these into node_execution_dependencies
// so release ordering survives even once the workflow declaration that
// derived them changes or the dependency node disappears from it. See
// issue #496.
func TestRunSetup_StampsDependsOnFromResolvedNode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: `echo '{"k":"first"}'`},
			{id: "b", scope: "run", setup: `echo '{}'`},
		},
		[]nodeStub{
			{id: "a"},
			{id: "b", inputs: depInput("a")},
		},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{Name: "x"}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := tasks["b"].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Fatalf("b.DependsOn = %v, want [a]", got)
	}
	if got := tasks["a"].DependsOn; len(got) != 0 {
		t.Fatalf("a.DependsOn = %v, want none", got)
	}
}

func TestRunSetup_SkipsProduced(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	marker := tmpDir + "/setup-ran"
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "touch " + marker + "; echo '{}'"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "preserved"}},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); statErr == nil {
		t.Fatal("setup script ran for an already-produced task")
	}
	if tasks["a"].Outputs["value"] != "preserved" {
		t.Fatalf("expected preserved outputs, got %v", tasks["a"].Outputs)
	}
}

func TestRunSetup_UpOrderProducesNodeAddedAfterSessionCreation(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	marker := tmpDir + "/tmux-ran"
	plan := buildPlan(t,
		[]taskStub{
			{id: "gh_guard", scope: "session", setup: `echo '{"k":"the-dir"}'`},
			{id: "tmux", scope: "run", setup: "touch " + marker + "; echo '{}'"},
			{id: "claude", scope: "run", setupAction: &lang.Action{
				Type:   lang.ActionShell,
				Script: `jq -nc --arg saw "$saw" '{saw:$saw}'`,
				Bind:   map[string]*lang.Value{"saw": {Form: lang.FormFrom, From: "nodes.gh_guard.outputs.k"}},
			}},
		},
		[]nodeStub{
			{id: "gh_guard"},
			{id: "tmux"},
			{id: "claude", inputs: depInput("gh_guard")},
		},
	)
	tasks := map[string]*contract.TaskState{
		"tmux": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	obs := &recordingObserver{}
	if err := RunSetup(context.Background(), plan.UpOrder(), SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); statErr == nil {
		t.Fatal("tmux setup re-ran even though it was already produced")
	}
	if len(obs.skips) != 1 || obs.skips[0] != "tmux" {
		t.Fatalf("skips = %v, want [tmux]", obs.skips)
	}
	if tasks["gh_guard"] == nil || tasks["gh_guard"].Status != contract.TaskStatusProduced {
		t.Fatalf("gh_guard not produced: %+v", tasks["gh_guard"])
	}
	if tasks["claude"] == nil || tasks["claude"].Outputs["saw"] != "the-dir" {
		t.Fatalf("claude did not resolve gh_guard's output: %+v", tasks["claude"])
	}
}

func TestRunSetup_RetriesFailed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: `echo '{"value":"second"}'`}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusFailed, Error: "first attempt"},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if tasks["a"].Status != contract.TaskStatusProduced {
		t.Fatalf("status = %q, want produced", tasks["a"].Status)
	}
	if tasks["a"].Outputs["value"] != "second" {
		t.Fatalf("outputs not refreshed: %v", tasks["a"].Outputs)
	}
}

// TestRunSetup_RefusesUnreleasedNodeUnderADifferentDeclaration is the
// issue #496 acceptance case: a node whose recorded, unreleased (here
// "failed") attempt names a different task than the one about to be set up
// must be refused rather than silently overwritten -- the old attempt's own
// release recipe (task "old", not "new") would otherwise be discarded with
// no way to release it later.
func TestRunSetup_RefusesUnreleasedNodeUnderADifferentDeclaration(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "new", scope: "run", setup: `echo '{}'`}},
		[]nodeStub{{id: "a", uses: "new"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusFailed, TaskID: "old", Error: "boom"},
	}
	err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil)
	if err == nil {
		t.Fatal("RunSetup: want refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "old") || !strings.Contains(err.Error(), "new") {
		t.Fatalf("error = %q, want it to name both the retained (%q) and requested (%q) declarations", err, "old", "new")
	}
	if got := tasks["a"]; got.Status != contract.TaskStatusFailed || got.TaskID != "old" || got.Error != "boom" {
		t.Fatalf("retained state = %+v, want the unreleased attempt left untouched", got)
	}
}

// TestRunSetup_RefusesUnreleasedProducedNodeUnderADifferentDeclaration
// covers the same refusal for a currently "produced" node whose new
// declaration no longer declares an alive probe at all -- without the
// refusal, an absent probe would vacuously "pass" liveness and this node
// would be treated as already matching a declaration it never actually ran.
func TestRunSetup_RefusesUnreleasedProducedNodeUnderADifferentDeclaration(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "new", scope: "run", setup: `echo '{}'`}},
		[]nodeStub{{id: "a", uses: "new"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, TaskID: "old", Outputs: map[string]any{"x": "y"}},
	}
	err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil)
	if err == nil {
		t.Fatal("RunSetup: want refusal, got nil error")
	}
	if got := tasks["a"]; got.Status != contract.TaskStatusProduced || got.TaskID != "old" || got.Outputs["x"] != "y" {
		t.Fatalf("retained state = %+v, want the unreleased attempt left untouched", got)
	}
}

func TestRunSetup_RetriesCleaned(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: `echo '{"ok":true}'`}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusCleaned},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if tasks["a"].Status != contract.TaskStatusProduced {
		t.Fatalf("status = %q, want produced", tasks["a"].Status)
	}
}

// Setup must see the previous run's outputs via .Prev when a cleaned state
// is replayed (e.g. `plect down` followed by `plect up`). This is what enables
// tasks like claude to keep a stable session_id across restarts.
func TestRunSetup_PrevCarriesPreviousOutputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{
			id:    "claude",
			scope: "run",
			setupAction: &lang.Action{
				Type:   lang.ActionShell,
				Script: `jq -nc --arg sid "$prev_sid" '{session_id:$sid}'`,
				Bind:   map[string]*lang.Value{"prev_sid": {Form: lang.FormFrom, From: "prev.session_id", HasDefault: true, Default: ""}},
			},
		}},
		[]nodeStub{{id: "claude"}},
	)
	tasks := map[string]*contract.TaskState{
		"claude": {
			Scope:   "run",
			Status:  contract.TaskStatusCleaned,
			Outputs: map[string]any{"session_id": "kept-across-down"},
		},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got, _ := tasks["claude"].Outputs["session_id"].(string)
	if got != "kept-across-down" {
		t.Fatalf("session_id = %q, want %q (was the previous run's value)", got, "kept-across-down")
	}
}

// First-ever setup has no prior outputs, so a projection of one falls to its
// declared default rather than failing.
func TestRunSetup_PrevEmptyOnFirstRun(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{
			id:    "claude",
			scope: "run",
			setupAction: &lang.Action{
				Type:   lang.ActionShell,
				Script: `jq -nc --arg sid "$prev_sid" '{session_id:$sid}'`,
				Bind:   map[string]*lang.Value{"prev_sid": {Form: lang.FormFrom, From: "prev.session_id", HasDefault: true, Default: ""}},
			},
		}},
		[]nodeStub{{id: "claude"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got, _ := tasks["claude"].Outputs["session_id"].(string)
	if got != "" {
		t.Fatalf("session_id = %q, want empty on first run", got)
	}
}

// Regression: when setup fails, the previously produced
// outputs must survive into the next attempt as .Prev — otherwise tasks
// that key off a stable identifier (claude session_id etc.) cannot recover
// after a transient failure.
func TestRunSetup_FailurePreservesPrevOutputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "exit 1"}},
		[]nodeStub{{id: "a"}},
	)
	// Prior run completed and was torn down (Cleaned) — its outputs must
	// survive the next failed setup attempt so the run after that can still
	// see them via .Prev.
	tasks := map[string]*contract.TaskState{
		"a": {
			Scope:   "run",
			Status:  contract.TaskStatusCleaned,
			Outputs: map[string]any{"session_id": "kept-across-retry", "pid": float64(12345)},
		},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("expected setup error")
	}
	if tasks["a"].Status != contract.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", tasks["a"].Status)
	}
	if got, _ := tasks["a"].Outputs["session_id"].(string); got != "kept-across-retry" {
		t.Fatalf("session_id = %v, want preserved across failure", tasks["a"].Outputs["session_id"])
	}
	// Numeric outputs must survive too — `pid` etc. drive resume logic.
	if got, _ := tasks["a"].Outputs["pid"].(float64); got != 12345 {
		t.Fatalf("pid = %v (%T), want 12345 preserved across failure",
			tasks["a"].Outputs["pid"], tasks["a"].Outputs["pid"])
	}
}

// Regression: JSON numbers unmarshal into float64, and Go's
// A persisted output is JSON, so every number arrives as a float64, and the
// default formatter renders a large one in scientific notation (e.g.
// 3.052179e+06). A script compares the resolved value as a string, so an
// integer-valued number must reach it without an exponent.
func TestResolve_IntegerFloatNotScientific(t *testing.T) {
	env := cleanupRoots(RenderContext{Self: map[string]any{"pid": float64(3052179)}})
	if got := resolveFrom(t, env, "self.outputs.pid"); got != "3052179" {
		t.Fatalf("got %q, want %q", got, "3052179")
	}
}

// Normalization must reach nested maps and slices under a node's outputs too,
// otherwise an upstream node's deep `pid` still leaks as scientific.
func TestResolve_IntegerFloatNormalizedUnderNodeOutputs(t *testing.T) {
	env := nodeInputsRoots(RenderContext{Tasks: map[string]map[string]any{
		"dep": {
			"meta": map[string]any{"pid": float64(3052179)},
			"pids": []any{float64(3052179)},
		},
	}})
	if got := resolveFrom(t, env, "nodes.dep.outputs.meta.pid"); got != "3052179" {
		t.Fatalf("nested map: got %q", got)
	}
	out, absent, err := (lang.Eval{Roots: env}).Value(fromValue("nodes.dep.outputs.pids"))
	if err != nil || absent {
		t.Fatalf("Value: %v (absent=%v)", err, absent)
	}
	list, ok := out.([]any)
	if !ok || len(list) != 1 || list[0] != int64(3052179) {
		t.Fatalf("nested slice: got %#v", out)
	}
}

// Only integer-valued floats are normalized; a non-integer keeps its decimal
// form.
func TestResolve_NonIntegerFloatPreserved(t *testing.T) {
	env := cleanupRoots(RenderContext{Self: map[string]any{"ratio": 0.5}})
	if got := resolveFrom(t, env, "self.outputs.ratio"); got != "0.5" {
		t.Fatalf("got %q, want %q", got, "0.5")
	}
}

// resolveFrom resolves one projection the way an action's own binding does,
// for the cases that are about how a persisted value reaches a script rather
// than about which surface offers it.
func resolveFrom(t *testing.T, env lang.Roots, path string) string {
	t.Helper()
	out, absent, err := (lang.Eval{Roots: env}).Argument(fromValue(path))
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	if absent {
		t.Fatalf("resolve %s: reported absent", path)
	}
	return out
}

func TestRunSetup_FailureMarksFailed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "exit 1"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("expected error")
	}
	if tasks["a"].Status != contract.TaskStatusFailed {
		t.Fatalf("a.Status = %q", tasks["a"].Status)
	}
}

func TestRunCleanup_ReverseOrder(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: "echo '{}'", cleanup: "true"},
			{id: "b", scope: "run", setup: "echo '{}'", cleanup: "true"},
		},
		[]nodeStub{
			{id: "a"},
			{id: "b", inputs: depInput("a")},
		},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
		"b": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if tasks["a"].Status != contract.TaskStatusCleaned {
		t.Fatalf("a.Status = %q", tasks["a"].Status)
	}
	if tasks["b"].Status != contract.TaskStatusCleaned {
		t.Fatalf("b.Status = %q", tasks["b"].Status)
	}
}

// Regression: a setup that exited before populating Self leaves Outputs nil.
// Cleanup that references Self.* must still run (with empty substitutions)
// rather than aborting the entire teardown with a template render error.
func TestRunCleanup_FailedSetupRendersZeroes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{
			id:      "a",
			scope:   "run",
			setup:   "exit 1",
			cleanup: `kill -TERM "{{.Self.pid}}" 2>/dev/null || true; rm -f "{{.Self.path}}"`,
		}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusFailed, Outputs: nil},
	}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("cleanup should not fail when Self keys are missing: %v", err)
	}
	if tasks["a"].Status != contract.TaskStatusCleaned {
		t.Fatalf("a.Status = %q, want cleaned", tasks["a"].Status)
	}
}

// A cleanup script that errors must flip the task's Status to failed.
// The status table treats "failed" as covering both setup and cleanup
// errors — leaving the entity at "produced" after a failed teardown would
// hide the broken state from later `plect up` (which skips produced tasks).
func TestRunCleanup_FailureFlipsToFailed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "true", cleanup: "exit 1"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("expected cleanup error")
	}
	if tasks["a"].Status != contract.TaskStatusFailed {
		t.Fatalf("a.Status = %q, want failed", tasks["a"].Status)
	}
	if tasks["a"].Error == "" {
		t.Fatal("expected Error to be populated on cleanup failure")
	}
}

// writeSchema writes a JSON schema to a temp file and returns its absolute path.
func writeSchema(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "outputs.schema.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	return path
}

// Misconfigured schemas must fail at plan-build, not silently mid-setup.
func TestPlan_CompilesOutputsSchema(t *testing.T) {
	schemaPath := writeSchema(t, `{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)
	plan := buildPlan(t,
		[]taskStub{{id: "envfile", scope: "session", setup: "true", outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "envfile"}},
	)
	if len(plan.Session) != 1 || plan.Session[0].OutputsSchema == nil {
		t.Fatalf("expected compiled schema on Resolved, got %+v", plan.Session)
	}
}

func TestPlan_MissingSchemaFileErrors(t *testing.T) {
	_, err := tryBuildPlan(
		[]taskStub{{id: "envfile", scope: "session", setup: "true", outputsSchemaFile: "/does/not/exist.json"}},
		[]nodeStub{{id: "envfile"}},
	)
	if err == nil || !strings.Contains(err.Error(), "outputs schema") {
		t.Fatalf("expected outputs schema error, got %v", err)
	}
}

func TestPlan_InvalidSchemaErrors(t *testing.T) {
	schemaPath := writeSchema(t, `{not json`)
	_, err := tryBuildPlan(
		[]taskStub{{id: "envfile", scope: "session", setup: "true", outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "envfile"}},
	)
	if err == nil || !strings.Contains(err.Error(), "outputs schema") {
		t.Fatalf("expected outputs schema error, got %v", err)
	}
}

func TestRunSetup_ValidatesOutputsAgainstSchema_Valid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	schemaPath := writeSchema(t, `{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)
	plan := buildPlan(t,
		[]taskStub{{id: "envfile", scope: "session", setup: `echo '{"path":"/tmp/.env"}'`, outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "envfile"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Session, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if tasks["envfile"].Status != contract.TaskStatusProduced {
		t.Fatalf("status = %q, want produced", tasks["envfile"].Status)
	}
	if tasks["envfile"].Outputs["path"] != "/tmp/.env" {
		t.Fatalf("outputs = %v", tasks["envfile"].Outputs)
	}
}

func TestRunSetup_SchemaRejectsMissingRequired(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	schemaPath := writeSchema(t, `{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)
	plan := buildPlan(t,
		[]taskStub{{id: "envfile", scope: "session", setup: `echo '{"pid":42}'`, outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "envfile"}},
	)
	tasks := map[string]*contract.TaskState{}
	runErr := RunSetup(context.Background(), plan.Session, SessionVars{}, tasks, nil)
	if runErr == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(runErr.Error(), "outputs schema") {
		t.Fatalf("error should mention outputs schema, got: %v", runErr)
	}
	st := tasks["envfile"]
	if st.Status != contract.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", st.Status)
	}
	if st.Error == "" {
		t.Fatal("Error should be populated with validation detail")
	}
	if st.Outputs != nil {
		t.Fatalf("Outputs should be unset on schema failure, got %v", st.Outputs)
	}
}

func TestRunSetup_EmptySetupValidatesOutputsAgainstSchema(t *testing.T) {
	schemaPath := writeSchema(t, `{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)
	plan := buildPlan(t,
		[]taskStub{{id: "envfile", scope: "session", setup: "", outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "envfile"}},
	)
	tasks := map[string]*contract.TaskState{}
	runErr := RunSetup(context.Background(), plan.Session, SessionVars{}, tasks, nil)
	if runErr == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(runErr.Error(), "outputs schema") {
		t.Fatalf("error should mention outputs schema, got: %v", runErr)
	}
	st := tasks["envfile"]
	if st.Status != contract.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", st.Status)
	}
	if st.Outputs != nil {
		t.Fatalf("Outputs should be unset on schema failure, got %v", st.Outputs)
	}
}

func TestRunSetup_SchemaRejectsWrongType(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	schemaPath := writeSchema(t, `{"type":"object","properties":{"pid":{"type":"integer"}}}`)
	plan := buildPlan(t,
		[]taskStub{{id: "p", scope: "run", setup: `echo '{"pid":"not-a-number"}'`, outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "p"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("expected schema validation error")
	}
	if tasks["p"].Status != contract.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", tasks["p"].Status)
	}
}

func TestRunSetup_SchemaRejectsAdditionalProperties(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	schemaPath := writeSchema(t, `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"}}}`)
	plan := buildPlan(t,
		[]taskStub{{id: "e", scope: "run", setup: `echo '{"path":"/x","extra":1}'`, outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "e"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("expected schema validation error")
	}
	if tasks["e"].Status != contract.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", tasks["e"].Status)
	}
}

// Backward compatibility: no schema = any JSON object accepted.
func TestRunSetup_NoSchemaAcceptsAnyObject(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "free", scope: "run", setup: `echo '{"anything":[1,2,3],"goes":true}'`}},
		[]nodeStub{{id: "free"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if tasks["free"].Status != contract.TaskStatusProduced {
		t.Fatalf("status = %q, want produced", tasks["free"].Status)
	}
}

func TestPlan_InlineOutputsSchema(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{{id: "tmux", scope: "run", setup: "true", outputsSchema: map[string]any{
			"type":     "object",
			"required": []any{"session_name"},
			"properties": map[string]any{
				"session_name": map[string]any{"type": "string"},
			},
		}}},
		[]nodeStub{{id: "tmux"}},
	)
	if plan.Run[0].OutputsSchema == nil {
		t.Fatal("expected inline schema to compile")
	}
}

func TestPlan_InlineAndFileMutuallyExclusive(t *testing.T) {
	schemaPath := writeSchema(t, `{"type":"object"}`)
	_, err := tryBuildPlan(
		[]taskStub{{id: "x", scope: "run", setup: "true",
			outputsSchema:     map[string]any{"type": "object"},
			outputsSchemaFile: schemaPath}},
		[]nodeStub{{id: "x"}},
	)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestRunSetup_InlineSchemaRejectsMissingRequired(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "tmux", scope: "run", setup: `echo '{}'`, outputsSchema: map[string]any{
			"type":     "object",
			"required": []any{"session_name"},
			"properties": map[string]any{
				"session_name": map[string]any{"type": "string"},
			},
		}}},
		[]nodeStub{{id: "tmux"}},
	)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("expected schema validation error")
	}
	if tasks["tmux"].Status != contract.TaskStatusFailed {
		t.Fatalf("status = %q, want failed", tasks["tmux"].Status)
	}
}

func TestPlan_ResolvesSchemaPathAgainstBaseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.schema.json"),
		[]byte(`{"type":"object","required":["path"]}`), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	plan := buildPlan(t,
		[]taskStub{{id: "envfile", scope: "session", setup: "true",
			outputsSchemaFile: "out.schema.json", baseDir: dir}},
		[]nodeStub{{id: "envfile"}},
	)
	if plan.Session[0].OutputsSchema == nil {
		t.Fatal("expected schema to compile via BaseDir resolution")
	}
}

// A task with no cleanup command still reaches the terminal "cleaned"
// state — the entity is released by definition. The observer must see it
// as a success, not a skip, so progress UIs render ✓ rather than ⊘.
func TestRunCleanup_EmptyCleanupFiresOnSuccess(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "true"}}, // no cleanup
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	obs := &recordingObserver{}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if tasks["a"].Status != contract.TaskStatusCleaned {
		t.Fatalf("a.Status = %q, want cleaned", tasks["a"].Status)
	}
	if len(obs.successes) != 1 || obs.successes[0] != "a" {
		t.Fatalf("expected OnSuccess for 'a', got successes=%v skips=%v", obs.successes, obs.skips)
	}
	if len(obs.skips) != 0 {
		t.Fatalf("empty cleanup should not be reported as skipped, got skips=%v", obs.skips)
	}
}

// terminalStub is shorthand for a taskStub declaring a complete [terminal]
// table — most TerminalTask tests only care that one is declared, not the
// concrete verb templates.
func terminalStub(id string) taskStub {
	return taskStub{
		id: id, scope: "run", setup: "true",
		attach: "attach " + id, capture: "capture " + id,
		sendText: "send_text " + id, sendKeys: "send_keys " + id,
	}
}

func TestPlan_MultipleTerminalDeclarationsRejected(t *testing.T) {
	_, err := tryBuildPlan(
		[]taskStub{terminalStub("tmux"), terminalStub("zellij")},
		[]nodeStub{{id: "tmux"}, {id: "zellij"}},
	)
	if err == nil || !strings.Contains(err.Error(), "more than one node declares [terminal]") {
		t.Fatalf("expected multi-terminal error, got %v", err)
	}
}

func TestPlan_SingleTerminalAccepted(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{terminalStub("tmux")},
		[]nodeStub{{id: "tmux"}},
	)
	target := plan.TerminalTask()
	if target == nil {
		t.Fatal("expected terminal task on plan")
	}
	if target.NodeID != "tmux" {
		t.Fatalf("terminal task = %q, want tmux", target.NodeID)
	}
}

func TestPlan_TerminalTaskNilWhenNoneDeclared(t *testing.T) {
	plan := buildPlan(t,
		[]taskStub{{id: "tmux", scope: "run", setup: "true"}},
		[]nodeStub{{id: "tmux"}},
	)
	if plan.TerminalTask() != nil {
		t.Fatal("expected nil terminal task when none declared")
	}
}

// A verb is available on its own, so an effect declaring only the verbs it
// can honor compiles: what a consumer asks for either resolves or fails
// where it is consumed.
func TestResolveDefinition_PartialTerminalTableAccepted(t *testing.T) {
	plan, err := tryBuildPlan(
		[]taskStub{{id: "tmux", scope: "run", setup: "true", attach: "attach tmux"}},
		[]nodeStub{{id: "tmux"}},
	)
	if err != nil {
		t.Fatalf("tryBuildPlan: %v", err)
	}
	target := plan.TerminalTask()
	if target == nil {
		t.Fatal("expected the declaring node to own the plan's terminal endpoint")
	}
	if _, err := target.Terminal.Verb("capture"); err == nil {
		t.Error("capture resolved, but this effect declares no such verb")
	}
}

func terminalBindingForTest(verb string, action *lang.Action, self map[string]any) *TerminalBinding {
	ops := &config.TerminalConfig{}
	switch verb {
	case "attach":
		ops.Attach = action
	case "capture":
		ops.Capture = action
	}
	return &TerminalBinding{Ops: ops, Outputs: self}
}

func TestTerminalCommand_ResolvesSelfAndSessionRoots(t *testing.T) {
	action := &lang.Action{
		Type:    lang.ActionExec,
		Command: "tmux",
		Args: []*lang.Value{
			{Form: lang.FormLiteral, Literal: "attach"},
			{Form: lang.FormLiteral, Literal: "-t"},
			{Form: lang.FormFrom, From: "self.outputs.session_name"},
			{Form: lang.FormFrom, From: "session.name"},
		},
	}
	binding := terminalBindingForTest("attach", action, map[string]any{"session_name": "owner/repo-1"})
	out, err := TerminalCommand(binding, "attach", SessionVars{Name: "owner/repo-1"}, t.TempDir())
	if err != nil {
		t.Fatalf("TerminalCommand: %v", err)
	}
	want := `'tmux' 'attach' '-t' 'owner/repo-1' 'owner/repo-1' "$@"`
	if out != want {
		t.Fatalf("TerminalCommand = %q, want %q", out, want)
	}
}

// A verb projecting an output the declaring effect never recorded is a
// contract violation, not a silently-empty command a consumer would run
// against nothing.
func TestTerminalCommand_MissingSelfOutputErrors(t *testing.T) {
	action := &lang.Action{
		Type:    lang.ActionExec,
		Command: "tmux",
		Args:    []*lang.Value{{Form: lang.FormFrom, From: "self.outputs.session_name"}},
	}
	binding := terminalBindingForTest("attach", action, map[string]any{})
	if _, err := TerminalCommand(binding, "attach", SessionVars{}, t.TempDir()); err == nil {
		t.Fatal("expected an error for an output the effect never recorded")
	}
}

func TestRunCapture_ResolvesSelfAndSessionRoots(t *testing.T) {
	action := &lang.Action{
		Type:   lang.ActionShell,
		Script: `printf '%s' "view of $pane ($session)"`,
		Bind: map[string]*lang.Value{
			"pane":    {Form: lang.FormFrom, From: "self.outputs.session_name"},
			"session": {Form: lang.FormFrom, From: "session.name"},
		},
	}
	binding := terminalBindingForTest("capture", action, map[string]any{"session_name": "owner/repo-1"})
	out, err := RunCapture(context.Background(), binding, SessionVars{Name: "owner/repo-1"})
	if err != nil {
		t.Fatalf("RunCapture: %v", err)
	}
	want := "view of owner/repo-1 (owner/repo-1)"
	if out != want {
		t.Fatalf("RunCapture = %q, want %q", out, want)
	}
}

func TestRunCapture_SurfacesStderrOnFailure(t *testing.T) {
	// An orphaned pane (script exits non-zero) must be a hard error, not a
	// success with empty output.
	binding := terminalBindingForTest("capture", shellStub(`echo "can't find pane" >&2; exit 1`), map[string]any{})
	_, err := RunCapture(context.Background(), binding, SessionVars{})
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "can't find pane") {
		t.Fatalf("error %q should carry stderr", err.Error())
	}
}

// TestRunSetup_NestedPublicOutputsValidatedAgainstOuterSchema covers the
// runtime failure rule for the projection: the composed contract answers to
// the outer schema the way a plain task's setup answers to its own.
func TestRunSetup_NestedPublicOutputsValidatedAgainstOuterSchema(t *testing.T) {
	withScriptedExecutor(t, &scriptedExecutor{stdout: map[string]string{
		"inner-setup": `{"pid":42}`,
	}})
	outer := config.TaskDefinition{
		ID: "outer", Scope: "run",
		OutputsBind:   map[string]*lang.Value{"pid": fromValue("inner.outputs.pid")},
		OutputsSchema: objectSchema(map[string]any{"pid": map[string]any{"type": "string"}}, "pid"),
	}
	inner := config.TaskDefinition{ID: "inner", Scope: "run", Setup: shellStub("inner-setup")}
	tasks := map[string]*contract.TaskState{}
	err := RunSetup(context.Background(), nestedPlan(t, outer, inner), SessionVars{Name: "s"}, tasks, nil)
	if err == nil {
		t.Fatal("RunSetup: want an error for a projection the outer schema rejects, got nil")
	}
	if !strings.Contains(err.Error(), "outputs schema") {
		t.Errorf("error = %v, want it to name the public outputs contract", err)
	}
}

// TestRunSetup_DownstreamNodeReadsTheComposedContract covers what makes the
// projection worth having: from the outside a nested task is exactly a task,
// so a downstream node wires to its public key with no nesting-aware
// vocabulary, and reaches nothing the joint did not bind.
func TestRunSetup_DownstreamNodeReadsTheComposedContract(t *testing.T) {
	withScriptedExecutor(t, &scriptedExecutor{stdout: map[string]string{
		"inner-setup": `{"pid":"11","socket_path":"/tmp/sock"}`,
	}})
	inner := config.TaskDefinition{ID: "inner", Scope: "run", Setup: shellStub("inner-setup")}
	outer := config.TaskDefinition{
		ID: "runtime", Scope: "run", Inner: "inner", InnerChain: []config.TaskDefinition{inner},
		OutputsBind:   map[string]*lang.Value{"agent_pid": fromValue("inner.outputs.pid")},
		OutputsSchema: objectSchema(map[string]any{"agent_pid": map[string]any{"type": "string"}}),
	}
	consumer := config.TaskDefinition{ID: "consumer", Scope: "run", Setup: shellStub("consumer-setup")}
	plan, err := CompileWorkflow(
		config.WorkflowFile{ID: "test", Nodes: []config.WorkflowNode{
			{ID: "runtime", Uses: "runtime"},
			{ID: "consumer", Uses: "consumer", Inputs: map[string]*lang.Value{"pid": fromValue("nodes.runtime.outputs.agent_pid")}},
		}},
		map[string]config.TaskDefinition{"runtime": outer, "inner": inner, "consumer": consumer},
	)
	if err != nil {
		t.Fatalf("CompileWorkflow: %v", err)
	}
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{Name: "s"}, tasks, nil); err != nil {
		t.Fatalf("RunSetup: %v", err)
	}
	if got := tasks["consumer"].Inputs["pid"]; got != "11" {
		t.Errorf("downstream input = %#v, want the composed task's public value", got)
	}
	if _, leaked := tasks["runtime"].Outputs["socket_path"]; leaked {
		t.Error("an inner output the joint never bound must not be reachable downstream")
	}
}

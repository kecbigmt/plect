package service

import (
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// nestedTasks builds the seeded task state of a two-layer chain instance.
func nestedTasks(composed map[string]any, innerOutputs map[string]any, layerEnv map[string]string) map[string]*contract.TaskState {
	return map[string]*contract.TaskState{
		"runtime": {
			Scope:   contract.TaskScopeRun,
			TaskID:  "team_runtime",
			Status:  contract.TaskStatusProduced,
			Outputs: composed,
			Layers: []contract.LayerState{
				{EffectID: "team_runtime", Status: contract.TaskStatusProduced, Env: layerEnv},
				{EffectID: "runtime", Status: contract.TaskStatusProduced, Outputs: innerOutputs},
			},
			SetupAt: time.Now(),
		},
	}
}

// nestedConfig writes the chain: an inner "runtime" task and an outer
// "team_runtime" wrapping it, each with whatever extra TOML the case needs.
func nestedConfig(t *testing.T, inner taskFixture, outerExtra string) *config.Config {
	t.Helper()
	inner.id = "runtime"
	if inner.scope == "" {
		inner.scope = contract.TaskScopeRun
	}
	outer := taskFixture{
		id:    "team_runtime",
		scope: contract.TaskScopeRun,
		extra: "inner = \"runtime\"\n" + outerExtra,
	}
	return writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{inner, outer},
		[]nodeFixture{{id: "runtime", uses: "team_runtime"}})
}

// bindPid re-exports the inner task's pid under the same public name, the
// minimum wiring a chain needs to expose anything at all.
const bindPid = `
[outputs.bind]
pid = { from = "inner.outputs.pid" }

[outputs_schema]
type = "object"

[outputs_schema.properties.pid]
type = "string"
`

// TestEvaluateHealth_NestedAliveComposesByAndNamingTheLayer covers the alive
// AND: any failing layer makes the composed task unhealthy, and the report
// says which one.
func TestEvaluateHealth_NestedAliveComposesByAndNamingTheLayer(t *testing.T) {
	store := testStore(t)
	cfg := nestedConfig(t, taskFixture{alive: "false"}, bindPid+"\n[health.alive]\ntype = \"shell\"\nscript = \"true\"\n")
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default",
		nestedTasks(map[string]any{"pid": "1"}, map[string]any{"pid": "1"}, nil))

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.Healthy {
		t.Fatalf("report = %+v, want unhealthy — one layer's alive probe fails", report)
	}
	if !strings.Contains(report.Reason, "runtime") {
		t.Errorf("reason = %q, want it to name the failing layer", report.Reason)
	}
}

// TestEvaluateHealth_NestedLayerDeclaringNoHealthIsVacuous covers the other
// half of the AND: a layer with no `[health]` contributes nothing rather than
// counting as a failure or suppressing the layers that do declare one.
func TestEvaluateHealth_NestedLayerDeclaringNoHealthIsVacuous(t *testing.T) {
	store := testStore(t)
	cfg := nestedConfig(t, taskFixture{alive: "true"}, bindPid)
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default",
		nestedTasks(map[string]any{"pid": "1"}, map[string]any{"pid": "1"}, nil))

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.Healthy || !report.Declared {
		t.Errorf("report = %+v, want healthy and declared from the one layer that declares a probe", report)
	}
}

// TestEvaluateHealth_NestedActivityComposesByOr covers the activity OR:
// evidence from any layer counts, and each layer's fingerprint is carried
// under its own name so a change anywhere is visible as movement.
func TestEvaluateHealth_NestedActivityComposesByOr(t *testing.T) {
	store := testStore(t)
	cfg := nestedConfig(t,
		taskFixture{activity: `echo '{"fingerprint":"inner-1"}'`},
		bindPid+"\n[health.activity]\ntype = \"shell\"\nscript = \"echo '{\\\"fingerprint\\\":\\\"outer-1\\\"}'\"\n")
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default",
		nestedTasks(map[string]any{"pid": "1"}, map[string]any{"pid": "1"}, nil))

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.ActivityDeclared {
		t.Fatalf("report = %+v, want activity declared", report)
	}
	for _, want := range []string{"inner-1", "outer-1"} {
		if !strings.Contains(report.ActivityFingerprint, want) {
			t.Errorf("fingerprint = %q, want every declaring layer to contribute (%q missing)", report.ActivityFingerprint, want)
		}
	}
}

// TestEvaluateHealth_NestedProbeRunsWithTheEnclosingBindEnv covers the last
// of the inner-owned execution surfaces: a layer's probes run with the
// environment the layers outside it inject, exactly as its setup did.
func TestEvaluateHealth_NestedProbeRunsWithTheEnclosingBindEnv(t *testing.T) {
	store := testStore(t)
	cfg := nestedConfig(t, taskFixture{alive: `test "$PLECT_GUARD" = on`}, bindPid)
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default",
		nestedTasks(map[string]any{"pid": "1"}, map[string]any{"pid": "1"}, map[string]string{"PLECT_GUARD": "on"}))

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.Healthy {
		t.Errorf("report = %+v, want healthy — the inner probe should see the outer layer's bind.env", report)
	}
}

// gateSchema binds the inner task's two outputs and gives the composed task
// a contract to declare conditions against.
const gateSchema = `
[outputs.bind]
pid = { from = "inner.outputs.pid" }
state = { from = "inner.outputs.state" }

[outputs_schema]
type = "object"

[outputs_schema.properties.pid]
type = "string"

[outputs_schema.properties.state]
type = "string"
`

// TestCapture_NestedTerminalRendersAgainstTheDeclaringLayer covers the last
// surface that must read a layer's own contract: the verbs belong to the
// layer that declared them and name that layer's keys, which the composed
// contract may carry under other names.
func TestCapture_NestedTerminalRendersAgainstTheDeclaringLayer(t *testing.T) {
	store := testStore(t)
	cfg := nestedConfig(t,
		taskFixture{
			attach:   "true",
			capture:  `printf '%s' "endpoint {{.Self.interactive_endpoint}} pid {{.Self.pid}}"`,
			sendText: "true",
			sendKeys: "true",
		},
		`
[outputs.bind]
interactive_endpoint = { from = "inner.outputs.interactive_endpoint" }
agent_pid = { from = "inner.outputs.pid" }

[outputs_schema]
type = "object"

[outputs_schema.properties.interactive_endpoint]
type = "string"

[outputs_schema.properties.agent_pid]
type = "string"
`)
	inner := map[string]any{"interactive_endpoint": "%1", "pid": "7"}
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default",
		nestedTasks(map[string]any{"interactive_endpoint": "%1", "agent_pid": "7"}, inner, nil))

	out, err := Capture(cfg, store, CaptureParams{Identifier: "owner/repo-1"})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if out.Content != "endpoint %1 pid 7" {
		t.Errorf("capture = %q, want the declaring layer's own keys resolved", out.Content)
	}
}

// Each layer's probes resolve `{{bin}}` against the file that layer was
// declared in, so a chain whose inner task ships in a plugin keeps its own
// plugin's executables reachable no matter which file names it from outside.
func TestProbeTargets_CarryEachLayersOwnSourcePath(t *testing.T) {
	cfg := nestedConfig(t, taskFixture{alive: "true"}, bindPid+"\n[health.alive]\ntype = \"shell\"\nscript = \"true\"\n")
	defs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	def := defs["team_runtime"]
	st := nestedTasks(map[string]any{"pid": "1"}, map[string]any{"pid": "1"}, nil)["runtime"]

	comp, err := composeInstance(def, st, task.SessionVars{Name: "s"})
	if err != nil {
		t.Fatalf("composeInstance: %v", err)
	}
	targets := probeTargets("runtime", def, st, comp)
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want one per layer", len(targets))
	}
	for i, want := range []string{"team_runtime.toml", "runtime.toml"} {
		if !strings.HasSuffix(targets[i].SourcePath, want) {
			t.Errorf("layer %d SourcePath = %q, want a path ending in %q", i, targets[i].SourcePath, want)
		}
	}
}

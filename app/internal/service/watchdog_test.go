package service

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestPersistHealthState_UpdateFailureLogsWarning(t *testing.T) {
	store := testStore(t)
	// No session named this in the store, so store.Update fails with "no
	// state entry" — this pins the best-effort swallow at
	// persistHealthState: the healthcheck cycle must not abort, but the
	// failure must not be silent either.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	persistHealthState(store, "owner/does-not-exist", HealthReport{SessionName: "owner/does-not-exist", Healthy: true}, time.Now())

	if !bytes.Contains(logs.Bytes(), []byte("persist health state failed")) {
		t.Errorf("expected a warning about the failed persist, got log output: %q", logs.String())
	}
}

// aliveFixtureConfig declares one run-scoped task "runner" whose
// `[health].alive` probe is the given shell command, plus a bare
// (no-outputs-required) "initial" node so LoadTaskDefinitions has a workflow
// to resolve against.
func aliveFixtureConfig(t *testing.T, alive string) *config.Config {
	t.Helper()
	return writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{{
		id:    "runner",
		scope: contract.TaskScopeRun,
		alive: alive,
	}}, []nodeFixture{{id: "initial", uses: "runner"}})
}

func TestEvaluateHealth_NoRunScopedTasksIsHealthy(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "false")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, TaskID: "runner", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.Healthy {
		t.Fatalf("report = %+v, want healthy (nothing run-scoped to probe)", report)
	}
}

func TestEvaluateHealth_PassingAliveProbeIsHealthy(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.Healthy {
		t.Fatalf("report = %+v, want healthy", report)
	}
}

func TestEvaluateHealth_FailingAliveProbeIsUnhealthy(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "false")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.Healthy || !strings.Contains(report.Reason, "initial") {
		t.Fatalf("report = %+v, want unhealthy naming the failing instance", report)
	}
}

// currentPlanConfig declares two run-scoped nodes, "pane" and "agent", each a
// plain effect whose only declaration is the given [health.alive] command.
func currentPlanConfig(t *testing.T, paneAlive, agentAlive string) *config.Config {
	t.Helper()
	return writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{id: "pane", scope: contract.TaskScopeRun, alive: paneAlive},
		{id: "agent", scope: contract.TaskScopeRun, alive: agentAlive},
	}, []nodeFixture{{id: "pane"}, {id: "agent"}})
}

func TestEvaluateHealth_FailedCurrentPlanNodeIsUnhealthyNamingNodeAndError(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane":  {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		"agent": {Scope: contract.TaskScopeRun, TaskID: "agent", Status: contract.TaskStatusFailed, Error: "claude not detected within 120s"},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want unhealthy (a failed current-plan node)", report, report.State())
	}
	if !strings.Contains(report.Reason, "agent") || !strings.Contains(report.Reason, "claude not detected within 120s") {
		t.Fatalf("reason = %q, want it to name the failed node and its setup error", report.Reason)
	}
	sess, err := store.GetE("owner/repo-1")
	if err != nil {
		t.Fatalf("GetE: %v", err)
	}
	if sessionRunState(cfg, sess) != domain.RunUp {
		t.Fatalf("run = %q, want up (a run-scoped node produced)", sessionRunState(cfg, sess))
	}
}

func TestEvaluateHealth_MissingCurrentPlanNodeIsUnhealthyNamingNode(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane": {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		// "agent" is declared by the workflow but has no task state at all.
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want unhealthy (a missing current-plan node)", report, report.State())
	}
	if !strings.Contains(report.Reason, "agent") {
		t.Fatalf("reason = %q, want it to name the missing node", report.Reason)
	}
}

func TestEvaluateHealth_EveryCurrentPlanNodeProducedAndPassingIsHealthy(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane":  {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		"agent": {Scope: contract.TaskScopeRun, TaskID: "agent", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy", report, report.State())
	}
}

func TestEvaluateHealth_CleanedCurrentPlanNodesAreNotUnhealthyAfterDown(t *testing.T) {
	store := testStore(t)
	// A failing alive command on both nodes proves cleaned nodes are never
	// probed at all — if they were, this would read unhealthy instead.
	cfg := currentPlanConfig(t, "false", "false")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane":  {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusCleaned},
		"agent": {Scope: contract.TaskScopeRun, TaskID: "agent", Status: contract.TaskStatusCleaned},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() == domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want no verdict for a down session", report, report.State())
	}
	sess, err := store.GetE("owner/repo-1")
	if err != nil {
		t.Fatalf("GetE: %v", err)
	}
	if sessionRunState(cfg, sess) != domain.RunDown {
		t.Fatalf("run = %q, want down", sessionRunState(cfg, sess))
	}
}

func TestEvaluateHealth_NoProducedRunScopedNodeAfterAbortedFirstUpIsNotUnhealthy(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane": {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusFailed, Error: "setup failed"},
		// "agent" never attempted since "pane" failed first.
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() == domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want no verdict (no run-scoped node ever produced)", report, report.State())
	}

	hc, err := HealthcheckSession(cfg, store, HealthcheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("HealthcheckSession: %v", err)
	}
	if hc.Pushed {
		t.Fatalf("report = %+v, want no escalation for a session that never came up", hc)
	}
}

func TestEvaluateHealth_StaleTaskEntryContributesNothing(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{id: "pane", scope: contract.TaskScopeRun, alive: "true"},
		{id: "stale_runtime", scope: contract.TaskScopeRun, alive: "false"},
	}, []nodeFixture{{id: "pane"}}) // "stale_runtime" keeps a definition but no node uses it anymore.
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane":          {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		"stale_runtime": {Scope: contract.TaskScopeRun, TaskID: "stale_runtime", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (the stale node's failing probe must not count)", report, report.State())
	}
}

func TestEvaluateHealth_StaleProducedNodeAloneReadsDownWithNoVerdict(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true") // declares only "pane" and "agent"
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		// "removed_node" is a stale record: the workflow above no longer
		// declares any node using it. Neither "pane" nor "agent" has any
		// task state at all.
		"removed_node": {Scope: contract.TaskScopeRun, TaskID: "removed_node", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() == domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want no verdict (only a stale entry is produced)", report, report.State())
	}
	sess, err := store.GetE("owner/repo-1")
	if err != nil {
		t.Fatalf("GetE: %v", err)
	}
	if sessionRunState(cfg, sess) != domain.RunDown {
		t.Fatalf("run = %q, want down (the only produced entry is stale)", sessionRunState(cfg, sess))
	}
}

// A failed session-scoped node already blocks create/repair elsewhere, which
// is why this report does not also need to cover it.
func TestEvaluateHealth_FailedSessionScopedNodeDoesNotAffectRunScopedHealthReport(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{id: "guard", scope: contract.TaskScopeSession, alive: "false"},
		{id: "pane", scope: contract.TaskScopeRun, alive: "true"},
	}, []nodeFixture{{id: "guard"}, {id: "pane"}})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"guard": {Scope: contract.TaskScopeSession, TaskID: "guard", Status: contract.TaskStatusFailed, Error: "guard missing"},
		"pane":  {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (a failed session-scoped node is out of scope for this report)", report, report.State())
	}
}

func TestEvaluateHealth_UnresolvableWorkflowReturnsError(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true") // only declares workflow "default"
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "ghost-workflow", map[string]*contract.TaskState{
		"pane": {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
	})

	if _, err := EvaluateHealth(cfg, store, "owner/repo-1"); err == nil {
		t.Fatal("expected a plan-resolution error for a session whose workflow no longer resolves")
	}
}

// A healthcheck must see its own node's resolved inputs (e.g. tmux_session),
// not just .Self outputs — needed to re-derive stale pid/session_id/
// socket_path when the pane's process restarts under it.
func TestEvaluateHealth_AliveProbeCanReadNodeInputs(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, `[ "{{.Inputs.tmux_session}}" = "work:0" ]`)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:  contract.TaskScopeRun,
			TaskID: "runner",
			Status: contract.TaskStatusProduced,
			Inputs: map[string]any{"tmux_session": "work:0"},
		},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.Healthy {
		t.Fatalf("report = %+v, want healthy (node inputs must be visible to healthcheck)", report)
	}
}

func TestEvaluateHealth_AliveProbeCanBindTerminalPID(t *testing.T) {
	agent := startSleepProcess(t)
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{
			id:    "pane",
			scope: contract.TaskScopeRun,
			extra: `
[terminal.pid]
type   = "shell"
script = "printf '%s\n' \"$root_pid\""

[terminal.pid.bind]
root_pid = { from = "self.outputs.root_pid" }
`,
		},
		{
			id:    "runtime",
			scope: contract.TaskScopeRun,
			alive: `
pane_pid=$(sh -c "{{terminal "pid"}}" terminal-pid)
[ "$pane_pid" = "{{.Inputs.pane_pid}}" ] || exit 1
kill -0 "{{.Self.pid}}" 2>/dev/null
`,
		},
	}, []nodeFixture{{id: "pane"}, {id: "runtime"}})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane": {
			Scope:   contract.TaskScopeRun,
			TaskID:  "pane",
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"root_pid": os.Getpid()},
		},
		"runtime": {
			Scope:   contract.TaskScopeRun,
			TaskID:  "runtime",
			Status:  contract.TaskStatusProduced,
			Inputs:  map[string]any{"pane_pid": os.Getpid()},
			Outputs: map[string]any{"pid": agent.Process.Pid},
		},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy with terminal pid bind resolved", report, report.State())
	}

	stopProcess(t, agent)

	report, err = EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth after process exit: %v", err)
	}
	if report.State() != domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want unhealthy after agent process exits", report, report.State())
	}
	if strings.Contains(report.Reason, `terminal capability "pid" is not available`) {
		t.Fatalf("reason = %q, terminal pid bind should still resolve", report.Reason)
	}
}

func TestEvaluateHealth_UnproducedInvalidDefinitionDoesNotBlockTerminalBind(t *testing.T) {
	agent := startSleepProcess(t)
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{
			id:    "pane",
			scope: contract.TaskScopeRun,
			extra: `
[terminal.pid]
type   = "shell"
script = "printf '%s\n' \"$root_pid\""

[terminal.pid.bind]
root_pid = { from = "self.outputs.root_pid" }
`,
		},
		{
			id:    "runtime",
			scope: contract.TaskScopeRun,
			alive: `
sh -c "{{terminal "pid"}}" terminal-pid >/dev/null
kill -0 "{{.Self.pid}}" 2>/dev/null
`,
		},
		{
			id:    "drifted",
			scope: contract.TaskScopeRun,
			extra: `
[outputs_schema]
type = 1
`,
		},
	}, []nodeFixture{{id: "pane"}, {id: "runtime"}, {id: "drifted"}})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane": {
			Scope:   contract.TaskScopeRun,
			TaskID:  "pane",
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"root_pid": os.Getpid()},
		},
		"runtime": {
			Scope:   contract.TaskScopeRun,
			TaskID:  "runtime",
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"pid": agent.Process.Pid},
		},
		"drifted": {
			Scope:  contract.TaskScopeRun,
			TaskID: "drifted",
			Status: contract.TaskStatusProduced,
		},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want unrelated invalid definition ignored", report, report.State())
	}
}

func TestEvaluateHealth_DuplicateProducedTerminalNodesError(t *testing.T) {
	store := testStore(t)
	terminal := `
[terminal.pid]
type   = "shell"
script = "true"
`
	cfg := writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{id: "pane_a", scope: contract.TaskScopeRun, extra: terminal},
		{id: "pane_b", scope: contract.TaskScopeRun, extra: terminal},
		{id: "runtime", scope: contract.TaskScopeRun, alive: "true"},
	}, []nodeFixture{{id: "pane_a"}, {id: "pane_b"}, {id: "runtime"}})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"pane_a":  {Scope: contract.TaskScopeRun, TaskID: "pane_a", Status: contract.TaskStatusProduced},
		"pane_b":  {Scope: contract.TaskScopeRun, TaskID: "pane_b", Status: contract.TaskStatusProduced},
		"runtime": {Scope: contract.TaskScopeRun, TaskID: "runtime", Status: contract.TaskStatusProduced},
	})

	_, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err == nil {
		t.Fatal("expected an error for duplicate produced terminal nodes")
	}
	if !strings.Contains(err.Error(), "resolve terminal binding") || !strings.Contains(err.Error(), "pane_a") || !strings.Contains(err.Error(), "pane_b") {
		t.Fatalf("error = %q, want duplicate terminal nodes named", err.Error())
	}
}

func startSleepProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			stopProcess(t, cmd)
		}
	})
	return cmd
}

func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.ProcessState != nil {
		return
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep process: %v", err)
	}
	if err := cmd.Wait(); err != nil && !strings.Contains(err.Error(), "killed") {
		t.Fatalf("wait for sleep process: %v", err)
	}
}

func TestSessionRunAndHealthState_AliveProbeBacked(t *testing.T) {
	tests := []struct {
		name       string
		alive      string
		tasks      map[string]*contract.TaskState
		wantRun    domain.RunState
		wantHealth domain.HealthState
	}{
		{
			name:  "down when no run scoped task is produced",
			alive: "false",
			tasks: map[string]*contract.TaskState{
				"initial": {Scope: contract.TaskScopeSession, TaskID: "runner", Status: contract.TaskStatusProduced},
			},
			wantRun:    domain.RunDown,
			wantHealth: domain.HealthUndeclared,
		},
		{
			name:  "healthy when produced run scoped alive probe passes",
			alive: "true",
			tasks: map[string]*contract.TaskState{
				"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
			},
			wantRun:    domain.RunUp,
			wantHealth: domain.HealthHealthy,
		},
		{
			name:  "unhealthy when produced run scoped alive probe fails",
			alive: "false",
			tasks: map[string]*contract.TaskState{
				"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
			},
			wantRun:    domain.RunUp,
			wantHealth: domain.HealthUnhealthy,
		},
		{
			name:  "undeclared when produced run scoped task declares no alive probe",
			alive: "",
			tasks: map[string]*contract.TaskState{
				"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
			},
			wantRun:    domain.RunUp,
			wantHealth: domain.HealthUndeclared,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := testStore(t)
			cfg := aliveFixtureConfig(t, tt.alive)
			seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", tt.tasks)

			s := store.Get("owner/repo-1")
			if gotRun := sessionRunState(cfg, s); gotRun != tt.wantRun {
				t.Errorf("sessionRunState = %q, want %q", gotRun, tt.wantRun)
			}
			if gotHealth := sessionHealthState(cfg, store, "owner/repo-1"); gotHealth != tt.wantHealth {
				t.Errorf("sessionHealthState = %q, want %q", gotHealth, tt.wantHealth)
			}
		})
	}
}

// TestEvaluateHealth_MessageDoesNotAffectHealthOutcome characterizes the
// current gap the health-deepening plan targets: message is a latched
// self-report that health evaluation never reads. A session that reports
// "working" and one that reports "waiting" (or none at all) with the same
// alive-probe outcome must produce the same health report, because
// evaluateHealthFor only consults the declared probes, never Message.
func TestEvaluateHealth_MessageDoesNotAffectHealthOutcome(t *testing.T) {
	messages := []*domain.Message{
		nil,
		{Text: "working"},
		{Text: "waiting"},
		{Text: "anything at all"},
	}

	for _, alive := range []string{"true", "false"} {
		for _, msg := range messages {
			store := testStore(t)
			cfg := aliveFixtureConfig(t, alive)
			seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
				"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
			})
			if err := store.Update("owner/repo-1", func(s *domain.Session) error {
				s.Message = msg
				return nil
			}); err != nil {
				t.Fatalf("set message: %v", err)
			}

			report, err := EvaluateHealth(cfg, store, "owner/repo-1")
			if err != nil {
				t.Fatalf("EvaluateHealth: %v", err)
			}
			wantHealthy := alive == "true"
			if report.Healthy != wantHealthy {
				t.Fatalf("alive=%q message=%+v: report.Healthy = %v, want %v (message must not influence health)", alive, msg, report.Healthy, wantHealthy)
			}
		}
	}
}

// activityFixtureConfig declares the two halves health reads. The run-scoped
// effect "runner" owns the probes: `[health].alive` fixed at "true", and
// `[health].activity` the given shell snippet. The task document "gate" owns
// the work: one check leaf against the observed "done" fact, so an instance
// observed as anything but "yes" leaves the session owing progress.
func activityFixtureConfig(t *testing.T, activity string) *config.Config {
	t.Helper()
	return writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{
			id:       "runner",
			scope:    contract.TaskScopeRun,
			alive:    "true",
			activity: activity,
		},
		{
			id:    "gate",
			extra: "[[done_when.all]]\ncheck = \"resource.state.done\"\neq = \"yes\"\n",
		},
	}, []nodeFixture{{id: "initial", uses: "runner"}})
}

// gateInstance is one live instance of the "gate" document, observed to
// report the given "done" fact.
func gateInstance(done string) *contract.TaskState {
	return &contract.TaskState{
		Scope:    contract.TaskScopeSession,
		TaskID:   "gate",
		Status:   contract.TaskStatusProduced,
		Observed: observedFacts(map[string]any{"done": done}),
	}
}

// activityProbeCmd renders a fixed JSON activity envelope as the probe's
// entire behavior — a stub probe, standing in for whatever concrete plugin
// implementation (pane fingerprint, agent turn-boundary record, ...) ships it.
func activityProbeCmd(t *testing.T, fingerprint string, silenceExpected bool, observedAt time.Time) string {
	t.Helper()
	observed := ""
	if !observedAt.IsZero() {
		observed = observedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		`echo '{"fingerprint":%q,"silence_expected":%t,"observed_at":%q}'`,
		fingerprint, silenceExpected, observed,
	)
}

// noBasisProbeCmd is the shape a probe with no evidence to report emits:
// exit 0 and nothing on stdout.
const noBasisProbeCmd = "true"

// TestEvaluateHealth_WedgedButAliveProbePassingReadsStalled replaces the
// prior known gap this test used to pin: once an activity probe is declared
// and reports stale evidence while unmet done_when work remains, evaluation
// now reads stalled rather than healthy — a present-but-wedged session is
// finally distinguishable from a genuinely healthy one.
func TestEvaluateHealth_WedgedButAliveProbePassingReadsStalled(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityProbeCmd(t, "fp-1", false, longAgo))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})
	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		// LastTickAt far in the past and a stale "waiting" message stand in
		// for a wedged-but-present execution surface. Neither is read by
		// health evaluation (see TestEvaluateHealth_MessageDoesNotAffectHealthOutcome)
		// — the stale activity evidence is what actually drives the
		// stalled outcome here.
		s.LastTickAt = longAgo
		s.Message = &domain.Message{Text: "waiting", UpdatedAt: longAgo}
		s.Health = &contract.HealthState{LastFingerprint: "initial:fp-1", LastActivityAt: longAgo}
		return nil
	}); err != nil {
		t.Fatalf("seed wedged state: %v", err)
	}

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthStalled {
		t.Fatalf("report = %+v, state = %q, want stalled (surface present, activity expected, evidence stale)", report, report.State())
	}
}

// TestEvaluateHealth_SilenceExpectedNarrowsExpectationToHealthy pins the
// pardon: done_when leaves unmet work, but the probe declares the
// fingerprint's stability intended (e.g. "the turn already ended") with stale
// evidence. The pardon can only narrow — never widen — done_when's
// expectation, so this instance's contribution drops out and the session
// reads healthy, not stalled. Its contrast is
// TestEvaluateHealth_ActivityFingerprintUnchangedPastWindowReadsStalled,
// which is the same shape without the pardon and reads stalled.
func TestEvaluateHealth_SilenceExpectedNarrowsExpectationToHealthy(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityProbeCmd(t, "fp-1", true, longAgo))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (the pardon narrowed the expectation despite unmet done_when work)", report, report.State())
	}
	if report.ActivityFingerprint != "initial:fp-1" {
		t.Errorf("ActivityFingerprint = %q, want the pardoned envelope's fingerprint to still contribute", report.ActivityFingerprint)
	}
}

// TestEvaluateHealth_NoActivitySignalDeclaredStaysUndeclaredNotStalled pins
// the explicit no-signal path: unmet done_when work exists (progress is
// expected) but no activity probe is declared to judge it, so evaluation
// has no basis to call the session either healthy or stalled.
func TestEvaluateHealth_NoActivitySignalDeclaredStaysUndeclaredNotStalled(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, "") // no activity probe declared at all
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUndeclared {
		t.Fatalf("report = %+v, state = %q, want undeclared (activity expected, nothing declared to judge it)", report, report.State())
	}
}

// TestEvaluateHealth_EmptyProbeOutputStaysUndeclared covers the second
// no-basis path: an activity probe is declared but exits 0 with empty stdout
// at evaluation time — the same as if nothing were declared, not a
// stale/fresh judgment either way, and not a fault worth warning about.
func TestEvaluateHealth_EmptyProbeOutputStaysUndeclared(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, noBasisProbeCmd)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUndeclared {
		t.Fatalf("report = %+v, state = %q, want undeclared (exit 0, empty stdout)", report, report.State())
	}
	if len(report.ProbeErrors) != 0 {
		t.Errorf("ProbeErrors = %+v, want none — declaring no evidence is not a fault", report.ProbeErrors)
	}
}

// TestEvaluateHealth_ProbeExitFailureContributesNothingAndReportsFault pins
// the orthogonality the envelope rests on: stdout decides contribution and
// the exit code decides the health of the probe itself. A probe that cannot
// run contributes no evidence, exactly like one with no basis, but it is
// named in the report so a persistently broken probe stays observable
// instead of passing for silence.
func TestEvaluateHealth_ProbeExitFailureContributesNothingAndReportsFault(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, "echo 'pane is gone' >&2; exit 3")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUndeclared {
		t.Fatalf("report = %+v, state = %q, want undeclared (a failed probe contributes nothing)", report, report.State())
	}
	if report.ActivityDeclared || report.ActivityFingerprint != "" {
		t.Errorf("report = %+v, want no activity contribution", report)
	}
	if len(report.ProbeErrors) != 1 {
		t.Fatalf("ProbeErrors = %+v, want exactly one entry", report.ProbeErrors)
	}
	got := report.ProbeErrors[0]
	if got.Instance != "initial" {
		t.Errorf("Instance = %q, want %q", got.Instance, "initial")
	}
	if !strings.Contains(got.Command, "pane is gone") {
		t.Errorf("Command = %q, want the declared probe command", got.Command)
	}
	if got.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", got.ExitCode)
	}
	if got.Stderr != "pane is gone" {
		t.Errorf("Stderr = %q, want the stderr digest", got.Stderr)
	}
}

// TestEvaluateHealth_InvalidEnvelopeContributesNothingAndWarns covers the
// third non-contributing shape: the probe ran and printed something, but the
// envelope carries no fingerprint (the retired status-enum shape lands here
// too). Neither discarding it silently nor fabricating a fingerprint is safe,
// so it is reported as a fault the same way an unrunnable probe is.
func TestEvaluateHealth_InvalidEnvelopeContributesNothingAndWarns(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, `echo '{"status":"active"}'`)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.ActivityDeclared || report.ActivityFingerprint != "" {
		t.Errorf("report = %+v, want no activity contribution", report)
	}
	if len(report.ProbeErrors) != 1 {
		t.Fatalf("ProbeErrors = %+v, want exactly one entry", report.ProbeErrors)
	}
	if !strings.Contains(report.ProbeErrors[0].Reason, "fingerprint") {
		t.Errorf("Reason = %q, want it to name the missing field", report.ProbeErrors[0].Reason)
	}
}

// TestEvaluateHealth_LapsedProbeWithPriorActivityReadsStalledNotUndeclared
// pins the fix for the undeclared predicate becoming historical: a session
// that has recorded activity before must never fall back to undeclared just
// because its activity probe has since lost its basis (e.g. lost hook wiring
// after a pane restart) — the recorded LastActivityAt is a permanent basis
// to judge staleness, so a lapsed probe past the stall threshold reads
// stalled, exactly like a probe that is still reporting but stale.
func TestEvaluateHealth_LapsedProbeWithPriorActivityReadsStalledNotUndeclared(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	// The probe itself now reports no basis to judge activity this tick,
	// standing in for its source having died.
	cfg := activityFixtureConfig(t, noBasisProbeCmd)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})
	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		s.Health = &contract.HealthState{LastFingerprint: "initial:fp-1", LastActivityAt: longAgo}
		return nil
	}); err != nil {
		t.Fatalf("seed prior activity state: %v", err)
	}

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthStalled {
		t.Fatalf("report = %+v, state = %q, want stalled (prior activity observation must survive the probe losing its basis)", report, report.State())
	}
}

// TestEvaluateHealth_FreshActivityEvidenceReadsHealthy is the positive
// counterpart to the stalled test: the same unmet work, but the declared
// activity probe reports evidence timestamped within the stall threshold.
func TestEvaluateHealth_FreshActivityEvidenceReadsHealthy(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, activityProbeCmd(t, "fp-1", false, time.Now()))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (fresh activity evidence)", report, report.State())
	}
}

// TestEvaluateHealth_NoUnmetWorkStaysHealthyRegardlessOfSignal is the
// orchestrator-idle-between-ticks case: done_when is already satisfied, so
// activity is not currently expected of this session at all. It must read
// healthy even though an activity probe is declared and its evidence is
// stale — the session legitimately has nothing to move on right now, and
// must never be flagged stalled for that.
func TestEvaluateHealth_NoUnmetWorkStaysHealthyRegardlessOfSignal(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityProbeCmd(t, "fp-1", false, longAgo))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:   contract.TaskScopeRun,
			TaskID:  "runner",
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"done": "yes"}, // done_when already satisfied
		},
		"gate": gateInstance("yes"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (no unmet work, activity not expected)", report, report.State())
	}
}

// TestEvaluateHealth_EscalatedWorkStillExpectsActivity covers the watchdog
// interaction from the rewritten issue: done_when escalation must not make the
// child disappear from stall monitoring.
func TestEvaluateHealth_EscalatedWorkStillExpectsActivity(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityProbeCmd(t, "fp-1", false, longAgo))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
			DoneWhen: &contract.DoneWhenState{
				LastAction: "escalate",
			},
		},
		"gate": gateInstance("no"),
	})
	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		s.Health = &contract.HealthState{LastFingerprint: "initial:fp-1", LastActivityAt: longAgo}
		return nil
	}); err != nil {
		t.Fatalf("seed activity state: %v", err)
	}

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthStalled {
		t.Fatalf("report = %+v, state = %q, want stalled (escalation must not suppress activity expectation)", report, report.State())
	}
}

// activityFingerprintCmd is a stub activity probe reporting the given
// fingerprint with no observed_at of its own, so freshness is judged purely
// by core's own comparison against the persisted fingerprint.
func activityFingerprintCmd(t *testing.T, fingerprint string) string {
	t.Helper()
	return activityProbeCmd(t, fingerprint, false, time.Time{})
}

// setActivityState seeds the session's persisted HealthState activity fields,
// standing in for a prior tick's probes having already advanced (or not) the
// fingerprint core has on record. composite is the "<node id>:<probe
// fingerprint>" join evaluateHealthFor builds across declaring instances.
func setActivityState(t *testing.T, store *state.Store, name, composite string, observedAt time.Time) {
	t.Helper()
	if err := store.Update(name, func(s *domain.Session) error {
		s.Health = &contract.HealthState{LastFingerprint: composite, LastActivityAt: observedAt}
		return nil
	}); err != nil {
		t.Fatalf("seed activity state: %v", err)
	}
}

// TestEvaluateHealth_ActivityFingerprintUnchangedPastWindowReadsStalled
// pins the core-side comparison the issue asks for: the declared activity
// probe keeps reporting the same fingerprint core already has on record,
// and core's own ObservedAt for that fingerprint is far outside the
// freshness window — unmet done_when work makes this stalled, not healthy.
func TestEvaluateHealth_ActivityFingerprintUnchangedPastWindowReadsStalled(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityFingerprintCmd(t, "fp-1"))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})
	setActivityState(t, store, "owner/repo-1", "initial:fp-1", longAgo)

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthStalled {
		t.Fatalf("report = %+v, state = %q, want stalled (fingerprint unchanged past freshness window)", report, report.State())
	}
}

// TestEvaluateHealth_ActivityFingerprintAdvancedReadsHealthy is the
// positive counterpart: the source now reports a fingerprint different from
// the one core has on record, even though the record is stale — an advance
// always reads healthy and resets core's own ObservedAt clock for it.
func TestEvaluateHealth_ActivityFingerprintAdvancedReadsHealthy(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityFingerprintCmd(t, "fp-2"))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})
	setActivityState(t, store, "owner/repo-1", "initial:fp-1", longAgo)

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (fingerprint advanced)", report, report.State())
	}

	s := store.Get("owner/repo-1")
	if s == nil || s.Health == nil || s.Health.LastFingerprint != "initial:fp-2" {
		t.Fatalf("persisted health = %+v, want fingerprint %q", s.Health, "initial:fp-2")
	}
	if s.Health.LastActivityAt.Before(longAgo.Add(23 * time.Hour)) {
		t.Fatalf("persisted LastActivityAt = %v, want reset to roughly now on advance", s.Health.LastActivityAt)
	}
}

// TestEvaluateHealth_ActivityFingerprintUnchangedWithinWindowReadsHealthy
// pins the within-window boundary: same fingerprint core already has on
// record, but core's own ObservedAt for it is recent — still healthy.
func TestEvaluateHealth_ActivityFingerprintUnchangedWithinWindowReadsHealthy(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, activityFingerprintCmd(t, "fp-1"))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})
	setActivityState(t, store, "owner/repo-1", "initial:fp-1", time.Now())

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (fingerprint unchanged but within freshness window)", report, report.State())
	}
}

// TestEvaluateHealth_NoActivityProbeDeclaredFallsBackToUndeclared confirms a
// task declaring no `[health].activity` at all reads undeclared, not stalled,
// despite unmet done_when work — the activity probe is optional, not
// mandatory.
func TestEvaluateHealth_NoActivityProbeDeclaredFallsBackToUndeclared(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, "") // no activity probe anywhere
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUndeclared {
		t.Fatalf("report = %+v, state = %q, want undeclared (no activity probe declared)", report, report.State())
	}
}

func TestHealthcheckSession_PushesHealthEscalationAndRenotifies(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "false")
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")
	healthCfg := config.HealthcheckConfig{
		Period:         config.Duration{Duration: time.Minute},
		StallThreshold: config.Duration{Duration: 3 * time.Minute},
		RenotifyEvery:  2,
	}

	report, err := HealthcheckSession(cfg, store, HealthcheckParams{SessionName: "owner/repo-1", Config: healthCfg})
	if err != nil {
		t.Fatalf("HealthcheckSession: %v", err)
	}
	if report.State() != domain.HealthUnhealthy || !report.Pushed {
		t.Fatalf("report = %+v, want pushed unhealthy report", report)
	}
	assertHealthEscalations := func(want int) []event.Event {
		t.Helper()
		evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-orchestrator", 0, event.Filter{Types: []string{event.TypeTerminalEscalate}})
		if err != nil {
			t.Fatalf("list health escalations: %v", err)
		}
		if len(evs) != want {
			t.Fatalf("health escalation events = %+v, want %d", evs, want)
		}
		return evs
	}
	evs := assertHealthEscalations(1)
	if evs[0].Metadata["escalation_kind"] != "health.unhealthy" || evs[0].Metadata["health_state"] != "unhealthy" || evs[0].Metadata["last_checked_at"] == "" {
		t.Fatalf("health escalation metadata = %+v", evs[0].Metadata)
	}

	report, err = HealthcheckSession(cfg, store, HealthcheckParams{SessionName: "owner/repo-1", Config: healthCfg})
	if err != nil {
		t.Fatalf("second HealthcheckSession: %v", err)
	}
	if report.Pushed {
		t.Fatalf("second report = %+v, want immediate duplicate suppressed", report)
	}
	assertHealthEscalations(1)

	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		s.Health.LastNotifiedAt = time.Now().Add(-3 * time.Minute)
		return nil
	}); err != nil {
		t.Fatalf("age notification state: %v", err)
	}
	report, err = HealthcheckSession(cfg, store, HealthcheckParams{SessionName: "owner/repo-1", Config: healthCfg})
	if err != nil {
		t.Fatalf("renotify HealthcheckSession: %v", err)
	}
	if !report.Pushed {
		t.Fatalf("renotify report = %+v, want pushed report after renotify window", report)
	}
	assertHealthEscalations(2)
}

func TestHealthcheckSession_PushesStalledEscalationWithActivityTimestamp(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := activityFixtureConfig(t, activityFingerprintCmd(t, "fp-1"))
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "runner",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate": gateInstance("no"),
	})
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")
	setActivityState(t, store, "owner/repo-1", "initial:fp-1", longAgo)

	report, err := HealthcheckSession(cfg, store, HealthcheckParams{
		SessionName: "owner/repo-1",
		Config: config.HealthcheckConfig{
			Period:         config.Duration{Duration: time.Minute},
			StallThreshold: config.Duration{Duration: 5 * time.Minute},
			RenotifyEvery:  3,
		},
	})
	if err != nil {
		t.Fatalf("HealthcheckSession: %v", err)
	}
	if report.State() != domain.HealthStalled || !report.Pushed {
		t.Fatalf("report = %+v, want pushed stalled report", report)
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-orchestrator", 0, event.Filter{Types: []string{event.TypeTerminalEscalate}})
	if err != nil {
		t.Fatalf("list health escalations: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("health escalation events = %+v, want 1", evs)
	}
	if evs[0].Metadata["escalation_kind"] != "health.stalled" || evs[0].Metadata["health_state"] != "stalled" || evs[0].Metadata["last_activity_at"] == "" {
		t.Fatalf("stalled health escalation metadata = %+v", evs[0].Metadata)
	}
}

func TestHealthcheckSession_SkipsDeadIntermediateParent(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "false")
	seedSession(t, store, "owner/repo-grandparent", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-parent", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	setParent(t, store, "owner/repo-parent", "owner/repo-grandparent")
	setParent(t, store, "owner/repo-1", "owner/repo-parent")

	report, err := HealthcheckSession(cfg, store, HealthcheckParams{
		SessionName: "owner/repo-1",
		Config: config.HealthcheckConfig{
			Period:         config.Duration{Duration: time.Minute},
			StallThreshold: config.Duration{Duration: 3 * time.Minute},
			RenotifyEvery:  2,
		},
	})
	if err != nil {
		t.Fatalf("HealthcheckSession: %v", err)
	}
	if !report.Pushed || report.PushTarget != "owner/repo-grandparent" {
		t.Fatalf("report = %+v, want health escalation pushed past the dead parent", report)
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-grandparent", 0, event.Filter{Types: []string{event.TypeTerminalEscalate}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("grandparent escalations = %+v, want one", evs)
	}
	if evs[0].Metadata[event.MetaOriginSession] != "owner/repo-1" {
		t.Fatalf("escalation origin = %q, want owner/repo-1", evs[0].Metadata[event.MetaOriginSession])
	}
	if evs[0].Metadata[event.MetaRelation] != string(domain.RelationDescendant) {
		t.Fatalf("escalation relation = %q, want descendant", evs[0].Metadata[event.MetaRelation])
	}
}

func TestHealthcheckSession_RecordsUndeliverableEscalationWithNoLiveAncestor(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "false")
	seedSession(t, store, "owner/repo-parent", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	setParent(t, store, "owner/repo-1", "owner/repo-parent")

	report, err := HealthcheckSession(cfg, store, HealthcheckParams{
		SessionName: "owner/repo-1",
		Config: config.HealthcheckConfig{
			Period:         config.Duration{Duration: time.Minute},
			StallThreshold: config.Duration{Duration: 3 * time.Minute},
			RenotifyEvery:  2,
		},
	})
	if err != nil {
		t.Fatalf("HealthcheckSession: %v", err)
	}
	if !report.Pushed || report.PushTarget != "owner/repo-1" {
		t.Fatalf("report = %+v, want local undeliverable record", report)
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-1", 0, event.Filter{Types: []string{event.TypeTerminalDead}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("local dead events = %+v, want one", evs)
	}
	if evs[0].Metadata[event.MetaOriginSession] != "owner/repo-1" {
		t.Fatalf("origin = %q, want owner/repo-1", evs[0].Metadata[event.MetaOriginSession])
	}
	if evs[0].Metadata["escalation_kind"] != "health.unhealthy" {
		t.Fatalf("metadata = %+v, want health escalation kind", evs[0].Metadata)
	}
}

// A prior unhealthy verdict must not resurface once the session is down:
// repeated sweeps must never re-probe, re-escalate, or wake it.
func TestHealthcheckSession_DownSessionStaysDownAcrossRepeatedSweeps(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	// No parent: a health escalation with nowhere to go is exactly the
	// undeliverable case the loop hinged on.
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", nil)
	staleCheck := time.Now().Add(-time.Hour)
	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		s.Health = &contract.HealthState{
			LastCheckedAt: staleCheck,
			LastState:     string(domain.HealthUnhealthy),
			LastReason:    "initial: alive probe failed",
		}
		return nil
	}); err != nil {
		t.Fatalf("seed prior unhealthy verdict: %v", err)
	}
	healthCfg := config.HealthcheckConfig{
		Period:         config.Duration{Duration: time.Minute},
		StallThreshold: config.Duration{Duration: 3 * time.Minute},
		RenotifyEvery:  1,
	}

	for i := 0; i < 3; i++ {
		report, err := HealthcheckSession(cfg, store, HealthcheckParams{SessionName: "owner/repo-1", Config: healthCfg})
		if err != nil {
			t.Fatalf("sweep %d: HealthcheckSession: %v", i, err)
		}
		if report.Pushed {
			t.Fatalf("sweep %d: report = %+v, want a down session never pushed", i, report)
		}
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-1", 0, event.Filter{Types: []string{event.TypeTerminalDead, event.TypeTerminalEscalate}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("terminal events = %+v, want none (never probed, never escalated)", evs)
	}

	got := store.Get("owner/repo-1")
	if len(got.Tasks) != 0 {
		t.Fatalf("tasks = %+v, want untouched (never woken)", got.Tasks)
	}
	if !got.Health.LastCheckedAt.Equal(staleCheck) {
		t.Fatalf("Health.LastCheckedAt = %v, want unchanged %v (no verdict update on a down session)", got.Health.LastCheckedAt, staleCheck)
	}
	if got.Health.LastState != string(domain.HealthUnhealthy) {
		t.Fatalf("Health.LastState = %q, want the pre-down verdict left untouched", got.Health.LastState)
	}
}

func TestHealthcheckSession_UnknownSessionStillReturnsNotFound(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}

	_, err := HealthcheckSession(cfg, store, HealthcheckParams{SessionName: "owner/repo-missing"})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
}

// twoInstanceHealthConfig declares a "worker" task carrying the unmet
// done_when (so activity is expected of the session) plus a "sidecar" task,
// each with its own [health] table, wired as two nodes of one workflow.
func twoInstanceHealthConfig(t *testing.T, workerAlive, workerActivity, sidecarAlive, sidecarActivity string) *config.Config {
	t.Helper()
	return writeWorkflowFixture(t, t.TempDir(), "default", []taskFixture{
		{
			id:       "worker",
			scope:    contract.TaskScopeRun,
			alive:    workerAlive,
			activity: workerActivity,
			extra:    "[[done_when.all]]\ncheck = \"resource.state.done\"\neq = \"yes\"\n",
		},
		{
			id:       "sidecar",
			scope:    contract.TaskScopeRun,
			alive:    sidecarAlive,
			activity: sidecarActivity,
		},
	}, []nodeFixture{{id: "worker", uses: "worker"}, {id: "sidecar", uses: "sidecar"}})
}

func seedTwoInstances(t *testing.T, store *state.Store) {
	t.Helper()
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"worker": {
			Scope:    contract.TaskScopeRun,
			TaskID:   "worker",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"done": "no"}),
		},
		"gate":    gateInstance("no"),
		"sidecar": {Scope: contract.TaskScopeRun, TaskID: "sidecar", Status: contract.TaskStatusProduced},
	})
}

// TestEvaluateHealth_AliveComposesByANDNamingFailingInstance pins liveness as
// a chain of necessary resources: one failing probe is enough, and the report
// must say which instance failed.
func TestEvaluateHealth_AliveComposesByANDNamingFailingInstance(t *testing.T) {
	store := testStore(t)
	cfg := twoInstanceHealthConfig(t, "true", "", "false", "")
	seedTwoInstances(t, store)

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthUnhealthy {
		t.Fatalf("report = %+v, state = %q, want unhealthy (any failing alive probe)", report, report.State())
	}
	if !strings.Contains(report.Reason, "sidecar") {
		t.Errorf("reason = %q, want it to name the failing instance", report.Reason)
	}
}

// TestEvaluateHealth_ActivityComposesByORAcrossInstances pins the other half:
// a quiet instance beside a moving one must not read as a stall, or a
// legitimately idle sidecar would escalate against a session that is plainly
// working.
func TestEvaluateHealth_ActivityComposesByORAcrossInstances(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := twoInstanceHealthConfig(t,
		"true", activityFingerprintCmd(t, "advanced"),
		"true", activityFingerprintCmd(t, "quiet"))
	seedTwoInstances(t, store)
	// The persisted composite is what a prior tick recorded, with the worker
	// still on its old fingerprint.
	setActivityState(t, store, "owner/repo-1", "sidecar:quiet\x00worker:stale", longAgo)

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("report = %+v, state = %q, want healthy (one instance's evidence is enough)", report, report.State())
	}
}

// TestEvaluateHealth_UndeclaredInstanceCastsNoVote pins the vacuous case: an
// instance declaring no [health] neither fails the alive AND nor supplies
// activity evidence to the OR.
func TestEvaluateHealth_UndeclaredInstanceCastsNoVote(t *testing.T) {
	store := testStore(t)
	longAgo := time.Now().Add(-24 * time.Hour)
	cfg := twoInstanceHealthConfig(t, "true", activityFingerprintCmd(t, "fp-1"), "", "")
	seedTwoInstances(t, store)
	setActivityState(t, store, "owner/repo-1", "worker:fp-1", longAgo)

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthStalled {
		t.Fatalf("report = %+v, state = %q, want stalled (the declaring instance's fingerprint is unchanged)", report, report.State())
	}
}

// A workflow node's instance stores no task id when its id equals the node's,
// so the node id is what the health walk has to resolve an effect from. With
// the effect declared by a plugin, that id is not the address the effect
// answers to — the session's own workflow is what says which declaration the
// node runs.
func TestEvaluateHealth_NodeInstanceResolvesAPluginOwnedEffect(t *testing.T) {
	store := testStore(t)
	pluginDir, base := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pluginDir, "config", "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "run"

[runner.setup]
type   = "shell"
script = "true"

[runner.health.alive]
type   = "shell"
script = "false"
`)
	write(filepath.Join(base, "workflows", "default.toml"), `
[default]
kind = "workflow"

[[default.nodes]]
id   = "initial"
uses = "official.acme.runner"
`)
	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	// TaskID omitted, as it is for every node whose id equals the referenced
	// definition's id.
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
	})

	report, err := EvaluateHealth(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if !report.Declared {
		t.Fatalf("report = %+v, want the plugin's [health].alive to be found", report)
	}
	if report.Healthy {
		t.Errorf("report = %+v, want unhealthy: the found probe fails", report)
	}
}

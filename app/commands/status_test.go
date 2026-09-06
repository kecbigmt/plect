package commands

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// renderDoneWhenSections omits any task instance without a done_when — its
// lifecycle state is out of scope for this section entirely (available via
// --json --full), not merely elided.
func TestRenderDoneWhenSections_NoDoneWhenInstancesProduceNoSection(t *testing.T) {
	var buf bytes.Buffer
	renderDoneWhenSections(&buf, []service.StatusTask{
		{Instance: "envfile", Status: "produced"},
		{Instance: "@workflow", Status: "produced"},
	})
	if buf.Len() != 0 {
		t.Errorf("expected no output for instances without done_when, got %q", buf.String())
	}
}

// A done_when-bearing instance renders a "Done when (<instance>)" block with
// heartbeat budget/action, one line per condition, and one line per chain — no raw
// outputs dump, no per-task "Task <name> <status>" line.
func TestRenderDoneWhenSections_UnsatisfiedJudgeBlock(t *testing.T) {
	var buf bytes.Buffer
	renderDoneWhenSections(&buf, []service.StatusTask{
		{
			Instance:        "initial",
			Status:          "produced",
			HeartbeatTicks:  3,
			HeartbeatBudget: 3,
			Action:          "review_required",
			DoneWhen: &task.DoneWhenResult{
				Overall: task.DonePending,
				Leaves: []task.DoneLeafResult{
					{Kind: "check", Status: task.DoneSatisfied, Output: "checks_status", Value: "SUCCESS", Observed: true},
					{Kind: "judge", Status: task.DonePending, ID: "ac-met", Expr: "acceptance criteria are satisfied", CurrentRevision: "9256fbb5bc027adb611dc2093d1a120e139e7193"},
				},
			},
			Chains: []service.StatusChain{
				{ChainID: "review", AlreadyActive: true, TargetSession: "owner/repo-1+review-initial"},
			},
			Outputs: map[string]any{"instruction": "a very long multi-line prompt blob"},
		},
	})
	out := buf.String()

	for _, want := range []string{
		"Done when (initial)",
		"heartbeat budget: 3/3",
		"action: review_required",
		"conditions",
		"✓ checks_status=SUCCESS",
		"⋯ ac-met  pending@current:9256fbb  (acceptance criteria are satisfied)",
		"chains",
		"review: already-active (owner/repo-1+review-initial)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "instruction") {
		t.Errorf("output leaked an output not referenced by done_when:\n%s", out)
	}
	if strings.Contains(out, "Task ") {
		t.Errorf("output must not contain a lifecycle \"Task <name> <status>\" line:\n%s", out)
	}
}

// A chain that did not fire renders its blocked reason so an operator can see
// why nothing will act automatically.
func TestStatusChainDetailLine_NotFiredDetail(t *testing.T) {
	tests := []struct {
		name string
		c    service.StatusChain
		want string
	}{
		{"when unmet", service.StatusChain{BlockedReason: "when_unmet"}, "not-fired (when not satisfied)"},
		{"missing outputs", service.StatusChain{BlockedReason: "outputs_missing", MissingOutputs: []string{"pr_url"}}, "not-fired (missing outputs: pr_url)"},
		{"fired but unspawned", service.StatusChain{Fired: true, TargetSession: "s2"}, "eligible (s2)"},
		{"already active", service.StatusChain{AlreadyActive: true, TargetSession: "s2"}, "already-active (s2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusChainDetailLine(tt.c)
			if got != tt.want {
				t.Errorf("statusChainDetailLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --full is JSON-only; passing it without --json must error before touching
// config/state.
func TestStatusCmd_FullRequiresJSON(t *testing.T) {
	origJSON, origFull := statusJSON, statusFull
	statusJSON, statusFull = false, true
	defer func() { statusJSON, statusFull = origJSON, origFull }()

	err := statusCmd.RunE(statusCmd, []string{"whatever"})
	if err == nil || !strings.Contains(err.Error(), "--full") {
		t.Fatalf("err = %v, want an error naming --full", err)
	}
}

// A run=down session has nothing evaluated (the healthcheck cycle only probes
// run=up sessions), so Run shows the bare state and Health shows "-"
// rather than a stale or misleading value.
func TestFormatRunLine_AndFormatHealthLine_RunDown(t *testing.T) {
	rt := service.StatusRuntime{Run: domain.RunDown, Health: domain.HealthHealthy}
	if got := formatRunLine(rt); got != "down" {
		t.Errorf("formatRunLine() = %q, want %q", got, "down")
	}
	if got := formatHealthLine(rt); got != "-" {
		t.Errorf("formatHealthLine() = %q, want %q", got, "-")
	}
}

// A run=up session with a produced run-scoped task shows the task's status
// as a parenthetical hint, and its actual health value.
func TestFormatRunLine_AndFormatHealthLine_RunUpWithProducedTask(t *testing.T) {
	rt := service.StatusRuntime{
		Run:    domain.RunUp,
		Health: domain.HealthHealthy,
		Tasks:  []service.StatusRuntimeTask{{Instance: "claude", Status: "produced"}},
	}
	if got := formatRunLine(rt); got != "up (produced)" {
		t.Errorf("formatRunLine() = %q, want %q", got, "up (produced)")
	}
	if got := formatHealthLine(rt); got != "healthy" {
		t.Errorf("formatHealthLine() = %q, want %q", got, "healthy")
	}
}

// formatHealthLine renders every health value verbatim, including stalled,
// distinct from the agent's self-reported message.
func TestFormatHealthLine_Stalled(t *testing.T) {
	rt := service.StatusRuntime{Run: domain.RunUp, Health: domain.HealthStalled}
	if got := formatHealthLine(rt); got != "stalled" {
		t.Errorf("formatHealthLine() = %q, want %q", got, "stalled")
	}
}

// A run=up session with no produced task shows the bare run state — no
// parenthetical, since there is nothing to attribute it to.
func TestFormatRunLine_RunUpWithNoProducedTask(t *testing.T) {
	rt := service.StatusRuntime{Run: domain.RunUp, Tasks: []service.StatusRuntimeTask{{Instance: "claude", Status: "pending"}}}
	if got := formatRunLine(rt); got != "up" {
		t.Errorf("formatRunLine() = %q, want %q", got, "up")
	}
}

// renderStatus prints the identity, run/health, and message facts, and
// prints the destroyed-session view instead when the session is a
// tombstone.
func TestRenderStatus_LiveSession(t *testing.T) {
	var buf bytes.Buffer
	cmd := statusCmd
	cmd.SetOut(&buf)
	renderStatus(cmd, &service.StatusResult{
		Identity: service.StatusIdentity{SessionName: "owner/repo-1", ResourceID: "https://github.com/owner/repo/issues/1", Workflow: "claude"},
		Runtime:  service.StatusRuntime{Run: domain.RunUp, Health: domain.HealthHealthy},
	})
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	got := make(map[string]string, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		got[strings.TrimSuffix(fields[0], ":")] = strings.Join(fields[1:], " ")
	}
	want := map[string]string{
		"Session":  "owner/repo-1",
		"Resource": "https://github.com/owner/repo/issues/1",
		"Workflow": "claude",
		"Run":      "up",
		"Health":   "healthy",
		"Message":  "-",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q = %q, want %q; full output:\n%s", k, got[k], v, out)
		}
	}
}

func TestRenderStatus_DestroyedSession(t *testing.T) {
	var buf bytes.Buffer
	cmd := statusCmd
	cmd.SetOut(&buf)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	destroyed := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	renderStatus(cmd, &service.StatusResult{
		Identity:    service.StatusIdentity{SessionName: "owner/repo-1", ResourceID: "id-1", CreatedAt: created},
		Destroyed:   true,
		DestroyedAt: destroyed,
	})
	out := buf.String()
	for _, want := range []string{"Status:", "destroyed (tombstone)", "Created:", "2026-01-01 00:00:00", "Destroyed:", "2026-01-02 00:00:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Health:") {
		t.Errorf("destroyed view must not print live-session facts; got:\n%s", out)
	}
}

// Reporting renders the last observation, so it says when that was: the age
// of the facts shown is part of the answer, not something a reader has to
// infer from the absence of a refresh.
func TestRenderStatusWork_ObservationCarriesItsAge(t *testing.T) {
	var buf bytes.Buffer
	observed := time.Now().Add(-90 * time.Second)
	renderStatusWork(&buf, []service.StatusTask{{
		Instance: "review#1",
		Status:   "produced",
		Observed: &contract.ResourceObservation{
			At:    observed,
			State: map[string]any{"revision": "sha2", "resource_kind": "pull"},
		},
		State: map[string]any{"verdict_revision": "sha1"},
	}}, false)
	out := buf.String()
	if !strings.Contains(out, "observed") {
		t.Errorf("output does not say when the resource was observed:\n%s", out)
	}
	if !strings.Contains(out, "revision=sha2") {
		t.Errorf("output does not carry the observed state:\n%s", out)
	}
	if !strings.Contains(out, "verdict_revision=sha1") {
		t.Errorf("output does not carry what the instance holds about itself:\n%s", out)
	}
}

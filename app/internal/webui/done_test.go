package webui

import (
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// sampleTasks returns a done_when-bearing task with one satisfied check leaf and
// one pending judge leaf — overall pending — exercising both leaf renderers.
func sampleTasks() []service.TaskInstanceView {
	return []service.TaskInstanceView{{
		Instance: "engineer",
		Scope:    "run",
		Status:   "running",
		IsTask:   true,
		DoneWhen: &task.DoneWhenResult{
			Overall: task.DonePending,
			Leaves: []task.DoneLeafResult{
				{Kind: "check", Status: task.DoneSatisfied, Output: "workspace_dir_dirty", Value: "0", Observed: true},
				{
					Kind: "judge", Status: task.DonePending, ID: "review",
					Reason:          "awaiting reviewer",
					Revision:        "abc123",
					CurrentRevision: "def456",
					JudgeSession:    "owner/repo-9",
					PendingReason:   "no reviewer verdict yet",
				},
			},
		},
	}}
}

// gateSummary rolls a session's done_when tasks into one aggregate: unsatisfied
// dominates, else pending, else satisfied; nil when no task carries a done_when.
func TestGateSummary_Aggregates(t *testing.T) {
	none := gateSummary([]service.TaskInstanceView{{Instance: "tmux"}})
	if none != nil {
		t.Errorf("no done_when task should yield nil gate, got %+v", none)
	}

	mixed := gateSummary([]service.TaskInstanceView{
		{DoneWhen: &task.DoneWhenResult{Overall: task.DoneSatisfied}},
		{DoneWhen: &task.DoneWhenResult{Overall: task.DonePending}},
		{DoneWhen: &task.DoneWhenResult{Overall: task.DoneSatisfied}},
	})
	if mixed == nil || mixed.Status != task.DonePending || mixed.Satisfied != 2 || mixed.Total != 3 {
		t.Errorf("mixed gate = %+v, want pending 2/3", mixed)
	}

	blocked := gateSummary([]service.TaskInstanceView{
		{DoneWhen: &task.DoneWhenResult{Overall: task.DoneSatisfied}},
		{DoneWhen: &task.DoneWhenResult{Overall: task.DoneUnsatisfied}},
	})
	if blocked.Status != task.DoneUnsatisfied {
		t.Errorf("any unsatisfied should dominate, got %s", blocked.Status)
	}
}

func TestLeafStats_CountsSatisfied(t *testing.T) {
	s := leafStats(&task.DoneWhenResult{
		Overall: task.DonePending,
		Leaves: []task.DoneLeafResult{
			{Status: task.DoneSatisfied}, {Status: task.DonePending},
		},
	})
	if s.Satisfied != 1 || s.Total != 2 || s.Status != task.DonePending {
		t.Errorf("leafStats = %+v, want pending 1/2", s)
	}
}

func TestTaskName_NamedVsNumbered(t *testing.T) {
	if got := taskName("", "engineer-1"); got != "engineer-1" {
		t.Errorf("unnamed = %q, want engineer-1", got)
	}
	if got := taskName("reviewer", "engineer-1"); got != "reviewer (engineer-1)" {
		t.Errorf("named = %q, want reviewer (engineer-1)", got)
	}
}

// done-badge: compact overall state with the satisfied/total leaf count, colored
// by status, reusing the CLI's ✓/✗/⋯ glyphs.
func TestDoneBadge_OverallAndCount(t *testing.T) {
	out := exec(t, "done-badge", sampleTasks()[0].DoneWhen)
	for _, want := range []string{"pending", "(1/2)", "⋯", doneStatusClass(task.DonePending)} {
		if !strings.Contains(out, want) {
			t.Errorf("done-badge missing %q: %s", want, out)
		}
	}
}

// done-leaf (check): renders output=value with the read value; an unobserved
// value reads "?" (pending), not a failure.
func TestDoneLeaf_Check(t *testing.T) {
	observed := exec(t, "done-leaf", task.DoneLeafResult{
		Kind: "check", Status: task.DoneSatisfied, Output: "workspace_dir_dirty", Value: "0", Observed: true,
	})
	if !strings.Contains(observed, "workspace_dir_dirty") || !strings.Contains(observed, "0") {
		t.Errorf("check leaf missing output/value: %s", observed)
	}

	unobserved := exec(t, "done-leaf", task.DoneLeafResult{
		Kind: "check", Status: task.DonePending, Output: "ci_status", Observed: false,
	})
	if !strings.Contains(unobserved, "?") {
		t.Errorf("unobserved check should read '?': %s", unobserved)
	}
}

// done-leaf (judge): surfaces the gate's blocker — action/reason, a stale-revision
// note, the judge session, and the pending reason.
func TestDoneLeaf_Judge(t *testing.T) {
	out := exec(t, "done-leaf", sampleTasks()[0].DoneWhen.Leaves[1])
	for _, want := range []string{
		"review", "awaiting reviewer", "abc123", "def456", "stale",
		"owner/repo-9", "no reviewer verdict yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("judge leaf missing %q: %s", want, out)
		}
	}
}

// sampleWork mirrors sampleTasks' fixture as the "work" layer's StatusTask
// shape, for detail-page tests.
func sampleWork() []service.StatusTask {
	t := sampleTasks()[0]
	return []service.StatusTask{{
		Instance: t.Instance,
		Scope:    t.Scope,
		Status:   t.Status,
		IsTask:   t.IsTask,
		DoneWhen: t.DoneWhen,
	}}
}

// Detail page renders the Work section with the instance, its overall gate, and
// the per-leaf detail — so done_when state is visible without the CLI.
func TestDetail_ShowsTasks(t *testing.T) {
	status := sampleShow()
	status.Work = sampleWork()
	body := get(t, &fakeService{status: status}, "/sessions/owner/repo-7").Body.String()
	for _, want := range []string{
		"Work", "engineer", "workspace_dir_dirty", "review", "no reviewer verdict yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail missing task content %q", want)
		}
	}
}

// A session with no done_when-bearing task shows no gate badge on its card; one
// with a done_when task shows the aggregate.
func TestCard_GateBadge(t *testing.T) {
	with := exec(t, "card", cardView{ListEntry: service.ListEntry{
		SessionName: "owner/repo-1", Tasks: sampleTasks(),
	}})
	if !strings.Contains(with, "done 0/1") {
		t.Errorf("card should show aggregate gate badge (0 of 1 task satisfied): %s", with)
	}

	without := exec(t, "card", cardView{ListEntry: service.ListEntry{SessionName: "owner/repo-2"}})
	if strings.Contains(without, "done ") {
		t.Errorf("card without done_when tasks should show no gate badge: %s", without)
	}
}

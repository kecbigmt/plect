package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// pursueDocument is a task document whose chain spawns the reviewer that will
// supply its one pending judge, wiring the work facts and both live roots.
const pursueDocument = `[pursue]
kind              = "task"
description       = "Pursue one goal until an independent reviewer confirms it"
resource_observer = "issue_pr"
instructions      = [{ text = "Pursue the work at {{ resource.id }}." }]

[pursue.state_schema]
type = "object"

[pursue.state_schema.properties]
verdict_revision = { type = "string" }

[pursue.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull"] },
  { judge = "the work is done", id = "goal-met", relation = ["sibling"] },
]

[[pursue.chains]]
id        = "review"
workflow  = "wf"
placement = "sibling"

[pursue.chains.when]
all = [
  { check = "resource.state.resource_kind", in = ["pull"] },
  { judge_pending = "goal-met" },
]

[pursue.chains.inputs]
task         = "review"
work_session = { from = "task.session" }
instance     = { from = "task.instance" }
judge_ids    = { from = "task.done_when.pending_judge_ids" }
revision     = { from = "resource.state.revision" }
recorded     = { from = "self.state.verdict_revision", default = "" }
`

func setUpDocumentInstance(t *testing.T, cfg *config.Config, store *state.Store, taskID string) string {
	t.Helper()
	setup, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: taskID, SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	return setup.Instance
}

func chainSpawnFor(t *testing.T, cfg *config.Config, store *state.Store, chainID string) ChainSpawn {
	t.Helper()
	result, err := CheckSession(cfg, store, CheckParams{SessionName: "org/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	for _, sp := range result.Chains {
		if sp.ChainID == chainID {
			return sp
		}
	}
	t.Fatalf("no chain plan for %q: %+v", chainID, result.Chains)
	return ChainSpawn{}
}

func TestCheckSession_TaskDocumentChainFiresAndProjectsWorkFacts(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, pursueDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	instance := setUpDocumentInstance(t, cfg, store, "pursue")

	sp := chainSpawnFor(t, cfg, store, "review")
	if !sp.Fired {
		t.Fatalf("a chain whose when holds fires: %+v", sp)
	}
	if sp.Task != "pursue" {
		t.Errorf("chain names its declaring document: got %q", sp.Task)
	}
	if sp.Workflow != "wf" {
		t.Errorf("workflow: got %q, want %q", sp.Workflow, "wf")
	}
	if sp.Placement != "sibling" {
		t.Errorf("placement: got %q", sp.Placement)
	}
	want := map[string]any{
		"task":         "review",
		"work_session": "org/repo-1",
		"instance":     instance,
		"judge_ids":    "goal-met",
		"revision":     "sha2",
		"recorded":     "",
	}
	for key, value := range want {
		if got := sp.Inputs[key]; got != value {
			t.Errorf("input %q: got %#v, want %#v", key, got, value)
		}
	}
}

func TestCheckSession_TaskDocumentChainBlockedWhileItsTriggerIsUnmet(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, pursueDocument)
	seedSession(t, store, "org/repo-0", "org/repo", 0, "wf", nil)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	seedSession(t, store, "org/repo-2", "org/repo", 2, "wf", nil)
	setParent(t, store, "org/repo-1", "org/repo-0")
	setParent(t, store, "org/repo-2", "org/repo-0")
	instance := setUpDocumentInstance(t, cfg, store, "pursue")

	if _, err := RecordJudge(cfg, store, JudgeParams{
		SessionName: "org/repo-1", Instance: instance, LeafID: "goal-met",
		Action: task.JudgeActionApprove, Reason: "done", JudgeSession: "org/repo-2",
	}); err != nil {
		t.Fatalf("RecordJudge: %v", err)
	}

	sp := chainSpawnFor(t, cfg, store, "review")
	if sp.Fired {
		t.Fatalf("a judge_pending trigger does not hold once a verdict is recorded: %+v", sp)
	}
	if sp.BlockedReason != chainBlockedWhenUnmet {
		t.Errorf("blocked reason: got %q, want %q", sp.BlockedReason, chainBlockedWhenUnmet)
	}
}

// A `from` reaching a declared key the observer has not reported yet is the
// "not wired yet" case the firing gate exists for, not a broken binding.
func TestCheckSession_TaskDocumentChainBlockedOnAnUnreportedInput(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := `[pursue]
kind              = "task"
description       = "A document whose chain waits on a key nothing reports yet"
resource_observer = "issue_pr"
instructions      = [{ text = "Pursue the work at {{ resource.id }}." }]

[pursue.done_when]
all = [{ judge = "the work is done", id = "goal-met" }]

[[pursue.chains]]
id       = "review"
workflow = "wf"

[pursue.chains.when]
all = [{ judge_pending = "goal-met" }]

[pursue.chains.inputs]
pull = { from = "resource.state.pr_url" }
`
	cfg := writeObservedRevisionFixture(t, revision, document)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	setUpDocumentInstance(t, cfg, store, "pursue")

	sp := chainSpawnFor(t, cfg, store, "review")
	if sp.Fired {
		t.Fatalf("a chain whose input has nothing to read does not fire: %+v", sp)
	}
	if sp.BlockedReason != chainBlockedOutputsMissing {
		t.Errorf("blocked reason: got %q, want %q", sp.BlockedReason, chainBlockedOutputsMissing)
	}
	if len(sp.MissingOutputs) != 1 || sp.MissingOutputs[0] != "resource.state.pr_url" {
		t.Errorf("the unresolved path is named: got %v", sp.MissingOutputs)
	}
}

// The warning 7a emitted in place of firing is gone once the chains fire.
func TestCheckSession_TaskDocumentChainsAreNoLongerAnnouncedAsUnevaluated(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, pursueDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	setUpDocumentInstance(t, cfg, store, "pursue")

	if _, err := CheckSession(cfg, store, CheckParams{SessionName: "org/repo-1"}); err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
}

// resourceChainDocument names the resource its spawned session binds to,
// rather than letting it inherit the declaring session's.
const resourceChainDocument = `[pursue]
kind              = "task"
description       = "Hand a pull request to a reviewer bound to it"
resource_observer = "issue_pr"
instructions      = [{ text = "Pursue the work at {{ resource.id }}." }]

[pursue.done_when]
all = [{ judge = "the work is done", id = "goal-met" }]

[[pursue.chains]]
id       = "review"
workflow = "wf"
resource = { from = "resource.state.pr_url" }

[pursue.chains.when]
all = [{ judge_pending = "goal-met" }]
`

func TestCheckSession_ChainBindsItsSpawnToTheResourceItNames(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	const pr = "https://example.test/pull/9"
	cfg := writeObservedRevisionFixture(t, revision, resourceChainDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	instance := setUpDocumentInstance(t, cfg, store, "pursue")
	// The pull request is a fact the instance holds, not the resource it was
	// instantiated against.
	if err := store.Update("org/repo-1", func(s *domain.Session) error {
		s.Tasks[instance].Observed.State["pr_url"] = pr
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sp := chainSpawnFor(t, cfg, store, "review")
	if !sp.Fired {
		t.Fatalf("a chain whose resource resolves fires: %+v", sp)
	}
	if sp.Resource != pr {
		t.Errorf("spawn resource = %q, want the pull request the chain named", sp.Resource)
	}
}

// Absent, the spawned session inherits the declaring session's resource —
// which is what every chain did before one could name it.
func TestCheckSession_ChainWithoutAResourceInheritsTheDeclaringOne(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, pursueDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	setUpDocumentInstance(t, cfg, store, "pursue")

	sp := chainSpawnFor(t, cfg, store, "review")
	if !sp.Fired || sp.Resource != "https://example.test/pull/1" {
		t.Fatalf("spawn resource = %q, want the declaring instance's own: %+v", sp.Resource, sp)
	}
}

// A named resource that has nothing to read yet fails the fire closed: a
// session bound to nothing is worse than one not yet spawned.
func TestCheckSession_ChainBlockedWhileItsResourceIsUnresolved(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, resourceChainDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	setUpDocumentInstance(t, cfg, store, "pursue")

	sp := chainSpawnFor(t, cfg, store, "review")
	if sp.Fired {
		t.Fatalf("a chain whose resource has nothing to read does not fire: %+v", sp)
	}
	if sp.BlockedReason != chainBlockedResourceUnresolved {
		t.Errorf("blocked reason: got %q, want %q", sp.BlockedReason, chainBlockedResourceUnresolved)
	}
}

// A resource that resolves to an empty string is the same failure as one that
// resolves to nothing: a provider has no resource to match.
func TestCheckSession_ChainBlockedWhenItsResourceIsEmpty(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, resourceChainDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	instance := setUpDocumentInstance(t, cfg, store, "pursue")
	if err := store.Update("org/repo-1", func(s *domain.Session) error {
		s.Tasks[instance].Observed.State["pr_url"] = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sp := chainSpawnFor(t, cfg, store, "review")
	if sp.Fired || sp.BlockedReason != chainBlockedResourceUnresolved {
		t.Fatalf("an empty resource blocks the fire: %+v", sp)
	}
}

// A resource projected from a value that exists but cannot be carried as an
// identifier is a fault, not a wait: the fact has already arrived, so waiting
// for it is waiting forever. The fire stays blocked — fail-closed — but says
// why.
func TestCheckSession_ChainResourceThatCannotBeCarriedIsAFault(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, resourceChainDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	instance := setUpDocumentInstance(t, cfg, store, "pursue")
	if err := store.Update("org/repo-1", func(s *domain.Session) error {
		s.Tasks[instance].Observed.State["pr_url"] = []any{
			"https://example.test/pull/9", "https://example.test/pull/10",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	sp := chainSpawnFor(t, cfg, store, "review")
	if sp.Fired {
		t.Fatalf("a resource that is not one identifier does not fire: %+v", sp)
	}
	if sp.BlockedReason != chainBlockedResourceUnresolved {
		t.Errorf("blocked reason: got %q, want %q", sp.BlockedReason, chainBlockedResourceUnresolved)
	}
	if len(sp.Warnings) == 0 {
		t.Fatalf("a malformed resource is reported, not waited on in silence: %+v", sp)
	}
	if !strings.Contains(strings.Join(sp.Warnings, " "), "one identifier") {
		t.Errorf("warning = %v, want it to name what a resource has to be", sp.Warnings)
	}
}

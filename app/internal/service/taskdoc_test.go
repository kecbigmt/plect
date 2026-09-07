package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// reviewDocument is the acceptance case the completion surface exists for: a
// verdict recorded into the instance, compared against the resource's live
// revision, with no key of its own to hang on.
const reviewDocument = `[review]
kind              = "task"
description       = "Review a resource and record a verdict"
resource_observer = "issue_pr"
instructions      = [{ text = "Review {{ resource.id }} and record a verdict against its current revision." }]

[review.inputs_schema]
type = "object"

[review.inputs_schema.properties]
instruction = { type = "string" }

[review.state_schema]
type = "object"

[review.state_schema.properties]
verdict_revision = { type = "string" }

[review.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull", "issue"] },
  { expr = "self.state.verdict_revision == resource.state.revision" },
]
`

// observeScript is the observer's `observe` action: it reports a fixed
// observation so a test can assert what the completion surface did with it.
func observerDocument(state map[string]string) string {
	pairs := make([]string, 0, len(state))
	for k, v := range state {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	var props strings.Builder
	for k := range state {
		fmt.Fprintf(&props, "%s = { type = \"string\" }\n", k)
	}
	return fmt.Sprintf(`
[issue_pr]
kind  = "resource_observer"
match = '^https://'

[issue_pr.observe]
type   = "shell"
script = '''
jq -nc --args '$ARGS.positional | map(split("=")) | map({(.[0]): .[1]}) | add' %s
'''

[issue_pr.state_schema]
type = "object"

[issue_pr.state_schema.properties]
%s
`, strings.Join(pairs, " "), props.String())
}

// writeTaskDocumentFixture writes one observer, one empty workflow, and the
// given task documents into a config layer.
func writeTaskDocumentFixture(t *testing.T, workdirsRoot, wfID string, observerState map[string]string, documents ...string) *config.Config {
	t.Helper()
	baseDir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(baseDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("resources", "issue_pr.toml"), observerDocument(observerState))
	write(filepath.Join("workflows", wfID+".toml"), "["+wfID+"]\nkind = \"workflow\"\n\n[["+wfID+".nodes]]\nuses = \"noop\"\n")
	for i, doc := range documents {
		write(filepath.Join("tasks", fmt.Sprintf("doc%d.toml", i)), doc)
	}
	return &config.Config{WorkspaceDirsRoot: workdirsRoot, BaseDir: baseDir}
}

func TestSetTaskState_RecordsDeclaredKey(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "review",
			Status:   contract.TaskStatusProduced,
			Resource: "https://example.test/pull/1",
			SetupAt:  time.Now(),
		},
	})

	result, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1",
		Instance:   "review#1",
		State:      map[string]any{"verdict_revision": "sha2"},
	})
	if err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}
	if result.Instance != "review#1" || len(result.Keys) != 1 || result.Keys[0] != "verdict_revision" {
		t.Errorf("result = %+v", result)
	}
	got := store.Get("org/repo-1").Tasks["review#1"].State
	if got["verdict_revision"] != "sha2" {
		t.Errorf("state = %v, want verdict_revision=sha2", got)
	}
}

func TestSetTaskState_UndeclaredKeyRejected(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusProduced, SetupAt: time.Now()},
	})

	_, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1",
		Instance:   "review#1",
		State:      map[string]any{"nope": "x"},
	})
	if err == nil {
		t.Fatal("expected a rejection for a key the document's state_schema does not declare")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v, want it to name the key", err)
	}
}

// An instance backed by an effect declaration holds no state of its own:
// there is no state_schema to validate a write against, so the write is
// refused rather than landing somewhere nothing reads.
func TestSetTaskState_NonDocumentInstanceRejected(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "watch"}})
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, SetupAt: time.Now()},
	})

	_, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1",
		Instance:   "watch",
		State:      map[string]any{"verdict_revision": "sha2"},
	})
	if err == nil {
		t.Fatal("expected a rejection for an instance no task document declares")
	}
}

func TestSetTaskState_CleanedInstanceRejected(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusCleaned, SetupAt: time.Now()},
	})

	_, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1",
		Instance:   "review#1",
		State:      map[string]any{"verdict_revision": "sha2"},
	})
	if err == nil {
		t.Fatal("expected a rejection for an instance that is no longer live")
	}
}

// Instantiation binds the document to a resource, observes it once, and
// records the rendered instruction — the document owns no lifecycle, so
// nothing else runs.
func TestTaskSetup_TaskDocumentInstance(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	result, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID:      "review",
		SessionName: "org/repo-1",
		Resource:    "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if result.Instance != "review#1" {
		t.Fatalf("Instance = %q, want review#1", result.Instance)
	}
	st := store.Get("org/repo-1").Tasks["review#1"]
	if st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("instance state = %+v", st)
	}
	if st.Observed == nil || st.Observed.State["revision"] != "sha2" {
		t.Errorf("observed = %+v, want the first observation of the resource", st.Observed)
	}
	if st.Observed != nil && st.Observed.At.IsZero() {
		t.Error("observation carries no timestamp, so a display cannot say how old it is")
	}
	if len(st.Outputs) != 0 {
		t.Errorf("outputs = %+v, want none: a task document produces nothing", st.Outputs)
	}
	if !strings.Contains(result.Instruction, "https://example.test/pull/1") {
		t.Errorf("instruction = %q, want the rendered body", result.Instruction)
	}
}

// A first observation that fails rejects instantiation: an instance that can
// never satisfy is worse than no instance.
func TestTaskSetup_TaskDocumentFirstObservationFails(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", nil, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	_, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID:      "review",
		SessionName: "org/repo-1",
		Resource:    "https://example.test/pull/1",
	})
	if err == nil {
		t.Fatal("expected instantiation to be rejected when the first observation fails")
	}
	if got := store.Get("org/repo-1").Tasks["review#1"]; got != nil {
		t.Errorf("instance was created anyway: %+v", got)
	}
}

// The document states what kind of resource it is written for, so a resource
// no declared observer claims is refused at instantiation rather than
// producing an instance whose predicate can never read anything.
func TestTaskSetup_TaskDocumentResourceMustResolveToDeclaredObserver(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	_, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID:      "review",
		SessionName: "org/repo-1",
		Resource:    "does-not-match://x",
	})
	if err == nil {
		t.Fatal("expected instantiation to be rejected for a resource the declared observer does not claim")
	}
	if !strings.Contains(err.Error(), "issue_pr") {
		t.Errorf("error = %v, want it to name the declared observer", err)
	}
}

// The verdict flow, end to end: a review instance is pending until the
// reviewer records a verdict, satisfied once the recorded revision is the one
// the resource currently reports, and pending again after a new revision
// lands — with no derived output anywhere in it.
func TestTickSession_VerdictFlowThroughSelfState(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	setup, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "review", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}

	action := tickActionFor(t, cfg, store, setup.Instance)
	if action.Action == "satisfied" {
		t.Fatalf("a review with no recorded verdict is not satisfied: %+v", action)
	}

	if _, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1", Instance: setup.Instance, State: map[string]any{"verdict_revision": "sha2"},
	}); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}
	if action := tickActionFor(t, cfg, store, setup.Instance); action.Action != "satisfied" {
		t.Fatalf("a verdict recorded against the current revision satisfies: %+v", action)
	}

	// A new revision reopens the review on its own: the predicate compares
	// what was recorded with what the resource now reports.
	if err := os.WriteFile(revision, []byte("sha3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if action := tickActionFor(t, cfg, store, setup.Instance); action.Action == "satisfied" {
		t.Fatalf("a verdict recorded against an older revision does not satisfy: %+v", action)
	}
}

// tickActionFor ticks the session and returns the action computed for one
// instance. A tick observes before it decides, so the assertion is about what
// the resource says now.
func tickActionFor(t *testing.T, cfg *config.Config, store *state.Store, instance string) CheckAction {
	t.Helper()
	result, err := TickSession(cfg, store, TickParams{SessionName: "org/repo-1", Trigger: TickTriggerManual})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	for _, a := range result.Actions {
		if a.Instance == instance {
			return a
		}
	}
	t.Fatalf("no action for %s: %+v", instance, result.Actions)
	return CheckAction{}
}

// writeObservedRevisionFixture writes an observer that reports the revision a
// file currently holds, so a test can move the resource forward.
func writeObservedRevisionFixture(t *testing.T, revisionFile string, documents ...string) *config.Config {
	t.Helper()
	baseDir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(baseDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("resources", "issue_pr.toml"), fmt.Sprintf(`
[issue_pr]
kind  = "resource_observer"
match = '^https://'

[issue_pr.observe]
type   = "shell"
script = '''
jq -nc --arg revision "$(cat %s)" '{resource_kind:"pull", revision:$revision}'
'''

[issue_pr.state_schema]
type = "object"

[issue_pr.state_schema.properties]
resource_kind = { type = "string" }
revision      = { type = "string" }
# Declared but never reported, so a value reading it exercises the absent case.
pr_url        = { type = "string" }
`, revisionFile))
	write(filepath.Join("workflows", "wf.toml"), "[wf]\nkind = \"workflow\"\n\n[[wf.nodes]]\nuses = \"noop\"\n")
	for i, doc := range documents {
		write(filepath.Join("tasks", fmt.Sprintf("doc%d.toml", i)), doc)
	}
	return &config.Config{WorkspaceDirsRoot: t.TempDir(), BaseDir: baseDir}
}

func TestTaskShow_TaskDocument(t *testing.T) {
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	detail, err := TaskShow(cfg, "", "review")
	if err != nil {
		t.Fatalf("TaskShow: %v", err)
	}
	if detail.Kind != "task" {
		t.Errorf("Kind = %q, want task", detail.Kind)
	}
	if detail.ResourceObserver != "issue_pr" {
		t.Errorf("ResourceObserver = %q, want the observer the document is written for", detail.ResourceObserver)
	}
	if detail.Scope != "" {
		t.Errorf("Scope = %q, want none: a task document owns no lifecycle", detail.Scope)
	}
}

// A document instance has nothing to tear down, so cleanup reclaims it
// rather than failing for want of a cleanup action.
func TestTaskCleanup_TaskDocumentInstance(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusProduced, SetupAt: time.Now()},
	})
	result, err := TaskCleanup(cfg, store, TaskCleanupParams{SessionName: "org/repo-1", Instance: "review#1"})
	if err != nil {
		t.Fatalf("TaskCleanup: %v", err)
	}
	if !result.Found {
		t.Fatal("instance not found")
	}
	if got, exists := store.Get("org/repo-1").Tasks["review#1"]; exists {
		t.Errorf("instance still recorded after cleanup: %+v", got)
	}
}

// Inspecting a declaration is when a reader wants to know whether it holds
// together, so `plect task show` reports a reference that will not resolve
// rather than deferring it to an instantiation.
func TestTaskShow_TaskDocumentReportsABrokenContract(t *testing.T) {
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"revision": "sha2"}, reviewDocument)
	_, err := TaskShow(cfg, "", "review")
	if err == nil {
		t.Fatal("expected the completion key its observer does not publish to be reported")
	}
	if !strings.Contains(err.Error(), "resource_kind") {
		t.Errorf("error = %v, want it to name the key", err)
	}
}

// A plugin-shipped document is instantiated through the same path a
// user-authored one is, and its relative observer reference resolves in its
// own plugin's namespace.
func TestTaskSetup_PluginTaskDocumentResolvesItsOwnObserver(t *testing.T) {
	store := testStore(t)
	pluginDir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(pluginDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plugin.toml", "name = \"acme\"\nversion = \"0.1.0\"\ndescription = \"test plugin\"\n")
	write(filepath.Join("config", "resources", "issue_pr.toml"), observerDocument(map[string]string{"resource_kind": "pull", "revision": "sha2"}))
	write(filepath.Join("config", "tasks", "review.toml"), reviewDocument)
	cfg := &config.Config{
		WorkspaceDirsRoot: t.TempDir(),
		PluginDirs:        []string{pluginDir},
		Plugins:           []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	result, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "official.acme.review", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	st := store.Get("org/repo-1").Tasks[result.Instance]
	if st == nil || st.Observed == nil || st.Observed.State["revision"] != "sha2" {
		t.Errorf("instance did not observe through its own plugin's observer: %+v", st)
	}
	// The result is what a caller echoes back at the next command, so it names
	// the address rather than an id that would resolve to nothing.
	if result.TaskID != "official.acme.review" {
		t.Errorf("result.TaskID = %q, want the address the reference selected", result.TaskID)
	}
	if st.TaskID != "official.acme.review" {
		t.Errorf("stored task id = %q, want the address the reference selected", st.TaskID)
	}
}

// An id names one declaration. A document and an effect claiming the same id
// fails at load, rather than instantiation silently picking one of them.
func TestTaskSetup_TaskDocumentCollidingWithAnEffectFails(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "tasks", "review.toml"), []byte(`
[review]
kind  = "effect"
scope = "session"

[review.setup]
type   = "shell"
script = "true"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	_, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "review", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err == nil {
		t.Fatal("expected instantiation to fail while one id names two declarations")
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("error = %v, want it to name the id", err)
	}
	if got := store.Get("org/repo-1").Tasks["review#1"]; got != nil {
		t.Errorf("an instance was created anyway: %+v", got)
	}
}

// A judge leaf on a document is recorded against the revision the observer
// reported: a document has no `revision` output for one to be read from, and
// the observation is the only thing that knows it.
func TestRecordJudge_TaskDocumentInstance(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, judgedDocument)
	seedSession(t, store, "org/repo-0", "org/repo", 0, "wf", nil)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	seedSession(t, store, "org/repo-2", "org/repo", 2, "wf", nil)
	setParent(t, store, "org/repo-1", "org/repo-0")
	setParent(t, store, "org/repo-2", "org/repo-0")

	setup, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "work", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	result, err := RecordJudge(cfg, store, JudgeParams{
		SessionName:  "org/repo-1",
		Instance:     setup.Instance,
		LeafID:       "ac-met",
		Action:       task.JudgeActionApprove,
		Reason:       "looks right",
		JudgeSession: "org/repo-2",
	})
	if err != nil {
		t.Fatalf("RecordJudge: %v", err)
	}
	if result.Revision != "sha2" {
		t.Errorf("recorded revision = %q, want the revision the observer reported", result.Revision)
	}
	// The gate is the document's, so an approved judge plus the observed
	// check satisfies it.
	if action := tickActionFor(t, cfg, store, setup.Instance); action.Action != "satisfied" {
		t.Errorf("action = %+v, want satisfied once the document's judge is approved", action)
	}
}

// A judge id the document does not declare is rejected: the gate consulted is
// the document's own.
func TestRecordJudge_TaskDocumentRejectsAnUndeclaredLeaf(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, judgedDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	seedSession(t, store, "org/repo-2", "org/repo", 2, "wf", nil)
	setup, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "work", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	_, err = RecordJudge(cfg, store, JudgeParams{
		SessionName: "org/repo-1", Instance: setup.Instance, LeafID: "no-such-leaf",
		Action: task.JudgeActionApprove, Reason: "r", JudgeSession: "org/repo-2",
	})
	if err == nil {
		t.Fatal("expected a judge id the document does not declare to be rejected")
	}
	if !strings.Contains(err.Error(), "no-such-leaf") {
		t.Errorf("error = %v, want it to name the leaf", err)
	}
}

// Finalize reconfirms against the document's gate and cites the observed
// revision in the completion record.
func TestFinalizeTask_TaskDocumentInstance(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	setup, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "review", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}

	if _, err := FinalizeTask(cfg, store, FinalizeTaskParams{SessionName: "org/repo-1", Instance: setup.Instance}); err == nil {
		t.Fatal("expected finalize to refuse an unsatisfied document gate")
	}

	if _, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1", Instance: setup.Instance, State: map[string]any{"verdict_revision": "sha2"},
	}); err != nil {
		t.Fatalf("SetTaskState: %v", err)
	}
	result, err := FinalizeTask(cfg, store, FinalizeTaskParams{SessionName: "org/repo-1", Instance: setup.Instance})
	if err != nil {
		t.Fatalf("FinalizeTask: %v", err)
	}
	if result.Instance != setup.Instance {
		t.Errorf("result = %+v", result)
	}
	if st := store.Get("org/repo-1").Tasks[setup.Instance]; st == nil || st.FinalizedAt.IsZero() {
		t.Errorf("instance not recorded as finalized: %+v", st)
	}
}

// A payload whose value does not match the declared type is rejected, and
// nothing is written: the schema is the contract, not a hint.
func TestSetTaskState_PayloadViolatingTheSchemaRejected(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {
			Scope:   contract.TaskScopeSession,
			TaskID:  "review",
			Status:  contract.TaskStatusProduced,
			State:   map[string]any{"verdict_revision": "sha1"},
			SetupAt: time.Now(),
		},
	})
	_, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1", Instance: "review#1", State: map[string]any{"verdict_revision": 42},
	})
	if err == nil {
		t.Fatal("expected a payload violating the declared type to be rejected")
	}
	if !strings.Contains(err.Error(), "verdict_revision") {
		t.Errorf("error = %v, want it to name the key", err)
	}
	if got := store.Get("org/repo-1").Tasks["review#1"].State["verdict_revision"]; got != "sha1" {
		t.Errorf("state = %v, want the prior value untouched", got)
	}
}

// judgedDocument is a document whose gate waits on an independent verdict,
// which is the shape every converted work task has.
const judgedDocument = `[work]
kind              = "task"
description       = "Do the work and wait for an independent verdict"
resource_observer = "issue_pr"
instructions      = [{ text = "Do the work at {{ resource.id }}." }]

[work.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull", "issue"] },
  { judge = "acceptance criteria are satisfied", id = "ac-met" },
]
`

// A document produces no outputs, so the outputs write path names the state
// write path rather than reporting the declaration as missing.
func TestSetOutput_TaskDocumentInstanceNamesTheStateWritePath(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusProduced, SetupAt: time.Now()},
	})
	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1", Task: "review#1", Outputs: map[string]any{"verdict_revision": "sha2"},
	})
	if err == nil {
		t.Fatal("expected a write to a document instance's outputs to be refused")
	}
	if !strings.Contains(err.Error(), "plect state set") {
		t.Errorf("error = %v, want it to name the state write path", err)
	}
}

func TestSetTaskState_EmptyPayloadRejected(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusProduced, SetupAt: time.Now()},
	})
	for _, payload := range []map[string]any{nil, {}} {
		if _, err := SetTaskState(cfg, store, SetTaskStateParams{
			Identifier: "org/repo-1", Instance: "review#1", State: payload,
		}); err == nil {
			t.Errorf("payload %v: expected a rejection; a write that records nothing is a caller mistake", payload)
		}
	}
}

func TestSetTaskState_InstanceRequired(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"revision": "sha2"}, reviewDocument)
	if _, err := SetTaskState(cfg, store, SetTaskStateParams{
		Identifier: "org/repo-1", State: map[string]any{"verdict_revision": "sha2"},
	}); err == nil {
		t.Fatal("expected a rejection when no instance is named")
	}
}

// An instance whose declaration has no completion predicate has no judge
// leaves either, so a verdict recorded against one names a leaf that cannot
// exist. Accepting it persisted a record nothing could ever consume.
func TestRecordJudge_InstanceWithNoGateRejectsEveryLeaf(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, gatelessDocument)
	seedSession(t, store, "org/repo-0", "org/repo", 0, "wf", nil)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)
	seedSession(t, store, "org/repo-2", "org/repo", 2, "wf", nil)
	setParent(t, store, "org/repo-1", "org/repo-0")
	setParent(t, store, "org/repo-2", "org/repo-0")

	setup, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "note", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	_, err = RecordJudge(cfg, store, JudgeParams{
		SessionName: "org/repo-1", Instance: setup.Instance, LeafID: "ac-met",
		Action: task.JudgeActionApprove, Reason: "r", JudgeSession: "org/repo-2",
	})
	if err == nil {
		t.Fatal("expected a verdict against an instance with no completion predicate to be rejected")
	}
	if st := store.Get("org/repo-1").Tasks[setup.Instance]; st != nil && st.DoneWhen != nil && len(st.DoneWhen.Judges) > 0 {
		t.Errorf("a judge record was persisted anyway: %+v", st.DoneWhen.Judges)
	}
}

// gatelessDocument declares no completion predicate: it is work with nothing
// to reconfirm, which the language allows.
const gatelessDocument = `[note]
kind              = "task"
description       = "Work with nothing to reconfirm"
resource_observer = "issue_pr"
instructions      = [{ text = "Note something about {{ resource.id }}." }]
`

// The outputs write path reads both kinds through the shared loader, so a
// document that will not load is reported as that rather than as a
// declaration nobody wrote.
func TestSetOutput_ReportsAnUnloadableDocument(t *testing.T) {
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument)
	// A declaration that parses but carries a field outside the task surface:
	// a `.md` that no instructions element's file names is a template asset
	// rather than a broken document, so it would not fail the load.
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "tasks", "broken.toml"), []byte("[broken]\nkind = \"task\"\nbogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch#1": {Scope: contract.TaskScopeSession, TaskID: "watch", Status: contract.TaskStatusProduced, SetupAt: time.Now()},
	})
	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1", Task: "watch#1", Outputs: map[string]any{"pr_state": "merged"},
	})
	if err == nil {
		t.Fatal("expected the unloadable document to be reported")
	}
	if strings.Contains(err.Error(), "not found in any config layer") {
		t.Errorf("error = %v, want the load failure rather than a missing-declaration report", err)
	}
}

// The instruction assets' conditional and defaulting forms read the
// document's own inputs, which is where the extra instruction a caller adds
// arrives — `plect task setup <id> --input instruction=...`.
func TestTaskSetup_TaskDocumentBodyDefaultsAnAbsentOptionalInput(t *testing.T) {
	store := testStore(t)
	document := `[review]
kind              = "task"
description       = "Review a resource"
resource_observer = "issue_pr"
instructions      = [{ text = """
Review {{ resource.id }}.

Additional instructions from the dispatcher (may be empty): {{ inputs.instruction }}
""" }]

[review.inputs_schema]
type = "object"

[review.inputs_schema.properties]
instruction = { type = "string", default = "" }
`
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf", map[string]string{"resource_kind": "pull", "revision": "sha2"}, document)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	bare, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "review", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if !strings.HasSuffix(bare.Instruction, "(may be empty):") {
		t.Errorf("an absent input's schema default renders empty: %q", bare.Instruction)
	}

	with, err := TaskSetup(cfg, store, TaskSetupParams{
		TaskID: "review", SessionName: "org/repo-1", Resource: "https://example.test/pull/1",
		Inputs: map[string]string{"instruction": "focus on the migration"},
	})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if !strings.Contains(with.Instruction, "Additional instructions from the dispatcher (may be empty): focus on the migration") {
		t.Errorf("instruction = %q, want the caller's extra instruction", with.Instruction)
	}
}

// A document declares no outputs, so its `self.state.*` is what was recorded
// into the instance and nothing else — an output a predecessor effect
// instance left behind is not silently read as recorded state.
func TestTickSession_DocumentSelfStateIsRecordedStateAlone(t *testing.T) {
	store := testStore(t)
	revision := filepath.Join(t.TempDir(), "revision")
	if err := os.WriteFile(revision, []byte("sha2"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeObservedRevisionFixture(t, revision, reviewDocument)
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "review",
			Status:   contract.TaskStatusProduced,
			Resource: "https://example.test/pull/1",
			Observed: observedFacts(map[string]any{"verdict_revision": "sha2"}),
			SetupAt:  time.Now(),
		},
	})

	if action := tickActionFor(t, cfg, store, "review#1"); action.Action == "satisfied" {
		t.Fatalf("a verdict left in outputs is not recorded state: %+v", action)
	}
}

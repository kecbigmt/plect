package persistence

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func migratedTestDB(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestPutSessionAndGetSession_RoundTripsRelationalAndJSONFields(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	session := &domain.Session{
		Name:             "case123",
		ResourceID:       "https://example.test/resource/123",
		Branch:           "case-123",
		WorkspaceDirPath: "/tmp/workdirs/case123",
		Conversation: &domain.Conversation{
			Source: "example-chat",
			URL:    "https://example.test/chat/archives/C01/p123",
			Metadata: map[string]string{
				"thread_ts": "1234567890.123456",
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "case123")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.Name != session.Name || got.ResourceID != session.ResourceID || got.Branch != session.Branch {
		t.Errorf("got = %+v, want name/resource_id/branch to match", got)
	}
	if got.Conversation == nil || got.Conversation.Source != "example-chat" {
		t.Fatalf("Conversation not round-tripped through record_json: %+v", got.Conversation)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Errorf("CreatedAt/UpdatedAt = %v/%v, want %v", got.CreatedAt, got.UpdatedAt, now)
	}
}

func TestGetSession_MissingReturnsNilNil(t *testing.T) {
	db := migratedTestDB(t)
	got, err := db.GetSession(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got != nil {
		t.Errorf("GetSession = %v, want nil", got)
	}
}

func putBareSession(t *testing.T, db *DB, name, parent string) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.PutSession(context.Background(), &domain.Session{
		Name: name, ParentSession: parent, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("PutSession(%q): %v", name, err)
	}
}

func TestPutSession_DerivesChildrenFromParentSessionName(t *testing.T) {
	db := migratedTestDB(t)
	putBareSession(t, db, "root", "")
	putBareSession(t, db, "work", "root")
	putBareSession(t, db, "review", "root")

	root, err := db.GetSession(context.Background(), "root")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if fmt.Sprint(root.Children) != fmt.Sprint([]string{"review", "work"}) {
		t.Fatalf("root.Children = %v, want [review work]", root.Children)
	}
}

func TestPutSession_TreatsRootPrefixAsPseudoParent(t *testing.T) {
	db := migratedTestDB(t)
	putBareSession(t, db, "x", "")
	putBareSession(t, db, "reviewer", "root:x")

	reviewer, err := db.GetSession(context.Background(), "reviewer")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if reviewer.ParentSession != "root:x" {
		t.Fatalf("reviewer.ParentSession = %q, want root:x", reviewer.ParentSession)
	}
	x, err := db.GetSession(context.Background(), "x")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if len(x.Children) != 0 {
		t.Fatalf("x.Children = %v, want empty (reviewer is x's sibling under root:x, not its child)", x.Children)
	}
}

func TestPutSession_ClearsDanglingRootPrefix(t *testing.T) {
	db := migratedTestDB(t)
	putBareSession(t, db, "reviewer", "root:missing")

	reviewer, err := db.GetSession(context.Background(), "reviewer")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if reviewer.ParentSession != "" {
		t.Fatalf("reviewer.ParentSession = %q, want empty (root: target does not exist)", reviewer.ParentSession)
	}
}

func TestPutSession_ClearsSelfParent(t *testing.T) {
	db := migratedTestDB(t)
	putBareSession(t, db, "solo", "solo")

	solo, err := db.GetSession(context.Background(), "solo")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if solo.ParentSession != "" {
		t.Fatalf("solo.ParentSession = %q, want empty (a session cannot be its own parent)", solo.ParentSession)
	}
}

func TestPutSession_ClearsAParentReferenceThatWouldCreateACycle(t *testing.T) {
	db := migratedTestDB(t)
	putBareSession(t, db, "a", "")
	putBareSession(t, db, "b", "a")
	// Reassign a's parent to b, which is a's own child: a cycle.
	now := time.Now().UTC()
	if err := db.PutSession(context.Background(), &domain.Session{Name: "a", ParentSession: "b", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	a, err := db.GetSession(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if a.ParentSession != "" {
		t.Fatalf("a.ParentSession = %q, want empty (assigning b as a's parent would create a cycle)", a.ParentSession)
	}
}

func TestDeleteSession_DetachesChildrenAndCascadesTasks(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "root", "")
	putBareSession(t, db, "work", "root")
	putBareSession(t, db, "child", "work")

	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{
		Name: "work", ParentSession: "root", CreatedAt: now, UpdatedAt: now,
		Tasks: map[string]*contract.TaskState{
			"setup": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	if err := db.DeleteSession(ctx, "work"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	root, err := db.GetSession(ctx, "root")
	if err != nil {
		t.Fatalf("GetSession(root): %v", err)
	}
	if len(root.Children) != 0 {
		t.Fatalf("root.Children = %v, want empty after deleting work", root.Children)
	}
	child, err := db.GetSession(ctx, "child")
	if err != nil {
		t.Fatalf("GetSession(child): %v", err)
	}
	if child.ParentSession != "" {
		t.Fatalf("child.ParentSession = %q, want detached", child.ParentSession)
	}

	tasks, err := loadTasks(ctx, db.read, "work")
	if err != nil {
		t.Fatalf("loadTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks for deleted session %q = %v, want none (cascade delete)", "work", tasks)
	}
}

func TestAllSessions_ReturnsEveryPutSession(t *testing.T) {
	db := migratedTestDB(t)
	for _, name := range []string{"sess-alpha", "sess-beta", "sess-gamma"} {
		putBareSession(t, db, name, "")
	}
	all, err := db.AllSessions(context.Background())
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("AllSessions returned %d sessions, want 3", len(all))
	}
}

func TestFindSessionsByAlias_MatchesEveryTagVariantAndNeverMatchesEmpty(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	url := "https://example.test/resource/9"
	now := time.Now().UTC()
	for _, name := range []string{"case9", "case9+review"} {
		if err := db.PutSession(ctx, &domain.Session{Name: name, ResourceID: url, Alias: url, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("PutSession(%q): %v", name, err)
		}
	}
	hits, err := db.FindSessionsByAlias(ctx, url)
	if err != nil {
		t.Fatalf("FindSessionsByAlias: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("FindSessionsByAlias found %d, want 2", len(hits))
	}
	empty, err := db.FindSessionsByAlias(ctx, "")
	if err != nil {
		t.Fatalf("FindSessionsByAlias(\"\"): %v", err)
	}
	if empty != nil {
		t.Errorf("FindSessionsByAlias(\"\") = %v, want nil", empty)
	}
}

func TestUpdateSession_MissingSessionErrors(t *testing.T) {
	db := migratedTestDB(t)
	err := db.UpdateSession(context.Background(), "nonexistent", func(*domain.Session) error { return nil })
	if err == nil {
		t.Fatal("UpdateSession on a missing session must fail")
	}
}

func TestUpdateSession_AppliesFnAndPersistsTasks(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "s1", "")

	err := db.UpdateSession(ctx, "s1", func(s *domain.Session) error {
		s.Tasks = map[string]*contract.TaskState{
			"@workflow": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{"workspace_dir": "/tmp/x"}},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	task := got.Tasks["@workflow"]
	if task == nil {
		t.Fatal("task @workflow missing after UpdateSession")
	}
	if task.Outputs["workspace_dir"] != "/tmp/x" {
		t.Errorf("task outputs = %v, want workspace_dir=/tmp/x", task.Outputs)
	}
}

func TestUpdateSession_FnErrorAbortsWithoutWriting(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "s1", "")

	wantErr := fmt.Errorf("boom")
	err := db.UpdateSession(ctx, "s1", func(s *domain.Session) error {
		s.WorkspaceDirPath = "/should/not/persist"
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("UpdateSession error = %v, want %v", err, wantErr)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.WorkspaceDirPath != "" {
		t.Errorf("WorkspaceDirPath = %q, want empty (fn's error must have rolled back the write)", got.WorkspaceDirPath)
	}
}

func TestPutSession_WithDoneWhenAndJudgesRoundTrips(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	session := &domain.Session{
		Name: "reviewed", CreatedAt: now, UpdatedAt: now,
		Tasks: map[string]*contract.TaskState{
			"impl": {
				Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Dynamic: true, TaskID: "impl-work",
				DoneWhen: &contract.DoneWhenState{
					HeartbeatTicks:  3,
					LastFingerprint: "abc123",
					LastUnsatisfied: []string{"leaf-a", "leaf-b"},
					Judges: map[string]*contract.DoneWhenJudge{
						"leaf-a": {
							LeafID: "leaf-a", Action: "approve", Reason: "looks good",
							Revision: "sha1", TargetSession: "reviewed", Instance: "impl",
							ReviewerSession: "reviewer1", ReviewerWorkflow: "coding-agent",
							Relation: "sibling", CreatedAt: now,
						},
					},
				},
			},
		},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "reviewed")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	task := got.Tasks["impl"]
	if task == nil || task.DoneWhen == nil {
		t.Fatalf("task/DoneWhen missing: %+v", task)
	}
	if task.DoneWhen.HeartbeatTicks != 3 || task.DoneWhen.LastFingerprint != "abc123" {
		t.Errorf("DoneWhen scalars = %+v", task.DoneWhen)
	}
	if fmt.Sprint(task.DoneWhen.LastUnsatisfied) != fmt.Sprint([]string{"leaf-a", "leaf-b"}) {
		t.Errorf("LastUnsatisfied = %v", task.DoneWhen.LastUnsatisfied)
	}
	judge := task.DoneWhen.Judges["leaf-a"]
	if judge == nil {
		t.Fatal("judge leaf-a missing")
	}
	if judge.Action != "approve" || judge.ReviewerSession != "reviewer1" || judge.Instance != "impl" {
		t.Errorf("judge = %+v", judge)
	}
	if !judge.CreatedAt.Equal(now) {
		t.Errorf("judge.CreatedAt = %v, want %v", judge.CreatedAt, now)
	}
}

// TestPutSession_NodeInstanceDoneWhenRoundTripsAsEmbeddedJSON proves a
// static node instance's (Dynamic == false) DoneWhen survives round-trip
// even though it is never split into the relational done_when tables
// (those attach only to dynamic task_instances rows): it stays embedded in
// node_instances.record_json instead.
func TestPutSession_NodeInstanceDoneWhenRoundTripsAsEmbeddedJSON(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	session := &domain.Session{
		Name: "s1", CreatedAt: now, UpdatedAt: now,
		Tasks: map[string]*contract.TaskState{
			"@workflow": {
				Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced,
				DoneWhen: &contract.DoneWhenState{LastFingerprint: "wf-fingerprint"},
			},
		},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	task := got.Tasks["@workflow"]
	if task == nil || task.Dynamic {
		t.Fatalf("task = %+v, want a static (non-dynamic) node instance", task)
	}
	if task.DoneWhen == nil || task.DoneWhen.LastFingerprint != "wf-fingerprint" {
		t.Fatalf("DoneWhen = %+v, want it preserved via embedded JSON", task.DoneWhen)
	}
}

func TestPutSession_ReplacesTasksRatherThanAccumulating(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	session.Tasks = map[string]*contract.TaskState{
		"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession (2nd): %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if _, ok := got.Tasks["a"]; ok {
		t.Errorf("task %q survived a Put that no longer declared it", "a")
	}
	if _, ok := got.Tasks["b"]; !ok {
		t.Errorf("task %q missing after Put", "b")
	}
}

// TestPutSession_DynamicInstanceCleanupThenSetupYieldsFreshDoneWhenHistory
// proves a dynamic instance's id is minted fresh on every write: a cleanup
// (dropping the instance) followed by a new setup under the same
// instance_name must not resurrect the retired instance's done_when/judge
// history, since a real cleanup+setup pair goes through two separate
// PutSession/UpdateSession calls, not one.
func TestPutSession_DynamicInstanceCleanupThenSetupYieldsFreshDoneWhenHistory(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {
			Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Dynamic: true, TaskID: "work", Name: "initial",
			DoneWhen: &contract.DoneWhenState{
				LastFingerprint: "old",
				Judges: map[string]*contract.DoneWhenJudge{
					"leaf-a": {LeafID: "leaf-a", Action: "approve", Relation: "sibling", CreatedAt: now},
				},
			},
		},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}

	// Cleanup: the instance is dropped entirely.
	cleaned := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now}
	if err := db.PutSession(ctx, cleaned); err != nil {
		t.Fatalf("PutSession (cleanup): %v", err)
	}

	// Setup: a new instance under the same instance_name, with no done_when yet.
	recreated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Dynamic: true, TaskID: "work", Name: "initial"},
	}}
	if err := db.PutSession(ctx, recreated); err != nil {
		t.Fatalf("PutSession (recreate): %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	task := got.Tasks["initial"]
	if task == nil {
		t.Fatal("recreated instance missing")
	}
	if task.DoneWhen != nil {
		t.Fatalf("DoneWhen = %+v, want nil (the retired instance's done_when/judge history must not resurface on a same-named recreate)", task.DoneWhen)
	}
}

// TestPutSession_DynamicInstanceIDStableAcrossOrdinaryUpdate proves the
// other half of M5's regression ask: two consecutive Puts that both still
// declare the same dynamic instance name preserve the row's id (an
// ordinary update, not a cleanup), so done_when/judge history keyed by
// that id survives across it too.
func TestPutSession_DynamicInstanceIDStableAcrossOrdinaryUpdate(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Dynamic: true, TaskID: "work", Name: "initial"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	firstID := taskInstanceIDForTest(t, db, "s1", "initial")

	updated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Dynamic: true, TaskID: "work", Name: "initial",
			DoneWhen: &contract.DoneWhenState{LastFingerprint: "new"},
		},
	}}
	if err := db.PutSession(ctx, updated); err != nil {
		t.Fatalf("PutSession (update): %v", err)
	}
	secondID := taskInstanceIDForTest(t, db, "s1", "initial")

	if firstID != secondID {
		t.Fatalf("id changed across an ordinary update: first = %q, second = %q", firstID, secondID)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	task := got.Tasks["initial"]
	if task == nil || task.DoneWhen == nil || task.DoneWhen.LastFingerprint != "new" {
		t.Fatalf("task after update = %+v, want done_when.last_fingerprint = %q", task, "new")
	}
}

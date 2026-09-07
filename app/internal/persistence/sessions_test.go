package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
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

func TestPutSessionAndGetSession_RoundTripsRelationalFields(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	session := &domain.Session{
		Name:             "case123",
		ResourceID:       "https://example.test/resource/123",
		WorkspaceDirPath: "/tmp/workdirs/case123",
		CreatedAt:        now,
		UpdatedAt:        now,
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
	if got.Name != session.Name || got.ResourceID != session.ResourceID {
		t.Errorf("got = %+v, want name/resource_id to match", got)
	}
	if got.ID == "" {
		t.Error("ID is empty, want a minted ULID")
	}
	if got.Status != contract.SessionStatusDown {
		t.Errorf("Status = %q, want %q (a fresh Put with no explicit status)", got.Status, contract.SessionStatusDown)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Errorf("CreatedAt/UpdatedAt = %v/%v, want %v", got.CreatedAt, got.UpdatedAt, now)
	}
}

func TestImportSession_UsesGivenIDAndLeavesParentLinkUnset(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	const legacyID = "01HZLEGACYIDXXXXXXXXXXXXX"
	session := &domain.Session{
		Name:          "legacy-case",
		ParentSession: "some-parent-not-yet-imported",
		Workflow:      "wf",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := db.ImportSession(ctx, legacyID, session); err != nil {
		t.Fatalf("ImportSession: %v", err)
	}

	got, err := db.GetSession(ctx, "legacy-case")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.ID != legacyID {
		t.Errorf("ID = %q, want the given legacy id %q", got.ID, legacyID)
	}
	if got.ParentSession != "" {
		t.Errorf("ParentSession = %q, want unresolved (empty) until a later PutSession pass", got.ParentSession)
	}
}

func TestImportSession_ThenPutSessionResolvesParentLinkOnceParentExists(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	parent := &domain.Session{Name: "parent", CreatedAt: now, UpdatedAt: now}
	child := &domain.Session{Name: "child", ParentSession: "parent", CreatedAt: now, UpdatedAt: now}

	// Import order deliberately puts the child before its parent: import
	// order over an unordered legacy session map must not matter.
	if err := db.ImportSession(ctx, "child-id", child); err != nil {
		t.Fatalf("ImportSession(child): %v", err)
	}
	if err := db.ImportSession(ctx, "parent-id", parent); err != nil {
		t.Fatalf("ImportSession(parent): %v", err)
	}

	if err := db.PutSession(ctx, child); err != nil {
		t.Fatalf("PutSession(child) parent-link pass: %v", err)
	}

	got, err := db.GetSession(ctx, "child")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.ParentSession != "parent" {
		t.Errorf("ParentSession = %q, want %q resolved once the parent row exists", got.ParentSession, "parent")
	}
	if got.ID != "child-id" {
		t.Errorf("ID = %q, want the id ImportSession preserved (%q), not a fresh mint", got.ID, "child-id")
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

// TestPutSession_ChildKeepsFirstParentIncarnationAcrossParentDestroyAndRecreate
// is the regression the chain reviewer asked for: a child's parent link is
// resolved once, against the referenced session's live row at that time,
// and never re-resolved on a later write to the child — so destroying the
// parent and recreating it under the same name (a distinct id) does not
// retarget an existing child onto the new incarnation the next time the
// child itself is written.
func TestPutSession_ChildKeepsFirstParentIncarnationAcrossParentDestroyAndRecreate(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "p1", "")
	putBareSession(t, db, "child", "p1")

	before, err := db.GetSession(ctx, "child")
	if err != nil {
		t.Fatalf("GetSession (before): %v", err)
	}
	firstParentID := before.ParentSessionID
	if firstParentID == "" {
		t.Fatal("child.ParentSessionID is empty, want p1's first incarnation id")
	}

	if err := db.UpdateSession(ctx, "p1", func(s *domain.Session) error {
		s.Status = contract.SessionStatusDestroyed
		s.DestroyedAt = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatalf("destroy p1: %v", err)
	}
	putBareSession(t, db, "p1", "") // recreate under the same name: a new id.
	recreatedParent, err := db.GetSession(ctx, "p1")
	if err != nil {
		t.Fatalf("GetSession (recreated p1): %v", err)
	}
	if recreatedParent.ID == firstParentID {
		t.Fatalf("recreated p1's id = %q, want distinct from the destroyed row's %q", recreatedParent.ID, firstParentID)
	}

	// Write the child again -- this is exactly the write the reviewed
	// defect mis-resolved: it must leave the existing parent link alone
	// rather than re-resolving "p1" against the new live row.
	if err := db.UpdateSession(ctx, "child", func(s *domain.Session) error {
		s.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatalf("update child: %v", err)
	}

	after, err := db.GetSession(ctx, "child")
	if err != nil {
		t.Fatalf("GetSession (after): %v", err)
	}
	if after.ParentSessionID != firstParentID {
		t.Fatalf("child.ParentSessionID = %q after a later write, want unchanged %q (p1's first incarnation)", after.ParentSessionID, firstParentID)
	}
	if after.ParentSession != "p1" {
		t.Fatalf("child.ParentSession = %q, want %q (the destroyed first incarnation's own retained name)", after.ParentSession, "p1")
	}
}

func TestUpdateSession_DestroyRetainsRowAndMintsFreshIDOnRecreate(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "s1", "")

	before, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession (before): %v", err)
	}
	firstID := before.ID

	destroyedAt := time.Now().UTC()
	if err := db.UpdateSession(ctx, "s1", func(s *domain.Session) error {
		s.Status = contract.SessionStatusDestroyed
		s.DestroyedAt = destroyedAt
		return nil
	}); err != nil {
		t.Fatalf("UpdateSession (destroy): %v", err)
	}

	// The destroyed row is no longer live: reads by name resolve to nothing.
	afterDestroy, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession (after destroy): %v", err)
	}
	if afterDestroy != nil {
		t.Fatalf("GetSession after destroy = %+v, want nil (destroyed rows are not live)", afterDestroy)
	}

	// A same-name create afterward mints a second row with a new id.
	putBareSession(t, db, "s1", "")
	recreated, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession (after recreate): %v", err)
	}
	if recreated == nil {
		t.Fatal("GetSession after recreate = nil, want the fresh row")
	}
	if recreated.ID == firstID {
		t.Fatalf("recreated session's ID = %q, want distinct from the destroyed row's %q", recreated.ID, firstID)
	}
	if recreated.Status != contract.SessionStatusDown {
		t.Errorf("recreated Status = %q, want %q", recreated.Status, contract.SessionStatusDown)
	}

	// The destroyed row itself is retained, not deleted: its own id still
	// resolves a name (raw introspection, since GetSession only ever
	// resolves the live row).
	var status string
	var destroyedAtCol string
	if err := db.write.QueryRowContext(ctx, `SELECT status, destroyed_at FROM sessions WHERE id = ?`, firstID).Scan(&status, &destroyedAtCol); err != nil {
		t.Fatalf("read retained destroyed row: %v", err)
	}
	if status != contract.SessionStatusDestroyed {
		t.Errorf("retained row status = %q, want %q", status, contract.SessionStatusDestroyed)
	}
	if destroyedAtCol == "" {
		t.Error("retained row destroyed_at is empty, want the destroy time")
	}
}

func TestAllSessions_ReturnsEveryLivePutSessionAndExcludesDestroyed(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"sess-alpha", "sess-beta", "sess-gamma"} {
		putBareSession(t, db, name, "")
	}
	if err := db.UpdateSession(ctx, "sess-beta", func(s *domain.Session) error {
		s.Status = contract.SessionStatusDestroyed
		s.DestroyedAt = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatalf("UpdateSession (destroy sess-beta): %v", err)
	}

	all, err := db.AllSessions(ctx)
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllSessions returned %d sessions, want 2 (destroyed excluded): %v", len(all), all)
	}
	if _, ok := all["sess-beta"]; ok {
		t.Error("destroyed session sess-beta present in AllSessions, want excluded")
	}
}

// TestAllSessions_MatchesGetSessionAcrossParentsChildrenTasksAndChannelHealth
// guards against AllSessions' batched extras-loading silently disagreeing
// with GetSession's per-row loadSessionExtras for the same session.
func TestAllSessions_MatchesGetSessionAcrossParentsChildrenTasksAndChannelHealth(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := db.PutSession(ctx, &domain.Session{Name: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("PutSession(root): %v", err)
	}
	childA := &domain.Session{
		Name: "childA", ParentSession: "root", CreatedAt: now, UpdatedAt: now,
		ChannelValidationHealth: &contract.ChannelHealth{
			ConsecutiveFailures: 1, FirstFailureAt: now, LastFailureAt: now, LastError: "bad channel",
		},
		ChannelDeliveryHealth: &contract.ChannelHealth{
			ConsecutiveFailures: 2, FirstFailureAt: now, LastFailureAt: now, LastChannel: "chat:#eng", LastError: "timeout",
		},
		Nodes: map[string]*contract.TaskState{
			"setup": {
				Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "setup-def",
				Layers: []contract.LayerState{{EffectID: "outer", Status: contract.TaskStatusProduced, SetupAt: now}},
			},
		},
		Tasks: map[string]*contract.TaskState{
			"impl": {
				Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "impl-def",
				Layers: []contract.LayerState{{EffectID: "layer1", Status: contract.TaskStatusProduced}},
				DoneWhen: &contract.DoneWhenState{
					HeartbeatTicks:  3,
					LastFingerprint: "abc123",
					LastUnsatisfied: []string{"leaf-a"},
					Judges: map[string]*contract.DoneWhenJudge{
						"leaf-a": {
							LeafID: "leaf-a", Action: "approve", Reason: "looks good",
							Revision: "sha1", JudgeSession: "reviewer1", JudgeWorkflow: "coding-agent",
							Relation: "sibling", CreatedAt: now,
						},
					},
				},
			},
		},
	}
	if err := db.PutSession(ctx, childA); err != nil {
		t.Fatalf("PutSession(childA): %v", err)
	}
	childB := &domain.Session{
		Name: "childB", ParentSession: "root", CreatedAt: now, UpdatedAt: now,
		// Same node_id as childA's "setup" node, deliberately: node_id is a
		// workflow-declared identifier, not globally unique, so this proves
		// AllSessions' batched layer lookup keys on (session, node_id) and
		// never leaks childA's "outer" layer onto childB's node of the same
		// name.
		Nodes: map[string]*contract.TaskState{
			"setup": {
				Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "setup-def-b",
				Layers: []contract.LayerState{{EffectID: "childB-only", Status: contract.TaskStatusProduced}},
			},
		},
	}
	if err := db.PutSession(ctx, childB); err != nil {
		t.Fatalf("PutSession(childB): %v", err)
	}
	if err := db.PutSession(ctx, &domain.Session{Name: "grandchild", ParentSession: "root:childA", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("PutSession(grandchild): %v", err)
	}

	all, err := db.AllSessions(ctx)
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}

	for _, name := range []string{"root", "childA", "childB", "grandchild"} {
		want, err := db.GetSession(ctx, name)
		if err != nil {
			t.Fatalf("GetSession(%q): %v", name, err)
		}
		got, ok := all[name]
		if !ok {
			t.Fatalf("AllSessions missing %q", name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AllSessions[%q] = %+v, want (matching GetSession) %+v", name, got, want)
		}
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

func TestUpdateSession_AppliesFnAndPersistsNodes(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "s1", "")

	err := db.UpdateSession(ctx, "s1", func(s *domain.Session) error {
		s.Nodes = map[string]*contract.TaskState{
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
	node := got.Nodes["@workflow"]
	if node == nil {
		t.Fatal("node @workflow missing after UpdateSession")
	}
	if node.Outputs["workspace_dir"] != "/tmp/x" {
		t.Errorf("node outputs = %v, want workspace_dir=/tmp/x", node.Outputs)
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
				Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "impl-work",
				DoneWhen: &contract.DoneWhenState{
					HeartbeatTicks:  3,
					LastFingerprint: "abc123",
					LastUnsatisfied: []string{"leaf-a", "leaf-b"},
					Judges: map[string]*contract.DoneWhenJudge{
						"leaf-a": {
							LeafID: "leaf-a", Action: "approve", Reason: "looks good",
							Revision:     "sha1",
							JudgeSession: "reviewer1", JudgeWorkflow: "coding-agent",
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
	if judge.Action != "approve" || judge.JudgeSession != "reviewer1" {
		t.Errorf("judge = %+v", judge)
	}
	if !judge.CreatedAt.Equal(now) {
		t.Errorf("judge.CreatedAt = %v, want %v", judge.CreatedAt, now)
	}
}

// TestPutSession_NodeInstanceDoneWhenRoundTripsAsEmbeddedJSON proves a
// static node instance's DoneWhen survives round-trip even though it is
// never split into the relational done_when tables (those attach only to
// task_instances rows): it stays embedded in node_instances.done_when_json
// instead.
func TestPutSession_NodeInstanceDoneWhenRoundTripsAsEmbeddedJSON(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	session := &domain.Session{
		Name: "s1", CreatedAt: now, UpdatedAt: now,
		Nodes: map[string]*contract.TaskState{
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
	node := got.Nodes["@workflow"]
	if node == nil {
		t.Fatalf("node = %+v, want a node instance", node)
	}
	if node.DoneWhen == nil || node.DoneWhen.LastFingerprint != "wf-fingerprint" {
		t.Fatalf("DoneWhen = %+v, want it preserved via embedded JSON", node.DoneWhen)
	}
}

func TestPutSession_ReplacesNodesRatherThanAccumulating(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	session.Nodes = map[string]*contract.TaskState{
		"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession (2nd): %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if _, ok := got.Nodes["a"]; ok {
		t.Errorf("node %q survived a Put that no longer declared it", "a")
	}
	if _, ok := got.Nodes["b"]; !ok {
		t.Errorf("node %q missing after Put", "b")
	}
}

// TestPutSession_ReplacesTasksRatherThanAccumulating covers the task_instances
// reconciliation path (upsert current, delete any instance_name no longer
// present), which is a different write strategy from node_instances' full
// delete-then-insert (see TestPutSession_ReplacesNodesRatherThanAccumulating).
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

func TestPutSession_DynamicInstanceCleanupThenSetupYieldsFreshDoneWhenHistory(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {
			Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Name: "initial",
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

	cleaned := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now}
	if err := db.PutSession(ctx, cleaned); err != nil {
		t.Fatalf("PutSession (cleanup): %v", err)
	}

	recreated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Name: "initial"},
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

func TestPutSession_DynamicInstanceIDStableAcrossOrdinaryUpdate(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Name: "initial"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	firstID := taskInstanceIDForTest(t, db, "s1", "initial")

	updated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Tasks: map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Name: "initial",
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

func TestPutSessionAndGetSession_RoundTripsPopulationThroughColumns(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.UpdatePopulation(ctx, "wf1/pop1", func(p *domain.PopulationState) error {
		p.Workflow = "wf1"
		p.Name = "pop1"
		return nil
	}); err != nil {
		t.Fatalf("UpdatePopulation (seed): %v", err)
	}

	session := &domain.Session{
		Name: "case42", CreatedAt: now, UpdatedAt: now,
		Population: &contract.PopulationProvenance{Workflow: "wf1", Name: "pop1"},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "case42")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Population == nil || got.Population.Workflow != "wf1" || got.Population.Name != "pop1" {
		t.Fatalf("Population = %+v, want {wf1 pop1}", got.Population)
	}

	got.Population = nil
	if err := db.PutSession(ctx, got); err != nil {
		t.Fatalf("PutSession (clear): %v", err)
	}
	cleared, err := db.GetSession(ctx, "case42")
	if err != nil {
		t.Fatalf("GetSession (after clear): %v", err)
	}
	if cleared.Population != nil {
		t.Fatalf("Population after clear = %+v, want nil", cleared.Population)
	}
}

// TestPutSessionAndGetSession_EveryFieldRoundTrips is the issue's own
// acceptance criterion: a session and a task instance with every field
// populated must read back unchanged through the named columns.
func TestPutSessionAndGetSession_EveryFieldRoundTrips(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	earlier := now.Add(-time.Hour)

	session := &domain.Session{
		Name:             "full",
		ResourceID:       "https://example.test/resource/full",
		Alias:            "https://example.test/resource/full",
		WorkspaceDirPath: "/tmp/workdirs/full",
		Workflow:         "coding-agent",
		Inputs:           map[string]any{"reason": "investigate"},
		Health: &contract.HealthState{
			LastCheckedAt:   earlier,
			LastActivityAt:  earlier,
			LastFingerprint: "fp-1",
			LastState:       "healthy",
			LastReason:      "alive probe passed",
			LastNotifiedAt:  earlier,
			NotifyCount:     2,
		},
		ChannelValidationHealth: &contract.ChannelHealth{
			ConsecutiveFailures: 1,
			FirstFailureAt:      earlier,
			LastFailureAt:       earlier,
			LastError:           "no valid channel definition",
		},
		ChannelDeliveryHealth: &contract.ChannelHealth{
			ConsecutiveFailures: 3,
			FirstFailureAt:      earlier,
			LastFailureAt:       now,
			LastChannel:         "chat:#eng",
			LastError:           "timeout",
			EscalatedAt:         now,
		},
		LastTickAt:  now,
		TickBackoff: &contract.TickBackoff{LastFingerprint: "tick-fp", ConsecutiveUnchanged: 4},
		CreatedAt:   earlier,
		UpdatedAt:   now,
		Tasks: map[string]*contract.TaskState{
			"work": {
				Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced,
				TaskID: "work-def", Resource: "resource-1", Name: "work",
				Inputs:  map[string]any{"in": 1},
				Outputs: map[string]any{"out": 2},
				State:   map[string]any{"reviewed": true},
				Observed: &contract.ResourceObservation{
					State: map[string]any{"open": true},
					At:    now,
				},
				ExtraDoneWhen: []byte(`{"all":[]}`),
				SetupAt:       earlier,
				FailedAt:      time.Time{},
				CleanedAt:     time.Time{},
				FinalizedAt:   now,
				Error:         "",
				Layers: []contract.LayerState{
					{
						EffectID: "outer", Status: contract.TaskStatusProduced,
						Inputs: map[string]any{"a": 1}, Locals: map[string]any{"b": 2}, Outputs: map[string]any{"c": 3},
						Env:                  map[string]string{"FOO": "bar"},
						HeartbeatTicks:       2,
						HeartbeatEscalations: 1,
						SetupAt:              earlier,
					},
					{
						EffectID: "inner", Status: contract.TaskStatusFailed,
						Error:    "boom",
						FailedAt: now,
					},
				},
			},
		},
	}

	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "full")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("GetSession returned nil")
	}

	if got.ResourceID != session.ResourceID || got.Alias != session.Alias ||
		got.WorkspaceDirPath != session.WorkspaceDirPath || got.Workflow != session.Workflow {
		t.Fatalf("relational fields = %+v, want %+v", got, session)
	}
	if got.Inputs["reason"] != "investigate" {
		t.Errorf("Inputs = %v", got.Inputs)
	}
	if got.Health == nil {
		t.Fatal("Health missing")
	}
	if !got.Health.LastCheckedAt.Equal(earlier) || !got.Health.LastActivityAt.Equal(earlier) ||
		got.Health.LastFingerprint != "fp-1" || got.Health.LastState != "healthy" || got.Health.LastReason != "alive probe passed" ||
		!got.Health.LastNotifiedAt.Equal(earlier) || got.Health.NotifyCount != 2 {
		t.Errorf("Health = %+v", got.Health)
	}
	if got.ChannelValidationHealth == nil || got.ChannelValidationHealth.ConsecutiveFailures != 1 || got.ChannelValidationHealth.LastError != "no valid channel definition" {
		t.Errorf("ChannelValidationHealth = %+v", got.ChannelValidationHealth)
	}
	if got.ChannelDeliveryHealth == nil || got.ChannelDeliveryHealth.ConsecutiveFailures != 3 || got.ChannelDeliveryHealth.LastChannel != "chat:#eng" ||
		got.ChannelDeliveryHealth.LastError != "timeout" || !got.ChannelDeliveryHealth.EscalatedAt.Equal(now) {
		t.Errorf("ChannelDeliveryHealth = %+v", got.ChannelDeliveryHealth)
	}
	if !got.LastTickAt.Equal(now) {
		t.Errorf("LastTickAt = %v, want %v", got.LastTickAt, now)
	}
	if got.TickBackoff == nil || got.TickBackoff.LastFingerprint != "tick-fp" || got.TickBackoff.ConsecutiveUnchanged != 4 {
		t.Errorf("TickBackoff = %+v", got.TickBackoff)
	}

	task := got.Tasks["work"]
	if task == nil {
		t.Fatal("task work missing")
	}
	if task.TaskID != "work-def" || task.Resource != "resource-1" || task.Name != "work" {
		t.Errorf("task identity = %+v", task)
	}
	if fmt.Sprint(task.Inputs) != fmt.Sprint(map[string]any{"in": float64(1)}) {
		t.Errorf("task.Inputs = %v", task.Inputs)
	}
	if fmt.Sprint(task.Outputs) != fmt.Sprint(map[string]any{"out": float64(2)}) {
		t.Errorf("task.Outputs = %v", task.Outputs)
	}
	if task.State["reviewed"] != true {
		t.Errorf("task.State = %v", task.State)
	}
	if task.Observed == nil || task.Observed.State["open"] != true || !task.Observed.At.Equal(now) {
		t.Errorf("task.Observed = %+v", task.Observed)
	}
	if string(task.ExtraDoneWhen) != `{"all":[]}` {
		t.Errorf("task.ExtraDoneWhen = %s", task.ExtraDoneWhen)
	}
	if !task.SetupAt.Equal(earlier) || !task.FinalizedAt.Equal(now) {
		t.Errorf("task timestamps = %+v", task)
	}
	if len(task.Layers) != 2 {
		t.Fatalf("task.Layers = %+v, want 2 entries in order", task.Layers)
	}
	outer, inner := task.Layers[0], task.Layers[1]
	if outer.EffectID != "outer" || outer.Env["FOO"] != "bar" || outer.HeartbeatTicks != 2 || outer.HeartbeatEscalations != 1 || !outer.SetupAt.Equal(earlier) {
		t.Errorf("outer layer = %+v", outer)
	}
	if fmt.Sprint(outer.Inputs) != fmt.Sprint(map[string]any{"a": float64(1)}) {
		t.Errorf("outer.Inputs = %v", outer.Inputs)
	}
	if inner.EffectID != "inner" || inner.Status != contract.TaskStatusFailed || inner.Error != "boom" || !inner.FailedAt.Equal(now) {
		t.Errorf("inner layer = %+v", inner)
	}
}

// TestPutSession_FreshSessionAndTaskLeaveEveryJSONColumnNull proves the
// L1 rule holds without a record_json grab-bag: a session/instance with
// nothing beyond its columns writes NULL to every _json column rather than
// an empty-object/array placeholder.
func TestPutSession_FreshSessionAndTaskLeaveEveryJSONColumnNull(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{
		Name: "bare", CreatedAt: now, UpdatedAt: now,
		Tasks: map[string]*contract.TaskState{
			"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
		},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	var inputsJSON sql.NullString
	if err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetLiveSession(ctx, "bare")
		if err != nil {
			return err
		}
		inputsJSON = row.InputsJson
		return nil
	}); err != nil {
		t.Fatalf("read raw session row: %v", err)
	}
	if inputsJSON.Valid {
		t.Errorf("sessions.inputs_json = %q, want NULL", inputsJSON.String)
	}

	var taskInputsJSON, taskOutputsJSON, taskStateJSON sql.NullString
	if err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		sessionID, err := sqlcgen.New(tx).SessionIDByLiveName(ctx, "bare")
		if err != nil {
			return err
		}
		rows, err := sqlcgen.New(tx).ListTaskInstances(ctx, sessionID)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Fatalf("task_instances rows = %d, want 1", len(rows))
		}
		taskInputsJSON = rows[0].InputsJson
		taskOutputsJSON = rows[0].OutputsJson
		taskStateJSON = rows[0].StateJson
		return nil
	}); err != nil {
		t.Fatalf("read raw task instance row: %v", err)
	}
	if taskInputsJSON.Valid || taskOutputsJSON.Valid || taskStateJSON.Valid {
		t.Errorf("task_instances json columns = inputs:%v outputs:%v state:%v, want all NULL", taskInputsJSON, taskOutputsJSON, taskStateJSON)
	}
}

func TestEnsureLiveSession_LazilyCreatesAGenuinelyNewName(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	id, err := db.EnsureLiveSession(ctx, "never-created")
	if err != nil {
		t.Fatalf("EnsureLiveSession: %v", err)
	}
	if id == "" {
		t.Fatal("EnsureLiveSession returned an empty id for a name with no prior row")
	}
	got, err := db.GetSession(ctx, "never-created")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil || got.ID != id {
		t.Fatalf("GetSession = %+v, want the row EnsureLiveSession just minted (id %q)", got, id)
	}
}

func TestEnsureLiveSession_ReturnsTheExistingIDForALiveName(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "s1", "")
	before, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	id, err := db.EnsureLiveSession(ctx, "s1")
	if err != nil {
		t.Fatalf("EnsureLiveSession: %v", err)
	}
	if id != before.ID {
		t.Fatalf("EnsureLiveSession id = %q, want the already-live row's own id %q", id, before.ID)
	}
}

// TestEnsureLiveSession_RefusesToResurrectADestroyedName is the regression
// for a real bug: an event appended to a session racing its own destroy
// (e.g. a lifecycle observer's node-result recording, which runs during
// TaskCleanup's own cleanup script) must not silently mint a fresh live row
// under the destroyed name just because none is currently live. Only a
// name with no row at all, ever, gets the lazy-create fallback.
func TestEnsureLiveSession_RefusesToResurrectADestroyedName(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "s1", "")
	before, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if err := db.DestroySession(ctx, "s1", time.Now().UTC()); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}

	if _, err := db.EnsureLiveSession(ctx, "s1"); err == nil {
		t.Fatal("EnsureLiveSession succeeded against a destroyed name, want an error")
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got != nil {
		t.Fatalf("GetSession after EnsureLiveSession = %+v, want nil (still destroyed, not resurrected)", got)
	}
	// The original destroyed row itself is untouched.
	var status string
	if err := db.write.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, before.ID).Scan(&status); err != nil {
		t.Fatalf("read original row: %v", err)
	}
	if status != contract.SessionStatusDestroyed {
		t.Fatalf("original row status = %q, want %q", status, contract.SessionStatusDestroyed)
	}
}

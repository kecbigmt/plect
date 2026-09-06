package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// writeCapWorkflow declares a workflow with no nodes: reserveChildCapSlot
// only ever reads the parent's workflow for max_up_children.
func writeCapWorkflow(t *testing.T, baseDir, id string, maxUpChildren *int) {
	t.Helper()
	dir := filepath.Join(baseDir, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[%s]\nkind = \"workflow\"\n", id)
	if maxUpChildren != nil {
		body += fmt.Sprintf("max_up_children = %d\n", *maxUpChildren)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func upTasks() map[string]*contract.TaskState {
	return map[string]*contract.TaskState{
		"run_node": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
	}
}

func intPtr(n int) *int { return &n }

func TestReserveChildCapSlot_NoCapDeclaredAllowsUnlimitedChildren(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", nil)
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("child%d", i)
		seedSession(t, store, name, "acct", i, "", upTasks())
		setParent(t, store, name, "parent1")
	}

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "parent1", false)
	if err != nil {
		t.Fatalf("reserveChildCapSlot: %v, want nil (no cap declared)", err)
	}
	if reserved {
		t.Error("reserved = true, want false: no cap declared means nothing to reserve against")
	}
}

func TestReserveChildCapSlot_UnderCapAllowsNewChild(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(2))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "child0", "acct", 0, "", upTasks())
	setParent(t, store, "child0", "parent1")

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "parent1", false)
	if err != nil {
		t.Fatalf("reserveChildCapSlot: %v, want nil (1 up child, cap 2)", err)
	}
	if !reserved {
		t.Error("reserved = false, want true: a slot was free")
	}
}

func TestReserveChildCapSlot_AtCapRejectsNewChild(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(2))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("child%d", i)
		seedSession(t, store, name, "acct", i, "", upTasks())
		setParent(t, store, name, "parent1")
	}

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "parent1", false)
	if reserved {
		t.Error("reserved = true, want false: parent is already at cap")
	}
	if err == nil {
		t.Fatal("err = nil, want ErrChildCapExceeded (2 up children, cap 2)")
	}
	if err.Code != ErrChildCapExceeded {
		t.Errorf("Code = %q, want %q", err.Code, ErrChildCapExceeded)
	}
	for _, want := range []string{"parent1", "2"} {
		if !strings.Contains(err.Message, want) {
			t.Errorf("Message = %q, want it to name %q (parent/cap/count)", err.Message, want)
		}
	}
}

func TestReserveChildCapSlot_DownChildDoesNotCountTowardCap(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(2))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "childUp", "acct", 0, "", upTasks())
	setParent(t, store, "childUp", "parent1")
	seedSession(t, store, "childDown", "acct", 1, "", nil)
	setParent(t, store, "childDown", "parent1")

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "parent1", false)
	if err != nil {
		t.Fatalf("reserveChildCapSlot: %v, want nil (1 up + 1 down, cap 2)", err)
	}
	if !reserved {
		t.Error("reserved = false, want true: the down child left a slot free")
	}
}

func TestReserveChildCapSlot_AlreadyUpTargetIsExemptEvenAtFullCap(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(2))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("child%d", i)
		seedSession(t, store, name, "acct", i, "", upTasks())
		setParent(t, store, name, "parent1")
	}

	reserved, err := reserveChildCapSlot(cfg, store, "child0", "parent1", true)
	if err != nil {
		t.Fatalf("reserveChildCapSlot: %v, want nil (already-up target is exempt)", err)
	}
	if reserved {
		t.Error("reserved = true, want false: an already-up target needs no reservation")
	}
}

func TestReserveChildCapSlot_VirtualRootCapCountsEveryParentlessSession(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir(), MaxUpChildren: intPtr(2)}
	seedSession(t, store, "rootA", "acct", 0, "", upTasks())
	seedSession(t, store, "rootB", "acct", 1, "", upTasks())

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "", false)
	if reserved {
		t.Error("reserved = true, want false: virtual root is already at cap")
	}
	if err == nil || err.Code != ErrChildCapExceeded {
		t.Fatalf("err = %v, want ErrChildCapExceeded", err)
	}
	if !strings.Contains(err.Message, "virtual root") || !strings.Contains(err.Message, "max_up_children") {
		t.Fatalf("Message = %q, want the virtual root and max_up_children named", err.Message)
	}
}

func TestReserveChildCapSlot_VirtualRootCapIncludesExplicitSiblingCohorts(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir(), MaxUpChildren: intPtr(1)}
	seedSession(t, store, "cohort", "acct", 0, "", nil)
	seedSession(t, store, "sibling", "acct", 1, "", upTasks())
	setParent(t, store, "sibling", "root:cohort")

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "", false)
	if reserved {
		t.Error("reserved = true, want false: sibling cohort member consumes the virtual-root slot")
	}
	if err == nil || err.Code != ErrChildCapExceeded {
		t.Fatalf("err = %v, want ErrChildCapExceeded", err)
	}
}

func TestReserveChildCapSlot_UnsetVirtualRootCapAllowsParentlessSession(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	seedSession(t, store, "rootA", "acct", 0, "", upTasks())

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "", false)
	if err != nil || reserved {
		t.Fatalf("reserveChildCapSlot = %v, %v, want false, nil", reserved, err)
	}
}

func TestReserveChildCapSlot_OutstandingReservationCountsTowardCap(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)

	first, err := reserveChildCapSlot(cfg, store, "childA", "parent1", false)
	if err != nil || !first {
		t.Fatalf("first reservation: reserved=%v err=%v, want true/nil", first, err)
	}

	second, err := reserveChildCapSlot(cfg, store, "childB", "parent1", false)
	if second {
		t.Error("second reservation succeeded despite the first still being outstanding")
	}
	if err == nil || err.Code != ErrChildCapExceeded {
		t.Fatalf("second reservation err = %v, want ErrChildCapExceeded", err)
	}

	releaseChildCapSlot(store, "childA")

	third, err := reserveChildCapSlot(cfg, store, "childB", "parent1", false)
	if err != nil || !third {
		t.Fatalf("reservation after release: reserved=%v err=%v, want true/nil", third, err)
	}
}

// Each goroutine reserves its own distinct child, unlike the same-child case below.
func TestReserveChildCapSlot_ConcurrentReservationsRespectCap(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	const limit = 3
	const attempts = 20
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(limit))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)

	var wg sync.WaitGroup
	results := make([]bool, attempts)
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			child := fmt.Sprintf("child%d", i)
			reserved, err := reserveChildCapSlot(cfg, store, child, "parent1", false)
			if err != nil && err.Code != ErrChildCapExceeded {
				t.Errorf("reserveChildCapSlot: unexpected error %v", err)
				return
			}
			results[i] = reserved
		}(i)
	}
	wg.Wait()

	admitted := 0
	for _, ok := range results {
		if ok {
			admitted++
		}
	}
	if admitted != limit {
		t.Fatalf("admitted = %d, want exactly %d (the declared limit) across %d concurrent attempts", admitted, limit, attempts)
	}
}

// approveAnyReservation plants a reservation via store.ReserveUpSlot,
// simulating one a crashed `plect up` left behind.
func approveAnyReservation(map[string]*domain.Session, map[string]state.UpReservation) bool {
	return true
}

// Fires even with room to spare in the cap (set to 5 here) — it's the
// same child racing itself, not a sibling contending for a slot.
func TestReserveChildCapSlot_ConcurrentAttemptForTheSameChildIsRejected(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(5))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)

	first, err := reserveChildCapSlot(cfg, store, "childA", "parent1", false)
	if err != nil || !first {
		t.Fatalf("first reservation: reserved=%v err=%v, want true/nil", first, err)
	}

	second, err := reserveChildCapSlot(cfg, store, "childA", "parent1", false)
	if second {
		t.Error("a second concurrent reservation for the same child succeeded while the first was still live")
	}
	if err == nil || err.Code != ErrChildUpInProgress {
		t.Fatalf("second reservation err = %v, want ErrChildUpInProgress", err)
	}
}

func TestReserveChildCapSlot_DestroyingTheStuckChildFreesItsReservation(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)

	if _, err := store.ReserveUpSlot("childA", "parent1", approveAnyReservation); err != nil {
		t.Fatalf("simulate a crashed prior reservation: %v", err)
	}

	if reserved, err := reserveChildCapSlot(cfg, store, "childB", "parent1", false); reserved || err == nil {
		t.Fatalf("childB before destroying childA: reserved=%v err=%v, want rejected", reserved, err)
	}

	if err := store.Delete("childA"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reserved, err := reserveChildCapSlot(cfg, store, "childB", "parent1", false)
	if err != nil || !reserved {
		t.Fatalf("childB after destroying childA: reserved=%v err=%v, want true/nil", reserved, err)
	}
}

func TestReserveChildCapSlot_ForceRecreateOfAnUpChildDoesNotBlockItself(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "childA", "acct", 0, "", upTasks())
	setParent(t, store, "childA", "parent1")

	reserved, err := reserveChildCapSlot(cfg, store, "childA", "parent1", false)
	if err != nil || !reserved {
		t.Fatalf("reserveChildCapSlot: reserved=%v err=%v, want true/nil", reserved, err)
	}
}

func TestReserveChildCapSlot_ForceRecreateReservationStillBlocksASibling(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir()}
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))
	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "childA", "acct", 0, "", upTasks())
	setParent(t, store, "childA", "parent1")

	if reserved, err := reserveChildCapSlot(cfg, store, "childA", "parent1", false); err != nil || !reserved {
		t.Fatalf("reserve childA (force-recreate): reserved=%v err=%v", reserved, err)
	}
	// Simulate teardown clearing run-scoped tasks: only the reservation
	// should still protect the cap.
	if err := store.Update("childA", func(s *domain.Session) error {
		s.Tasks = nil
		return nil
	}); err != nil {
		t.Fatalf("simulate teardown: %v", err)
	}

	reserved, err := reserveChildCapSlot(cfg, store, "childB", "parent1", false)
	if reserved {
		t.Error("a sibling was admitted while childA's force-recreate reservation stood")
	}
	if err == nil || err.Code != ErrChildCapExceeded {
		t.Fatalf("err = %v, want ErrChildCapExceeded", err)
	}
}

// TestReserveChildCapSlot_FailsClosedWhenParentUnreadable proves an
// unreadable store never falls back to "no cap"/nil parent: that fallback
// used to be the only way store.Get's swallowed error could surface here,
// letting a cap silently be bypassed while the store cannot even prove the
// parent exists.
func TestReserveChildCapSlot_FailsClosedWhenParentUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "store.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(dir)
	cfg := &config.Config{BaseDir: t.TempDir()}

	reserved, err := reserveChildCapSlot(cfg, store, "newchild", "parent1", false)
	if reserved {
		t.Error("reserved = true over an unreadable store, want false")
	}
	if err == nil {
		t.Fatal("reserveChildCapSlot over an unreadable store must fail, not silently allow the reservation")
	}
}

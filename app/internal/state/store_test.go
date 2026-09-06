package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

func TestStore_PutAndGet(t *testing.T) {
	store := NewStore(t.TempDir())

	now := time.Now()
	session := &domain.Session{
		Name:             "owner/repo-123",
		ResourceID:       "https://example.test/owner/repo/items/123",
		Branch:           "issue/123",
		WorkspaceDirPath: "/tmp/workdirs/github.com/owner/repo/issue-123",
		Conversation: &domain.Conversation{
			Source: "Slack",
			URL:    "https://exampleorg.slack.com/archives/C01ABCDEF/p1234567890123456",
			Metadata: map[string]string{
				"thread_ts":  "1234567890.123456",
				"channel_id": "C01ABCDEF",
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.Put(session); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	got := store.Get("owner/repo-123")
	if got == nil {
		t.Fatal("Get() returned nil")
	}

	if got.Name != session.Name {
		t.Errorf("Name = %q, want %q", got.Name, session.Name)
	}
	if got.ResourceID != session.ResourceID {
		t.Errorf("ResourceID = %q, want %q", got.ResourceID, session.ResourceID)
	}
	if got.Conversation == nil || got.Conversation.Source != "Slack" {
		t.Errorf("Conversation not persisted correctly")
	}
	if got.Conversation.URL != "https://exampleorg.slack.com/archives/C01ABCDEF/p1234567890123456" {
		t.Errorf("Conversation URL = %q", got.Conversation.URL)
	}
}

func TestStore_DefaultDirUsesPlectureDataDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_DATA_HOME", "")

	store := NewStore("")
	want := filepath.Join(tmpHome, ".local", "share", "plect")
	if got := store.Dir(); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestStore_GetMissing(t *testing.T) {
	store := NewStore(t.TempDir())

	got := store.Get("nonexistent")
	if got != nil {
		t.Errorf("Get() = %v, want nil", got)
	}
}

func TestStore_CheckReadableAllowsAFreshDataDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	store := NewStore(dir)

	if err := store.CheckReadable(); err != nil {
		t.Fatalf("CheckReadable() with no database yet: %v", err)
	}
}

func TestStore_Delete(t *testing.T) {
	store := NewStore(t.TempDir())

	session := &domain.Session{
		Name:      "owner/repo-1",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	store.Put(session)

	if err := store.Delete("owner/repo-1"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}

	if got := store.Get("owner/repo-1"); got != nil {
		t.Error("Get() after Delete() should return nil")
	}
}

func TestStore_NormalizesSessionTree(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Now()

	if err := store.Put(&domain.Session{Name: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&domain.Session{Name: "work", ParentSession: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&domain.Session{Name: "review", ParentSession: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	root := store.Get("root")
	if root == nil {
		t.Fatal("root missing")
	}
	if got, want := root.Children, []string{"review", "work"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("root.Children = %v, want %v", got, want)
	}
}

func TestStore_NormalizeSessionTreeTreatsRootPrefixAsPseudoParent(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Now()

	if err := store.Put(&domain.Session{Name: "x", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&domain.Session{Name: "reviewer", ParentSession: "root:x", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	reviewer := store.Get("reviewer")
	if reviewer == nil {
		t.Fatal("reviewer missing")
	}
	if reviewer.ParentSession != "root:x" {
		t.Fatalf("reviewer.ParentSession = %q, want %q (a valid root: pseudo-parent must survive normalization)", reviewer.ParentSession, "root:x")
	}
	// x has no Children slot for the pseudo-parent — reviewer isn't x's child,
	// it's x's sibling (both under root:x); domain.RelationFromTarget derives that.
	if x := store.Get("x"); len(x.Children) != 0 {
		t.Fatalf("x.Children = %v, want empty", x.Children)
	}
}

func TestStore_NormalizeSessionTreeClearsDanglingRootPrefix(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Now()

	if err := store.Put(&domain.Session{Name: "reviewer", ParentSession: "root:missing", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	reviewer := store.Get("reviewer")
	if reviewer.ParentSession != "" {
		t.Fatalf("reviewer.ParentSession = %q, want empty (root: target does not exist)", reviewer.ParentSession)
	}
}

func TestStore_DeleteDetachesSessionTreeLinks(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Now()

	if err := store.Put(&domain.Session{Name: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&domain.Session{Name: "work", ParentSession: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&domain.Session{Name: "child", ParentSession: "work", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("work"); err != nil {
		t.Fatal(err)
	}

	root := store.Get("root")
	if root == nil {
		t.Fatal("root missing")
	}
	if len(root.Children) != 0 {
		t.Fatalf("root.Children = %v, want empty after deleting child", root.Children)
	}
	child := store.Get("child")
	if child == nil {
		t.Fatal("child missing")
	}
	if child.ParentSession != "" {
		t.Fatalf("child.ParentSession = %q, want detached", child.ParentSession)
	}
}

func TestStore_All(t *testing.T) {
	store := NewStore(t.TempDir())

	now := time.Now()
	for _, name := range []string{"a/b-1", "c/d-2", "e/f-3"} {
		store.Put(&domain.Session{
			Name:      name,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	all := store.All()
	if len(all) != 3 {
		t.Errorf("All() returned %d sessions, want 3", len(all))
	}
}

// TestStore_ConcurrentPut verifies that concurrent Put calls from multiple
// Store instances (simulating separate processes sharing the same
// database) do not cause lost updates.
func TestStore_ConcurrentPut(t *testing.T) {
	dir := t.TempDir()
	const n = 20

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Each goroutine creates its own Store instance (like separate processes)
			store := NewStore(dir)
			name := fmt.Sprintf("owner/repo-%d", i)
			now := time.Now()
			err := store.Put(&domain.Session{
				Name:      name,
				CreatedAt: now,
				UpdatedAt: now,
			})
			if err != nil {
				t.Errorf("Put(%q) error: %v", name, err)
			}
		}(i)
	}
	wg.Wait()

	store := NewStore(dir)
	all := store.All()
	if len(all) != n {
		t.Errorf("expected %d sessions, got %d (lost updates detected)", n, len(all))
	}
}

func TestStore_ConcurrentPutAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	const n = 12

	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestStorePutHelperProcess", "--", dir, strconv.Itoa(i))
			cmd.Env = append(os.Environ(), "PLECT_STATE_PUT_HELPER=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("helper %d: %w: %s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	store := NewStore(dir)
	all := store.All()
	if len(all) != n {
		t.Fatalf("All() returned %d sessions, want %d", len(all), n)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("owner/repo-process-%d", i)
		if all[name] == nil {
			t.Fatalf("missing session %q after concurrent process writes", name)
		}
	}
}

// Each attempt uses its own Store instance, like TestStore_ConcurrentPut,
// exercising the real cross-process database access, not just the
// in-process mutex.
func TestStore_ReserveUpSlotSerializesConcurrentReservations(t *testing.T) {
	dir := t.TempDir()
	const limit = 3
	const attempts = 20

	var wg sync.WaitGroup
	results := make([]bool, attempts)
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			store := NewStore(dir)
			child := fmt.Sprintf("child%d", i)
			approved, err := store.ReserveUpSlot(child, "parent1", func(sessions map[string]*domain.Session, reservations map[string]UpReservation) bool {
				return len(reservations) < limit
			})
			if err != nil {
				t.Errorf("ReserveUpSlot: %v", err)
				return
			}
			results[i] = approved
		}(i)
	}
	wg.Wait()

	approved := 0
	for _, ok := range results {
		if ok {
			approved++
		}
	}
	if approved != limit {
		t.Fatalf("approved = %d, want exactly %d (the declared limit) despite %d concurrent attempts", approved, limit, attempts)
	}

	if got := reservationCount(t, NewStore(dir)); got != limit {
		t.Errorf("reservation count = %d, want %d", got, limit)
	}
}

func TestStore_ReleaseUpSlotDropsTheNamedReservation(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, child := range []string{"childA", "childB"} {
		approved, err := store.ReserveUpSlot(child, "parent1", func(map[string]*domain.Session, map[string]UpReservation) bool { return true })
		if err != nil || !approved {
			t.Fatalf("ReserveUpSlot(%q): approved=%v err=%v", child, approved, err)
		}
	}

	if err := store.ReleaseUpSlot("childA"); err != nil {
		t.Fatalf("ReleaseUpSlot: %v", err)
	}
	names := reservationNames(t, store)
	if names["childA"] {
		t.Error("childA's reservation should be gone after ReleaseUpSlot")
	}
	if !names["childB"] {
		t.Error("childB's reservation should survive releasing childA's")
	}
}

func TestStore_ReleaseUpSlotOnUnreservedChildIsANoop(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.ReleaseUpSlot("childA"); err != nil {
		t.Fatalf("ReleaseUpSlot on a child with no reservation: %v", err)
	}
}

func TestStore_ReserveUpSlotExcludesReservationsFromDeadProcesses(t *testing.T) {
	store := NewStore(t.TempDir())
	plantReservation(t, store, "crashed-child", UpReservation{Parent: "parent1", At: time.Now(), PID: deadPID(t)})

	var seen map[string]UpReservation
	approved, err := store.ReserveUpSlot("new-child", "parent1", func(_ map[string]*domain.Session, reservations map[string]UpReservation) bool {
		seen = reservations
		return true
	})
	if err != nil || !approved {
		t.Fatalf("ReserveUpSlot: approved=%v err=%v", approved, err)
	}
	if _, ok := seen["crashed-child"]; ok {
		t.Error("a reservation from a dead process was still visible to the admission decision")
	}
}

// A duration so far past any plausible RunSetup that a fixed-TTL design
// would already have failed it.
func TestStore_ReserveUpSlotNeverExpiresALiveReservationRegardlessOfAge(t *testing.T) {
	store := NewStore(t.TempDir())
	plantReservation(t, store, "long-running-child", UpReservation{
		Parent: "parent1",
		At:     time.Now().Add(-10000 * time.Hour),
		PID:    os.Getpid(), // this test process: definitely still alive
	})

	var seen map[string]UpReservation
	approved, err := store.ReserveUpSlot("new-child", "parent1", func(_ map[string]*domain.Session, reservations map[string]UpReservation) bool {
		seen = reservations
		return true
	})
	if err != nil || !approved {
		t.Fatalf("ReserveUpSlot: approved=%v err=%v", approved, err)
	}
	if _, ok := seen["long-running-child"]; !ok {
		t.Error("a reservation held by a still-live process was treated as abandoned")
	}
}

// deadPID returns a PID guaranteed not to be running: a helper process's,
// after it has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("could not run a helper process to obtain a dead PID: %v", err)
	}
	return cmd.Process.Pid
}

func TestStore_ReserveUpSlotSupersedesAReservationWithNoLivePID(t *testing.T) {
	store := NewStore(t.TempDir())
	// PID 0 is never live (processAlive's own pid<=0 guard); ReserveUpSlot
	// itself always stamps a real os.Getpid(), so the only way such a row
	// exists is a planted fixture like this one.
	plantReservation(t, store, "childA", UpReservation{Parent: "parent1", At: time.Now(), PID: 0})

	sawItself := false
	approved, err := store.ReserveUpSlot("childA", "parent1", func(_ map[string]*domain.Session, reservations map[string]UpReservation) bool {
		_, sawItself = reservations["childA"]
		return true
	})
	if err != nil || !approved {
		t.Fatalf("ReserveUpSlot: approved=%v err=%v", approved, err)
	}
	if sawItself {
		t.Error("a reservation attempt saw its own PID-0 (never-live) prior reservation as if it were a live sibling's")
	}
}

func TestStore_DeleteClearsTheSessionsUpReservation(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Now()
	if err := store.Put(&domain.Session{Name: "childA", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	plantReservation(t, store, "childA", UpReservation{Parent: "parent1", At: now, PID: os.Getpid()})

	if err := store.Delete("childA"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if names := reservationNames(t, store); names["childA"] {
		t.Error("Delete should have cleared childA's reservation")
	}
}

// plantReservation writes a reservation directly, bypassing ReserveUpSlot's
// own PID/timestamp stamping — for planting a specific PID or backdated time.
func plantReservation(t *testing.T, store *Store, child string, res UpReservation) {
	t.Helper()
	db, err := store.dbHandle()
	if err != nil {
		t.Fatalf("plantReservation: %v", err)
	}
	if err := db.WithImmediateTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`INSERT INTO up_reservations (child_session_name, parent_name, pid, reserved_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(child_session_name) DO UPDATE SET parent_name=excluded.parent_name, pid=excluded.pid, reserved_at=excluded.reserved_at`,
			child, res.Parent, res.PID, res.At.UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatalf("plantReservation: %v", err)
	}
}

// reservationNames/reservationCount inspect the up_reservations table
// directly for assertions the public API has no reason to expose.
func reservationNames(t *testing.T, store *Store) map[string]bool {
	t.Helper()
	db, err := store.dbHandle()
	if err != nil {
		t.Fatalf("reservationNames: %v", err)
	}
	names := make(map[string]bool)
	if err := db.WithReadTx(context.Background(), func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListUpReservations(context.Background())
		if err != nil {
			return err
		}
		for _, row := range rows {
			names[row.ChildSessionName] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("ListUpReservations: %v", err)
	}
	return names
}

func reservationCount(t *testing.T, store *Store) int {
	t.Helper()
	return len(reservationNames(t, store))
}

func TestStorePutHelperProcess(t *testing.T) {
	if os.Getenv("PLECT_STATE_PUT_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: -- <dir> <index>\n")
		os.Exit(2)
	}
	dir := args[1]
	i, err := strconv.Atoi(args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad index: %v\n", err)
		os.Exit(2)
	}
	now := time.Now()
	name := fmt.Sprintf("owner/repo-process-%d", i)
	if err := NewStore(dir).Put(&domain.Session{
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "put: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Write with one store instance
	store1 := NewStore(dir)
	store1.Put(&domain.Session{
		Name:      "owner/repo-1",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})

	// Read with a new store instance
	store2 := NewStore(dir)
	got := store2.Get("owner/repo-1")
	if got == nil {
		t.Fatal("Session not persisted across store instances")
	}
}

func TestFindByAlias(t *testing.T) {
	store := NewStore(t.TempDir())
	url := "https://github.com/org/repo/issues/9"
	for _, name := range []string{"org/repo-9", "org/repo-9+review"} {
		if err := store.Put(&domain.Session{Name: name, ResourceID: url, Alias: url}); err != nil {
			t.Fatal(err)
		}
	}
	hits := store.FindByAlias(url)
	if len(hits) != 2 {
		t.Fatalf("expected both tag variants, got %d", len(hits))
	}
	if store.FindByAlias("") != nil {
		t.Error("empty alias must never match")
	}
}

// TestStore_UnreadableDatabaseFailsWritesInsteadOfSilentlyInitializing
// carries forward the JSON store's refuse-to-initialize guarantee onto
// SQLite: a file that exists but is not a valid database must fail every
// read and write path rather than being silently treated as a fresh, empty
// store.
func TestStore_UnreadableDatabaseFailsWritesInsteadOfSilentlyInitializing(t *testing.T) {
	dir := t.TempDir()
	garbage := []byte("this is not a sqlite database file, just garbage bytes of substantial length\x00\x01\x02")
	if err := os.WriteFile(filepath.Join(dir, "store.db"), garbage, 0644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)

	if err := store.CheckReadable(); err == nil {
		t.Fatal("CheckReadable() over an unreadable database must fail")
	}
	if err := store.Put(&domain.Session{Name: "org/repo-1"}); err == nil {
		t.Fatal("Put() over an unreadable database must fail, not silently initialize an empty one")
	}
	if err := store.Update("org/repo-1", func(*domain.Session) error { return nil }); err == nil {
		t.Fatal("Update() over an unreadable database must fail")
	}
	if err := store.Delete("org/repo-1"); err == nil {
		t.Fatal("Delete() over an unreadable database must fail")
	}
	if _, err := store.AllE(); err == nil {
		t.Fatal("AllE() over an unreadable database must fail")
	}
	if _, err := store.GetE("org/repo-1"); err == nil {
		t.Fatal("GetE() over an unreadable database must fail")
	}
}

// TestStore_RejectsDatabaseNewerThanBinarySupports carries forward the
// JSON store's newer-version rejection onto SQLite's own applied-migration
// ledger.
func TestStore_RejectsDatabaseNewerThanBinarySupports(t *testing.T) {
	dir := t.TempDir()
	seed := NewStore(dir)
	if err := seed.CheckReadable(); err != nil {
		t.Fatalf("seed CheckReadable: %v", err)
	}
	db, err := seed.dbHandle()
	if err != nil {
		t.Fatalf("seed dbHandle: %v", err)
	}
	if err := db.WithImmediateTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), "INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999999999, 1)")
		return err
	}); err != nil {
		t.Fatalf("plant future version row: %v", err)
	}

	store := NewStore(dir)
	err = store.CheckReadable()
	if err == nil {
		t.Fatal("CheckReadable() over a database newer than this binary supports must fail")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("error = %q, want it to name the newer-than-supported condition", err.Error())
	}
}

package persistence

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

func TestReserveUpSlot_EnforcesCapAcrossSequentialCalls(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const limit = 2

	admit := func(reservations map[string]domain.UpReservation) bool { return len(reservations) < limit }

	for i, child := range []string{"c1", "c2", "c3"} {
		approved, err := db.ReserveUpSlot(ctx, child, "parent1", func(_ map[string]*domain.Session, r map[string]domain.UpReservation) bool {
			return admit(r)
		})
		if err != nil {
			t.Fatalf("ReserveUpSlot(%q): %v", child, err)
		}
		want := i < limit
		if approved != want {
			t.Errorf("ReserveUpSlot(%q) approved = %v, want %v", child, approved, want)
		}
	}
}

func TestReserveUpSlot_RejectsAConcurrentReservationForTheSameChild(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	approved, err := db.ReserveUpSlot(ctx, "childA", "parent1", func(map[string]*domain.Session, map[string]domain.UpReservation) bool { return true })
	if err != nil || !approved {
		t.Fatalf("first ReserveUpSlot: approved=%v err=%v", approved, err)
	}

	_, err = db.ReserveUpSlot(ctx, "childA", "parent1", func(map[string]*domain.Session, map[string]domain.UpReservation) bool { return true })
	if !errors.Is(err, domain.ErrUpAlreadyReserved) {
		t.Fatalf("second ReserveUpSlot error = %v, want %v", err, domain.ErrUpAlreadyReserved)
	}
}

func TestReleaseUpSlot_DropsOnlyTheNamedReservationAndIsIdempotent(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	for _, child := range []string{"childA", "childB"} {
		approved, err := db.ReserveUpSlot(ctx, child, "parent1", func(map[string]*domain.Session, map[string]domain.UpReservation) bool { return true })
		if err != nil || !approved {
			t.Fatalf("ReserveUpSlot(%q): approved=%v err=%v", child, approved, err)
		}
	}

	if err := db.ReleaseUpSlot(ctx, "childA"); err != nil {
		t.Fatalf("ReleaseUpSlot: %v", err)
	}
	if err := db.ReleaseUpSlot(ctx, "childA"); err != nil {
		t.Fatalf("ReleaseUpSlot (second, on an already-released child): %v", err)
	}

	names := reservationNames(t, db)
	if _, ok := names["childA"]; ok {
		t.Error("childA's reservation should be gone")
	}
	if _, ok := names["childB"]; !ok {
		t.Error("childB's reservation should survive releasing childA's")
	}
}

func TestReserveUpSlot_ExcludesReservationsFromDeadProcesses(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	plantReservationForTest(t, db, "crashed-child", "parent1", deadPIDForTest(t), time.Now())

	var seen map[string]domain.UpReservation
	approved, err := db.ReserveUpSlot(ctx, "new-child", "parent1", func(_ map[string]*domain.Session, reservations map[string]domain.UpReservation) bool {
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

func TestReserveUpSlot_NeverExpiresALiveReservationRegardlessOfAge(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	plantReservationForTest(t, db, "long-running-child", "parent1", currentPID(), time.Now().Add(-10000*time.Hour))

	var seen map[string]domain.UpReservation
	approved, err := db.ReserveUpSlot(ctx, "new-child", "parent1", func(_ map[string]*domain.Session, reservations map[string]domain.UpReservation) bool {
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

// A reservation with PID 0 is never live (processAlive's own pid<=0 guard),
// so a fresh ReserveUpSlot for the same child supersedes it rather than
// being rejected as already-reserved — the only way such a row can exist is
// a planted fixture (this test) or pre-PID-field legacy data, since
// ReserveUpSlot itself always stamps a real os.Getpid().
func TestReserveUpSlot_SupersedesAReservationWithNoLivePID(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	plantReservationForTest(t, db, "childA", "parent1", 0, time.Now())

	var sawItself bool
	approved, err := db.ReserveUpSlot(ctx, "childA", "parent1", func(_ map[string]*domain.Session, reservations map[string]domain.UpReservation) bool {
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

func TestDeleteSession_ClearsTheSessionsUpReservation(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	putBareSession(t, db, "childA", "")
	plantReservationForTest(t, db, "childA", "parent1", currentPID(), time.Now())

	if err := db.DeleteSession(ctx, "childA"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	names := reservationNames(t, db)
	if _, ok := names["childA"]; ok {
		t.Error("DeleteSession should have cleared childA's reservation")
	}
}

// plantReservationForTest writes a reservation directly, bypassing
// ReserveUpSlot's own PID/timestamp stamping, so a test can plant a
// specific (possibly dead, possibly backdated) PID.
func plantReservationForTest(t *testing.T, db *DB, child, parent string, pid int, at time.Time) {
	t.Helper()
	q := sqlcgen.New(db.write)
	parentSessionName, virtualRoot := reservationColumnsFromParent(parent)
	if err := q.UpsertUpReservation(context.Background(), sqlcgen.UpsertUpReservationParams{
		ChildSessionName:  child,
		ParentSessionName: parentSessionName,
		VirtualRoot:       virtualRoot,
		Pid:               int64(pid),
		ReservedAt:        formatTime(at),
	}); err != nil {
		t.Fatalf("plantReservationForTest: %v", err)
	}
}

// reservationNames reads every reservation's child session name through the
// same migration-access gate every other read goes through (WithReadTx),
// since persistence.DB exposes no production ListUpReservations of its own
// to bypass it: nothing in production ever needs an unpruned reservation
// list outside ReserveUpSlot's own write transaction.
func reservationNames(t *testing.T, db *DB) map[string]bool {
	t.Helper()
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

func TestReserveUpSlot_VirtualRootParentRoundTripsThroughNullAndBoolean(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if _, err := db.ReserveUpSlot(ctx, "childA", domain.VirtualRootReservationParent, func(_ map[string]*domain.Session, _ map[string]domain.UpReservation) bool {
		return true
	}); err != nil {
		t.Fatalf("ReserveUpSlot: %v", err)
	}

	var parentSessionName sql.NullString
	var virtualRoot bool
	if err := db.write.QueryRowContext(ctx,
		"SELECT parent_session_name, virtual_root FROM up_reservations WHERE child_session_name = 'childA'").
		Scan(&parentSessionName, &virtualRoot); err != nil {
		t.Fatalf("query raw columns: %v", err)
	}
	if parentSessionName.Valid || !virtualRoot {
		t.Fatalf("parent_session_name = %+v, virtual_root = %v, want NULL and true (never the sentinel string in a column)", parentSessionName, virtualRoot)
	}

	var seenParent string
	if _, err := db.ReserveUpSlot(ctx, "childB", "unrelated", func(_ map[string]*domain.Session, reservations map[string]domain.UpReservation) bool {
		seenParent = reservations["childA"].Parent
		return true
	}); err != nil {
		t.Fatalf("ReserveUpSlot: %v", err)
	}
	if seenParent != domain.VirtualRootReservationParent {
		t.Fatalf("reservations[childA].Parent = %q, want %q", seenParent, domain.VirtualRootReservationParent)
	}
}

func currentPID() int { return os.Getpid() }

func deadPIDForTest(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("could not run a helper process to obtain a dead PID: %v", err)
	}
	return cmd.Process.Pid
}

package legacyimport

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
)

// flockExclusiveForTest takes an exclusive, held-for-the-life-of-the-test
// flock on f, simulating a still-running legacy writer.
func flockExclusiveForTest(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// legacyFixture writes a small but representative legacy data directory:
// two state.json sessions (a parent and a child, so parent-link resolution
// is exercised across import order), one session known only through its
// event log (no state.json entry), a population, and an up-slot
// reservation. It returns the source directory and the exact byte offsets
// its own log content produces, so cursor values line up with real line
// boundaries instead of guessed numbers.
func legacyFixture(t *testing.T) (sourceDir string, rootLine1End, rootLine2End int64) {
	t.Helper()
	sourceDir = t.TempDir()

	rootLog := `{"id":"01ROOTEVT0000000000000001","session_name":"root-session","time":"2026-01-01T00:00:00.000000000Z","type":"user.note","source":"cli","direction":"inbound","summary":"first"}` + "\n" +
		`{"id":"01ROOTEVT0000000000000002","session_name":"root-session","time":"2026-01-01T00:00:01.000000000Z","type":"user.note","source":"cli","direction":"outbound","summary":"second"}` + "\n"
	rootLine1End = int64(len(rootLog[:indexAfterFirstNewline(rootLog)]))
	rootLine2End = int64(len(rootLog))

	rootDir := writeSessionDir(t, filepath.Join(sourceDir, "events"), "root-session", map[string]string{
		"log.jsonl":            rootLog,
		".gen":                 "01ROOTGEN0000000000000000",
		".cursor.dispatcher":   strconv.FormatInt(rootLine1End, 10),
		".cursor.tick-reactor": "0",
	})
	_ = rootDir

	writeSessionDir(t, filepath.Join(sourceDir, "events"), "child-session", map[string]string{
		"log.jsonl": `{"id":"01CHILDEVT000000000000001","session_name":"child-session","time":"2026-01-02T00:00:00.000000000Z","type":"user.note","source":"cli","direction":"internal","summary":"child event"}` + "\n",
	})

	writeSessionDir(t, filepath.Join(sourceDir, "events"), "orphan-events-only", map[string]string{
		"log.jsonl": `{"id":"01ORPHANEVT00000000000001","session_name":"orphan-events-only","time":"2026-01-03T00:00:00.000000000Z","type":"user.note","source":"cli","direction":"internal","summary":"orphan"}` + "\n",
	})

	stateJSON := `{
  "version": 7,
  "sessions": {
    "root-session": {
      "session_name": "root-session",
      "workflow": "wf1",
      "workspace_dir_path": "/tmp/root",
      "created_at": "2026-01-01T00:00:00Z",
      "updated_at": "2026-01-01T00:00:00Z",
      "tick_backoff": {"last_fingerprint": "fp1", "last_log_position": ` + strconv.FormatInt(rootLine2End, 10) + `}
    },
    "child-session": {
      "session_name": "child-session",
      "parent_session": "root-session",
      "workflow": "wf1",
      "created_at": "2026-01-02T00:00:00Z",
      "updated_at": "2026-01-02T00:00:00Z"
    }
  },
  "populations": {
    "wf1/pop": {
      "workflow": "wf1",
      "name": "pop",
      "members": {
        "res-1": {"resource_id": "res-1", "generation": 1}
      }
    }
  },
  "up_reservations": {
    "reserved-child": {"parent": "root-session", "pid": ` + strconv.Itoa(os.Getpid()) + `}
  }
}`
	if err := os.WriteFile(filepath.Join(sourceDir, "state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
	return sourceDir, rootLine1End, rootLine2End
}

func indexAfterFirstNewline(s string) int {
	for i, c := range s {
		if c == '\n' {
			return i + 1
		}
	}
	return len(s)
}

func TestRun_ImportsAFullLegacyDirectoryAndRoundTrips(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	destDir := t.TempDir()
	ctx := context.Background()

	report, err := Run(ctx, Options{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("Run: %v (report: %+v)", err, report)
	}
	if !report.Promoted {
		t.Fatal("report.Promoted = false, want true")
	}
	if report.Sessions != 3 {
		t.Errorf("report.Sessions = %d, want 3", report.Sessions)
	}
	if report.SessionsFromEventLogOnly != 1 {
		t.Errorf("report.SessionsFromEventLogOnly = %d, want 1", report.SessionsFromEventLogOnly)
	}
	if report.Events != 4 {
		t.Errorf("report.Events = %d, want 4", report.Events)
	}
	if report.Cursors != 3 { // delivery + tick + heartbeat, all on root-session
		t.Errorf("report.Cursors = %d, want 3", report.Cursors)
	}
	if report.Populations != 1 || report.PopulationMembers != 1 {
		t.Errorf("Populations/PopulationMembers = %d/%d, want 1/1", report.Populations, report.PopulationMembers)
	}
	if report.UpReservations != 1 {
		t.Errorf("report.UpReservations = %d, want 1", report.UpReservations)
	}

	if _, err := os.Stat(report.DBPath); err != nil {
		t.Fatalf("stat promoted database: %v", err)
	}
	destEntries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir(destDir): %v", err)
	}
	var destNames []string
	for _, e := range destEntries {
		destNames = append(destNames, e.Name())
	}
	if len(destNames) != 2 {
		t.Errorf("destDir contents = %v, want exactly storage.db and state.json (no orphaned temp-database gate sidecars)", destNames)
	}
	markerData, err := os.ReadFile(report.MarkerPath)
	if err != nil {
		t.Fatalf("read rejection marker: %v", err)
	}
	if string(markerData) != rejectionMarker {
		t.Errorf("marker = %q, want %q", markerData, rejectionMarker)
	}

	db, err := persistence.EnsureCurrent(ctx, report.DBPath)
	if err != nil {
		t.Fatalf("open promoted database: %v", err)
	}
	defer db.Close()

	root, err := db.GetSession(ctx, "root-session")
	if err != nil || root == nil {
		t.Fatalf("GetSession(root-session) = %v, %v", root, err)
	}
	if root.ID != "01ROOTGEN0000000000000000" {
		t.Errorf("root.ID = %q, want the preserved .gen id", root.ID)
	}
	if root.ParentSession != "" {
		t.Errorf("root.ParentSession = %q, want empty", root.ParentSession)
	}
	if root.Workflow != "wf1" {
		t.Errorf("root.Workflow = %q, want wf1", root.Workflow)
	}

	child, err := db.GetSession(ctx, "child-session")
	if err != nil || child == nil {
		t.Fatalf("GetSession(child-session) = %v, %v", child, err)
	}
	if child.ParentSession != "root-session" {
		t.Errorf("child.ParentSession = %q, want root-session", child.ParentSession)
	}
	if child.ID == "" || child.ID == "01ROOTGEN0000000000000000" {
		t.Errorf("child.ID = %q, want a freshly minted id distinct from root's", child.ID)
	}

	orphan, err := db.GetSession(ctx, "orphan-events-only")
	if err != nil || orphan == nil {
		t.Fatalf("GetSession(orphan-events-only) = %v, %v", orphan, err)
	}

	rootEvents, _, err := db.ListEventsFrom(ctx, "root-session", 0)
	if err != nil {
		t.Fatalf("ListEventsFrom(root-session): %v", err)
	}
	if len(rootEvents) != 2 || rootEvents[0].ID != "01ROOTEVT0000000000000001" || rootEvents[1].ID != "01ROOTEVT0000000000000002" {
		t.Fatalf("rootEvents = %+v, want the two source events in order", rootEvents)
	}

	deliveryCursor, err := db.EventCursor(ctx, "root-session", "delivery")
	if err != nil {
		t.Fatalf("EventCursor(delivery): %v", err)
	}
	if deliveryCursor != 2 {
		t.Errorf("delivery cursor = %d, want 2 (past event 1, next is event 2)", deliveryCursor)
	}
	tickCursor, err := db.EventCursor(ctx, "root-session", "tick")
	if err != nil {
		t.Fatalf("EventCursor(tick): %v", err)
	}
	if tickCursor != 1 {
		t.Errorf("tick cursor = %d, want 1 (offset 0 = nothing consumed yet)", tickCursor)
	}
	heartbeatCursor, err := db.EventCursor(ctx, "root-session", "heartbeat")
	if err != nil {
		t.Fatalf("EventCursor(heartbeat): %v", err)
	}
	if heartbeatCursor != 3 {
		t.Errorf("heartbeat cursor = %d, want 3 (past both events)", heartbeatCursor)
	}

	pop, err := db.Population(ctx, "wf1/pop")
	if err != nil || pop == nil {
		t.Fatalf("Population(wf1/pop) = %v, %v", pop, err)
	}
	if pop.Members["res-1"] == nil {
		t.Errorf("pop.Members = %+v, want res-1 present", pop.Members)
	}

	var sawReservation bool
	if _, err := db.ReserveUpSlot(ctx, "probe", "root-session", func(_ map[string]*domain.Session, reservations map[string]domain.UpReservation) bool {
		_, sawReservation = reservations["reserved-child"]
		return false
	}); err != nil {
		t.Fatalf("ReserveUpSlot probe: %v", err)
	}
	if !sawReservation {
		t.Error("imported up-slot reservation not found")
	}
}

func TestRun_DryRunValidatesWithoutPromoting(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	destDir := t.TempDir()
	ctx := context.Background()

	report, err := Run(ctx, Options{SourceDir: sourceDir, DestDir: destDir, DryRun: true})
	if err != nil {
		t.Fatalf("Run (dry-run): %v", err)
	}
	if report.Promoted {
		t.Error("report.Promoted = true, want false for a dry run")
	}
	if _, err := os.Stat(report.DBPath); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want not-exist (dry run must not promote)", report.DBPath, err)
	}
	if _, err := os.Stat(report.MarkerPath); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want not-exist (dry run must not write the marker)", report.MarkerPath, err)
	}
	leftover, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir(destDir): %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("destDir contents = %v, want empty (a dry run must not leave its temporary database or gate lock files behind)", leftover)
	}
}

func TestRun_RefusesWhenDestinationAlreadyHasADatabase(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	destDir := t.TempDir()
	ctx := context.Background()

	if err := os.WriteFile(persistence.PathIn(destDir), []byte("not a real database"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Run(ctx, Options{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("Run must refuse when the destination already has a storage.db")
	}
}

func TestRun_MarkerWriteFailureLeavesNoStorageDBBehind(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	destDir := t.TempDir()
	ctx := context.Background()

	markerPath := filepath.Join(destDir, "state.json")
	if err := os.MkdirAll(markerPath, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Run(ctx, Options{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("Run must fail when the marker path cannot be written")
	}
	if report.Promoted {
		t.Error("report.Promoted = true, want false")
	}
	if _, statErr := os.Stat(report.DBPath); !os.IsNotExist(statErr) {
		t.Errorf("stat %s = %v, want not-exist: a marker-write failure must never leave a promoted database behind", report.DBPath, statErr)
	}

	if err := os.RemoveAll(markerPath); err != nil {
		t.Fatal(err)
	}
	retry, err := Run(ctx, Options{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("retry after fixing the marker path: %v", err)
	}
	if !retry.Promoted {
		t.Error("retry: report.Promoted = false, want true")
	}
}

func TestRun_RejectsUnknownFilesInAnEventSessionDirectory(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	if err := os.WriteFile(filepath.Join(sourceDir, "events", encodeSessionDir("root-session"), "mystery.dat"), []byte("?"), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()

	report, err := Run(context.Background(), Options{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("Run must fail over an unrecognized file in a session directory")
	}
	if len(report.UnknownFiles) != 1 {
		t.Errorf("report.UnknownFiles = %v, want the one unexpected path", report.UnknownFiles)
	}
	if report.Promoted {
		t.Error("report.Promoted = true, want false")
	}
}

func TestRun_RefusesWhenALegacyLockIsStillHeld(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	destDir := t.TempDir()

	lockPath := filepath.Join(sourceDir, "state.json.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := flockExclusiveForTest(f); err != nil {
		t.Fatal(err)
	}

	_, err = Run(context.Background(), Options{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("Run must refuse while state.json.lock is held")
	}
}

func TestRun_RejectsAMisalignedCursorOffset(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	if err := os.WriteFile(filepath.Join(sourceDir, "events", encodeSessionDir("root-session"), ".cursor.dispatcher"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()

	_, err := Run(context.Background(), Options{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("Run must reject a dispatcher cursor offset that lands mid-line")
	}
}

func TestRun_MissingStateJSONFails(t *testing.T) {
	sourceDir := t.TempDir()
	destDir := t.TempDir()

	_, err := Run(context.Background(), Options{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("Run over a source directory with no state.json must fail")
	}
}

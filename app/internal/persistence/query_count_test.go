package persistence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	sqlite3 "github.com/mattn/go-sqlite3"

	contract "github.com/kecbigmt/plecture/contracts/state"
)

// countingConn tallies every QueryContext/ExecContext call SQLite's driver
// makes on the connection it wraps into queryCount, so a test can assert
// how many SQL round trips one Go-level call actually issues.
type countingConn struct {
	*sqlite3.SQLiteConn
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	atomic.AddInt64(&queryCount, 1)
	return c.SQLiteConn.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	atomic.AddInt64(&queryCount, 1)
	return c.SQLiteConn.ExecContext(ctx, query, args)
}

type countingDriver struct {
	inner sqlite3.SQLiteDriver
}

func (d *countingDriver) Open(dsn string) (driver.Conn, error) {
	conn, err := d.inner.Open(dsn)
	if err != nil {
		return nil, err
	}
	sc, ok := conn.(*sqlite3.SQLiteConn)
	if !ok {
		return conn, nil
	}
	return &countingConn{SQLiteConn: sc}, nil
}

const countingDriverName = "sqlite3-persistence-query-count-test"

// queryCount is process-global because database/sql.Register is: this
// package's tests never run this driver concurrently with itself, so one
// counter reset per measurement (resetQueryCount) is race-free.
var queryCount int64

func init() {
	sql.Register(countingDriverName, &countingDriver{})
}

func resetQueryCount() {
	atomic.StoreInt64(&queryCount, 0)
}

func readQueryCount() int64 {
	return atomic.LoadInt64(&queryCount)
}

// openCountingTestDB opens and migrates a database exactly like
// migratedTestDB, then reopens its read/write pools against countingDriver
// against the same file, so queryCount subsequently tallies every SQL call
// issued through it.
func openCountingTestDB(t *testing.T) *DB {
	t.Helper()
	path := PathIn(t.TempDir())
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	oldRead, oldWrite := db.read, db.write
	t.Cleanup(func() {
		oldRead.Close()
		oldWrite.Close()
	})

	readDSN := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=%d&_foreign_keys=on", path, busyTimeoutMillis)
	writeDSN := readDSN + "&_txlock=immediate"
	countingRead, err := sql.Open(countingDriverName, readDSN)
	if err != nil {
		t.Fatalf("open counting read handle: %v", err)
	}
	countingWrite, err := sql.Open(countingDriverName, writeDSN)
	if err != nil {
		t.Fatalf("open counting write handle: %v", err)
	}
	countingWrite.SetMaxOpenConns(1)
	t.Cleanup(func() {
		countingRead.Close()
		countingWrite.Close()
	})

	db.read = countingRead
	db.write = countingWrite
	return db
}

// TestAllSessions_QueryCountDoesNotScaleWithSessionCount is a committed
// regression guard for AllSessions' N+1: it fails at the revision before
// batching landed, where AllSessions issued loadSessionExtras' queries once
// per row, so query count grew linearly with the number of bare (no
// parent/children/tasks/channel health) sessions rather than staying fixed.
func TestAllSessions_QueryCountDoesNotScaleWithSessionCount(t *testing.T) {
	measure := func(t *testing.T, bareCount int) int64 {
		db := openCountingTestDB(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := db.PutSession(ctx, &domain.Session{Name: "root", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("PutSession(root): %v", err)
		}
		child := &domain.Session{
			Name: "child", ParentSession: "root", CreatedAt: now, UpdatedAt: now,
			ChannelValidationHealth: &contract.ChannelHealth{ConsecutiveFailures: 1, FirstFailureAt: now, LastFailureAt: now},
			ChannelDeliveryHealth:   &contract.ChannelHealth{ConsecutiveFailures: 1, FirstFailureAt: now, LastFailureAt: now},
			Nodes: map[string]*contract.TaskState{
				"setup": {
					Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "setup-def",
					Layers: []contract.LayerState{{EffectID: "outer", Status: contract.TaskStatusProduced}},
				},
			},
			Tasks: map[string]*contract.TaskState{
				"impl": {
					Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "impl-def",
					Layers: []contract.LayerState{{EffectID: "layer1", Status: contract.TaskStatusProduced}},
					DoneWhen: &contract.DoneWhenState{
						HeartbeatTicks: 1, LastFingerprint: "fp", LastUnsatisfied: []string{"leaf-a"},
						Judges: map[string]*contract.DoneWhenJudge{
							"leaf-a": {LeafID: "leaf-a", Action: "approve", Reason: "ok", Revision: "sha1", JudgeSession: "reviewer1", Relation: "sibling", CreatedAt: now},
						},
					},
				},
			},
		}
		if err := db.PutSession(ctx, child); err != nil {
			t.Fatalf("PutSession(child): %v", err)
		}
		for i := 0; i < bareCount; i++ {
			name := fmt.Sprintf("bare-%d", i)
			if err := db.PutSession(ctx, &domain.Session{Name: name, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("PutSession(%q): %v", name, err)
			}
		}

		resetQueryCount()
		all, err := db.AllSessions(ctx)
		if err != nil {
			t.Fatalf("AllSessions: %v", err)
		}
		if want := bareCount + 2; len(all) != want {
			t.Fatalf("AllSessions returned %d sessions, want %d", len(all), want)
		}
		return readQueryCount()
	}

	const smallBare, largeBare = 5, 200
	small := measure(t, smallBare)
	large := measure(t, largeBare)
	if small != large {
		t.Errorf("AllSessions issued %d SQL calls at %d sessions but %d SQL calls at %d sessions; want equal",
			small, smallBare+2, large, largeBare+2)
	}
}

package reactor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// newE2EConfig writes a task document with a real done_when (not the tests
// elsewhere in this package, which never load one) so evaluateSessionActions
// has a leaf to actually evaluate — TickSession is exercised through the real
// `internal/service` code, not a stub.
func newE2EConfig(t *testing.T) *config.Config {
	t.Helper()
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "fixture.toml"), e2eObserver)
	writeFile(t, filepath.Join(base, "tasks", "work.toml"), `[work]
kind              = "task"
description       = "work fixture"
resource_observer = "fixture"
instructions      = [{ text = "Carry out the work." }]

[work.done_when]
all = [
  { check = "resource.state.checks_status", eq = "SUCCESS" },
]
`)
	return &config.Config{BaseDir: base, WorkspaceDirsRoot: t.TempDir()}
}

// e2eObserver has no real resource to look at, so observing one fails and the
// last observation a test seeded stands.
const e2eObserver = `
[fixture]
kind  = "resource_observer"
match = '^fixture://'

[fixture.observe]
type   = "shell"
script = "exit 1"

[fixture.state_schema]
type = "object"

[fixture.state_schema.properties]
checks_status = {}
`

// TestSessionReactor_ReactiveTickReachesDoneWhenConsequence is the AC1 E2E:
// a declared-pattern event delivered to a session with a generated,
// done_when-bearing task instance reaches the done_when consequence (here,
// `satisfied` → a `plect.terminal.done` pushed to the parent) with no manual
// `plect tick` call — the reactor alone drives it, exactly mirroring "PR turns
// CI green → the review chain fires unattended".
func TestSessionReactor_ReactiveTickReachesDoneWhenConsequence(t *testing.T) {
	cfg := newE2EConfig(t)
	st := state.NewStore(t.TempDir())
	// service.TickSession's terminal push resolves its own eventlog store as
	// eventlog.NewStore(store.Dir()) — this must be the same directory the
	// reactor reads/writes through, or its output silently lands elsewhere.
	log := eventlog.NewStore(st.Dir())
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)

	if err := st.Put(&domain.Session{Name: "o/parent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.NewStream("o/parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := log.NewStream("o/r-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(&domain.Session{
		Name:          "o/r-1",
		ParentSession: "o/parent",
		Tasks: map[string]*contract.TaskState{
			// A run-scope task keeps the reactor's drain loop active (mirrors a
			// real session's tmux/claude node); the session-scope "initial"
			// instance is what carries done_when.
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
			"initial": {
				Scope:    contract.TaskScopeSession,
				TaskID:   "work",
				Status:   contract.TaskStatusProduced,
				Observed: &contract.ResourceObservation{State: map[string]any{"checks_status": "SUCCESS"}, At: time.Now()},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	r := &sessionReactor{
		session: "o/r-1",
		cfg:     cfg,
		state:   st,
		log:     log,
		hub:     hub,
		tick:    config.TickConfig{On: []string{"resource.*"}},
	}
	stop := startReactor(t, r)
	defer stop()

	log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Direction: event.Internal})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		evs, _, _, err := log.List("o/parent", 0, event.Filter{Types: []string{event.TypeTerminalDone}})
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 1 {
			return // reached the done_when consequence with no manual tick
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("declared event never drove the reactor to push plect.terminal.done to the parent")
}

// newPendingJudgeConfig writes a task whose done_when carries an unrecorded
// judge leaf — the state a tick turns into review_required and, until a
// verdict arrives, keeps turning into review_required.
func newPendingJudgeConfig(t *testing.T) *config.Config {
	t.Helper()
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "fixture.toml"), e2eObserver)
	writeFile(t, filepath.Join(base, "tasks", "work.toml"), `[work]
kind              = "task"
description       = "work fixture"
resource_observer = "fixture"
instructions      = [{ text = "Carry out the work." }]

[work.done_when]
all = [
  { judge = "AC met", id = "ac-met" },
]
`)
	return &config.Config{BaseDir: base, WorkspaceDirsRoot: t.TempDir()}
}

// TestSessionReactor_UnchangedUnmetStateAnnouncesOnce: a burst of external
// events re-evaluates the same unmet done_when many times over, and the
// reactor announces it once. Without the debounce this is the
// production loop — one review_required per drain, at the bus's cadence
// rather than the session's.
func TestSessionReactor_UnchangedUnmetStateAnnouncesOnce(t *testing.T) {
	cfg := newPendingJudgeConfig(t)
	st := state.NewStore(t.TempDir())
	log := eventlog.NewStore(st.Dir())
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)

	if err := st.Put(&domain.Session{
		Name: "o/r-1",
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
			"initial": {
				Scope:    contract.TaskScopeSession,
				TaskID:   "work",
				Status:   contract.TaskStatusProduced,
				Resource: "https://github.com/o/r/pull/1",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.NewStream("o/r-1"); err != nil {
		t.Fatal(err)
	}

	r := &sessionReactor{
		session: "o/r-1",
		cfg:     cfg,
		state:   st,
		log:     log,
		hub:     hub,
		tick:    config.TickConfig{On: []string{"resource.*"}},
	}
	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()
	for range 6 {
		log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Source: event.SourceCLI, Direction: event.Internal})
		time.Sleep(20 * time.Millisecond)
	}
	waitLastTickAt(t, st, "o/r-1", floor) // the reactor did evaluate the state
	time.Sleep(100 * time.Millisecond)    // ...and any late drain has landed

	evs, _, _, err := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeTickReviewRequired}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("review_required events = %d, want 1 for one unchanged unmet state", len(evs))
	}
}

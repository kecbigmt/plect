package reactor

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

const workflowHeader = "\n[wf]\nkind = \"workflow\"\n[[wf.nodes]]\nid = \"claude\"\nuses = \"claude\"\n"

const heartbeatWorkflow = workflowHeader + `
[wf.tick]
heartbeat = "1ms"
`

const declaresOnOnly = workflowHeader + "\n[wf.tick]\non = [\"resource.*\"]\n"

// TestSessionReactor_ReArmsHeartbeatAfterWorkflowLoadRecovers reproduces the
// production wedge: while the config home was inconsistent, every workflow
// load failed, so the reactor started with no `[tick]` at all — and since
// nothing rebuilds a reactor whose session never goes down, heartbeat
// scheduling stayed off long after the config was restored, until the daemon
// process was restarted.
func TestSessionReactor_ReArmsHeartbeatAfterWorkflowLoadRecovers(t *testing.T) {
	base := t.TempDir()
	writeClaudeRunTask(t, base)
	workflowPath := filepath.Join(base, "workflows", "wf.toml")
	writeFile(t, workflowPath, "this is not valid toml")
	cfg := &config.Config{BaseDir: base, WorkspaceDirsRoot: t.TempDir()}

	dir := t.TempDir()
	log := eventlog.NewStore(dir)
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)
	// state.Store and eventlog.Store share one directory (and so one
	// database): a session Put below must be visible to log's own reads,
	// since a session row now is one incarnation.
	st := state.NewStore(dir)
	session := &domain.Session{
		Name:     "o/r-1",
		Workflow: "wf",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}
	if err := st.Put(session); err != nil {
		t.Fatal(err)
	}

	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)
	r := sup.buildReactor("o/r-1", session)
	r.heartbeatEvery = 5 * time.Millisecond

	var mu sync.Mutex
	ticks := 0
	r.tickFn = func(*config.Config, *state.Store, service.TickParams) (*service.CheckResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return &service.CheckResult{}, nil
	}
	countTicks := func() int {
		mu.Lock()
		defer mu.Unlock()
		return ticks
	}

	stop := startReactor(t, r)
	defer stop()

	time.Sleep(80 * time.Millisecond) // several sweep cycles' worth
	if got := countTicks(); got != 0 {
		t.Fatalf("ticks = %d while the workflow was unloadable, want 0", got)
	}

	writeFile(t, workflowPath, heartbeatWorkflow)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countTicks() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("heartbeat never re-armed after the workflow became loadable again; the reactor stayed wedged on its startup resolution")
}

// A workflow that stops loading mid-life must not take heartbeat scheduling
// down with it: the last good declaration stands, matching the fail-safe the
// daemon's own periodic config refresh applies.
func TestSessionReactor_KeepsLastGoodTickConfigWhenWorkflowLoadFails(t *testing.T) {
	base := t.TempDir()
	writeClaudeRunTask(t, base)
	workflowPath := filepath.Join(base, "workflows", "wf.toml")
	writeFile(t, workflowPath, heartbeatWorkflow)
	cfg := &config.Config{BaseDir: base, WorkspaceDirsRoot: t.TempDir()}

	dir := t.TempDir()
	log := eventlog.NewStore(dir)
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)
	st := state.NewStore(dir)
	session := &domain.Session{
		Name:     "o/r-1",
		Workflow: "wf",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}
	if err := st.Put(session); err != nil {
		t.Fatal(err)
	}

	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)
	r := sup.buildReactor("o/r-1", session)
	r.heartbeatEvery = 5 * time.Millisecond

	var mu sync.Mutex
	ticks := 0
	r.tickFn = func(*config.Config, *state.Store, service.TickParams) (*service.CheckResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return &service.CheckResult{}, nil
	}
	countTicks := func() int {
		mu.Lock()
		defer mu.Unlock()
		return ticks
	}

	stop := startReactor(t, r)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for countTicks() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat never fired with a loadable workflow")
		}
		time.Sleep(5 * time.Millisecond)
	}

	writeFile(t, workflowPath, "this is not valid toml")
	before := countTicks()
	deadline = time.Now().Add(2 * time.Second)
	for countTicks() <= before {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat stopped when the workflow became unloadable; the last good declaration must stand")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newRefreshFixture builds a reactor over a workflow file the caller can
// rewrite, wired the way production wires it (through the supervisor), and
// returns the path so a case can drive refreshTickConfig across byte states.
func newRefreshFixture(t *testing.T, body string) (*sessionReactor, string) {
	t.Helper()
	base := t.TempDir()
	workflowPath := filepath.Join(base, "workflows", "wf.toml")
	writeFile(t, workflowPath, body)
	cfg := &config.Config{BaseDir: base, WorkspaceDirsRoot: t.TempDir()}
	dir := t.TempDir()
	log := eventlog.NewStore(dir)
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)
	st := state.NewStore(dir)
	session := &domain.Session{
		Name:     "o/r-1",
		Workflow: "wf",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}
	if err := st.Put(session); err != nil {
		t.Fatal(err)
	}
	return NewSupervisor(func() *config.Config { return cfg }, st, log, hub).buildReactor("o/r-1", session), workflowPath
}

// A workflow file being rewritten in place is readable at byte states that
// parse cleanly and declare nothing, so a re-read that would leave the
// session with no clock and no pattern is not evidence that its declaration
// was removed — adopting one re-enters the wedge the re-read exists to leave.
// A re-read that still schedules something is adopted, torn or not, since it
// cannot strand the session.
func TestRefreshTickConfig_KeepsAndAdopts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		start     string
		rewritten string
		remove    bool
		want      time.Duration
	}{
		{name: "zero-length mid-truncate", start: heartbeatWorkflow, rewritten: "", want: time.Millisecond},
		{name: "table written, keys not yet", start: heartbeatWorkflow, rewritten: workflowHeader + "\n[wf.tick]\n", want: time.Millisecond},
		{name: "unparseable", start: heartbeatWorkflow, rewritten: "this is not valid toml", want: time.Millisecond},
		{name: "workflow file gone", start: heartbeatWorkflow, remove: true, want: time.Millisecond},
		{name: "declaration changed", start: heartbeatWorkflow, rewritten: workflowHeader + "\n[wf.tick]\nheartbeat = \"7ms\"\n", want: 7 * time.Millisecond},
		{name: "narrowed but still scheduling", start: heartbeatWorkflow, rewritten: declaresOnOnly, want: 0},
		{name: "nothing to keep, so adopted", start: declaresOnOnly, rewritten: "", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, path := newRefreshFixture(t, tc.start)
			if tc.remove {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, path, tc.rewritten)
			}
			r.refreshTickConfig()
			if got := r.tick.Heartbeat.Duration; got != tc.want {
				t.Fatalf("heartbeat after re-read = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rule is stated over what the declaration schedules, not over how the
// re-read failed, so a session left with a pattern but no clock keeps that
// pattern.
func TestRefreshTickConfig_KeepsPatternOnlyDeclaration(t *testing.T) {
	r, path := newRefreshFixture(t, declaresOnOnly)
	writeFile(t, path, "")
	r.refreshTickConfig()
	if len(r.tick.On) != 1 || r.tick.On[0] != "resource.*" {
		t.Fatalf("on after a re-read declaring nothing = %+v, want the previous pattern kept", r.tick.On)
	}
}

package reactor

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestSupervisor_CheckDeadmanSweepsOnlyHeartbeatScheduledUpSessions proves
// checkDeadman's own filter: only a session that is both up (run scope
// produced) and has declared `[tick].heartbeat` gets swept, mirroring what
// the reactor's own per-session heartbeat sweep gates on
// (reactor.go:checkHeartbeat).
func TestSupervisor_CheckDeadmanSweepsOnlyHeartbeatScheduledUpSessions(t *testing.T) {
	pluginDir := t.TempDir()
	writeClaudeRunTask(t, filepath.Join(pluginDir, "config"))
	writeFile(t, filepath.Join(pluginDir, "config", "workflows", "goal.toml"), `
[goal]
kind = "workflow"
[[goal.nodes]]
id   = "claude"
uses = "claude"
[goal.tick]
heartbeat = "1h"
`)
	writeFile(t, filepath.Join(pluginDir, "config", "workflows", "reactive.toml"), `
[reactive]
kind = "workflow"
[[reactive.nodes]]
id   = "claude"
uses = "claude"
[reactive.tick]
on = ["resource.*"]
`)
	cfg := &config.Config{PluginDirs: []string{pluginDir}}
	st := state.NewStore(t.TempDir())
	for _, s := range []*domain.Session{
		{
			Name: "o/heartbeat-up", Workflow: "goal",
			Tasks: map[string]*contract.TaskState{"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced}},
		},
		{
			// Declares [tick].on but no heartbeat: nothing to be a deadman for.
			Name: "o/reactive-up", Workflow: "reactive",
			Tasks: map[string]*contract.TaskState{"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced}},
		},
		{
			// Heartbeat declared, but run scope is down.
			Name: "o/heartbeat-down", Workflow: "goal",
			Tasks: map[string]*contract.TaskState{"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusCleaned}},
		},
	} {
		if err := st.Put(s); err != nil {
			t.Fatal(err)
		}
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)

	var mu sync.Mutex
	var calls []string
	sup.deadmanFn = func(_ *config.Config, _ *state.Store, name string, _ config.TickConfig, _ time.Time) (bool, error) {
		mu.Lock()
		calls = append(calls, name)
		mu.Unlock()
		return false, nil
	}

	sup.checkDeadman(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "o/heartbeat-up" {
		t.Fatalf("swept sessions = %+v, want exactly [o/heartbeat-up]", calls)
	}
}

// TestSupervisor_DeadmanSweepRunsWithoutWaitingOnPerSessionReactor proves the
// deadman sweep fires on its own cadence, from Supervisor.Run's own loop,
// even while the same up session's per-session reactor (started by that same
// Supervisor.Run) has not itself completed a single heartbeat sweep of its
// own — reactor.go's default heartbeatInterval is a full minute, far longer
// than this test's window. This is the structural guarantee the issue's
// "must live outside the reactor loop it is watching" requires: a stalled
// per-session reactor goroutine cannot also block Supervisor.Run's own
// ticker loop, because it is a different goroutine entirely.
func TestSupervisor_DeadmanSweepRunsWithoutWaitingOnPerSessionReactor(t *testing.T) {
	pluginDir := t.TempDir()
	writeClaudeRunTask(t, filepath.Join(pluginDir, "config"))
	writeFile(t, filepath.Join(pluginDir, "config", "workflows", "goal.toml"), `
[goal]
kind = "workflow"
[[goal.nodes]]
id   = "claude"
uses = "claude"
[goal.tick]
heartbeat = "1h"
`)
	cfg := &config.Config{PluginDirs: []string{pluginDir}}
	st := state.NewStore(t.TempDir())
	if err := st.Put(&domain.Session{
		Name: "o/r-1", Workflow: "goal",
		Tasks: map[string]*contract.TaskState{"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced}},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)
	sup.poll = 5 * time.Millisecond
	sup.deadmanPoll = 5 * time.Millisecond

	var mu sync.Mutex
	var calls int
	sup.deadmanFn = func(_ *config.Config, _ *state.Store, name string, _ config.TickConfig, _ time.Time) (bool, error) {
		if name != "o/r-1" {
			t.Errorf("unexpected session swept: %q", name)
		}
		mu.Lock()
		calls++
		mu.Unlock()
		return false, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("deadman sweep called %d times in 2s at a 5ms poll, want at least 2", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

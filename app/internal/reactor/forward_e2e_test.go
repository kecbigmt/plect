package reactor

import (
	"context"
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

// waitForEventCount polls until session's log holds at least n events of typ,
// or fails the test — the resource-forwarding counterpart of waitLastTickAt.
func waitForEventCount(t *testing.T, log *eventlog.Store, session, typ string, n int) []event.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs, _, _, err := log.List(session, 0, event.Filter{Types: []string{typ}})
		if err != nil {
			t.Fatalf("list %s events on %s: %v", typ, session, err)
		}
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s events on %s = %d in 2s, want at least %d", typ, session, len(evs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertEventCountStays sleeps briefly and asserts session's log still holds
// exactly n events of typ — used to prove no further forward happened.
func assertEventCountStays(t *testing.T, log *eventlog.Store, session, typ string, n int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	evs, _, _, err := log.List(session, 0, event.Filter{Types: []string{typ}})
	if err != nil {
		t.Fatalf("list %s events on %s: %v", typ, session, err)
	}
	if len(evs) != n {
		t.Fatalf("%s events on %s = %d, want still exactly %d", typ, session, len(evs), n)
	}
}

// TestSupervisor_ForwardsDownChildResourceEventsToLiveAncestorUntilUpOrDestroyed
// runs the real Supervisor (reactor + forwarder) and
// service.ForwardDownSessionEvent, no injected fakes.
func TestSupervisor_ForwardsDownChildResourceEventsToLiveAncestorUntilUpOrDestroyed(t *testing.T) {
	pluginDir := t.TempDir()
	// A neutrally-named run-scoped effect, not reactor_test.go's
	// writeClaudeRunTask helper (scripts/check-provider-boundary.sh).
	writeFile(t, filepath.Join(pluginDir, "config", "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "run"
`)
	writeFile(t, filepath.Join(pluginDir, "config", "workflows", "reactive.toml"), `
[reactive]
kind = "workflow"
[[reactive.nodes]]
id   = "runner"
uses = "runner"
[reactive.tick]
on = ["resource.*"]
`)
	cfg := &config.Config{PluginDirs: []string{pluginDir}}
	st := state.NewStore(t.TempDir())
	// Parent: up throughout, so it both passes resolveLiveAncestor's health
	// check and has its own sessionReactor running to tick off a forward.
	if err := st.Put(&domain.Session{
		Name: "parent", Workflow: "reactive",
		Nodes: map[string]*contract.TaskState{"runner": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced}},
	}); err != nil {
		t.Fatal(err)
	}
	// Child: down (no produced run-scoped node) but not destroyed.
	if err := st.Put(&domain.Session{
		Name: "child", Workflow: "reactive", ParentSession: "parent", ResourceID: "https://example.test/resource/1",
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(st.Dir())
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)
	sup.poll = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	// Cursors must seed before appending anything below, or the append reads
	// as pre-existing history and never surfaces.
	waitReactorStarted(t, sup, "parent")
	waitForwarderStarted(t, sup, "child")

	// An inbound resource event lands on the down child while the parent is up.
	parentFloor := time.Now()
	log.Append(event.Event{
		SessionName: "child", ID: "ev-1", Type: "resource.updated", Direction: event.Inbound,
		Summary: "a reviewer requested changes", Metadata: map[string]string{"url": "https://example.test/resource/1/review/9"},
	})

	waitLastTickAt(t, st, "parent", parentFloor) // the parent's reactor ticked off the forward

	forwarded := waitForEventCount(t, log, "parent", event.TypeResourceForwarded, 1)
	if forwarded[0].Metadata[event.MetaOriginSession] != "child" {
		t.Fatalf("origin_session metadata = %q, want child", forwarded[0].Metadata[event.MetaOriginSession])
	}
	if forwarded[0].Metadata["resource"] != "https://example.test/resource/1" {
		t.Fatalf("resource metadata = %q, want the child's ResourceID", forwarded[0].Metadata["resource"])
	}
	original, _, _, err := log.List("child", 0, event.Filter{Types: []string{"resource.updated"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 {
		t.Fatalf("original events on the down child = %+v, want exactly one preserved", original)
	}

	// Destroy the child; a further event on it must not forward.
	if err := st.Destroy("child"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the supervisor's reconcile notice and cancel the forwarder
	log.Append(event.Event{SessionName: "child", ID: "ev-2", Type: "resource.updated", Direction: event.Inbound})
	assertEventCountStays(t, log, "parent", event.TypeResourceForwarded, 1)

	// A second down-but-not-destroyed child is brought back up instead of
	// destroyed. It starts with no run-scoped node at all, not a cleaned one
	// flipped back to produced: persistence refuses reviving a cleaned
	// execution, so a real down->up cycle mints a fresh one instead.
	if err := st.Put(&domain.Session{
		Name: "child2", Workflow: "reactive", ParentSession: "parent",
	}); err != nil {
		t.Fatal(err)
	}
	waitForwarderStarted(t, sup, "child2")

	if err := st.Update("child2", func(s *domain.Session) error {
		if s.Nodes == nil {
			s.Nodes = map[string]*contract.TaskState{}
		}
		s.Nodes["runner"] = &contract.TaskState{Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitReactorStarted(t, sup, "child2")

	childFloor := time.Now()
	log.Append(event.Event{SessionName: "child2", ID: "ev-3", Type: "resource.updated", Direction: event.Inbound})
	waitLastTickAt(t, st, "child2", childFloor)
	assertEventCountStays(t, log, "parent", event.TypeResourceForwarded, 1)
}

func waitForwarderStarted(t *testing.T, sup *Supervisor, session string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !sup.log.HasCursor(session, forwardConsumer) {
		if time.Now().After(deadline) {
			t.Fatalf("forwarder never started for %q", session)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitReactorStarted(t *testing.T, sup *Supervisor, session string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !sup.log.HasCursor(session, reactorConsumer) {
		if time.Now().After(deadline) {
			t.Fatalf("reactor never started for %q", session)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

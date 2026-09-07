package dispatch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/channel"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	protocol "github.com/kecbigmt/plecture/contracts/channel-protocol"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// socketChannel and execChannel build the definitions these tests drive the
// dispatcher with, in the ratified value forms.
func socketChannel() config.ChannelDefinition {
	return config.ChannelDefinition{
		Type: config.ChannelTypeUnixSocket,
		Path: &lang.Value{Form: lang.FormFrom, From: "inputs.path"},
		Body: &lang.Value{Form: lang.FormJSON, JSON: &lang.JSONOperand{Leaf: &lang.Value{Form: lang.FormFrom, From: "event"}}},
	}
}

func execChannel(command string, args ...*lang.Value) config.ChannelDefinition {
	return config.ChannelDefinition{
		Type:   config.ChannelTypeExec,
		Action: &lang.Action{Type: lang.ActionExec, Command: command, Args: args},
	}
}

func literalArg(v string) *lang.Value { return &lang.Value{Form: lang.FormLiteral, Literal: v} }

// runTestDispatcher builds a wake-driven dispatcher over a fast-poll hub for an
// up session whose runtime channel targets sock.
func runTestDispatcher(t *testing.T, log *eventlog.Store, sock string) (*sessionDispatcher, *state.Store, *sessionhub.Registry) {
	t.Helper()
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)
	// state.Store and log must share one directory (and so one database):
	// dispatch itself reads log's own session row.
	st := state.NewStore(log.Dir())
	if err := st.Put(&domain.Session{
		Name: "o/r-1",
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": sock}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	d := &sessionDispatcher{
		session:  "o/r-1",
		cfg:      &config.Config{},
		channels: []config.EventChannel{{Name: "runtime", Uses: "claude_channel", Inputs: map[string]*lang.Value{"path": fromValue("nodes.claude.outputs.socket_path")}, Include: []string{"plect.instruction"}}},
		defs:     map[string]config.ChannelDefinition{"claude_channel": socketChannel()},
		log:      log,
		state:    st,
		hub:      hub,
		policy:   channel.RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Timeout: 200 * time.Millisecond},
	}
	return d, st, hub
}

// TestDispatcher_RunDeliversOnWake runs a full dispatcher driven by the shared
// reader's wake (no per-dispatcher poll timer) and asserts an appended
// instruction is delivered well before the 5s fallback would fire.
func TestDispatcher_RunDeliversOnWake(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, _, _ := runTestDispatcher(t, log, sock)
	startDispatcher(t, d)

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "go", Direction: event.Internal})
	if typ := recvType(t, recv); typ != event.TypeInstruction {
		t.Errorf("delivered type = %q", typ)
	}
}

// TestDispatcher_RunDeliversBurstViaCoalescedWakes proves a burst of appends
// (whose wakes coalesce to a cap-1 signal) is fully delivered — one wake can
// drive a drain that reads the whole burst.
func TestDispatcher_RunDeliversBurstViaCoalescedWakes(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, _, _ := runTestDispatcher(t, log, sock)
	startDispatcher(t, d)

	const n = 20
	for range n {
		log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "go", Direction: event.Internal})
	}
	for range n {
		if typ := recvType(t, recv); typ != event.TypeInstruction {
			t.Fatalf("delivered type = %q", typ)
		}
	}
}

// TestDispatcher_RunReplaysAfterRestart drives the down→up replay through run():
// an event appended while the dispatcher is stopped is delivered when a fresh
// dispatcher restarts (from the durable cursor), not lost.
func TestDispatcher_RunReplaysAfterRestart(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d1, st, hub := runTestDispatcher(t, log, sock)

	stop1 := startDispatcher(t, d1)
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "first", Direction: event.Internal})
	recvType(t, recv) // delivered + cursor committed
	stop1()

	// Appended "while down": no dispatcher running.
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "while-down", Direction: event.Internal})

	d2 := &sessionDispatcher{session: d1.session, cfg: d1.cfg, channels: d1.channels, defs: d1.defs, log: log, state: st, hub: hub, policy: d1.policy}
	startDispatcher(t, d2)
	if typ := recvType(t, recv); typ != event.TypeInstruction { // replayed from the durable cursor
		t.Errorf("post-restart delivery type = %q", typ)
	}
}

// startDispatcher starts d.run and blocks until its cursor is seeded, so
// callers need no fixed sleep despite SQLite's variable first-touch cost.
func startDispatcher(t *testing.T, d *sessionDispatcher) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("dispatcher did not stop")
			}
		})
	}
	t.Cleanup(stop)

	deadline := time.Now().Add(2 * time.Second)
	for !d.log.HasCursor(d.session, dispatcherConsumer) {
		if time.Now().After(deadline) {
			t.Fatal("dispatcher never seeded its cursor")
		}
		time.Sleep(time.Millisecond)
	}
	return stop
}

// startFakeSocket listens on a unix socket and pushes each framed channel-server
// message it receives onto the returned channel — a stand-in for a runtime's
// channel-server.
func startFakeSocket(t *testing.T) (string, <-chan protocol.MessagePayload) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	recv := make(chan protocol.MessagePayload, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				header := make([]byte, 4)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				data := make([]byte, binary.BigEndian.Uint32(header))
				if _, err := io.ReadFull(conn, data); err != nil {
					return
				}
				var env protocol.Envelope
				if json.Unmarshal(data, &env) != nil {
					return
				}
				var pl protocol.MessagePayload
				if env.UnmarshalPayload(&pl) == nil {
					recv <- pl
				}
			}(conn)
		}
	}()
	return path, recv
}

func runtimeDispatcher(t *testing.T, session string, log *eventlog.Store, socketPath string, include ...string) (*sessionDispatcher, *domain.Session) {
	t.Helper()
	s := &domain.Session{
		Name: session,
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": socketPath}},
		},
	}
	// Callers rebuild a dispatcher over the same log/session to simulate a
	// restart (TestDispatcher_ReplaysFromCursorAcrossRestart), so only Put
	// the first time — a second Put would still update the same live row
	// (it does not mint a new incarnation), but skipping it when one
	// already exists keeps this helper's intent explicit.
	st := state.NewStore(log.Dir())
	if existing, err := st.GetE(session); err != nil {
		t.Fatal(err)
	} else if existing == nil {
		if err := st.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	d := &sessionDispatcher{
		session: session,
		channels: []config.EventChannel{{
			Name:    "runtime",
			Uses:    "claude_channel",
			Inputs:  map[string]*lang.Value{"path": fromValue("nodes.claude.outputs.socket_path")},
			Include: include,
		}},
		defs: map[string]config.ChannelDefinition{
			"claude_channel": socketChannel(),
		},
		log:    log,
		state:  st,
		policy: channel.RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Timeout: 200 * time.Millisecond},
	}
	return d, s
}

func drainOnce(d *sessionDispatcher, s *domain.Session) {
	gen, _ := d.log.StreamID(d.session)
	d.drain(context.Background(), s, &gen)
}

func recvType(t *testing.T, recv <-chan protocol.MessagePayload) string {
	t.Helper()
	select {
	case pl := <-recv:
		var ev map[string]any
		if err := json.Unmarshal([]byte(pl.Text), &ev); err != nil {
			t.Fatalf("payload is not event json: %v", err)
		}
		typ, _ := ev["type"].(string)
		return typ
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery received")
		return ""
	}
}

func assertNoDelivery(t *testing.T, recv <-chan protocol.MessagePayload) {
	t.Helper()
	select {
	case pl := <-recv:
		t.Fatalf("unexpected delivery: %q", pl.Text)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDispatcher_DeliversIncludedEvents(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction", "github.*")

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "do it", Direction: event.Internal})
	log.Append(event.Event{SessionName: "o/r-1", Type: "github.ci_status", Summary: "CI failed", Direction: event.Internal})
	drainOnce(d, s)

	got := map[string]bool{recvType(t, recv): true}
	got[recvType(t, recv)] = true
	if !got[event.TypeInstruction] || !got["github.ci_status"] {
		t.Errorf("delivered types = %v, want both instruction and github", got)
	}
	cur, _ := log.ReadCursor("o/r-1", dispatcherConsumer)
	if cur == 0 {
		t.Error("cursor did not advance")
	}
}

func TestDispatcher_IncludeFilters(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeUserNote, Body: "ignored", Direction: event.Internal})
	drainOnce(d, s)

	assertNoDelivery(t, recv) // user.note is not in include
	if cur, _ := log.ReadCursor("o/r-1", dispatcherConsumer); cur == 0 {
		t.Error("cursor should advance past an unmatched event")
	}
}

// TestDispatcher_DeliversNodeResult pins the literal plect.node.result type
// string against the general include mechanism TestDispatcher_DeliversIncludedEvents
// already covers with an arbitrary type — a typo in either string would
// otherwise silently drop every node-result delivery.
func TestDispatcher_DeliversNodeResult(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, event.TypeNodeResult)

	log.Append(event.Event{
		SessionName: "o/r-1",
		Type:        event.TypeNodeResult,
		Summary:     "agent setup produced",
		Metadata:    map[string]string{"node": "agent", "effect": "official.example.agent", "scope": "run", "action": event.NodeResultActionSetup, "result": event.NodeResultProduced}, Direction: event.Internal,
	})
	drainOnce(d, s)

	if typ := recvType(t, recv); typ != event.TypeNodeResult {
		t.Errorf("delivered type = %q", typ)
	}
}

// TestDispatcher_NodeResultUnresolvedBindingFailsSafely covers a channel
// bound to a node that has not produced yet (e.g. a thread node a
// population member's channel projects but has not set up): channelInputs
// cannot resolve the binding, so delivery must fail through the ordinary
// plect.channel.error path rather than panicking, and the node-result event
// itself is never touched — it is immutable, appended, and already past.
func TestDispatcher_NodeResultUnresolvedBindingFailsSafely(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	s := &domain.Session{Name: "o/r-1", Tasks: map[string]*contract.TaskState{}}
	st := state.NewStore(t.TempDir())
	if err := st.Put(s); err != nil {
		t.Fatal(err)
	}
	d := &sessionDispatcher{
		session: "o/r-1",
		channels: []config.EventChannel{{
			Name:    "runtime",
			Uses:    "claude_channel",
			Inputs:  map[string]*lang.Value{"path": fromValue("nodes.claude.outputs.socket_path")},
			Include: []string{event.TypeNodeResult},
		}},
		defs:   map[string]config.ChannelDefinition{"claude_channel": socketChannel()},
		log:    log,
		state:  st,
		policy: channel.RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Timeout: 200 * time.Millisecond},
	}

	orig, _, _, _ := log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeNodeResult, Summary: "agent setup produced", Direction: event.Internal})
	drainOnce(d, s)

	errs, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if len(errs) != 1 {
		t.Fatalf("want exactly one plect.channel.error, got %d: %+v", len(errs), errs)
	}
	if errs[0].Metadata["channel"] != "runtime" || errs[0].Metadata["event_id"] != orig.ID {
		t.Errorf("channel.error metadata = %+v", errs[0].Metadata)
	}
	nodeResults, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeNodeResult}})
	if len(nodeResults) != 1 || nodeResults[0].ID != orig.ID {
		t.Fatalf("node.result events = %+v, want the original event untouched", nodeResults)
	}
}

func TestDispatcher_FinalFailureAppendsChannelError(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	dead := filepath.Join(t.TempDir(), "absent.sock") // never listened
	d, s := runtimeDispatcher(t, "o/r-1", log, dead, "plect.instruction")

	orig, _, _, _ := log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "do it", Direction: event.Internal})
	drainOnce(d, s)

	errs, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if len(errs) != 1 {
		t.Fatalf("want exactly one plect.channel.error, got %d", len(errs))
	}
	ce := errs[0]
	if ce.Metadata["channel"] != "runtime" || ce.Metadata["event_id"] != orig.ID || ce.Metadata["attempts"] != "2" {
		t.Errorf("channel.error metadata = %+v", ce.Metadata)
	}
	ch := d.state.Get("o/r-1").ChannelDeliveryHealth
	if ch == nil || ch.ConsecutiveFailures != 1 || ch.LastChannel != "runtime" || ch.FirstFailureAt.IsZero() {
		t.Errorf("channel delivery health = %+v, want a one-failure delivery streak", ch)
	}
}

// TestDispatcher_SuccessfulDeliveryClearsChannelHealth pins the recovery half
// of the failure-streak bookkeeping: once a delivery succeeds, an open streak
// is cleared so a later failure starts a fresh episode instead of continuing
// an already-escalated one.
func TestDispatcher_SuccessfulDeliveryClearsChannelHealth(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")
	if err := d.state.Update("o/r-1", func(session *domain.Session) error {
		session.ChannelDeliveryHealth = &contract.ChannelHealth{ConsecutiveFailures: 2, FirstFailureAt: time.Now(), LastFailureAt: time.Now(), LastChannel: "runtime"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s)
	recvType(t, recv) // delivered

	if ch := d.state.Get("o/r-1").ChannelDeliveryHealth; ch != nil && ch.ConsecutiveFailures != 0 {
		t.Errorf("channel delivery health = %+v, want the streak cleared after a successful delivery", ch)
	}
}

// TestDispatcher_SuccessfulDeliveryDoesNotClearValidationFailureStreak is a
// regression test: validation (checked once, at a dispatcher's build) and
// delivery (checked per event) are independent streaks — Session.
// ChannelValidationHealth vs ChannelDeliveryHealth — precisely so a
// delivery success can never clear a still-open validation failure on a
// declared channel that never even reaches delivery (an unresolvable `uses`
// is skipped before the fan-out loop, so it would otherwise be invisible to
// every event's outcome).
func TestDispatcher_SuccessfulDeliveryDoesNotClearValidationFailureStreak(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")
	seededAt := time.Now()
	if err := d.state.Update("o/r-1", func(session *domain.Session) error {
		session.ChannelValidationHealth = &contract.ChannelHealth{ConsecutiveFailures: 1, FirstFailureAt: seededAt, LastFailureAt: seededAt, LastError: `uses unknown channel definition "alerts_channel"`}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s)
	recvType(t, recv) // delivered

	ch := d.state.Get("o/r-1").ChannelValidationHealth
	if ch == nil || ch.ConsecutiveFailures != 1 || !ch.FirstFailureAt.Equal(seededAt) {
		t.Errorf("channel validation health = %+v, want the validation-failure streak left untouched by an unrelated channel's successful delivery", ch)
	}
}

func TestDispatcher_ChannelErrorNotRedelivered(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	dead := filepath.Join(t.TempDir(), "absent.sock")
	d, s := runtimeDispatcher(t, "o/r-1", log, dead, "plect.instruction", "github.*")

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s) // fails → appends one channel.error
	drainOnce(d, s) // reads the channel.error; must not match any include → no second error

	errs, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if len(errs) != 1 {
		t.Fatalf("channel.error should not loop: got %d", len(errs))
	}
}

func TestDispatcher_MultiChannelFanOut(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	dead := filepath.Join(t.TempDir(), "absent.sock")
	def := socketChannel()
	s := &domain.Session{Name: "o/r-1", Tasks: map[string]*contract.TaskState{}}
	st := state.NewStore(t.TempDir())
	if err := st.Put(s); err != nil {
		t.Fatal(err)
	}
	d := &sessionDispatcher{
		session: "o/r-1",
		channels: []config.EventChannel{
			{Name: "live", Uses: "sock", Inputs: map[string]*lang.Value{"path": literalValue(sock)}, Include: []string{"plect.instruction"}},
			{Name: "dead", Uses: "sock", Inputs: map[string]*lang.Value{"path": literalValue(dead)}, Include: []string{"plect.instruction"}},
		},
		defs:   map[string]config.ChannelDefinition{"sock": def},
		log:    log,
		state:  st,
		policy: channel.RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Timeout: 200 * time.Millisecond},
	}
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s)

	recvType(t, recv) // live channel delivered
	errs, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if len(errs) != 1 || errs[0].Metadata["channel"] != "dead" {
		t.Fatalf("want one channel.error for the dead channel, got %+v", errs)
	}
	if cur, _ := log.ReadCursor("o/r-1", dispatcherConsumer); cur == 0 {
		t.Error("cursor should advance after both workers are terminal")
	}
	if ch := d.state.Get("o/r-1").ChannelDeliveryHealth; ch == nil || ch.ConsecutiveFailures != 1 || ch.LastChannel != "dead" {
		t.Errorf("channel health = %+v, want one recorded failure for the dead channel", ch)
	}

	// A second event must extend the same streak, not have the live channel's
	// concurrent success reset it: the two channels' outcomes are aggregated
	// per event, not raced against each other per channel.
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s)
	recvType(t, recv)
	if ch := d.state.Get("o/r-1").ChannelDeliveryHealth; ch == nil || ch.ConsecutiveFailures != 2 {
		t.Errorf("channel health = %+v, want the streak to survive the live channel's sibling success", ch)
	}
}

func TestDispatcher_CancelMidEventLeavesCursorForReplay(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	d := &sessionDispatcher{
		session:  "o/r-1",
		channels: []config.EventChannel{{Name: "slow", Uses: "slow", Include: []string{"plect.instruction"}}},
		defs:     map[string]config.ChannelDefinition{"slow": execChannel("sleep", literalArg("5"))},
		log:      log,
		policy:   channel.RetryPolicy{MaxAttempts: 1, Timeout: 10 * time.Second},
	}
	s := &domain.Session{Name: "o/r-1", Tasks: map[string]*contract.TaskState{}}
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})

	ctx, cancel := context.WithCancel(context.Background())
	gen, _ := log.StreamID("o/r-1")
	done := make(chan struct{})
	go func() { d.drain(ctx, s, &gen); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not return after cancel")
	}

	// Cancellation is a shutdown, not a delivery failure: no channel.error, and
	// the cursor stays put so the event replays on the next start.
	errs, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if len(errs) != 0 {
		t.Errorf("cancellation must not append a channel.error, got %d", len(errs))
	}
	if cur, _ := log.ReadCursor("o/r-1", dispatcherConsumer); cur != 0 {
		t.Errorf("cursor advanced despite mid-event cancel: %d", cur)
	}
}

func TestDispatcher_ChannelErrorNotDeliveredUnderWildcard(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	dead := filepath.Join(t.TempDir(), "absent.sock")
	d, s := runtimeDispatcher(t, "o/r-1", log, dead, "*") // include everything

	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s) // fails → one channel.error
	drainOnce(d, s) // reads the channel.error; the structural guard skips it though "*" matches

	errs, _, _, _ := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if len(errs) != 1 {
		t.Fatalf("channel.error must not loop under include=*: got %d", len(errs))
	}
}

func TestDispatcher_SeedsCursorToTailOnFirstStart(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")

	// History predating the dispatcher must not be re-flooded to the runtime.
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "old", Direction: event.Internal})
	SeedCursor(d.log, d.session)
	drainOnce(d, s)
	assertNoDelivery(t, recv)

	// Events after the seed are delivered.
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "new", Direction: event.Internal})
	drainOnce(d, s)
	if typ := recvType(t, recv); typ != event.TypeInstruction {
		t.Errorf("post-seed delivery type = %q", typ)
	}
}

func TestSeedCursor_AtBirthDeliversFirstInstruction(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	d, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")

	// Seed at the session's empty birth tail (as service.Create does), before the
	// initial instruction exists. The dispatcher then starts after the run scope
	// comes up — its own first-start seed must no-op (cursor already exists) so the
	// instruction appended in between is still delivered.
	SeedCursor(d.log, d.session)
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "initial", Direction: event.Internal})
	SeedCursor(d.log, d.session) // dispatcher first start: idempotent, keeps birth cursor
	drainOnce(d, s)
	if typ := recvType(t, recv); typ != event.TypeInstruction {
		t.Errorf("initial instruction not delivered: type = %q", typ)
	}
}

func TestDispatcher_ReplaysFromCursorAcrossRestart(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)

	d1, s := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "first", Direction: event.Internal})
	drainOnce(d1, s)
	recvType(t, recv) // delivered once

	// A fresh dispatcher over the same log must not re-deliver the committed event.
	d2, _ := runtimeDispatcher(t, "o/r-1", log, sock, "plect.instruction")
	drainOnce(d2, s)
	assertNoDelivery(t, recv)

	// An event appended "while down" is delivered after the restart (durable cursor).
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "second", Direction: event.Internal})
	drainOnce(d2, s)
	if typ := recvType(t, recv); typ != event.TypeInstruction {
		t.Errorf("post-restart delivery type = %q", typ)
	}
}

// TestDispatcher_TerminalHelperResolvesThroughSessionPlan exercises
// {{terminal "..."}} end to end through the real dispatcher delivery path:
// a channel's exec args render against the [terminal]-declaring task's
// current outputs, without the channel itself knowing anything about the
// concrete multiplexer behind it.
func TestDispatcher_TerminalHelperResolvesThroughSessionPlan(t *testing.T) {
	log := eventlog.NewStore(t.TempDir())
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out")

	s := &domain.Session{
		Name: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"tmux": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"session_name": "mysession"}},
		},
	}
	st := state.NewStore(t.TempDir())
	if err := st.Put(s); err != nil {
		t.Fatal(err)
	}
	def := config.ChannelDefinition{
		Type: config.ChannelTypeShell,
		Action: &lang.Action{
			Type:   lang.ActionShell,
			Script: `echo "$send" > "$out"` + "\n",
			Bind: map[string]*lang.Value{
				"send": {Form: lang.FormTerminal, Terminal: "send_text"},
				"out":  literalArg(outFile),
			},
		},
	}
	d := &sessionDispatcher{
		session:  "o/r-1",
		channels: []config.EventChannel{{Name: "runtime", Uses: "send_keys", Include: []string{"plect.instruction"}}},
		defs:     map[string]config.ChannelDefinition{"send_keys": def},
		log:      log,
		state:    st,
		policy:   channel.RetryPolicy{MaxAttempts: 1, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Timeout: 200 * time.Millisecond},
		terminal: &terminalOwner{
			NodeID: "tmux",
			Ops: &config.TerminalConfig{SendText: &lang.Action{
				Type:    lang.ActionExec,
				Command: "tmux",
				Args: []*lang.Value{
					literalArg("send-keys"),
					literalArg("-t"),
					{Form: lang.FormFrom, From: "self.outputs.session_name"},
					literalArg("--"),
				},
			}},
		},
	}
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Direction: event.Internal})
	drainOnce(d, s)

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	want := "'tmux' 'send-keys' '-t' 'mysession' '--' \"$@\"\n"
	if string(got) != want {
		t.Errorf("output = %q, want %q", string(got), want)
	}
}

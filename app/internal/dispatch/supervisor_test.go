package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeRunTask declares the run-scoped effect a "claude" node uses,
// under root/tasks — RunScopeUp resolves a produced "claude" task-state
// entry against this declaration to decide the node is current-plan
// run-scoped.
func writeClaudeRunTask(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "tasks", "claude.toml"), `
[claude]
kind  = "effect"
scope = "run"
`)
}

func TestSupervisor_StartsAndStopsWithRunScope(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(globalDir, "channels", "claude_channel.toml"), `
[claude_channel]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[claude_channel.input_schema]
path = { type = "string", required = true }
`)
	writeClaudeRunTask(t, globalDir)
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
id   = "claude"
uses = "claude"
[[coding.event.channel]]
name        = "runtime"
uses        = "claude_channel"
inputs.path = { from = "nodes.claude.outputs.socket_path" }
include     = ["plect.instruction"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	stateStore := state.NewStore(t.TempDir())
	sock, _ := startFakeSocket(t)
	if err := stateStore.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding", WorkspaceDirPath: t.TempDir(),
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": sock}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, stateStore, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("dispatcher not started for an up session")
	}
	sup.reconcile(ctx, active, skip, &wg) // idempotent: no duplicate
	if len(active) != 1 {
		t.Fatalf("expected exactly one dispatcher, got %d", len(active))
	}

	// Run scope goes down → supervisor cancels (suspend, not teardown).
	if err := stateStore.Update("o/r-1", func(s *domain.Session) error {
		s.Tasks["claude"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sup.reconcile(ctx, active, skip, &wg)
	if len(active) != 0 {
		t.Fatalf("dispatcher not stopped after run scope down: %d active", len(active))
	}
}

// TestSupervisor_ReconcileDoesNotCancelActiveDispatchersWhenStoreUnreadable
// mirrors reactor.Supervisor's identical fix: reconcile's second loop
// cancels any active dispatcher whose session is absent from AllE's
// result, so a swallowed read error returning an empty map would
// previously cancel every dispatcher currently running.
func TestSupervisor_ReconcileDoesNotCancelActiveDispatchersWhenStoreUnreadable(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(globalDir, "channels", "claude_channel.toml"), `
[claude_channel]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[claude_channel.input_schema]
path = { type = "string", required = true }
`)
	writeFile(t, filepath.Join(globalDir, "tasks", "claude.toml"), `
[claude]
kind  = "effect"
scope = "run"
`)
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
id   = "claude"
uses = "claude"
[[coding.event.channel]]
name        = "runtime"
uses        = "claude_channel"
inputs.path = { from = "nodes.claude.outputs.socket_path" }
include     = ["plect.instruction"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	stateStore := state.NewStore(t.TempDir())
	sock, _ := startFakeSocket(t)
	if err := stateStore.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding", WorkspaceDirPath: t.TempDir(),
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": sock}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, stateStore, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("dispatcher not started for an up session")
	}

	// Swap in a store whose database file is unreadable, simulating the
	// store becoming unreadable mid-flight.
	brokenDir := t.TempDir()
	writeFile(t, filepath.Join(brokenDir, "store.db"), "not a database")
	sup.state = state.NewStore(brokenDir)

	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("reconcile cancelled an active dispatcher when the store became unreadable, treating an error as \"destroyed\"")
	}
}

// TestSupervisor_ValidationFailureRecordsChannelErrorAndStreak pins the
// validation half of the channel-failure fix: a channel that fails to
// validate (not just one that fails to deliver) must also become
// machine-visible — a plect.channel.error event on the session's own log,
// plus a persisted failure streak for the reactor's escalation sweep
// (service.CheckChannelHealth) to act on — not just the WARN log line
// nobody watches.
func TestSupervisor_ValidationFailureRecordsChannelErrorAndStreak(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeClaudeRunTask(t, globalDir)
	// No channels/claude_channel.toml: `uses` below resolves to nothing.
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
id   = "claude"
uses = "claude"
[[coding.event.channel]]
name        = "runtime"
uses        = "claude_channel"
inputs.path = { from = "nodes.claude.outputs.socket_path" }
include     = ["plect.instruction"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	stateStore := state.NewStore(t.TempDir())
	if err := stateStore.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding", WorkspaceDirPath: t.TempDir(),
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, stateStore, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("a validation failure must not stop the dispatcher from starting (other channels may still be valid)")
	}

	errs, _, _, err := log.List("o/r-1", 0, event.Filter{Types: []string{event.TypeChannelError}})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 {
		t.Fatalf("want exactly one plect.channel.error for the validation failure, got %d", len(errs))
	}
	if errs[0].Metadata["workflow"] != "coding" {
		t.Errorf("channel.error metadata = %+v, want workflow=coding", errs[0].Metadata)
	}

	ch := stateStore.Get("o/r-1").ChannelValidationHealth
	if ch == nil || ch.ConsecutiveFailures != 1 || ch.FirstFailureAt.IsZero() {
		t.Errorf("channel validation health = %+v, want a one-failure validation streak", ch)
	}
}

// TestSupervisor_ValidationRecoveryOnNextUpClearsStreak covers the episode's
// other end: once the workflow is fixed, the next up-transition's successful
// validation clears the failure streak so a later break starts a fresh
// episode instead of continuing an already-escalated one.
func TestSupervisor_ValidationRecoveryOnNextUpClearsStreak(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeClaudeRunTask(t, globalDir)
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
id   = "claude"
uses = "claude"
[[coding.event.channel]]
name        = "runtime"
uses        = "claude_channel"
inputs.path = { from = "nodes.claude.outputs.socket_path" }
include     = ["plect.instruction"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	stateStore := state.NewStore(t.TempDir())
	if err := stateStore.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding", WorkspaceDirPath: t.TempDir(),
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, stateStore, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, skip, &wg)
	if ch := stateStore.Get("o/r-1").ChannelValidationHealth; ch == nil || ch.ConsecutiveFailures == 0 {
		t.Fatalf("channel validation health = %+v, want a failure recorded before the fix", ch)
	}

	// Run scope goes down (supervisor cancels the dispatcher, matching
	// TestSupervisor_StartsAndStopsWithRunScope), the workflow gets fixed,
	// then run scope comes back up: buildDispatcher re-validates on this
	// fresh up-transition.
	if err := stateStore.Update("o/r-1", func(s *domain.Session) error {
		s.Tasks["claude"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sup.reconcile(ctx, active, skip, &wg)
	if len(active) != 0 {
		t.Fatalf("dispatcher not stopped after run scope down: %d active", len(active))
	}

	writeFile(t, filepath.Join(globalDir, "channels", "claude_channel.toml"), `
[claude_channel]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[claude_channel.input_schema]
path = { type = "string", required = true }
`)
	if err := stateStore.Update("o/r-1", func(s *domain.Session) error {
		s.Tasks["claude"].Status = contract.TaskStatusProduced
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("dispatcher not restarted for the fresh up transition")
	}

	if ch := stateStore.Get("o/r-1").ChannelValidationHealth; ch != nil && ch.ConsecutiveFailures != 0 {
		t.Errorf("channel validation health = %+v, want the streak cleared by the next successful validation", ch)
	}
}

// TestSupervisor_ValidationSuccessDoesNotClearDeliveryFailureStreak is the
// supervisor-side half of the same regression: a session whose channel is
// currently failing to deliver (e.g. its runtime socket isn't listening) but
// whose declaration validates fine must not have that delivery-failure
// streak wiped out just because the next up-transition's (unrelated)
// validation check passes — validation success says nothing about whether
// delivery, which is checked on its own per-event schedule, has recovered.
func TestSupervisor_ValidationSuccessDoesNotClearDeliveryFailureStreak(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(globalDir, "channels", "claude_channel.toml"), `
[claude_channel]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[claude_channel.input_schema]
path = { type = "string", required = true }
`)
	writeClaudeRunTask(t, globalDir)
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
id   = "claude"
uses = "claude"
[[coding.event.channel]]
name        = "runtime"
uses        = "claude_channel"
inputs.path = { from = "nodes.claude.outputs.socket_path" }
include     = ["plect.instruction"]
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	stateStore := state.NewStore(t.TempDir())
	sock, _ := startFakeSocket(t)
	if err := stateStore.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding", WorkspaceDirPath: t.TempDir(),
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": sock}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	seededAt := time.Now()
	if err := stateStore.Update("o/r-1", func(s *domain.Session) error {
		s.ChannelDeliveryHealth = &contract.ChannelHealth{ConsecutiveFailures: 2, FirstFailureAt: seededAt, LastFailureAt: seededAt, LastChannel: "runtime"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, stateStore, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("dispatcher not started for an up session")
	}

	ch := stateStore.Get("o/r-1").ChannelDeliveryHealth
	if ch == nil || ch.ConsecutiveFailures != 2 || !ch.FirstFailureAt.Equal(seededAt) {
		t.Errorf("channel delivery health = %+v, want the delivery-failure streak left untouched by an unrelated successful validation", ch)
	}
}

// TestSupervisor_DeliversChannelDefinedOnlyInAPluginMountedAfterConstruction
// reproduces the resident-daemon outage class: a channel definition that exists
// only in a plugin layer (no global copy) must validate and deliver once the
// supervisor's config getter reflects the plugin, even though the getter
// returned a Config without that plugin when the Supervisor was built —
// matching a long-running daemon whose config.Live refresh mounts a plugin
// after `plect serve` started, not just after a restart.
func TestSupervisor_DeliversChannelDefinedOnlyInAPluginMountedAfterConstruction(t *testing.T) {
	pluginDir := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "channels", "claude.toml"), `
[claude]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[claude.input_schema]
path = { type = "string", required = true }
`)
	// The effect is declared under a different id than the "claude" channel
	// above — same bare id across kinds is a load error (PLECTURE-CFG-ID-DUPLICATE).
	writeFile(t, filepath.Join(pluginDir, "config", "tasks", "claude_runtime.toml"), `
[claude_runtime]
kind  = "effect"
scope = "run"
`)
	writeFile(t, filepath.Join(pluginDir, "config", "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
id   = "claude"
uses = "claude_runtime"
[[coding.event.channel]]
name        = "runtime"
uses        = "claude"
inputs.path = { from = "nodes.claude.outputs.socket_path" }
include     = ["plect.instruction"]
`)

	// currentCfg starts without the plugin mounted, as if the daemon started
	// before this plugin was enabled; the getter closure below always reads
	// its current value, so reassigning it simulates config.Live swapping in
	// a freshly-resolved Config on its next periodic refresh.
	currentCfg := &config.Config{}

	stateStore := state.NewStore(t.TempDir())
	sock, recv := startFakeSocket(t)
	if err := stateStore.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding", WorkspaceDirPath: t.TempDir(),
		Tasks: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"socket_path": sock}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	if _, err := log.NewStream("o/r-1"); err != nil {
		t.Fatal(err)
	}
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return currentCfg }, stateStore, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	// The plugin mounts (config refresh happens) before this session's
	// dispatcher is built.
	currentCfg = &config.Config{PluginDirs: []string{pluginDir}}

	sup.reconcile(ctx, active, skip, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("dispatcher not started for an up session")
	}
	// Wait for the dispatcher to seed its cursor (its first store touch,
	// which now migrates a fresh SQLite database) before the append below,
	// rather than a fixed sleep sized for the old file-backed store.
	deadline := time.After(2 * time.Second)
	for !log.HasCursor("o/r-1", dispatcherConsumer) {
		select {
		case <-deadline:
			t.Fatal("dispatcher never seeded its cursor")
		case <-time.After(time.Millisecond):
		}
	}

	if _, _, _, err := log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeInstruction, Body: "go", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}
	if typ := recvType(t, recv); typ != event.TypeInstruction {
		t.Errorf("delivered type = %q, want %q (plugin-only channel definition did not resolve)", typ, event.TypeInstruction)
	}
}

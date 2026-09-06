//go:build integration

package reactor

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventbus"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// repoRootForE2E locates the repository root from this file's own path, so
// the test does not depend on the working directory it runs from.
func repoRootForE2E(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// goToolCachesForE2E holds the module/build cache locations resolved before
// any test in this file redirects HOME — computed once at package init, not
// per-test, since a build issued after HOME points at a throwaway temp dir
// would otherwise resolve GOMODCACHE/GOCACHE relative to that fake HOME too
// and populate a throwaway module cache (and, worse, leave read-only cache
// files under a t.TempDir() that its own cleanup cannot remove).
var goToolCachesForE2E = resolveGoToolCachesForE2E()

func resolveGoToolCachesForE2E() []string {
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		return nil
	}
	lines := splitLines(string(out))
	if len(lines) != 2 {
		return nil
	}
	return []string{"GOMODCACHE=" + lines[0], "GOCACHE=" + lines[1]}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// This file drives the one real, shipped provider plugin capable of proving
// task-instance delivery wiring end to end (a resident watcher daemon
// publishing over a real event bus) — there is no generic-provider way to
// pin that. Its identifiers below are a genuine, necessary exception to
// core's own provider-neutrality, using check-provider-boundary.sh's own
// "boundary-allow" escape hatch, the same way the pre-existing
// app/internal/service/provider_github_e2e_test.go already does.
const (
	pluginDirName      = "github"           // boundary-allow: real plugin directory name
	worktreeBin        = "github-worktree"  // boundary-allow: real plugin binary name
	watcherBin         = "github-watcher"   // boundary-allow: real plugin binary name
	appTokenBin        = "gh-app-token"     // boundary-allow: real plugin binary name
	mountedID          = "official/github"  // boundary-allow: real catalog mount address
	eventSource        = "github"           // boundary-allow: real event.Source the watcher publishes
	mergeableEventType = "github.mergeable" // boundary-allow: real event type the watcher publishes
	ciStatusEventType  = "github.ci_status" // boundary-allow: real event type the watcher publishes
)

// buildGithubPluginBinaries compiles plect and the shipped plugin's two
// executables into a temp bin dir, prepends it to PATH, and returns the
// mounted-plugin entry the shipped worktree.toml's `{{bin ...}}` references
// need to resolve. Mirrors app/internal/service's own
// buildWorkspaceProviderBinaries — duplicated here rather than shared,
// because that helper is unexported in a package this one cannot import (see
// the comment on TestE2E_TaskSetupResourceDeliversRealWatcherEventToReactiveTick
// for why this test lives in this package at all).
func buildGithubPluginBinaries(t *testing.T, root string) []plugins.Mounted {
	t.Helper()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	build := func(moduleDir, pkg, out string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, out), pkg)
		cmd.Dir = filepath.Join(root, moduleDir)
		cmd.Env = append(os.Environ(), goToolCachesForE2E...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("build %s: %v", out, err)
		}
	}
	build("app", "./cmd/plect", "plect")
	build(filepath.Join("plugins", pluginDirName, "src"), "./cmd/"+worktreeBin, worktreeBin)
	build(filepath.Join("plugins", pluginDirName, "src"), "./cmd/"+watcherBin, watcherBin)
	build(filepath.Join("plugins", pluginDirName, "src"), "./cmd/"+appTokenBin, appTokenBin)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return []plugins.Mounted{{
		ID:  mountedID,
		Dir: binDir,
		Manifest: plugins.Manifest{Executables: []plugins.Executable{
			{Name: worktreeBin, Path: worktreeBin},
			{Name: watcherBin, Path: watcherBin},
			{Name: appTokenBin, Path: appTokenBin},
		}},
	}}
}

// apiCLIBin is the API CLI binary name the real watcher poller shells out
// to for its own resource fetches by default — faking it under this exact
// name is what lets the poller pick it up unmodified.
const apiCLIBin = "gh" // boundary-allow: the real watcher poller's own hardcoded CLI binary name

// fakeGhForWatcherPoll writes a stand-in for the CLI the real watcher
// poller shells out to for its own API calls. Only the pulls/<number>
// endpoint's mergeable_state varies (read fresh from stateFile on every
// invocation); check-runs/status answer an empty-but-parseable response,
// which the poller's fetchChecks tolerates without affecting the diff this
// test drives. This is independent of the task document's own
// resource_observer below, which never shells out to that CLI at all — see
// this file's own top-level comment for why two independent fact sources
// are in play.
func fakeGhForWatcherPoll(t *testing.T, binDir, stateFile string) {
	t.Helper()
	script := `#!/usr/bin/env bash
path="$2"
case "$path" in
  */pulls/*)
    state=$(cat "` + stateFile + `" 2>/dev/null || echo unknown)
    printf 'HTTP/1.1 200 OK\n\n{"state":"open","merged":false,"sha":"abc1234","mergeable_state":"%s","draft":false}\n' "$state"
    ;;
  *)
    printf 'HTTP/1.1 200 OK\n\n'
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, apiCLIBin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeGhForIssueLinkedPRDiscovery stands in for the CLI the real watcher
// poller shells out to when resolving an issue subscription's linked PR
// through its branch (poll.go's fetchIssue/fetchLinkedPRNumber): the issue's
// own state, the `pulls?head=` discovery list (governed by prNumberFile —
// "none" until the test flips it to a PR number, matching the real
// endpoint's empty-body "no match" answer), the discovered PR's own detail
// call, and a check-runs response reporting one successful run. The last one
// is unconditional because fetchIssue only ever reaches check-runs once
// discovery has already found a PR (an unresolved "none" answer returns
// before that call), so it is never observed while prNumberFile still reads
// "none".
func fakeGhForIssueLinkedPRDiscovery(t *testing.T, binDir, prNumberFile string) {
	t.Helper()
	script := `#!/usr/bin/env bash
path="$2"
case "$path" in
  */issues/*)
    printf 'HTTP/1.1 200 OK\n\n{"state":"open"}\n'
    ;;
  */pulls/*)
    printf 'HTTP/1.1 200 OK\n\n{"state":"open","merged":false,"sha":"abc1234","mergeable_state":"unknown","draft":false}\n'
    ;;
  */pulls)
    n=$(cat "` + prNumberFile + `" 2>/dev/null || echo none)
    if [ "$n" = "none" ]; then
      printf 'HTTP/1.1 200 OK\n\n'
    else
      printf 'HTTP/1.1 200 OK\n\n%s\n' "$n"
    fi
    ;;
  */check-runs)
    printf 'HTTP/1.1 200 OK\n\n{"conclusion":"SUCCESS","name":"ci-ok","started_at":"2024-01-01T00:00:00Z","id":1}\n'
    ;;
  *)
    printf 'HTTP/1.1 200 OK\n\n'
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, apiCLIBin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runWatcherServeOnce runs the watcher's serve subcommand long enough for
// its synchronous startup Sweep+Tick to complete (it ticks once
// immediately, before ever reaching its --interval ticker), then kills it —
// a long --interval means it never re-ticks on its own, so killing it after
// the startup tick is equivalent to "poll exactly once".
func runWatcherServeOnce(t *testing.T) {
	t.Helper()
	cmd := exec.Command(watcherBin, "serve", "--interval", "1h")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start watcher serve: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// watcherSubscriptions reads the real watcher's on-disk subscription
// registry directly — this package never imports the plugin's watcher
// package, so this is the only way to see what the real subscribe hook (run
// by service.TaskSetup's subscribeIfWired) actually persisted. dataHome is
// the XDG_DATA_HOME this test set explicitly (not a bare $HOME), so the
// registry sits directly under it rather than under a "share/.local" nesting.
func watcherSubscriptions(t *testing.T, dataHome string) map[string]json.RawMessage {
	t.Helper()
	path := filepath.Join(dataHome, watcherBin, "subscriptions.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read subscriptions.json: %v", err)
	}
	var doc struct {
		Subscriptions map[string]json.RawMessage `json:"subscriptions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse subscriptions.json: %v", err)
	}
	return doc.Subscriptions
}

func waitUntilOrFatal(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// writeEventDrivenObserverDoc writes a resource_observer whose facts this
// test flips mid-run by calling it again — matching e2eObserver's own
// `^fixture://` match pattern loosened to '.', which (an unanchored
// single-char regex) matches any non-empty resource id, including the real
// https://github.com/... PR URL this test's instance binds.
func writeEventDrivenObserverDoc(t *testing.T, baseDir string, facts map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	// %q escapes the JSON's own embedded quotes the same way TOML's basic
	// string syntax requires, matching app/internal/service's
	// stubObservedFacts test helper (a proven fixture-writer for exactly this
	// "cat a heredoc of JSON" observer shape).
	script := fmt.Sprintf("%q", "cat <<'JSON'\n"+string(encoded)+"\nJSON")
	doc := "[fixture]\nkind  = \"resource_observer\"\nmatch = '.'\n\n" +
		"[fixture.observe]\ntype   = \"shell\"\nscript = " + script + "\n\n" +
		"[fixture.state_schema]\ntype = \"object\"\n\n[fixture.state_schema.properties]\nchecks_status = {}\n"
	writeFile(t, filepath.Join(baseDir, "resources", "fixture.toml"), doc)
}

// This test lives in package reactor, not app/internal/service (where its
// setup logic would read more naturally), because app/internal/reactor
// already imports app/internal/service (reactor.go, for service.TickSession)
// — a test needing both service.TaskSetup and this package's own
// sessionReactor from the service side would be an import cycle. Living here
// instead is the only direction the existing dependency edge allows.
//
// TestE2E_TaskSetupResourceDeliversRealWatcherEventToReactiveTick is the
// acceptance-level pin for the delivery-wiring gap this package's own
// change closes: binding a task instance to a resource via TaskSetup's
// --resource (subscribeIfWired, running the real shipped workspace
// provider's `subscribe` hook against the real watcher binary) must make a
// real watcher poll cycle's published event actually reach the session's
// event log and drive done_when to satisfied through a reactive tick —
// never a manually-called TickSession/plect tick.
func TestE2E_TaskSetupResourceDeliversRealWatcherEventToReactiveTick(t *testing.T) {
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("HOME", xdgHome)

	root := repoRootForE2E(t)
	mounted := buildGithubPluginBinaries(t, root)
	workspacesDir := filepath.Join(mounted[0].Dir, "config", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(root, "plugins", pluginDirName, "config", "workspaces", "worktree.toml"))
	if err != nil {
		t.Fatalf("read shipped workspace provider: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacesDir, "provider.toml"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}

	mergeableStateFile := filepath.Join(t.TempDir(), "mergeable_state")
	if err := os.WriteFile(mergeableStateFile, []byte("unknown"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeGhForWatcherPoll(t, mounted[0].Dir, mergeableStateFile)

	const prURL = "https://github.com/eventdriven/proj/pull/9" // boundary-allow: must be a real GitHub-shaped URL for the shipped provider's own resolver to match
	const session = "eventdriven/pr9+work"
	const parent = "eventdriven/orchestrator"

	base := t.TempDir()
	writeEventDrivenObserverDoc(t, base, map[string]any{"checks_status": "PENDING"})
	// The chain's target is a second, independent workflow/task pair (not
	// this test's own "work" document) — a bare no-op is enough, since what
	// this test is pinning is that the fire happens at all, not what the
	// spawned session goes on to do.
	writeFile(t, filepath.Join(base, "tasks", "noop.toml"), `[noop]
kind  = "effect"
scope = "session"

[noop.setup]
type   = "shell"
script = "echo '{}'"

[noop.health.alive]
type = "noop"
`)
	writeFile(t, filepath.Join(base, "workflows", "notify.toml"), `[notify]
kind               = "workflow"
workspace_provider = "followup"

[[notify.nodes]]
uses = "noop"
`)
	// A chain's spawn always tags the resolved name with (chain id, firing
	// instance) — Up refuses that combination for an identity dispatch (a
	// resource no resolver matches, whose "resolved name" is already the
	// bare resource itself, with nowhere for a tag to attach) — so the
	// target resource needs its own resolver, distinct from the real shipped
	// provider mounted below (whose match is anchored to prURL's own scheme,
	// so it never claims this one) and cheap to "acquire" since nothing here
	// cares what the spawned session's workspace looks like.
	writeFile(t, filepath.Join(base, "workspaces", "followup.toml"), `[followup]
kind  = "workspace_provider"
match = '^followup://(?P<id>.+)$'
name  = { expr = "'followup-' + match.id" }

[followup.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/followup"}']
`)
	const chainResource = "followup://9"
	writeFile(t, filepath.Join(base, "tasks", "work.toml"), `[work]
kind              = "task"
description       = "work fixture"
resource_observer = "fixture"
instructions      = [{ text = "Carry out the work." }]

[work.done_when]
all = [
  { check = "resource.state.checks_status", eq = "SUCCESS" },
]

[[work.chains]]
id       = "notify"
workflow = "notify"
resource = "`+chainResource+`"

[work.chains.when]
all = [
  { check = "resource.state.checks_status", eq = "SUCCESS" },
]
`)
	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{mounted[0].Dir},
		Plugins:    mounted,
	}

	st := state.NewStore("")
	if err := st.Put(&domain.Session{Name: parent}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(&domain.Session{
		Name:          session,
		ParentSession: parent,
		// A live run-scoped task keeps the reactor's drain loop active,
		// mirroring a real session's persistent terminal/agent node — the
		// same role TestSessionReactor_ReactiveTickReachesDoneWhenConsequence's
		// own seed task plays in this same package.
		Tasks: map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	setupResult, err := service.TaskSetup(cfg, st, service.TaskSetupParams{TaskID: "work", SessionName: session, Name: "initial", Resource: prURL})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if !setupResult.Subscribed {
		t.Fatalf("TaskSetup did not wire delivery for %q (SubscribeError=%q)", prURL, setupResult.SubscribeError)
	}

	log := eventlog.NewStore(st.Dir())
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(5*time.Millisecond))
	t.Cleanup(hub.Close)

	socket := filepath.Join(t.TempDir(), "bus.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on bus socket: %v", err)
	}
	busSrv := &http.Server{Handler: eventbus.New(log, "", hub).Routes()}
	go busSrv.Serve(ln)
	t.Cleanup(func() { busSrv.Close() })
	t.Setenv("PLECT_BUS_SOCKET", socket)

	r := &sessionReactor{
		session: session,
		cfg:     cfg,
		state:   st,
		log:     log,
		hub:     hub,
		tick:    config.TickConfig{On: []string{eventSource + ".*"}},
	}
	stop := startReactor(t, r)
	t.Cleanup(stop)

	// First poll cycle: no prior baseline for this subscription, so the
	// watcher can only establish one — never notify (summarizeChanges:
	// "initial observations produce no notifications"). Waiting for the
	// baseline to actually land means tick two's diff is against a real
	// prior value, not a race with tick one's own write.
	runWatcherServeOnce(t)
	waitUntilOrFatal(t, 10*time.Second, "watcher never established its polling baseline", func() bool {
		subs := watcherSubscriptions(t, xdgHome)
		raw, ok := subs[session+"\x00"+prURL]
		if !ok {
			return false
		}
		var sub struct {
			Last map[string]string `json:"last"`
		}
		return json.Unmarshal(raw, &sub) == nil && len(sub.Last) > 0
	})

	// Flip both independent fact sources together: the watcher's own next
	// poll now sees a changed, resolved mergeable_state (triggering a
	// publish), and the task document's own observer now reports the
	// done_when-satisfying fact a reactive tick will read.
	if err := os.WriteFile(mergeableStateFile, []byte("clean"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeEventDrivenObserverDoc(t, base, map[string]any{"checks_status": "SUCCESS"})

	sessionsBeforeFire := st.All()

	runWatcherServeOnce(t)

	// The watcher's publish is the delivery this issue's wiring exists to
	// make possible: assert it landed in the BOUND session's own log,
	// independent of whatever the reactor goes on to do with it — a broken
	// SessionName/resource carried through subscribeIfWired would still let
	// the reactor's done_when assertion below pass by coincidence (the
	// observer facts flipped regardless of delivery), but would fail here.
	waitUntilOrFatal(t, 10*time.Second, "the watcher's published event never reached the bound session's own log", func() bool {
		evs, _, _, err := log.List(session, 0, event.Filter{Types: []string{mergeableEventType}})
		return err == nil && len(evs) == 1
	})
	evs, _, _, err := log.List(session, 0, event.Filter{Types: []string{mergeableEventType}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("List(%q, %s) = %d events, err=%v, want exactly one", session, mergeableEventType, len(evs), err)
	}
	ev := evs[0]
	if ev.SessionName != session {
		t.Errorf("event SessionName = %q, want %q", ev.SessionName, session)
	}
	if ev.Source != eventSource {
		t.Errorf("event Source = %q, want %q", ev.Source, eventSource)
	}
	if ev.Direction != event.Inbound {
		t.Errorf("event Direction = %q, want %q", ev.Direction, event.Inbound)
	}
	if ev.Metadata["url"] != prURL || ev.Metadata["resource"] != prURL {
		t.Errorf("event Metadata = %+v, want url/resource = %q", ev.Metadata, prURL)
	}

	waitUntilOrFatal(t, 15*time.Second, "the watcher's published event never drove a reactive tick to done_when-satisfied", func() bool {
		terminalEvs, _, _, err := log.List(parent, 0, event.Filter{Types: []string{event.TypeTerminalDone}})
		return err == nil && len(terminalEvs) == 1
	})

	// The same reactive tick that satisfied done_when must also have fired
	// work's [[chains]] entry — a live judge/check condition on a document
	// can be satisfied without ever exercising the chain-fire path, so this
	// is a distinct assertion from the done_when one above, not a restating
	// of it. Identifying the spawn by its Workflow, rather than recomputing
	// the exact tagged name the followup resolver and the chain's own
	// tagging would together produce, avoids duplicating that naming logic
	// in the test while still being enough to prove the chain fired.
	waitUntilOrFatal(t, 5*time.Second, "the chain never spawned a session for its target workflow", func() bool {
		for name, s := range st.All() {
			if _, existed := sessionsBeforeFire[name]; existed {
				continue
			}
			if s.Workflow == "notify" {
				return true
			}
		}
		return false
	})
}

// TestE2E_IssueSessionBranchDrivesLinkedPRCIStatusToReactiveTick pins the
// delivery gap the session-branch hook context closes: a session bound to an
// *issue* resource — never itself subscribed to the PR that closes it — must
// still receive a change event for that PR once the session's own branch
// reaches the real shipped subscribe hook, because that is what lets the
// real watcher's issue polling discover the linked PR
// (poll.go's fetchIssue/fetchLinkedPRNumber) and publish for it. Before the
// branch-forwarding fix, TaskSetup's subscribe hook carried no branch, so
// the persisted subscription's own Branch stayed empty, discovery never
// ran, and this event never arrived.
func TestE2E_IssueSessionBranchDrivesLinkedPRCIStatusToReactiveTick(t *testing.T) {
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("HOME", xdgHome)

	root := repoRootForE2E(t)
	mounted := buildGithubPluginBinaries(t, root)
	workspacesDir := filepath.Join(mounted[0].Dir, "config", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(root, "plugins", pluginDirName, "config", "workspaces", "worktree.toml"))
	if err != nil {
		t.Fatalf("read shipped workspace provider: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacesDir, "provider.toml"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}

	prNumberFile := filepath.Join(t.TempDir(), "linked_pr_number")
	if err := os.WriteFile(prNumberFile, []byte("none"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeGhForIssueLinkedPRDiscovery(t, mounted[0].Dir, prNumberFile)

	const issueURL = "https://github.com/eventdriven/proj/issues/21" // boundary-allow: must be a real GitHub-shaped URL for the shipped provider's own resolver to match
	const session = "eventdriven/issue21+work"
	const parent = "eventdriven/orchestrator"
	const branch = "issue/21+work"

	base := t.TempDir()
	writeEventDrivenObserverDoc(t, base, map[string]any{"checks_status": "PENDING"})
	// Same target/no-op shape as TestE2E_TaskSetupResourceDeliversRealWatcherEventToReactiveTick:
	// what matters here is that the chain fires, not what the spawned session
	// goes on to do.
	writeFile(t, filepath.Join(base, "tasks", "noop.toml"), `[noop]
kind  = "effect"
scope = "session"

[noop.setup]
type   = "shell"
script = "echo '{}'"

[noop.health.alive]
type = "noop"
`)
	writeFile(t, filepath.Join(base, "workflows", "notify.toml"), `[notify]
kind               = "workflow"
workspace_provider = "followup"

[[notify.nodes]]
uses = "noop"
`)
	writeFile(t, filepath.Join(base, "workspaces", "followup.toml"), `[followup]
kind  = "workspace_provider"
match = '^followup://(?P<id>.+)$'
name  = { expr = "'followup-' + match.id" }

[followup.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/followup"}']
`)
	const chainResource = "followup://21"
	writeFile(t, filepath.Join(base, "tasks", "work.toml"), `[work]
kind              = "task"
description       = "work fixture"
resource_observer = "fixture"
instructions      = [{ text = "Carry out the work." }]

[work.done_when]
all = [
  { check = "resource.state.checks_status", eq = "SUCCESS" },
]

[[work.chains]]
id       = "notify"
workflow = "notify"
resource = "`+chainResource+`"

[work.chains.when]
all = [
  { check = "resource.state.checks_status", eq = "SUCCESS" },
]
`)
	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{mounted[0].Dir},
		Plugins:    mounted,
	}

	st := state.NewStore("")
	if err := st.Put(&domain.Session{Name: parent}); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(&domain.Session{
		Name:          session,
		ParentSession: parent,
		// The session's own workspace branch: what a workspace provider's
		// setup hook would have produced for a session created on this issue
		// URL. Seeded directly (as the pre-existing sibling test seeds
		// Tasks) since what this test pins is delivery once the branch
		// exists, not how a workspace provider derives one — that path has
		// its own coverage in app/internal/service.
		Branch: branch,
		Tasks: map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	setupResult, err := service.TaskSetup(cfg, st, service.TaskSetupParams{TaskID: "work", SessionName: session, Name: "initial", Resource: issueURL})
	if err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if !setupResult.Subscribed {
		t.Fatalf("TaskSetup did not wire delivery for %q (SubscribeError=%q)", issueURL, setupResult.SubscribeError)
	}
	// The real subscribe hook must have persisted the session's own branch —
	// the fact this whole test pins — not just the session/resource pair.
	if subs := watcherSubscriptions(t, xdgHome); len(subs) != 1 {
		t.Fatalf("subscriptions after TaskSetup = %v, want exactly one", subs)
	} else {
		var sub struct {
			Branch string `json:"branch"`
		}
		for _, raw := range subs {
			if err := json.Unmarshal(raw, &sub); err != nil {
				t.Fatalf("parse persisted subscription: %v", err)
			}
		}
		if sub.Branch != branch {
			t.Fatalf("persisted subscription branch = %q, want %q", sub.Branch, branch)
		}
	}

	log := eventlog.NewStore(st.Dir())
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(5*time.Millisecond))
	t.Cleanup(hub.Close)

	socket := filepath.Join(t.TempDir(), "bus.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on bus socket: %v", err)
	}
	busSrv := &http.Server{Handler: eventbus.New(log, "", hub).Routes()}
	go busSrv.Serve(ln)
	t.Cleanup(func() { busSrv.Close() })
	t.Setenv("PLECT_BUS_SOCKET", socket)

	r := &sessionReactor{
		session: session,
		cfg:     cfg,
		state:   st,
		log:     log,
		hub:     hub,
		tick:    config.TickConfig{On: []string{eventSource + ".*"}},
	}
	stop := startReactor(t, r)
	t.Cleanup(stop)

	// First poll cycle: no linked PR exists yet (prNumberFile reads "none"),
	// so this only establishes the issue subscription's baseline — never a
	// notification (summarizeChanges: "initial observations produce no
	// notifications").
	runWatcherServeOnce(t)
	waitUntilOrFatal(t, 10*time.Second, "watcher never established its polling baseline", func() bool {
		subs := watcherSubscriptions(t, xdgHome)
		raw, ok := subs[session+"\x00"+issueURL]
		if !ok {
			return false
		}
		var sub struct {
			Last map[string]string `json:"last"`
		}
		return json.Unmarshal(raw, &sub) == nil && len(sub.Last) > 0
	})

	// A PR opens on the session's own branch, and its checks go green: flip
	// the fake API CLI's linked-PR discovery to find it, and the task
	// document's own observer (an independent fact source, per this file's
	// own top-comment) to report the done_when-satisfying fact a reactive tick
	// will read.
	if err := os.WriteFile(prNumberFile, []byte("77"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeEventDrivenObserverDoc(t, base, map[string]any{"checks_status": "SUCCESS"})

	sessionsBeforeFire := st.All()

	runWatcherServeOnce(t)

	// The watcher's publish is the delivery this session-branch wiring exists
	// to make possible for an issue session: assert it landed in the issue
	// session's own log — never subscribed to the PR directly — independent
	// of whatever the reactor goes on to do with it.
	waitUntilOrFatal(t, 10*time.Second, "the watcher's linked-PR discovery never reached the issue session's own log", func() bool {
		evs, _, _, err := log.List(session, 0, event.Filter{Types: []string{ciStatusEventType}})
		return err == nil && len(evs) == 1
	})
	evs, _, _, err := log.List(session, 0, event.Filter{Types: []string{ciStatusEventType}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("List(%q, %s) = %d events, err=%v, want exactly one", session, ciStatusEventType, len(evs), err)
	}
	ev := evs[0]
	if ev.SessionName != session {
		t.Errorf("event SessionName = %q, want %q", ev.SessionName, session)
	}
	if ev.Metadata["url"] != issueURL || ev.Metadata["resource"] != issueURL {
		t.Errorf("event Metadata = %+v, want url/resource = %q", ev.Metadata, issueURL)
	}

	waitUntilOrFatal(t, 15*time.Second, "the watcher's published event never drove a reactive tick to done_when-satisfied", func() bool {
		terminalEvs, _, _, err := log.List(parent, 0, event.Filter{Types: []string{event.TypeTerminalDone}})
		return err == nil && len(terminalEvs) == 1
	})

	// The same reactive tick that satisfied done_when must also have fired
	// work's [[chains]] entry — the `[[chains]]` review predicate this
	// issue's acceptance criteria names.
	waitUntilOrFatal(t, 5*time.Second, "the chain never spawned a session for its target workflow", func() bool {
		for name, s := range st.All() {
			if _, existed := sessionsBeforeFire[name]; existed {
				continue
			}
			if s.Workflow == "notify" {
				return true
			}
		}
		return false
	})
}

//go:build integration

package task

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// An effect's receiver is whatever its lifecycle actions invoke — a
// multiplexer, an agent CLI's launch line typed into a terminal, a
// plugin-owned executable. What is under contract is therefore the sequence
// of invocations those actions produce, and the outputs the effect reports
// from them; the shell source that produces them is an implementation of
// that contract, not the contract itself.
//
// Each plugin declares, in its own testdata, the scenario each of its
// effects runs under (inputs, prior outputs, seeded files) and supplies the
// stand-ins its scripts call — including stand-ins that pin what would
// otherwise vary per run, so the record is reproducible. This test drives
// the shipped declarations through the real runtime with those stand-ins in
// place and records what they invoked. Nothing here names a plugin: core
// must not know which effects exist.
//
// Set PLECT_UPDATE_EFFECT_RECORDS=1 to rewrite the records after an intended
// contract change.

const effectRecordName = "effect-invocations.txt"

func TestShippedEffects_InvocationsMatchTheirPluginsRecord(t *testing.T) {
	for _, source := range repoPluginDirs(t) {
		t.Run(filepath.Base(source), func(t *testing.T) {
			scenarios := loadEffectScenarios(t, source)
			if len(scenarios) == 0 {
				t.Skip("this plugin declares no effect scenarios")
			}
			produced := recordShippedEffects(t, source, scenarios)
			record := filepath.Join(source, "testdata", effectRecordName)
			if os.Getenv("PLECT_UPDATE_EFFECT_RECORDS") == "1" {
				if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(record, []byte(produced), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("rewrote %s", record)
				return
			}
			want, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("a plugin declaring effect scenarios records the invocations they must produce: %v", err)
			}
			if string(want) != produced {
				t.Errorf("invocations changed.\n--- recorded\n%s\n--- produced\n%s", want, produced)
			}
		})
	}
}

func loadEffectScenarios(t *testing.T, source string) map[string][]effectScenario {
	t.Helper()
	path := filepath.Join(source, "testdata", "effects", "scenarios.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	scenarios := map[string][]effectScenario{}
	if err := toml.Unmarshal(raw, &scenarios); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := validateScenarioVariantNames(scenarios); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return scenarios
}

// effectHarness is one plugin's sandbox: the copied plugin with spying
// executables, the directory of stand-ins on PATH, and the scrubbing that
// keeps machine-specific paths out of the record.
type effectHarness struct {
	mounted   plugins.Mounted
	spyDir    string
	homeDir   string
	stateDir  string
	workspace string
	argvLog   string
	// liveProcess is a process the sandbox owns, standing in for whatever an
	// agent launch would have started: a script that discovers a pid and
	// checks it is alive finds this one, and a cleanup that signals the pid
	// it recorded signals this one rather than the test. liveCmd is the same
	// process's *exec.Cmd, kept so a scenario can wait for its real exit
	// rather than polling signal 0 (which a zombie still answers).
	liveProcess int
	liveCmd     *exec.Cmd
	// workerProcess/workerCmd stands in for a launched thing distinct from
	// the endpoint's own root process (liveProcess), so a scenario can
	// assert this one died without the endpoint dying too.
	workerProcess int
	workerCmd     *exec.Cmd
	scrub         []struct{ from, to string }
	expand        []struct{ from, to string }
}

func recordShippedEffects(t *testing.T, source string, scenarios map[string][]effectScenario) string {
	t.Helper()
	h := newEffectHarness(t, source)
	cfg := &config.Config{PluginDirs: []string{h.mounted.Dir}, Plugins: []plugins.Mounted{h.mounted}}
	defs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	// A scenario names a declaration by the id its own plugin gave it, not by
	// the catalog address that depends on the alias this harness mounts under,
	// so the record stays the same under any alias.
	byID := make(map[string]config.TaskDefinition, len(defs))
	for _, def := range defs {
		byID[def.ID] = def
	}
	ids := make([]string, 0, len(scenarios))
	for id := range scenarios {
		if _, ok := byID[id]; !ok {
			t.Fatalf("scenario names %q, which this plugin does not declare", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	for _, id := range ids {
		for _, variant := range scenarios[id] {
			label := id
			if variant.Name != "" {
				label = id + ":" + variant.Name
			}
			h.removeAbsentExecutables(t, variant.AbsentExecutables)
			h.runScenario(t, &b, byID[id], id, label, variant)
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// removeAbsentExecutables deletes the spy this harness installed for each
// named executable, simulating a mount whose manifest declares it but whose
// build never produced it — the one condition spyPlugin's blanket spy
// installation can't otherwise represent.
func (h *effectHarness) removeAbsentExecutables(t *testing.T, names []string) {
	t.Helper()
	for _, name := range names {
		for _, exec := range h.mounted.Manifest.Executables {
			if exec.Name != name {
				continue
			}
			path := filepath.Join(h.mounted.Dir, exec.Path)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove %s: %v", name, err)
			}
		}
	}
}

func newEffectHarness(t *testing.T, source string) *effectHarness {
	t.Helper()
	root := t.TempDir()
	h := &effectHarness{
		spyDir:    filepath.Join(root, "bin"),
		homeDir:   filepath.Join(root, "home"),
		stateDir:  filepath.Join(root, "state"),
		workspace: filepath.Join(root, "workspace"),
		argvLog:   filepath.Join(root, "argv.log"),
	}
	for _, dir := range []string{h.spyDir, h.homeDir, h.stateDir, h.workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.mounted = spyPlugin(t, source, h.argvLog, effectSpyAnswer(t, source))
	h.writeRecorders(t)
	copyDirOver(t, filepath.Join(source, "testdata", "effects", "bin"), h.spyDir)

	// The sandbox is what makes a script's own discovery reproducible: it
	// looks for its subject under HOME and writes its scratch files under
	// TMPDIR, and both are this run's own.
	t.Setenv("PATH", h.spyDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", h.homeDir)
	t.Setenv("TMPDIR", h.stateDir)
	t.Setenv("XDG_RUNTIME_DIR", h.stateDir)
	t.Setenv("PLECT_EFFECT_BIN", h.spyDir)
	t.Setenv("PLECT_EFFECT_LOG", h.argvLog)
	t.Setenv("PLECT_EFFECT_STATE", h.stateDir)

	h.scrub = []struct{ from, to string }{
		{h.mounted.Dir, "<plugin>"},
		{h.spyDir, "<spy>"},
		{h.homeDir, "<home>"},
		{h.workspace, "<workspace>"},
		{h.stateDir, "<state>"},
	}
	return h
}

// startLiveProcess runs a sleeper the current scenario owns, so a liveness
// check has something real to find and a cleanup that signals the pid it
// recorded reaches this rather than the test. One per scenario, because a
// cleanup under test is expected to end it.
func (h *effectHarness) startLiveProcess(t *testing.T) {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	h.liveProcess = cmd.Process.Pid
	h.liveCmd = cmd
	t.Setenv("PLECT_EFFECT_PID", strconv.Itoa(h.liveProcess))
	// A seeded file is written before the run, so anything in it that only
	// this run knows is named by a placeholder.
	h.expand = []struct{ from, to string }{
		{"PID", strconv.Itoa(h.liveProcess)},
		{"WORKSPACE", h.workspace},
		{"HOME", h.homeDir},
		{"STATE", h.stateDir},
	}
}

// startWorkerProcess is startLiveProcess's counterpart for
// effectHarness.workerProcess. A scenario that never reads
// PLECT_EFFECT_WORKER_PID just leaves it idle until cleanup reaps it.
func (h *effectHarness) startWorkerProcess(t *testing.T) {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	h.workerProcess = cmd.Process.Pid
	h.workerCmd = cmd
	t.Setenv("PLECT_EFFECT_WORKER_PID", strconv.Itoa(h.workerProcess))
}

// effectSpyAnswer lets each plugin say how its own executables answer, as
// shell source run after the call is recorded. It is source rather than fixed
// output because one executable answers differently per verb, and a script
// that consumes its own executable's output has to keep working under the
// spy.
func effectSpyAnswer(t *testing.T, source string) func(string) string {
	t.Helper()
	dir := filepath.Join(source, "testdata", "effects", "answers")
	return func(name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "true"
		}
		return string(raw)
	}
}

// writeRecorders installs the default stand-ins: the recorder every
// stand-in appends through, and one per terminal verb. A plugin whose script
// needs a verb to answer with something replaces the verb's stand-in from its
// own testdata.
func (h *effectHarness) writeRecorders(t *testing.T) {
	t.Helper()
	record := "#!/usr/bin/env sh\n" +
		"{ printf '%s' \"$1\"; shift; for a in \"$@\"; do printf '\\036%s' \"$a\"; done; printf '\\035'; } >> \"$PLECT_EFFECT_LOG\"\n"
	h.writeSpy(t, "plect-effect-record", record)
	// A stand-in for a command the script needs to really run records the
	// call and then hands it to the real one, which is off this PATH.
	h.writeSpy(t, "plect-effect-passthrough", "#!/usr/bin/env sh\n"+
		"plect-effect-record \"$@\"\n"+
		"PATH=$(printf '%s' \"$PATH\" | sed 's|^'\"$PLECT_EFFECT_BIN\"':||')\n"+
		"export PATH\n"+
		"exec \"$@\"\n")
	for _, verb := range []string{"attach", "capture", "send_text", "send_keys", "pid"} {
		body := "#!/usr/bin/env sh\n" +
			"plect-effect-record terminal-" + verb + " \"$@\"\n"
		switch verb {
		case "capture":
			body += "[ -n \"$PLECT_EFFECT_CAPTURE\" ] && printf '%s\\n' \"$PLECT_EFFECT_CAPTURE\"\ntrue\n"
		case "pid":
			// The scenario's own live process stands in for the endpoint's
			// root process, the same one a liveness check finds.
			body += "printf '%s\\n' \"$PLECT_EFFECT_PID\"\n"
		}
		h.writeSpy(t, "terminal-"+verb, body)
	}
}

func (h *effectHarness) writeSpy(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.spyDir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func copyDirOver(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, entry.Name()), raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *effectHarness) runScenario(t *testing.T, b *strings.Builder, def config.TaskDefinition, id, label string, scenario effectScenario) {
	t.Helper()
	h.startLiveProcess(t)
	h.startWorkerProcess(t)
	t.Setenv("PLECT_EFFECT_CAPTURE", scenario.Capture)
	t.Setenv("PLECT_EFFECT_RETRY", "")
	for _, file := range scenario.Files {
		path := filepath.Join(h.homeDir, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(h.expanded(file.Content)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := ResolveDefinition(def, id)
	if err != nil {
		t.Fatalf("ResolveDefinition(%s): %v", id, err)
	}
	session := h.sessionVars()
	tasks := map[string]*contract.TaskState{}
	if len(scenario.Prev) > 0 {
		tasks[id] = &contract.TaskState{
			Scope:   resolved.Scope,
			Status:  contract.TaskStatusCleaned,
			Outputs: asAnyMap(scenario.Prev),
		}
	}

	self := asAnyMap(scenario.Self)
	// A hook after "setup" (health.alive, cleanup, ...) must see whichever
	// inputs actually produced the instance it inspects.
	currentInputs := scenario.Inputs
	for _, hook := range scenario.hooks(def) {
		if hook == "setup" && len(scenario.RetryInputs) > 0 {
			// Two runs against the same sandbox and live processes, not two
			// scenarios, is what makes "retry succeeds" a claim about this
			// exact failed state rather than a coincidence of shared paths.
			self = h.runOneHook(t, b, def, "setup", "", resolved, id, label, session, tasks, self, scenario.Inputs, scenario.Artifacts)
			if scenario.ExpectWorkerProcessDead {
				h.assertWorkerProcessDead(t)
				h.assertPaneProcessAlive(t)
			}
			h.startWorkerProcess(t)
			if scenario.RetryCapture != "" {
				t.Setenv("PLECT_EFFECT_CAPTURE", scenario.RetryCapture)
			}
			t.Setenv("PLECT_EFFECT_RETRY", "1")
			currentInputs = scenario.RetryInputs
			self = h.runOneHook(t, b, def, "setup", " (retry)", resolved, id, label, session, tasks, self, currentInputs, scenario.Artifacts)
			continue
		}
		self = h.runOneHook(t, b, def, hook, "", resolved, id, label, session, tasks, self, currentInputs, scenario.Artifacts)
	}
	if scenario.ExpectLiveProcessDead {
		h.assertLiveProcessDead(t)
	}
	if scenario.ExpectWorkerProcessDead && len(scenario.RetryInputs) == 0 {
		h.assertWorkerProcessDead(t)
		h.assertPaneProcessAlive(t)
	}
}

// runOneHook runs one hook and records it into b. suffix distinguishes a
// retried "setup" run from its first attempt in the transcript.
func (h *effectHarness) runOneHook(t *testing.T, b *strings.Builder, def config.TaskDefinition, hook, suffix string, resolved Resolved, id, label string, session SessionVars, tasks map[string]*contract.TaskState, self map[string]any, inputs map[string]string, artifacts []effectScenarioArtifact) map[string]any {
	t.Helper()
	// A node input is a literal here rather than a projection of another
	// node's outputs: this record is one effect's own contract, so the
	// scenario states the values it is set up with directly.
	resolved.Inputs = literalValues(inputs)
	if err := os.Remove(h.argvLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	fmt.Fprintf(b, "== %s / %s%s\n", label, hook, suffix)
	outcome := h.runHook(t, hook, def, resolved, session, tasks, self, inputs)
	for i, call := range recordedCalls(t, h.argvLog, h.mounted.Dir) {
		fmt.Fprintf(b, "call[%d]: %s\n", i, h.scrubbed(call))
	}
	fmt.Fprintf(b, "%s\n", h.scrubbed(outcome))
	if state := tasks[id]; state != nil && state.Status == contract.TaskStatusProduced {
		self = state.Outputs
	}
	if hook == "setup" {
		h.writeArtifacts(t, b, artifacts, self)
	}
	b.WriteString("\n")
	return self
}

// assertLiveProcessDead waits for the sandbox's live process to be reaped,
// proving a script actually terminated it rather than merely invoking
// something that claims to. Waiting for exit, not polling signal 0, is what
// makes this reliable: a killed child is a zombie — and still answers signal
// 0 — until its parent (this test) reaps it.
func (h *effectHarness) assertLiveProcessDead(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = h.liveCmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("scenario declares expect_live_process_dead, but the sandbox's live process is still running")
		_ = h.liveCmd.Process.Kill()
		<-done
	}
}

// assertWorkerProcessDead is assertLiveProcessDead's counterpart for the
// worker process (see effectHarness.workerProcess).
func (h *effectHarness) assertWorkerProcessDead(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = h.workerCmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("scenario declares expect_worker_process_dead, but the sandbox's worker process is still running")
		_ = h.workerCmd.Process.Kill()
		<-done
	}
}

// assertPaneProcessAlive is assertWorkerProcessDead's complement: proving
// the worker died is only useful alongside proof the endpoint didn't. A
// killed-but-unreaped process is a zombie that still answers Signal(0), so
// this uses a non-blocking Wait4 (reaping it if it did exit, harmlessly
// racing nothing since nothing else reaps it first) rather than that.
func (h *effectHarness) assertPaneProcessAlive(t *testing.T) {
	t.Helper()
	wpid, err := syscall.Wait4(h.liveProcess, nil, syscall.WNOHANG, nil)
	if err != nil || wpid == h.liveProcess {
		t.Error("expected the endpoint's own root process to survive a worker-only kill, but it is gone")
	}
}

func (h *effectHarness) writeArtifacts(t *testing.T, b *strings.Builder, artifacts []effectScenarioArtifact, self map[string]any) {
	t.Helper()
	for _, artifact := range artifacts {
		dir, _ := self[artifact.Output].(string)
		if dir == "" {
			fmt.Fprintf(b, "artifact %s/%s: no %s output\n", artifact.Output, artifact.Path, artifact.Output)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, artifact.Path))
		if err != nil {
			fmt.Fprintf(b, "artifact %s/%s: %v\n", artifact.Output, artifact.Path, err)
			continue
		}
		fmt.Fprintf(b, "artifact %s/%s:\n%s", artifact.Output, artifact.Path, h.scrubbed(string(raw)))
		if !strings.HasSuffix(string(raw), "\n") {
			b.WriteString("\n")
		}
	}
}

// hooks defaults to every action the declaration carries, in lifecycle
// order, so a plugin's scenario file only names them when it wants a subset.
func (s effectScenario) hooks(def config.TaskDefinition) []string {
	if len(s.Hooks) > 0 {
		return s.Hooks
	}
	out := []string{}
	if def.Setup != nil {
		out = append(out, "setup")
	}
	if def.Health.AliveProbe() != nil {
		out = append(out, "health.alive")
	}
	if def.Health.ActivityProbe() != nil {
		out = append(out, "health.activity")
	}
	if def.Terminal != nil && def.Terminal.Capture != nil {
		out = append(out, "terminal.capture")
	}
	if def.Terminal != nil && def.Terminal.PID != nil {
		out = append(out, "terminal.pid")
	}
	if def.Cleanup != nil {
		out = append(out, "cleanup")
	}
	return out
}

func (h *effectHarness) sessionVars() SessionVars {
	return SessionVars{
		Name:             "acme/widget-1",
		ResourceID:       "example://acme/widget/1",
		WorkspaceDirPath: h.workspace,
		Branch:           "spy-branch",
		Inputs:           map[string]any{},
		Plugins:          []plugins.Mounted{h.mounted},
		Terminal:         h.terminalBinding(),
	}
}

// terminalBinding stands in for the effect that owns the plan's interactive
// endpoint. Each verb resolves to a stand-in that records what it received,
// which is what makes a launch sequence's keystrokes part of the record.
func (h *effectHarness) terminalBinding() *TerminalBinding {
	verb := func(name string) *lang.Action {
		return &lang.Action{Type: lang.ActionExec, Command: filepath.Join(h.spyDir, "terminal-"+name)}
	}
	return &TerminalBinding{
		Ops: &config.TerminalConfig{
			Attach:   verb("attach"),
			Capture:  verb("capture"),
			SendText: verb("send_text"),
			SendKeys: verb("send_keys"),
			PID:      verb("pid"),
		},
		Outputs: map[string]any{"session_name": "acme/widget-1"},
	}
}

func (h *effectHarness) runHook(t *testing.T, hook string, def config.TaskDefinition, r Resolved, session SessionVars, tasks map[string]*contract.TaskState, self map[string]any, scenarioInputs map[string]string) string {
	t.Helper()
	ctx := context.Background()
	inputs := asAnyMap(scenarioInputs)
	switch hook {
	case "setup":
		if err := RunSetup(ctx, []Resolved{r}, session, tasks, nil); err != nil {
			return "declined: " + err.Error()
		}
		return "outputs: " + jsonLine(t, tasks[r.NodeID].Outputs)
	case "cleanup":
		if state := tasks[r.NodeID]; state == nil || state.Status != contract.TaskStatusProduced {
			tasks[r.NodeID] = &contract.TaskState{
				Scope:   r.Scope,
				Status:  contract.TaskStatusProduced,
				Inputs:  inputs,
				Outputs: self,
			}
		}
		if err := RunCleanup(ctx, []Resolved{r}, session, tasks, nil); err != nil {
			return "declined: " + err.Error()
		}
		return "cleaned"
	case "health.alive":
		if err := RunAliveProbe(ctx, h.probe(def, def.Health.AliveProbe(), self, inputs), session); err != nil {
			return "declined: " + err.Error()
		}
		return "alive"
	case "health.activity":
		signal, err := RunActivityProbe(ctx, h.probe(def, def.Health.ActivityProbe(), self, inputs), session)
		switch {
		case err != nil:
			return "declined: " + err.Error()
		case signal == nil:
			return "no evidence"
		default:
			return fmt.Sprintf("activity: fingerprint=%q silence_expected=%v", signal.Fingerprint, signal.SilenceExpected)
		}
	case "terminal.capture":
		binding := &TerminalBinding{Ops: def.Terminal, Outputs: self, SourcePath: def.SourcePath, From: def.Ownership()}
		out, err := RunCapture(ctx, binding, session)
		if err != nil {
			return "declined: " + err.Error()
		}
		return "captured: " + strconv.Quote(out)
	case "terminal.pid":
		binding := &TerminalBinding{Ops: def.Terminal, Outputs: self, SourcePath: def.SourcePath, From: def.Ownership()}
		stdout, _, err := runTerminalVerb(ctx, binding, "pid", session, nil, nil)
		if err != nil {
			return "declined: " + err.Error()
		}
		return "pid: " + strconv.Quote(string(stdout))
	default:
		t.Fatalf("scenario names unknown hook %q", hook)
		return ""
	}
}

func (h *effectHarness) probe(def config.TaskDefinition, action *lang.Action, self, inputs map[string]any) Probe {
	return Probe{Action: action, Self: self, Inputs: inputs, SourcePath: def.SourcePath, From: def.Ownership()}
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// literalValues states a scenario's inputs as the node wiring they stand for:
// one literal per key, which is what a record of a single effect's own
// contract needs.
func literalValues(in map[string]string) map[string]*lang.Value {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*lang.Value, len(in))
	for key, value := range in {
		out[key] = &lang.Value{Form: lang.FormLiteral, Literal: value}
	}
	return out
}

func asAnyMap(in map[string]string) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (h *effectHarness) scrubbed(s string) string {
	for _, rule := range h.scrub {
		s = strings.ReplaceAll(s, rule.from, rule.to)
	}
	s = strings.ReplaceAll(s, strconv.Itoa(h.liveProcess), "<pid>")
	return strings.ReplaceAll(s, strconv.Itoa(h.workerProcess), "<pid>")
}

func (h *effectHarness) expanded(s string) string {
	for _, rule := range h.expand {
		s = strings.ReplaceAll(s, rule.from, rule.to)
	}
	return s
}

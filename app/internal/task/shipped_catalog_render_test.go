package task

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// loadShippedCatalogTasks returns every shipped effect declaration plus the
// mounted-plugin list a `bin` reference resolves against.
func loadShippedCatalogTasks(t *testing.T) (map[string]config.TaskDefinition, []plugins.Mounted) {
	t.Helper()
	cfg := shippedCatalogConfig(t)
	tasks, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions(shipped catalog): %v", err)
	}
	return tasks, cfg.Plugins
}

// shippedCatalogConfig mounts this repository's own official catalog
// (plugins/catalog.toml plus the plugin directories it lists) the way a real
// config load would, so a test over shipped declarations reads them through
// the same loaders a session does.
func shippedCatalogConfig(t *testing.T) *config.Config {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	catalogRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "plugins")

	manifest, err := plugins.LoadCatalogManifest(catalogRoot)
	if err != nil {
		t.Fatalf("LoadCatalogManifest(catalog root): %v", err)
	}
	var pluginDirs []string
	var mounted []plugins.Mounted
	for _, rel := range manifest.Plugins {
		dir := filepath.Join(catalogRoot, rel)
		m, err := plugins.LoadManifest(dir)
		if err != nil {
			t.Fatalf("LoadManifest(%s): %v", rel, err)
		}
		mounted = append(mounted, plugins.Mounted{ID: "official/" + rel, Dir: dir, Manifest: m})
		pluginDirs = append(pluginDirs, dir)
	}
	return &config.Config{PluginDirs: pluginDirs, Plugins: mounted}
}

// TestShippedCatalog_EffectActionsResolve guards this repository's own
// official catalog against a declaration nothing can run: an unresolvable
// `bin`, a projection of a root that has nothing to report, a computation
// that does not evaluate. Any of those would otherwise surface the first
// time a real session ran the effect. A kitchen-sink context supplies every
// key any shipped action reads, so this exercises actual resolution rather
// than TOML decoding.
func TestShippedCatalog_EffectActionsResolve(t *testing.T) {
	tasks, mounted := loadShippedCatalogTasks(t)

	session := SessionVars{
		Name:             "test-session",
		ResourceID:       "owner:test",
		WorkspaceDirPath: "/tmp/test-workdir",
		Branch:           "issue/1",
		Plugins:          mounted,
		Inputs:           map[string]any{},
		// Every shipped agent effect composes the plan's terminal verbs
		// against whichever effect declares them — this kitchen-sink
		// resolution has no real multiplexer in scope, so a stand-in stands
		// for it.
		Terminal: &TerminalBinding{
			Ops: &config.TerminalConfig{
				Attach:   shellStub("tmux attach -t test-session"),
				Capture:  shellStub("tmux capture-pane -p -t test-session"),
				SendText: shellStub(`tmux send-keys -t test-session -- "$1"`),
				SendKeys: shellStub(`tmux send-keys -t test-session "$1"`),
				PID:      shellStub(`tmux display-message -p -t test-session '#{pane_pid}'`),
			},
			Outputs: map[string]any{"session_name": "test-session"},
		},
	}

	// Superset of every key a shipped action reads off self/prev, plus every
	// input a shipped effect declares.
	kitchen := map[string]any{
		"session_id":     "11111111-1111-1111-1111-111111111111",
		"pid":            12345,
		"socket_path":    "/tmp/claude-channel/x.sock",
		"mcp_config":     "/tmp/plect-mcp.json",
		"hooks_settings": "/tmp/plect-hooks.json",
		"profile_path":   "/tmp/plect-x.config.toml",
		"queue_dir":      "/tmp/plect-codex-exec/test-session/queue",
		"state_dir":      "/tmp/plect-codex-exec/test-session",
		"sent":           "true",
		"session_name":   "test-session",
		"dir":            "/tmp/plect-gh-guard.x",
		"thread_ts":      "1234567890.123456",
	}
	inputs := map[string]any{
		"terminal_ready":   "test-session",
		"model":            "fable",
		"effort":           "high",
		"path_prepend":     "/tmp/plect-gh-guard.x",
		"mcp_servers":      `[{"name":"kbn","command":"kbn-mcp","args":["--scoped"]}]`,
		"launch_env":       `{"PLECT_TEAM_CONTEXT":"acme"}`,
		"state_root":       "/tmp/plect-codex-exec",
		"prompt":           "resolve the issue",
		"agent_session":    "",
		"repeat":           "",
		"owner":            "acme",
		"assignees":        "",
		"instruction":      "",
		"base_url":         "http://127.0.0.1:7890",
		"channel_id":       "C123",
		"root_text":        "[AI review] Fix widgets — https://github.com/acme/widgets/pull/7",
		"thread_ts":        "1234567890.123456",
		"socket_path":      "/tmp/claude-channel/x.sock",
		"app_id":           "123456",
		"installation_id":  "987654",
		"private_key_path": "/etc/plect/gh-app.pem",
	}

	for id, def := range tasks {
		ctx := RenderContext{
			Self:       kitchen,
			Prev:       kitchen,
			Inputs:     inputs,
			Session:    session,
			SourcePath: def.SourcePath,
		}
		for _, probe := range []struct {
			field  string
			action *lang.Action
			env    lang.Roots
		}{
			{"setup", def.Setup, setupRoots(ctx)},
			{"cleanup", def.Cleanup, cleanupRoots(ctx)},
			{"health.alive", def.Health.AliveProbe(), healthRoots(ctx)},
			{"health.activity", def.Health.ActivityProbe(), healthRoots(ctx)},
		} {
			// A noop action deliberately runs nothing, so there is nothing
			// for a resolution check to exercise.
			if probe.action == nil || probe.action.Type == lang.ActionNoop {
				continue
			}
			resolved, err := resolveEffect(probe.action, probe.env, ctx, def.Ownership(), nil)
			if err != nil {
				t.Errorf("effect %q %s: %v", id, probe.field, err)
				continue
			}
			resolved.Close()
		}
		if def.Terminal == nil {
			continue
		}
		for _, verb := range []string{"attach", "capture", "send_text", "send_keys", "pid"} {
			action, err := def.Terminal.Verb(verb)
			if err != nil {
				continue // a verb this effect does not offer
			}
			resolved, err := resolveEffect(action, terminalRoots(kitchen, session), ctx, def.Ownership(), nil)
			if err != nil {
				t.Errorf("effect %q terminal.%s: %v", id, verb, err)
				continue
			}
			resolved.Close()
		}
	}
}

// TestShippedCatalog_ModelEffortInputsRejectShellMetacharacters is a
// regression test: every shipped task's model/effort inputs are spliced
// directly into that task's own setup script (and, for claude/codex, resent
// as pane keystrokes), so a value carrying a shell metacharacter must be
// rejected by inputs_schema before it ever reaches that splice point. This
// previously held for agent/codex's tasks but not agent/claude's, letting a
// value like `x' ; rm -rf ~ ; echo '` break out of claude.toml's `MODEL='...'`
// literal at render time.
func TestShippedCatalog_ModelEffortInputsRejectShellMetacharacters(t *testing.T) {
	tasks, _ := loadShippedCatalogTasks(t)

	malicious := map[string]any{"model": "x' ; touch /tmp/pwned ; echo '"}
	benign := map[string]any{"model": "fable"}

	checked := 0
	for id, def := range tasks {
		props, ok := def.InputsSchema["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := props["model"]; !ok {
			continue
		}
		checked++
		schema, err := lang.CompileSchema(def.InputsSchema, def.ResolvedInputsSchemaPath(), "test:"+id)
		if err != nil {
			t.Fatalf("task %q: CompileSchema: %v", id, err)
		}
		if schema == nil {
			t.Fatalf("task %q declares a model input but compiled to no schema", id)
		}
		if err := schema.Validate(toJSONShape(malicious)); err == nil {
			t.Errorf("task %q: inputs_schema accepted a model value containing shell metacharacters", id)
		}
		if err := schema.Validate(toJSONShape(benign)); err != nil {
			t.Errorf("task %q: inputs_schema rejected a plain model token: %v", id, err)
		}
	}
	if checked == 0 {
		t.Fatal("no shipped task declares a model input; this test is checking nothing")
	}
}

// TestShippedCatalog_LaunchEnvRejectsQuoteBreakout: launch_env crosses the
// same splice point model/effort do — the setup script assigns it inside a
// single-quoted literal — so a value carrying a single quote must be rejected
// before the script ever renders, no matter how carefully the jq expansion
// downstream quotes what it parses.
func TestShippedCatalog_LaunchEnvRejectsQuoteBreakout(t *testing.T) {
	tasks, _ := loadShippedCatalogTasks(t)

	breakout := map[string]any{"launch_env": `{"A":"x'; touch /tmp/pwned; echo '"}`}
	benign := map[string]any{"launch_env": `{"PLECT_TEAM_CONTEXT":"acme"}`}

	checked := 0
	for id, def := range tasks {
		props, ok := def.InputsSchema["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := props["launch_env"]; !ok {
			continue
		}
		checked++
		schema, err := lang.CompileSchema(def.InputsSchema, def.ResolvedInputsSchemaPath(), "test:"+id)
		if err != nil {
			t.Fatalf("task %q: CompileSchema: %v", id, err)
		}
		if err := schema.Validate(toJSONShape(breakout)); err == nil {
			t.Errorf("task %q: inputs_schema accepted a launch_env value containing a single quote", id)
		}
		if err := schema.Validate(toJSONShape(benign)); err != nil {
			t.Errorf("task %q: inputs_schema rejected a plain launch_env object: %v", id, err)
		}
	}
	if checked == 0 {
		t.Fatal("no shipped task declares a launch_env input; this test is checking nothing")
	}
}

// TestShippedCatalog_McpServersRejectsQuoteBreakout: mcp_servers never reaches
// a command line, but its JSON does cross the same single-quoted assignment
// launch_env crosses, so a value carrying a single quote must be rejected
// before the setup script renders — the serialization step downstream is only
// reached if that guard holds.
func TestShippedCatalog_McpServersRejectsQuoteBreakout(t *testing.T) {
	tasks, _ := loadShippedCatalogTasks(t)

	breakout := map[string]any{"mcp_servers": `[{"name":"x'; touch /tmp/pwned; echo '","command":"y"}]`}
	benign := map[string]any{"mcp_servers": `[{"name":"kbn","command":"kbn-mcp","args":["--scoped"]}]`}

	checked := 0
	for id, def := range tasks {
		props, ok := def.InputsSchema["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := props["mcp_servers"]; !ok {
			continue
		}
		checked++
		schema, err := lang.CompileSchema(def.InputsSchema, def.ResolvedInputsSchemaPath(), "test:"+id)
		if err != nil {
			t.Fatalf("task %q: CompileSchema: %v", id, err)
		}
		if err := schema.Validate(toJSONShape(breakout)); err == nil {
			t.Errorf("task %q: inputs_schema accepted an mcp_servers value containing a single quote", id)
		}
		if err := schema.Validate(toJSONShape(benign)); err != nil {
			t.Errorf("task %q: inputs_schema rejected a plain mcp_servers array: %v", id, err)
		}
	}
	if checked == 0 {
		t.Fatal("no shipped task declares an mcp_servers input; this test is checking nothing")
	}
}

func TestShippedGithubTaskDocuments_AppGitAuthInputsMatchGuardShape(t *testing.T) {
	cfg := shippedCatalogConfig(t)
	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments(shipped catalog): %v", err)
	}

	benign := map[string]any{
		"instruction":      "",
		"app_id":           "123456",
		"installation_id":  "987654",
		"owner":            "acme",
		"repo":             "widgets",
		"private_key_path": "/etc/plect/gh-app.pem",
	}
	badAppID := map[string]any{"app_id": "123; touch /tmp/pwned"}
	badOwner := map[string]any{"owner": "acme/widgets"}
	badRepo := map[string]any{"repo": "widgets;touch"}
	badKeyPath := map[string]any{"private_key_path": "/etc/plect/app key.pem"}

	for _, address := range []string{
		"official.github.work",
		"official.github.review",
		"official.github.investigate",
		"official.github.respond",
	} {
		doc, ok := docs[address]
		if !ok {
			t.Errorf("shipped catalog task document %q not found", address)
			continue
		}
		schema, err := lang.CompileSchema(doc.InputsSchema, doc.ResolvedInputsSchemaPath(), "test:"+address)
		if err != nil {
			t.Fatalf("task document %q: CompileSchema: %v", address, err)
		}
		if err := schema.Validate(toJSONShape(benign)); err != nil {
			t.Errorf("task document %q rejected App git auth inputs: %v", address, err)
		}
		for name, value := range map[string]map[string]any{
			"app_id":           badAppID,
			"owner":            badOwner,
			"repo":             badRepo,
			"private_key_path": badKeyPath,
		} {
			if err := schema.Validate(toJSONShape(value)); err == nil {
				t.Errorf("task document %q accepted unsafe %s input", address, name)
			}
		}
	}
}

// TestShippedCatalog_TaskDocumentInstructionsRender guards the six task
// documents this repository ships (official.github's work/review/
// investigate/respond, official.okf's pursue_goal/goal_review) against a
// body that fails to render: an unresolvable `{{ resource.id }}` projection,
// or any other instruction-body projection error a plugin author would
// otherwise only discover the first time a session dispatched against it.
func TestShippedCatalog_TaskDocumentInstructionsRender(t *testing.T) {
	cfg := shippedCatalogConfig(t)
	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments(shipped catalog): %v", err)
	}

	for _, address := range []string{
		"official.github.work",
		"official.github.review",
		"official.github.investigate",
		"official.github.respond",
		"official.okf.pursue_goal",
		"official.okf.goal_review",
	} {
		doc, ok := docs[address]
		if !ok {
			t.Errorf("shipped catalog task document %q not found", address)
			continue
		}

		env := lang.Roots{
			"resource": map[string]any{"id": "https://github.com/acme/widgets/issues/1", "state": map[string]any{}},
			"self":     map[string]any{"state": map[string]any{}},
			// Every shipped document's optional `instruction` input declares
			// its own schema default; the taskdoc binding pipeline is what
			// applies that default in production, so it is supplied directly
			// here since this test bypasses that pipeline.
			"inputs":   map[string]any{"instruction": ""},
			"session":  map[string]any{"name": "test-session", "inputs": map[string]any{}},
			"workflow": map[string]any{"outputs": map[string]any{}},
		}
		body, err := lang.RenderInstruction(doc.Instruction, env)
		if err != nil {
			t.Errorf("task document %q: RenderInstruction: %v", address, err)
			continue
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("task document %q rendered to an empty body", address)
		}
	}
}

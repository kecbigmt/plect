package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// loadTasksFromFiles writes each task body under a fresh global-layer
// `tasks/` directory and loads the whole set, so a table row can describe a
// nesting chain as the files an author would actually write.
func loadTasksFromFiles(t *testing.T, files map[string]string) (map[string]TaskDefinition, error) {
	t.Helper()
	base := t.TempDir()
	for id, body := range files {
		writeFile(t, filepath.Join(base, "tasks", id+".toml"), body)
	}
	return (&Config{BaseDir: base}).LoadTaskDefinitions("")
}

// innerRuntime is the plain inner task most rows nest: two outputs, one of
// them mutable, so binding type and mutability rules have something to
// disagree with.
const innerRuntime = `
[claude]
kind  = "effect"
scope = "run"

[claude.setup]
type   = "shell"
script = "true"

[claude.health.alive]
type = "noop"

[claude.outputs_schema]
type = "object"

[claude.outputs_schema.properties]
pid         = { type = "integer", mutable = true }
socket_path = { type = "string" }
`

// TestLoadTaskDefinitions_NestedDefinitionParsesEveryLayerField covers the
// whole nested-task field set in one file: the joint vocabulary plus every
// plain-task field an outer layer declares for its own layer.
func TestLoadTaskDefinitions_NestedDefinitionParsesEveryLayerField(t *testing.T) {
	defs, err := loadTasksFromFiles(t, map[string]string{
		"claude": innerRuntime,
		"myclaude": `
[myclaude]
kind     = "effect"

[myclaude.inner]
uses = "claude"

[myclaude.setup]
type   = "shell"
script = "jq -nc '{guard_dir:\"/tmp/guard\"}'"

[myclaude.cleanup]
type   = "shell"
script = "true"

[myclaude.inner.inputs]
tmux_session = { from = "inputs.tmux_session" }

[myclaude.inner.env]
PLECT_TEAM_CONTEXT = { from = "session.name" }

[myclaude.outputs.bind]
pid = { from = "inner.outputs.pid" }
guard_dir = { from = "locals.guard_dir" }

[myclaude.inputs_schema]
type = "object"

[myclaude.inputs_schema.properties]
tmux_session = { type = "string" }

[myclaude.locals_schema]
type = "object"
required = ["guard_dir"]

[myclaude.locals_schema.properties]
guard_dir = { type = "string" }

[myclaude.outputs_schema]
type = "object"

[myclaude.outputs_schema.properties]
pid       = { type = "integer", mutable = true }
guard_dir = { type = "string" }

[myclaude.health.alive]
type   = "shell"
script = "kill -0 \"$pid\""

[myclaude.health.alive.bind]
pid = { from = "self.outputs.pid" }
`,
	})
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	def, ok := defs["myclaude"]
	if !ok {
		t.Fatal("task \"myclaude\" was not loaded")
	}
	if def.Inner != "claude" {
		t.Errorf("Inner = %q, want %q", def.Inner, "claude")
	}
	if got := def.InnerInputs["tmux_session"].Source(); got != `{ from = "inputs.tmux_session" }` {
		t.Errorf("inner.inputs[tmux_session] = %s", got)
	}
	if got := def.InnerEnv["PLECT_TEAM_CONTEXT"].Source(); got != `{ from = "session.name" }` {
		t.Errorf("inner.env[PLECT_TEAM_CONTEXT] = %s", got)
	}
	if got := def.OutputsBind["guard_dir"].Source(); got != `{ from = "locals.guard_dir" }` {
		t.Errorf("outputs.bind[guard_dir] = %s", got)
	}
	if _, ok := def.LocalsSchema["properties"]; !ok {
		t.Errorf("locals_schema was not decoded: %v", def.LocalsSchema)
	}
	if def.Health.AliveProbe() == nil {
		t.Error("[health].alive was not decoded")
	}
	if len(def.InnerChain) != 1 || def.InnerChain[0].ID != "claude" {
		t.Fatalf("InnerChain = %v, want the resolved [claude]", layerIDs(def.InnerChain))
	}
}

func layerIDs(chain []TaskDefinition) []string {
	ids := make([]string, len(chain))
	for i, d := range chain {
		ids[i] = d.ID
	}
	return ids
}

// TestLoadTaskDefinitions_NestedScopeFollowsInnermost covers the effective
// scope rule: an outer layer that declares no scope of its own takes the
// innermost task's, rather than the plain-task "run" default.
func TestLoadTaskDefinitions_NestedScopeFollowsInnermost(t *testing.T) {
	defs, err := loadTasksFromFiles(t, map[string]string{
		"tmux": `
[tmux]
kind = "effect"
scope = "session"

[tmux.setup]
type   = "shell"
script = "true"

[tmux.health.alive]
type = "noop"
`,
		"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "tmux"
`,
	})
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	if got := defs["outer"].EffectiveScope(); got != TaskScopeSession {
		t.Errorf("EffectiveScope() = %q, want %q", got, TaskScopeSession)
	}
}

// TestLoadTaskDefinitions_NestingValidationRules is the load-error rule suite
// of docs/design/task-nesting.md's Validation Rules section, one row per rule.
func TestLoadTaskDefinitions_NestingValidationRules(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name: "inner names an unknown effect",
			files: map[string]string{
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "nope"
`,
			},
			wantErr: `unknown effect "nope"`,
		},
		{
			name: "inner self-reference forms a nesting cycle",
			files: map[string]string{
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "outer"
`,
			},
			wantErr: "nesting cycle",
		},
		{
			name: "inner forms a nesting cycle through another task",
			files: map[string]string{
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "middle"
`,
				"middle": `
[middle]
kind = "effect"

[middle.inner]
uses = "outer"
`,
			},
			wantErr: "nesting cycle",
		},
		{
			name: "the outer task declares a scope that differs from the inner task's",
			files: map[string]string{
				"claude": innerRuntime,
				"outer": `
[outer]
kind = "effect"
scope = "session"

[outer.inner]
uses = "claude"
`,
			},
			wantErr: "scope",
		},
		{
			name: "two layers of the nesting chain declare [terminal]",
			files: map[string]string{
				"claude": innerRuntime + `
[claude.terminal.attach]
type   = "shell"
script = "true"
`,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.terminal.attach]
type   = "shell"
script = "true"

[outer.terminal.capture]
type   = "shell"
script = "true"

[outer.terminal.send_text]
type   = "shell"
script = "true"

[outer.terminal.send_keys]
type   = "shell"
script = "true"
`,
			},
			wantErr: "[terminal]",
		},
		{
			name: "outputs.bind reads a root the surface does not observe",
			files: map[string]string{
				"claude": innerRuntime,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.outputs.bind]
pid = { from = "session.name" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
pid = { type = "string" }
`,
			},
			wantErr: "effect.outputs.bind",
		},
		{
			name: "outputs.bind declares a public key missing from outputs_schema",
			files: map[string]string{
				"claude": innerRuntime,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.outputs.bind]
socket_path = { from = "inner.outputs.socket_path" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
pid = { type = "integer", mutable = true }
`,
			},
			wantErr: "outputs_schema",
		},
		{
			name: "outputs.bind declares a computed output mutable",
			files: map[string]string{
				"claude": innerRuntime,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.outputs.bind]
label = { expr = "'pid-' + string(inner.outputs.pid)" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
label = { type = "string", mutable = true }
`,
			},
			wantErr: "mutable",
		},
		{
			name: "a layer declares [terminal] and the outer contract does not bind interactive_endpoint",
			files: map[string]string{
				"claude": innerRuntime + `
[claude.terminal.attach]
type   = "shell"
script = "true"
`,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.outputs.bind]
pid = { from = "inner.outputs.pid" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
pid = { type = "integer", mutable = true }
`,
			},
			wantErr: "interactive_endpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadTasksFromFiles(t, tt.files)
			if err == nil {
				t.Fatalf("LoadTaskDefinitions: want an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadTaskDefinitions error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadTaskDefinitions_NestingAcceptsValidChains is the positive half of
// the rule suite: shapes each rule above is meant to leave alone must load.
func TestLoadTaskDefinitions_NestingAcceptsValidChains(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "re-export, rename, and a computed local binding",
			files: map[string]string{
				"claude": innerRuntime,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.setup]
type   = "shell"
script = "jq -nc '{guard_dir:\"/tmp/guard\"}'"

[outer.health.alive]
type = "noop"

[outer.outputs.bind]
pid = { from = "inner.outputs.pid" }
socket = { from = "inner.outputs.socket_path" }
label = { expr = "'pid-' + string(inner.outputs.pid)" }
guard_dir = { from = "locals.guard_dir" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
pid       = { type = "integer", mutable = true }
socket    = { type = "string" }
label     = { type = "string" }
guard_dir = { type = "string" }
`,
			},
		},
		{
			name: "an outer layer composing a terminal over a headless inner",
			files: map[string]string{
				"claude": innerRuntime,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.setup]
type   = "shell"
script = "jq -nc '{pane:\"%1\"}'"

[outer.health.alive]
type = "noop"

[outer.terminal.attach]
type   = "shell"
script = "true"

[outer.terminal.capture]
type   = "shell"
script = "true"

[outer.terminal.send_text]
type   = "shell"
script = "true"

[outer.terminal.send_keys]
type   = "shell"
script = "true"

[outer.outputs.bind]
interactive_endpoint = { from = "locals.pane" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
interactive_endpoint = { type = "string" }
`,
			},
		},
		{
			name: "three layers with disjoint bind.env keys",
			files: map[string]string{
				"claude": innerRuntime,
				"middle": `
[middle]
kind = "effect"

[middle.inner]
uses = "claude"

[middle.inner.env]
PLECT_MIDDLE = "1"

[middle.outputs.bind]
pid = { from = "inner.outputs.pid" }

[middle.outputs_schema]
type = "object"

[middle.outputs_schema.properties]
pid = { type = "integer", mutable = true }
`,
				"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "middle"

[outer.inner.env]
PLECT_OUTER = "1"

[outer.outputs.bind]
pid = { from = "inner.outputs.pid" }

[outer.outputs_schema]
type = "object"

[outer.outputs_schema.properties]
pid = { type = "integer", mutable = true }
`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadTasksFromFiles(t, tt.files); err != nil {
				t.Fatalf("LoadTaskDefinitions: unexpected error: %v", err)
			}
		})
	}
}

// TestLoadTaskDefinitions_RetiredBindTableRejected: the joint's tables are
// `inner.inputs`, `inner.env`, and `outputs.bind`, so the retired `[bind]`
// spelling fails as the field outside the surface that it is, rather than
// decoding into nothing.
func TestLoadTaskDefinitions_RetiredBindTableRejected(t *testing.T) {
	_, err := loadTasksFromFiles(t, map[string]string{
		"claude": innerRuntime,
		"outer": `
[outer]
kind = "effect"

[outer.inner]
uses = "claude"

[outer.bind.output]
pid = "{{.Inner.outputs.pid}}"
`,
	})
	if err == nil {
		t.Fatal("LoadTaskDefinitions: want an error for an unknown [bind] table, got nil")
	}
	if !strings.Contains(err.Error(), `"bind" is not part of the effect surface`) {
		t.Errorf("LoadTaskDefinitions error = %v, want it to name the unknown bind table", err)
	}
}

package lang

import (
	"os"
	"path/filepath"
	"testing"
)

// A provider builds its session name from its own captures and reads its own
// declared outputs, so a key that neither declares is a load error rather
// than an empty value at dispatch or teardown. These run inside
// ValidateDefinition, so a consumer cannot load a provider without them.
func TestProviderContracts(t *testing.T) {
	tests := []struct {
		name       string
		src        string
		schemaFile string
		want       Code
		layer      Layer
	}{
		{
			name: "a name projecting a declared capture",
			src: `[p]
kind  = "workspace_provider"
match = '^x/(?P<owner>[^/]+)$'
name  = { from = "match.owner" }

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"
`,
		},
		{
			name: "a name computing over declared captures",
			src: `[p]
kind  = "workspace_provider"
match = '^x/(?P<owner>[^/]+)/(?P<repo>[^/]+)$'
name  = { expr = "match.owner + '/' + match.repo" }

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"
`,
		},
		{
			name: "a name projecting an undeclared capture",
			src: `[p]
kind  = "workspace_provider"
match = '^x/(?P<owner>[^/]+)$'
name  = { from = "match.repo" }

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"
`,
			want: CodeFromPath,
		},
		{
			name: "a name computing over an undeclared capture",
			src: `[p]
kind  = "workspace_provider"
match = '^x/(?P<owner>[^/]+)$'
name  = { expr = "match.owner + '-' + match.number" }

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"
`,
			want: CodeFromPath,
		},
		{
			name: "a cleanup reading a declared output",
			src: `[p]
kind = "workspace_provider"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type = "exec"
bin  = "okf-bundle"
args = [{ from = "self.outputs.workspace_dir" }]

[p.outputs_schema]
type = "object"

[p.outputs_schema.properties]
workspace_dir = { type = "string" }
`,
		},
		{
			name: "a cleanup reading an undeclared output",
			src: `[p]
kind = "workspace_provider"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type = "exec"
bin  = "okf-bundle"
args = [{ from = "self.outputs.branch" }]

[p.outputs_schema]
type = "object"

[p.outputs_schema.properties]
workspace_dir = { type = "string" }
`,
			want: CodeFromPath,
		},
		{
			name: "a cleanup computing over an undeclared output",
			src: `[p]
kind = "workspace_provider"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type   = "shell"
script = "true"

[p.cleanup.bind]
label = { expr = "'dir-' + self.outputs.branch" }

[p.outputs_schema]
type = "object"

[p.outputs_schema.properties]
workspace_dir = { type = "string" }
`,
			want: CodeFromPath,
		},
		{
			name: "a cleanup reading an output a contract file does not declare",
			src: `[p]
kind = "workspace_provider"
outputs_schema_file = "outputs.json"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type = "exec"
bin  = "okf-bundle"
args = [{ from = "self.outputs.branch" }]
`,
			schemaFile: `{"type":"object","properties":{"workspace_dir":{"type":"string"}}}`,
			want:       CodeFromPath,
		},
		{
			name: "a cleanup reading an output a contract file declares",
			src: `[p]
kind = "workspace_provider"
outputs_schema_file = "outputs.json"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type = "exec"
bin  = "okf-bundle"
args = [{ from = "self.outputs.branch" }]
`,
			schemaFile: `{"type":"object","properties":{"branch":{"type":"string"}}}`,
		},
		{
			// An expression that cannot compile is what has to be reported:
			// naming every capture reference unresolvable would bury it.
			name: "a match that does not compile reports itself",
			src: `[p]
kind  = "workspace_provider"
match = '^x/(?P<owner>[^/]+$'
name  = { from = "match.owner" }

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"
`,
			want:  CodeFieldType,
			layer: LayerStructural,
		},
		{
			// A provider declaring no contract declares no property, so the
			// read fails closed rather than being waved through.
			name: "a cleanup reading an output where no contract is declared",
			src: `[p]
kind = "workspace_provider"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type = "exec"
bin  = "okf-bundle"
args = [{ from = "self.outputs.branch" }]
`,
			want: CodeFromPath,
		},
		{
			name: "a cleanup reading nothing needs no contract",
			src: `[p]
kind = "workspace_provider"

[p.setup]
type = "exec"
bin  = "okf-bundle"

[p.health.alive]
type = "noop"

[p.cleanup]
type = "exec"
bin  = "okf-bundle"
args = ["cleanup", { from = "session.name" }]
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "provider.toml")
			if err := os.WriteFile(path, []byte(tt.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.schemaFile != "" {
				if err := os.WriteFile(filepath.Join(dir, "outputs.json"), []byte(tt.schemaFile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			defs, err := ParseDefinitionDocument(path, []byte(tt.src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			v := Validation{
				From:        Ownership{IsPlugin: true, Alias: fixtureAlias, Path: fixturePath},
				Executables: NewExecutableRegistry(PluginExecutables{Alias: fixtureAlias, Path: fixturePath, Names: fixtureExecutables}),
			}
			got := v.ValidateDefinition(defs[0])
			if tt.want == "" {
				if got != nil {
					t.Fatalf("expected this to load, but: %v", got)
				}
				return
			}
			layer := tt.layer
			if layer == "" {
				layer = LayerSemantic
			}
			wantDiag(t, got, tt.want, layer)
		})
	}
}

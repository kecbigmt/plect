package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProviderDoc(t *testing.T, baseDir, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(baseDir, "workspaces", name+".toml"), body)
}

const providerSetupHook = `
[github.setup]
type    = "exec"
command = "true"
args    = ["setup", { from = "resource.id" }]

[github.health.alive]
type = "noop"
`

func TestLoadWorkspaceProviders_GlobalLayer(t *testing.T) {
	baseDir := t.TempDir()
	writeProviderDoc(t, baseDir, "github", `
[github]
kind  = "workspace_provider"
match = '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)'
name  = { expr = "match.owner + '/' + match.repo" }
`+providerSetupHook)
	got, err := (&Config{BaseDir: baseDir}).LoadWorkspaceProviders()
	if err != nil {
		t.Fatal(err)
	}
	prov, ok := got["github"]
	if !ok {
		t.Fatalf("expected github workspace provider, got %+v", got)
	}
	if prov.Setup == nil || prov.Name == nil || prov.Match == "" {
		t.Errorf("fields lost: %+v", prov)
	}
	if !prov.HasResolver() {
		t.Error("a provider declaring match and name has a resolver")
	}
	if prov.FromPlugin {
		t.Error("a global-layer definition is not plugin-owned")
	}
}

// The id is the declaration's table name, so one document may declare
// several providers and none is named after the file.
func TestLoadWorkspaceProviders_IDComesFromTheTableNotTheFilename(t *testing.T) {
	baseDir := t.TempDir()
	writeProviderDoc(t, baseDir, "anything", `
[first]
kind = "workspace_provider"

[first.setup]
type    = "exec"
command = "true"

[first.health.alive]
type = "noop"

[second]
kind = "workspace_provider"

[second.setup]
type    = "exec"
command = "true"

[second.health.alive]
type = "noop"
`)
	got, err := (&Config{BaseDir: baseDir}).LoadWorkspaceProviders()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, ok := got[id]; !ok {
			t.Errorf("expected provider %q, got %v", id, got)
		}
	}
	if _, ok := got["anything"]; ok {
		t.Error("the filename must not become an id")
	}
}

func TestLoadWorkspaceProviders_GlobalOverridesPlugin(t *testing.T) {
	pluginDir := t.TempDir()
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "workspaces", "github.toml"), `
[github]
kind = "workspace_provider"

[github.setup]
type    = "exec"
command = "plugin"

[github.health.alive]
type = "noop"
`)
	writeProviderDoc(t, baseDir, "github", `
[github]
kind = "workspace_provider"

[github.setup]
type    = "exec"
command = "global"

[github.health.alive]
type = "noop"
`)
	got, err := (&Config{BaseDir: baseDir, PluginDirs: []string{pluginDir}}).LoadWorkspaceProviders()
	if err != nil {
		t.Fatal(err)
	}
	if got["github"].Setup.Command != "global" {
		t.Errorf("Command = %q, want global to win (deeper layer)", got["github"].Setup.Command)
	}
}

// Coexistence follows from addressability. A hand-authored plugin_dirs entry
// carries no catalog identity, so no address can name one of these two layers
// apart from the other, and choosing between them would fall to declaration
// order.
func TestLoadWorkspaceProviders_TwoUnaddressablePluginLayersSameIDFailsLoud(t *testing.T) {
	pluginA := t.TempDir()
	pluginB := t.TempDir()
	for _, dir := range []string{pluginA, pluginB} {
		writeFile(t, filepath.Join(dir, "config", "workspaces", "github.toml"), `
[github]
kind = "workspace_provider"

[github.setup]
type    = "exec"
command = "true"
`)
	}
	_, err := (&Config{PluginDirs: []string{pluginA, pluginB}}).LoadWorkspaceProviders()
	if err == nil || !strings.Contains(err.Error(), "github") {
		t.Fatalf("expected a collision error naming \"github\", got %v", err)
	}
}

func TestLoadWorkspaceProviders_RejectedDeclarations(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "setup required",
			body: "[p]\nkind = \"workspace_provider\"\n",
			want: "setup",
		},
		{
			name: "kind missing",
			body: "[p]\nmatch = '^x'\n",
			want: "kind",
		},
		{
			name: "match without name",
			body: "[p]\nkind = \"workspace_provider\"\nmatch = '^x'\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n\n[p.health.alive]\ntype = \"noop\"\n",
			want: "declared together",
		},
		{
			name: "name without match",
			body: "[p]\nkind = \"workspace_provider\"\nname = \"n\"\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n\n[p.health.alive]\ntype = \"noop\"\n",
			want: "declared together",
		},
		{
			name: "match does not compile",
			body: "[p]\nkind = \"workspace_provider\"\nmatch = '('\nname = \"n\"\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n\n[p.health.alive]\ntype = \"noop\"\n",
			want: "does not compile as a regular expression",
		},
		{
			name: "a field outside the provider surface",
			body: "[p]\nkind = \"workspace_provider\"\nenvironment = \"docker\"\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n",
			want: "environment",
		},
		{
			name: "a name reading something other than a match capture",
			body: "[p]\nkind = \"workspace_provider\"\nmatch = '^x'\nname = { from = \"resource.id\" }\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n",
			want: "resource.id",
		},
		{
			name: "a setup reading a root only cleanup offers",
			body: "[p]\nkind = \"workspace_provider\"\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\nargs = [{ from = \"self.outputs.workspace_dir\" }]\n",
			want: "self.outputs.workspace_dir",
		},
		{
			name: "a subscribe reading a provider parameter",
			body: "[p]\nkind = \"workspace_provider\"\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n\n[p.subscribe]\ntype = \"exec\"\ncommand = \"true\"\nargs = [{ from = \"inputs.x\" }]\n",
			want: "inputs.x",
		},
		{
			name: "the reserved workspace_dir output declared mutable",
			body: "[p]\nkind = \"workspace_provider\"\n\n[p.setup]\ntype = \"exec\"\ncommand = \"true\"\n\n[p.health.alive]\ntype = \"noop\"\n\n[p.outputs_schema.properties.workspace_dir]\ntype = \"string\"\nmutable = true\n",
			want: "reserved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDir := t.TempDir()
			writeProviderDoc(t, baseDir, "broken", tt.body)
			_, err := (&Config{BaseDir: baseDir}).LoadWorkspaceProviders()
			if err == nil {
				t.Fatalf("expected a load error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error does not mention %q: %v", tt.want, err)
			}
		})
	}
}

func TestLoadWorkspaceProviders_NotInWorkspaceDirCascade(t *testing.T) {
	// Workspace providers are trusted-base-layer only: a workspaces/ dir
	// inside a workspace-dir overlay chain must never be picked up, because
	// setup must be resolvable before any workspace exists and it runs a
	// process.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	workspaceDirPath := filepath.Join(tmpHome, "workspace_dirs", "session")
	writeFile(t, filepath.Join(workspaceDirPath, ".plect", "workspaces", "evil.toml"), `
[evil]
kind = "workspace_provider"

[evil.setup]
type    = "exec"
command = "curl"
args    = ["evil.example"]
`)
	got, err := (&Config{}).LoadWorkspaceProviders()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["evil"]; ok {
		t.Fatal("workspace-dir-layer workspace provider must never load")
	}
}

func TestLoadWorkflows_WorkspaceProviderRedeclarationRejected(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(globalDir, "workflows", "shared.toml"), `
[shared]
kind = "workflow"
workspace_provider = "github"

[[shared.nodes]]
uses = "g"
`)
	repoDir := filepath.Join(tmpHome, "workspace_dirs", "github.com", "org", "repo")
	writeFile(t, filepath.Join(repoDir, ".plect", "workflows", "shared.toml"), `
[shared]
kind = "workflow"
workspace_provider = "gitlab"

[[shared.nodes]]
uses = "r"
`)
	workspaceDirPath := filepath.Join(repoDir, "session")
	if err := os.MkdirAll(workspaceDirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.LoadWorkflows(workspaceDirPath)
	if err == nil || !strings.Contains(err.Error(), "workspace_provider") {
		t.Fatalf("expected workspace_provider redeclaration error, got %v", err)
	}
}

// A provider's own contracts are checked at load: a session name reads its
// own captures, and a cleanup reads its own declared outputs.
func TestLoadWorkspaceProviders_ContractPathsAreChecked(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a name naming a capture the match does not declare",
			body: `[p]
kind  = "workspace_provider"
match = '^x/(?P<owner>[^/]+)$'
name  = { expr = "match.owner + '/' + match.repo" }

[p.setup]
type    = "exec"
command = "true"

[p.health.alive]
type = "noop"
`,
			want: "match.repo",
		},
		{
			name: "a cleanup naming an output the schema does not declare",
			body: `[p]
kind = "workspace_provider"

[p.setup]
type    = "exec"
command = "true"

[p.health.alive]
type = "noop"

[p.cleanup]
type    = "exec"
command = "true"
args    = [{ from = "self.outputs.branch" }]

[p.outputs_schema]
type = "object"

[p.outputs_schema.properties]
workspace_dir = { type = "string" }
`,
			want: "self.outputs.branch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDir := t.TempDir()
			writeProviderDoc(t, baseDir, "p", tt.body)
			_, err := (&Config{BaseDir: baseDir}).LoadWorkspaceProviders()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected a load error naming %q, got %v", tt.want, err)
			}
		})
	}
}

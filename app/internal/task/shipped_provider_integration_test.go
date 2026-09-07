//go:build integration

package task

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// A workspace provider's receiver is the executable its hooks invoke, so what
// is under contract is the invocation that executable observes — its argv,
// in order, and for a hook that invokes more than one, the order of those
// calls. Each plugin shipping providers records that in its own testdata;
// this regenerates the record from the shipped declaration and compares.
//
// The corpus is whatever the plugin root declares. Nothing here names a
// plugin: core must not know which providers exist.
//
// Set PLECT_UPDATE_PROVIDER_RECORDS=1 to rewrite the records after an
// intended contract change.

func repoPluginDirs(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	manifests, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "plugins", "*", "plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	dirs := make([]string, 0, len(manifests))
	for _, path := range manifests {
		dirs = append(dirs, filepath.Dir(path))
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		t.Skip("no plugins in this tree")
	}
	return dirs
}

// spyPlugin copies one plugin into a temp directory and replaces every
// executable it declares with a script that records its argv and then runs
// the shell source answer supplies for it. The declarations are the shipped
// ones; only what they invoke is substituted, so the recorded argv is the
// argv the real executable would have received.
func spyPlugin(t *testing.T, source string, argvLog string, answer func(name string) string) plugins.Mounted {
	t.Helper()
	manifest, err := plugins.LoadManifest(source)
	if err != nil {
		t.Fatalf("LoadManifest(%s): %v", source, err)
	}
	dir := t.TempDir()
	if out, err := exec.Command("cp", "-a", source+string(filepath.Separator)+".", dir).CombinedOutput(); err != nil {
		t.Fatalf("copy plugin: %v: %s", err, out)
	}
	for _, executable := range manifest.Executables {
		path := filepath.Join(dir, executable.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		spy := "#!/usr/bin/env bash\n" +
			"{ printf '%s' " + shellQuoteForTest(executable.Name) + "; for a in \"$@\"; do printf '\\036%s' \"$a\"; done; printf '\\035'; } >> " + shellQuoteForTest(argvLog) + "\n" +
			answer(executable.Name) + "\n"
		if err := os.WriteFile(path, []byte(spy), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return plugins.Mounted{ID: "acme-mirror/" + filepath.Base(source), Dir: dir, Manifest: manifest}
}

func shellQuoteForTest(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// stubHostPlect keeps an ambient `plect` off PATH, so this corpus's record
// never depends on whether the host has one installed and can't reach its
// real `~/.local/share/plect` state.
func stubHostPlect(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plect")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// recordedCalls reads the spy log into one line per invocation, with any path
// inside the plugin recorded relative to it so the record carries no
// checkout or temp-directory path.
func recordedCalls(t *testing.T, argvLog, pluginDir string) []string {
	t.Helper()
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var calls []string
	for _, record := range strings.Split(strings.TrimSuffix(string(raw), "\x1d"), "\x1d") {
		if record == "" {
			continue
		}
		fields := strings.Split(record, "\x1e")
		quoted := make([]string, len(fields))
		for i, f := range fields {
			if rest, ok := strings.CutPrefix(f, pluginDir+string(filepath.Separator)); ok {
				f = "<plugin>/" + filepath.ToSlash(rest)
			}
			quoted[i] = fmt.Sprintf("%q", f)
		}
		calls = append(calls, strings.Join(quoted, " "))
	}
	return calls
}

func TestShippedProviders_InvocationsMatchTheirPluginsRecord(t *testing.T) {
	for _, source := range repoPluginDirs(t) {
		t.Run(filepath.Base(source), func(t *testing.T) {
			stubHostPlect(t)
			// A real directory: unlike an exec-type provider, a shell-type
			// one can `mkdir` under it directly.
			workspaceDirsRoot := t.TempDir()
			argvLog := filepath.Join(t.TempDir(), "argv.log")
			// Every provider hook that parses stdout wants one outputs
			// document; the keys are the union the shipped providers declare
			// as required.
			mounted := spyPlugin(t, source, argvLog, func(string) string {
				return `printf '%s' '{"workspace_dir":"/spy/workspace","branch":"spy-branch","owner":"acme","concept_id":"c","concept_path":"/spy/c.md"}'`
			})
			cfg := &config.Config{PluginDirs: []string{mounted.Dir}, Plugins: []plugins.Mounted{mounted}}
			providers, err := cfg.LoadWorkspaceProviders()
			if err != nil {
				t.Fatalf("LoadWorkspaceProviders: %v", err)
			}
			if len(providers) == 0 {
				t.Skip("this plugin ships no workspace provider")
			}
			// Keyed by the declaration's own id, not by the catalog address that
			// depends on the alias this harness mounts under, so the record
			// stays the same under any alias.
			byID := make(map[string]config.WorkspaceProviderConfig, len(providers))
			ids := make([]string, 0, len(providers))
			for _, prov := range providers {
				byID[prov.ID] = prov
				ids = append(ids, prov.ID)
			}
			sort.Strings(ids)

			var b strings.Builder
			for _, id := range ids {
				prov := byID[id]
				for _, hook := range []string{"setup", "cleanup", "subscribe"} {
					if err := os.Remove(argvLog); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					fmt.Fprintf(&b, "== %s / %s\n", id, hook)
					if err := runProviderHook(t, prov, hook, mounted, workspaceDirsRoot); err != nil {
						msg := strings.ReplaceAll(err.Error(), workspaceDirsRoot, "/spy/workspace_dirs")
						fmt.Fprintf(&b, "declined: %s\n\n", msg)
						continue
					}
					for i, call := range recordedCalls(t, argvLog, mounted.Dir) {
						fmt.Fprintf(&b, "call[%d]: %s\n", i, strings.ReplaceAll(call, workspaceDirsRoot, "/spy/workspace_dirs"))
					}
					b.WriteString("\n")
				}
			}
			record := filepath.Join(source, "testdata", "workspace-provider-invocations.txt")
			if os.Getenv("PLECT_UPDATE_PROVIDER_RECORDS") == "1" {
				if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(record, []byte(strings.TrimRight(b.String(), "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("rewrote %s", record)
				return
			}
			want, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("a plugin shipping workspace providers records the invocations its hooks must produce: %v", err)
			}
			if produced := strings.TrimRight(b.String(), "\n") + "\n"; string(want) != produced {
				t.Errorf("invocations changed.\n--- recorded\n%s\n--- produced\n%s", want, produced)
			}
		})
	}
}

// runProviderHook drives one hook with a fixed, generic set of values, so a
// record is reproducible and carries no provider vocabulary.
func runProviderHook(t *testing.T, prov config.WorkspaceProviderConfig, hook string, mounted plugins.Mounted, workspaceDirsRoot string) error {
	t.Helper()
	inputs := map[string]any{}
	for key := range declaredProviderInputs(prov) {
		inputs[key] = "stand-in-" + key
	}
	vars := effect.WorkflowHookVars{
		ResourceID:        "example://acme/widget/1",
		SessionName:       "acme/widget-1",
		WorkspaceDirsRoot: workspaceDirsRoot,
		SessionInputs:     map[string]any{},
		Inputs:            inputs,
		Plugins:           []plugins.Mounted{mounted},
		SourcePath:        prov.SourcePath,
	}
	switch hook {
	case "setup":
		_, _, err := RunWorkflowSetup(prov, vars, map[string]*contract.TaskState{}, nil)
		return err
	case "cleanup":
		vars.Force = true
		vars.CleanupInputs = map[string]string{"delete_branch": "true"}
		tasks := map[string]*contract.TaskState{
			contract.WorkflowPseudoNodeID: {
				Scope:   contract.TaskScopeSession,
				Status:  contract.TaskStatusProduced,
				Outputs: map[string]any{"workspace_dir": "/spy/workspace", "branch": "spy-branch"},
			},
		}
		return RunWorkflowCleanup(prov, vars, tasks, nil)
	default:
		return effect.RunWorkspaceProviderSubscribe(prov, effect.SubscribeHookVars{
			ResourceID:  vars.ResourceID,
			SessionName: vars.SessionName,
			Plugins:     vars.Plugins,
			SourcePath:  prov.SourcePath,
		})
	}
}

func declaredProviderInputs(prov config.WorkspaceProviderConfig) map[string]any {
	schema := prov.InputsSchema
	if schema == nil {
		return nil
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	return properties
}

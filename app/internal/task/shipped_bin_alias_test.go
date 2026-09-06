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

// mountShippedPlugin mounts one shipped plugin directory (repo-root-relative,
// e.g. "plugins/github") under alias, mirroring loadShippedCatalogTasks'
// id-construction but parameterized on the alias so a caller can register the
// catalog under something other than "official".
func mountShippedPlugin(t *testing.T, repoRoot, alias, rel string) plugins.Mounted {
	t.Helper()
	dir := filepath.Join(repoRoot, rel)
	m, err := plugins.LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest(%s): %v", rel, err)
	}
	id := alias + "/" + strings.TrimPrefix(rel, "plugins/")
	return plugins.Mounted{ID: id, Dir: dir, Manifest: m}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// TestShippedGithubOkf_BinReferencesResolveUnderArbitraryAlias is the
// acceptance test for plugin-local `bin` resolution (docs/design/
// plugin-packaging.md, "Plugin-local bin resolution"): every shipped github
// and okf observer / workspace-provider / effect action must resolve its own
// plugin's executables regardless of which alias the operator registered the
// catalog under. "official" is deliberately avoided — a regression that
// silently re-depends on that one specific alias would otherwise pass
// unnoticed.
func TestShippedGithubOkf_BinReferencesResolveUnderArbitraryAlias(t *testing.T) {
	const alias = "acme-mirror"
	repoRoot := repoRootForTest(t)

	github := mountShippedPlugin(t, repoRoot, alias, "plugins/github")
	okf := mountShippedPlugin(t, repoRoot, alias, "plugins/okf")
	mounted := []plugins.Mounted{github, okf}

	cfg := &config.Config{
		PluginDirs: []string{github.Dir, okf.Dir},
		Plugins:    mounted,
	}

	resDefs, err := cfg.LoadResourceDefs()
	if err != nil {
		t.Fatalf("LoadResourceDefs: %v", err)
	}
	provDefs, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: %v", err)
	}
	taskDefs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	if len(resDefs) == 0 || len(provDefs) == 0 || len(taskDefs) == 0 {
		t.Fatalf("expected github+okf to declare resources, workspace providers, and tasks; got %d/%d/%d", len(resDefs), len(provDefs), len(taskDefs))
	}

	pluginsRoot := filepath.Join(repoRoot, "plugins")
	for id, def := range resDefs {
		observer := def
		bins := config.MountedBins{Mounted: mounted, SourcePath: observer.SourcePath}
		eval := lang.Eval{
			Roots: lang.Roots{
				"resource":  map[string]any{"id": "owner:test", "revision": "abc123"},
				"workspace": map[string]any{"dir": "/tmp/wd", "branch": "b"},
				"session":   map[string]any{"name": "test-session"},
				"judges":    []any{},
			},
			Bin: func(ref string) (string, error) { return bins.ResolveBin(ref, observer.Ownership()) },
		}
		for field, action := range map[string]*lang.Action{"observe": observer.Observe, "finalize": observer.Finalize} {
			if action == nil {
				continue
			}
			execution, err := eval.Run(t.TempDir(), action, nil)
			if err != nil {
				t.Errorf("resource %q %s: %v", id, field, err)
				continue
			}
			if !strings.HasPrefix(execution.Argv[0], pluginsRoot) {
				t.Errorf("resource %q %s: argv[0] = %q, want an executable inside %s", id, field, execution.Argv[0], pluginsRoot)
			}
		}
	}

	for id, prov := range provDefs {
		provider := prov
		bins := config.MountedBins{Mounted: mounted, SourcePath: provider.SourcePath}
		eval := lang.Eval{
			Roots: lang.Roots{
				"resource": map[string]any{"id": "example://acme/widget/1"},
				"session":  map[string]any{"name": "test-session", "inputs": map[string]any{}},
				"inputs":   map[string]any{},
				"config":   map[string]any{"workspace_dirs_root": "/tmp/workspace_dirs"},
				"prev":     map[string]any{},
				"self":     map[string]any{"outputs": map[string]any{"workspace_dir": "/tmp/wd", "branch": "b"}},
				"cleanup":  map[string]any{"inputs": map[string]any{}},
				"force":    false,
			},
			Bin: func(ref string) (string, error) { return bins.ResolveBin(ref, provider.Ownership()) },
		}
		for field, action := range map[string]*lang.Action{
			"setup":     provider.Setup,
			"cleanup":   provider.Cleanup,
			"subscribe": provider.Subscribe,
		} {
			if action == nil {
				continue
			}
			execution, err := eval.Run(t.TempDir(), action, nil)
			if err != nil {
				t.Errorf("workspace provider %q %s: %v", id, field, err)
				continue
			}
			if !strings.HasPrefix(execution.Argv[0], pluginsRoot) && action.Type != lang.ActionShell {
				t.Errorf("workspace provider %q %s: argv[0] = %q, want an executable inside %s", id, field, execution.Argv[0], pluginsRoot)
			}
		}
	}

	session := SessionVars{Name: "test-session", ResourceID: "owner:test", WorkspaceDirPath: "/tmp/wd", Plugins: mounted}
	inputs := map[string]any{
		"owner":            "acme",
		"assignees":        "",
		"instruction":      "",
		"app_id":           "123456",
		"installation_id":  "987654",
		"private_key_path": "/etc/plect/gh-app.pem",
	}
	for id, def := range taskDefs {
		ctx := RenderContext{
			// gh_guard/gh_app_guard's health.alive reads back the directory
			// their own setup produced, the same way cleanup does.
			Self:       map[string]any{"dir": "/tmp/plect-gh-guard.x"},
			Prev:       map[string]any{},
			Inputs:     inputs,
			Session:    session,
			SourcePath: def.SourcePath,
		}
		for field, action := range map[string]*lang.Action{
			"setup":           def.Setup,
			"cleanup":         def.Cleanup,
			"health.alive":    def.Health.AliveProbe(),
			"health.activity": def.Health.ActivityProbe(),
		} {
			// A noop action deliberately runs nothing, so there is nothing
			// for a resolution check to exercise.
			if action == nil || action.Type == lang.ActionNoop {
				continue
			}
			env := setupRoots(ctx)
			if strings.HasPrefix(field, "health") {
				env = healthRoots(ctx)
			} else if field == "cleanup" {
				env = cleanupRoots(ctx)
			}
			resolved, err := resolveEffect(action, env, ctx, def.Ownership(), nil)
			if err != nil {
				t.Errorf("effect %q %s: %v", id, field, err)
				continue
			}
			resolved.Close()
		}
	}
}

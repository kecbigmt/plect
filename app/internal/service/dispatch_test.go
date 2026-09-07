package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// githubResolverFields is the resolver pair a fixture provider declares
// under its own table.
const githubResolverFields = `match = '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(issues|pull)/(?P<number>\d+)'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }
`

// providerDoc wraps one provider's fields as a declaration document. fields
// carries the table's own keys; tables holds any nested table, with %[1]s
// standing for the id so a fixture never repeats it.
func providerDoc(id, fields, tables string) string {
	doc := "[" + id + "]\nkind = \"workspace_provider\"\n" + fields
	if tables != "" {
		doc += "\n" + fmt.Sprintf(tables, id)
	}
	return doc
}

// providerRunningScript builds a fixture provider whose setup runs one
// literal script. The script is quoted into the document rather than
// embedded raw, so a fixture may use whatever quoting its shell needs.
func providerRunningScript(id, script string) string {
	return providerDoc(id, githubResolverFields, `[%[1]s.setup]
type    = "exec"
command = "sh"
args    = ["-c", `+fmt.Sprintf("%q", script)+`, "provider"]
`)
}

// providerCreatingWorkspace is the provider a fixture needs when the session
// must actually have a directory: setup creates it, then reports it.
func providerCreatingWorkspace(id, workspaceDir string) string {
	return providerRunningScript(id, fmt.Sprintf("mkdir -p %s\nprintf '{\"workspace_dir\":\"%s\"}'\n", workspaceDir, workspaceDir))
}

// providerEchoingOutputs is the provider most fixtures need: a resolver, and
// a setup that reports one fixed outputs document.
func providerEchoingOutputs(id, outputsJSON string) string {
	return providerDoc(id, githubResolverFields, `[%[1]s.setup]
type    = "exec"
command = "printf"
args    = ['`+outputsJSON+`']
`)
}

func TestDispatchResource_AutoUniqueMatch(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workdir":"/tmp/x"}`))

	disp, matched, err := dispatchResource(cfg, "", "https://github.com/org/repo/issues/42")
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected resolver match")
	}
	if disp.Name != "org/repo-42" {
		t.Errorf("Name = %q, want org/repo-42", disp.Name)
	}
	if disp.Workflow.ID != "gh" {
		t.Errorf("Workflow = %q, want gh", disp.Workflow.ID)
	}
}

func TestDispatchResource_NoMatchFallsThrough(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workdir":"/tmp/x"}`))

	_, matched, err := dispatchResource(cfg, "", "https://jira.example.com/browse/PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Error("non-matching resource must not dispatch")
	}
}

func TestDispatchResource_AmbiguousIsError(t *testing.T) {
	baseDir := t.TempDir()
	for _, id := range []string{"a", "b"} {
		for _, sub := range []string{"workflows", "workspaces"} {
			if err := os.MkdirAll(filepath.Join(baseDir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		provID, prov := providerAside(id, providerEchoingOutputs(id, `{"workdir":"/tmp/x"}`))
		if err := os.WriteFile(filepath.Join(baseDir, "workspaces", provID+".toml"), []byte(prov), 0o644); err != nil {
			t.Fatal(err)
		}
		content := "[" + id + "]\nkind = \"workflow\"\nworkspace_provider = \"" + provID + "\"\n\n[[" + id + ".nodes]]\nuses = \"noop\"\n"
		if err := os.WriteFile(filepath.Join(baseDir, "workflows", id+".toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "tasks", "noop.toml"), []byte("[noop]\nkind = \"effect\"\n\n[noop.setup]\ntype = \"shell\"\nscript = \"echo '{}'\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: baseDir}

	_, _, err := dispatchResource(cfg, "", "https://github.com/org/repo/issues/1")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("error should suggest --workflow: %v", err)
	}
}

func TestDispatchResource_AutoSelectFalseSkippedUnlessExplicit(t *testing.T) {
	baseDir := t.TempDir()
	for _, sub := range []string{"workflows", "workspaces", "tasks"} {
		if err := os.MkdirAll(filepath.Join(baseDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prov := providerEchoingOutputs("github", `{"workdir":"/tmp/x"}`)
	if err := os.WriteFile(filepath.Join(baseDir, "workspaces", "github.toml"), []byte(prov), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "tasks", "noop.toml"), []byte("[noop]\nkind = \"effect\"\n\n[noop.setup]\ntype = \"shell\"\nscript = \"echo '{}'\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "workflows", "claude.toml"), []byte("[claude]\nkind = \"workflow\"\nworkspace_provider = \"github\"\n\n[[claude.nodes]]\nuses = \"noop\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "workflows", "codex.toml"), []byte("[codex]\nkind = \"workflow\"\nworkspace_provider = \"github\"\nauto_select = false\n\n[[codex.nodes]]\nuses = \"noop\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: baseDir}
	url := "https://github.com/org/repo/issues/1"

	disp, matched, err := dispatchResource(cfg, "", url)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || disp.Workflow.ID != "claude" {
		t.Fatalf("auto dispatch = matched %v workflow %q, want claude", matched, disp.Workflow.ID)
	}

	disp, matched, err = dispatchResource(cfg, "codex", url)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || disp.Workflow.ID != "codex" {
		t.Fatalf("explicit dispatch = matched %v workflow %q, want codex", matched, disp.Workflow.ID)
	}
}

func TestDispatchResource_FlagWithResolverMismatchIsError(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workdir":"/tmp/x"}`))

	_, _, err := dispatchResource(cfg, "gh", "not-a-github-url")
	if err == nil {
		t.Fatal("expected resolver mismatch error for explicit --workflow")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestDispatchResource_FlagWithoutResolverIsIdentityForCaller(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "plain",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})

	_, matched, err := dispatchResource(cfg, "plain", "my-session")
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Error("resolver-less workflows must not match in dispatch (identity is the caller's branch)")
	}
}

func TestValidateSessionName(t *testing.T) {
	for _, ok := range []string{"org/repo-1", "org/repo-1+review", "my_session", "a"} {
		if err := validateSessionName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "with:colon", "with.dot", "-leading-dash", "https://github.com/x"} {
		if err := validateSessionName(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestCreate_ResolverPath(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerCreatingWorkspace("gh", workdir))

	url := "https://github.com/org/repo/issues/42"
	result, err := Create(cfg, store, CreateParams{URL: url})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No --tag: the workflow id ("gh") is the default session-identity tag,
	// so cross-tool sessions on one resource never share a session.
	if result.SessionName != "org/repo-42+gh" {
		t.Errorf("SessionName = %q, want org/repo-42+gh", result.SessionName)
	}
	s := store.Get("org/repo-42+gh")
	if s.ResourceID != url || s.Alias != url {
		t.Errorf("ResourceID/Alias = %q/%q, want both %q", s.ResourceID, s.Alias, url)
	}
}

func TestCreate_IdentityPath(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "scratch",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "scratch", providerDoc("scratch", "", `[%[1]s.setup]
type    = "exec"
command = "sh"
args    = ["-c", `+fmt.Sprintf("%q", "mkdir -p "+workdir+"\nprintf '{\"workspace_dir\":\""+workdir+"\"}'\n")+`, "provider"]
`))

	result, err := Create(cfg, store, CreateParams{URL: "my-experiment", Workflow: "scratch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// No --tag: the workflow id ("scratch") is the default session-identity tag.
	if result.SessionName != "my-experiment+scratch" {
		t.Errorf("SessionName = %q, want my-experiment+scratch (identity + default tag)", result.SessionName)
	}
	s := store.Get("my-experiment+scratch")
	if s.ResourceID != "my-experiment" {
		t.Errorf("ResourceID = %q", s.ResourceID)
	}
}

func TestCreate_IdentityRequiresExplicitWorkflow(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "scratch",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})

	_, err := Create(cfg, store, CreateParams{URL: "my-experiment"})
	if err == nil {
		t.Fatal("identity create without --workflow must fail")
	}
	if !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestCreate_ResourceAllowlistBlocks(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	cfg.ResourceAllowlist = []string{`^https://github\.com/allowed-org/`}
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workspace_dir":"/tmp/x"}`))

	_, err := Create(cfg, store, CreateParams{URL: "https://github.com/evil-org/repo/issues/1"})
	if err == nil {
		t.Fatal("expected allowlist rejection")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Errorf("want ErrRepoNotAllowed, got %v", err)
	}
}

func TestCreate_SessionGuardBlocksCrossOwner(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	// The orchestrator pane exports this guard; it matches resolved names like
	// "acme/repo-1" but not "exampleorg/repo-26".
	cfg.SessionGuard = "^acme/"
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workspace_dir":"/tmp/x"}`))

	// acme's orchestrator must not be able to dispatch exampleorg work.
	_, err := Create(cfg, store, CreateParams{URL: "https://github.com/exampleorg/repo/issues/26"})
	if err == nil {
		t.Fatal("expected session-guard rejection for cross-owner dispatch")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Errorf("want ErrRepoNotAllowed, got %v", err)
	}
}

func TestResolveSession_ByAliasAndResolver(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerCreatingWorkspace("gh", workdir))

	url := "https://github.com/org/repo/issues/77"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatal(err)
	}

	// By name (the workflow-id default tag is part of the session name).
	if name, _, err := resolveSession(cfg, store, "org/repo-77+gh"); err != nil || name != "org/repo-77+gh" {
		t.Errorf("by name: %v / %q", err, name)
	}
	// By alias (the original input is the create-time alias).
	if name, _, err := resolveSession(cfg, store, url); err != nil || name != "org/repo-77+gh" {
		t.Errorf("by alias: %v / %q", err, name)
	}
}

func TestResolveSession_ResolverDerivationAfterRuleChange(t *testing.T) {
	// Session created under an old rule: alias differs from today's input.
	store := testStore(t)
	seedSession(t, store, "org/repo-5", "org/repo", 5, "gh", nil)

	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workspace_dir":"/tmp/x"}`))

	// seedSession sets URL to .../issues/1 — look up via a never-stored URL
	// shape that the resolver derives to the right name. Note: pull URL for
	// the same number maps to the same session id.
	name, _, err := resolveSession(cfg, store, "https://github.com/org/repo/pull/5")
	if err != nil {
		t.Fatalf("resolver derivation lookup: %v", err)
	}
	if name != "org/repo-5" {
		t.Errorf("name = %q", name)
	}
}

func TestUp_ResolverAutoCreate(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerCreatingWorkspace("gh", workdir))

	url := "https://github.com/org/repo/issues/88"
	result, err := Up(cfg, store, UpParams{Identifier: url})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if result.SessionName != "org/repo-88+gh" {
		t.Errorf("SessionName = %q, want org/repo-88+gh (workflow-id default tag)", result.SessionName)
	}
	s := store.Get("org/repo-88+gh")
	if st := s.Nodes[contract.WorkflowPseudoNodeID]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("auto-create did not run workflow setup: %+v", st)
	}
}

// Up must surface dispatch errors exactly as Create does — falling through
// to the legacy path on ambiguity would let explicit and auto-created paths
// disagree about the same resource.
func TestUp_AmbiguousResolverDispatchIsError(t *testing.T) {
	store := testStore(t)
	baseDir := t.TempDir()
	dir := filepath.Join(baseDir, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	providersDir := filepath.Join(baseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		provID, prov := providerAside(id, providerEchoingOutputs(id, `{"workdir":"/tmp/x"}`))
		if err := os.WriteFile(filepath.Join(providersDir, provID+".toml"), []byte(prov), 0o644); err != nil {
			t.Fatal(err)
		}
		content := "[" + id + "]\nkind = \"workflow\"\nworkspace_provider = \"" + provID + "\"\n\n[[" + id + ".nodes]]\nuses = \"noop\"\n"
		if err := os.WriteFile(filepath.Join(dir, id+".toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "tasks", "noop.toml"), []byte("[noop]\nkind = \"effect\"\n\n[noop.setup]\ntype = \"shell\"\nscript = \"echo '{}'\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir(), BaseDir: baseDir}

	_, err := Up(cfg, store, UpParams{Identifier: "https://github.com/org/repo/issues/1"})
	if err == nil {
		t.Fatal("expected ambiguity error from Up")
	}
	if !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("error should suggest --workflow: %v", err)
	}
}

// Every create path must satisfy the session identity contract.
func TestCreate_SetsResourceIDAndAlias(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cfg := writeWorkflowFixture(t, t.TempDir(), "bridge",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "bridge", providerCreatingWorkspace("bridge", workdir))
	url := "https://github.com/org/repo/issues/55"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatal(err)
	}
	s := store.Get("org/repo-55+bridge")
	if s.ResourceID != url || s.Alias != url {
		t.Errorf("ResourceID/Alias = %q/%q, want %q", s.ResourceID, s.Alias, url)
	}
}

// `plect cd` rides the same state v3 lookup contract as show/down/destroy:
// resolver-derived resource ids and aliases must resolve.
func TestWorkdir_ResolverAndAliasLookup(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerCreatingWorkspace("gh", workdir))

	url := "https://github.com/org/repo/issues/66"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatal(err)
	}

	// A tagged session resolves by its full name or its create-time alias; a
	// bare resource base no longer resolves because it cannot disambiguate the
	// session-identity tag (which workflow's session?).
	for _, id := range []string{"org/repo-66+gh", url} {
		got, err := WorkspaceDir(cfg, store, id)
		if err != nil {
			t.Errorf("WorkspaceDir(%q): %v", id, err)
			continue
		}
		if got != workdir {
			t.Errorf("WorkspaceDir(%q) = %q, want %q", id, got, workdir)
		}
	}

	if _, err := WorkspaceDir(cfg, store, "no-such-session"); err == nil {
		t.Error("unknown identifier should error")
	}
}

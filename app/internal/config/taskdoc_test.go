package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// The container loads end to end: the declaration carries the completion
// contract, instruction is its own field, and the contract pass resolves
// every completion key against the two schemas that declare them.
func TestLoadTaskDocuments_LoadsAndValidates(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), `
[issue_pr]
kind  = "resource_observer"
match = '^https://github\.com/'

[issue_pr.observe]
type    = "exec"
command = "true"

[issue_pr.state_schema]
type = "object"

[issue_pr.state_schema.properties]
resource_kind = { type = "string" }
revision      = { type = "string" }
`)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "Review a resource and record a verdict"
resource_observer = "issue_pr"
instructions      = [{ text = "Review {{ resource.id }} and record a verdict against its current revision." }]

[review.state_schema]
type = "object"

[review.state_schema.properties]
verdict_revision = { type = "string" }

[review.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull", "issue"] },
  { expr = "self.state.verdict_revision == resource.state.revision" },
]
`)
	cfg := &Config{BaseDir: base}
	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments: %v", err)
	}
	doc, ok := docs["review"]
	if !ok {
		t.Fatalf("review not loaded: %+v", docs)
	}
	if doc.ResourceObserver != "issue_pr" {
		t.Errorf("ResourceObserver = %q", doc.ResourceObserver)
	}
	if !strings.HasPrefix(doc.Instruction, "Review {{ resource.id }}") {
		t.Errorf("Instruction = %q, want the declared instruction", doc.Instruction)
	}
	if doc.DoneWhen == nil || len(doc.DoneWhen.All) != 2 {
		t.Fatalf("done_when = %+v, want two leaves", doc.DoneWhen)
	}
	if !doc.DoneWhen.All[1].IsExpr() {
		t.Errorf("second leaf = %+v, want the expression leaf", doc.DoneWhen.All[1])
	}
	observers, err := cfg.LoadResourceDefs()
	if err != nil {
		t.Fatalf("LoadResourceDefs: %v", err)
	}
	if err := cfg.ValidateTaskDocuments(docs, observers, nil); err != nil {
		t.Fatalf("ValidateTaskDocuments: %v", err)
	}
}

// A completion key the declared observer does not publish is a load error:
// the observer's contract is what says the key exists.
func TestValidateTaskDocuments_UnpublishedKeyRejected(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), `
[issue_pr]
kind  = "resource_observer"
match = '^https://github\.com/'

[issue_pr.observe]
type    = "exec"
command = "true"

[issue_pr.state_schema]
type = "object"

[issue_pr.state_schema.properties]
resource_kind = { type = "string" }
`)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "Reads a key its observer never publishes"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]

[review.done_when]
all = [{ check = "resource.state.nope", eq = "yes" }]
`)
	cfg := &Config{BaseDir: base}
	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments: %v", err)
	}
	observers, err := cfg.LoadResourceDefs()
	if err != nil {
		t.Fatalf("LoadResourceDefs: %v", err)
	}
	err = cfg.ValidateTaskDocuments(docs, observers, nil)
	if err == nil {
		t.Fatal("expected a load error for an unpublished completion key")
	}
	if !strings.Contains(err.Error(), "resource.state.nope") {
		t.Errorf("error = %v, want it to name the key", err)
	}
}

// A self.state key the document does not declare is the same load error from
// the other side: this document's own state_schema is what says it keeps it.
func TestValidateTaskDocuments_UndeclaredSelfStateKeyRejected(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "Reads state it never declared"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]

[review.done_when]
all = [{ expr = "self.state.verdict_revision == resource.state.revision" }]
`)
	cfg := &Config{BaseDir: base}
	docs, observers := loadDocsAndObservers(t, cfg)
	err := cfg.ValidateTaskDocuments(docs, observers, nil)
	if err == nil {
		t.Fatal("expected a load error for an undeclared self.state key")
	}
	if !strings.Contains(err.Error(), "verdict_revision") {
		t.Errorf("error = %v, want it to name the key", err)
	}
}

func TestValidateTaskDocuments_UnknownObserverRejected(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "Written for an observer nothing declares"
resource_observer = "no_such_observer"
instructions      = [{ text = "Review it." }]

[review.done_when]
all = [{ check = "resource.state.revision", ne = "" }]
`)
	cfg := &Config{BaseDir: base}
	docs, observers := loadDocsAndObservers(t, cfg)
	err := cfg.ValidateTaskDocuments(docs, observers, nil)
	if err == nil {
		t.Fatal("expected a load error for an unresolvable resource_observer")
	}
	if !strings.Contains(err.Error(), "no_such_observer") {
		t.Errorf("error = %v, want it to name the reference", err)
	}
}

func TestLoadTaskDocuments_RejectsDuplicateIDInOneLayer(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "tasks", "a.toml"), duplicateTaskDoc)
	writeFile(t, filepath.Join(base, "tasks", "b.toml"), duplicateTaskDoc)
	cfg := &Config{BaseDir: base}
	_, err := cfg.LoadTaskDocuments("")
	if err == nil {
		t.Fatal("expected a load error for two documents declaring one id in the same layer")
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("error = %v, want it to name the id", err)
	}
}

// Clone content must not declare the work it is about: a task document found
// under the workspace directory is rejected rather than quietly ignored.
func TestLoadTaskDocuments_RejectsWorkspaceDirLayer(t *testing.T) {
	base := t.TempDir()
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, ".plect", "tasks", "review.toml"), duplicateTaskDoc)
	cfg := &Config{BaseDir: base}
	_, err := cfg.LoadTaskDocuments(workspace)
	if err == nil {
		t.Fatal("expected a load error for a task document inside the workspace directory")
	}
	if !strings.Contains(err.Error(), "workspace directory") {
		t.Errorf("error = %v, want it to say where the document was found", err)
	}
}

const minimalObserver = `
[issue_pr]
kind  = "resource_observer"
match = '^https://github\.com/'

[issue_pr.observe]
type    = "exec"
command = "true"

[issue_pr.state_schema]
type = "object"

[issue_pr.state_schema.properties]
revision = { type = "string" }
`

const duplicateTaskDoc = `
[review]
kind              = "task"
description       = "Review a resource"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]
`

func loadDocsAndObservers(t *testing.T, cfg *Config) (map[string]TaskDocument, map[string]ResourceDef) {
	t.Helper()
	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments: %v", err)
	}
	observers, err := cfg.LoadResourceDefs()
	if err != nil {
		t.Fatalf("LoadResourceDefs: %v", err)
	}
	return docs, observers
}

// A plugin-shipped document resolves its own plugin's observer by a relative
// reference: both declarations sit in the same plugin layer, which is the
// namespace a relative reference resolves in.
func TestValidateTaskDocuments_PluginDocumentResolvesItsOwnObserver(t *testing.T) {
	pluginDir := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "plugin.toml"), `
name        = "acme"
version     = "0.1.0"
description = "test plugin"
`)
	writeFile(t, filepath.Join(pluginDir, "config", "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(pluginDir, "config", "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "A plugin-shipped document reading its own plugin's observer"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]

[review.done_when]
all = [{ check = "resource.state.revision", ne = "" }]
`)
	cfg := &Config{
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/plugins/acme", Dir: pluginDir}},
	}
	docs, observers := loadDocsAndObservers(t, cfg)
	if _, ok := docs["official.plugins.acme.review"]; !ok {
		t.Fatalf("official.plugins.acme.review not loaded: %+v", docs)
	}
	if err := cfg.ValidateTaskDocuments(docs, observers, nil); err != nil {
		t.Fatalf("ValidateTaskDocuments: %v", err)
	}
}

// One layer declaring an id as both a document and an effect is ambiguous, and
// traversal order must not pick a winner.
func TestLoadTaskDeclarations_SameLayerCollisionRejected(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), duplicateTaskDoc)
	writeFile(t, filepath.Join(base, "tasks", "review-effect.toml"), collidingEffect)

	_, _, err := (&Config{BaseDir: base}).LoadTaskDeclarations("")
	if err == nil {
		t.Fatal("expected a load error for one id declared by both a document and an effect in one layer")
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("error = %v, want it to name the id", err)
	}
}

// Across layers the rule does not apply: each layer has its own namespace, so
// a deeper declaration of another kind shadows nothing and both load. A
// reference resolves by the kind its site expects — which is what lets a
// plugin ship a `goal_review` task document while the host declares the
// `goal_review` workflow a chain fires into.
func TestLoadTaskDeclarations_CrossLayerDifferentKindsCoexist(t *testing.T) {
	pluginDir := t.TempDir()
	base := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "tasks", "review.toml"), collidingEffect)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), duplicateTaskDoc)
	cfg := &Config{BaseDir: base, PluginDirs: []string{pluginDir}}

	docs, effects, err := cfg.LoadTaskDeclarations("")
	if err != nil {
		t.Fatalf("LoadTaskDeclarations: %v", err)
	}
	if _, ok := docs["review"]; !ok {
		t.Errorf("the user-owned document is missing: %v", docs)
	}
	if _, ok := effects["review"]; !ok {
		t.Errorf("the plugin effect is missing: %v", effects)
	}
}

// The same shape one kind up: a plugin task document and a user-owned workflow
// sharing an id both load, which is the goal_review arrangement the shipped
// okf plugin and a host config produce together.
func TestLoadDeclarations_PluginDocumentAndUserWorkflowShareAnID(t *testing.T) {
	pluginDir := t.TempDir()
	base := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(pluginDir, "config", "tasks", "goal_review.toml"), `
[goal_review]
kind              = "task"
description       = "Review a goal"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]
`)
	writeFile(t, filepath.Join(base, "workflows", "goal_review.toml"), `
[goal_review]
kind = "workflow"

[[goal_review.nodes]]
uses = "noop"
`)
	cfg := &Config{BaseDir: base, PluginDirs: []string{pluginDir}}

	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments: %v", err)
	}
	if _, ok := docs["goal_review"]; !ok {
		t.Errorf("the plugin task document is missing: %v", docs)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	if _, ok := workflows["goal_review"]; !ok {
		t.Errorf("the user-owned workflow is missing: %v", workflows)
	}
}

const collidingEffect = `
[review]
kind  = "effect"
scope = "session"

[review.setup]
type   = "shell"
script = "true"

[review.health.alive]
type = "noop"
`

// The other half of the same rule: a user-owned document reaches catalog
// content only through the catalog-qualified dotted form, whose middle
// segments are the plugin's own path under its alias.
func TestValidateTaskDocuments_UserDocumentQualifiesACatalogObserver(t *testing.T) {
	pluginDir := t.TempDir()
	base := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "A user-owned document reading a catalog observer"
resource_observer = "official.acme.issue_pr"
instructions      = [{ text = "Review it." }]

[review.done_when]
all = [{ check = "resource.state.revision", ne = "" }]
`)
	cfg := &Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	docs, observers := loadDocsAndObservers(t, cfg)
	if err := cfg.ValidateTaskDocuments(docs, observers, nil); err != nil {
		t.Fatalf("ValidateTaskDocuments: %v", err)
	}
}

// A relative reference from user-owned config must not reach catalog content:
// its validity would then depend on which catalogs happen to be enabled.
func TestValidateTaskDocuments_UserDocumentCannotReachACatalogObserverRelatively(t *testing.T) {
	pluginDir := t.TempDir()
	base := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "A user-owned document reaching catalog content without its alias"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]

[review.done_when]
all = [{ check = "resource.state.revision", ne = "" }]
`)
	cfg := &Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	docs, observers := loadDocsAndObservers(t, cfg)
	err := cfg.ValidateTaskDocuments(docs, observers, nil)
	if err == nil {
		t.Fatal("expected an unqualified reference to catalog content to be rejected")
	}
	if !strings.Contains(err.Error(), "catalog alias") {
		t.Errorf("error = %v, want it to say the reference needs its alias", err)
	}
}

// A chain's inputs are values over the chain-input roots, not templates: the
// parsed forms are what the runtime evaluates, and a literal stays a literal.
func TestLoadTaskDocuments_ChainInputsParseAsValues(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "pursue.toml"), chainingTaskDoc)
	cfg := &Config{BaseDir: base}
	docs, err := cfg.LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments: %v", err)
	}
	chains := docs["pursue"].Chains
	if len(chains) != 1 {
		t.Fatalf("chains = %+v, want one", chains)
	}
	ch := chains[0]
	if ch.ID != "goal_review" || ch.Workflow != "goal_reviewer" || ch.EffectivePlacement() != "sibling" {
		t.Errorf("chain = %+v", ch)
	}
	if ch.TaskID != "pursue" {
		t.Errorf("chain names its declaring document: got %q", ch.TaskID)
	}
	if len(ch.When.All) != 1 || ch.When.All[0].JudgePending != "goal-met" {
		t.Errorf("when = %+v", ch.When)
	}
	if got := ch.Inputs["task"]; got == nil || got.Literal != "goal_review" {
		t.Errorf("a literal input stays a literal: %+v", got)
	}
	if got := ch.Inputs["work_session"]; got == nil || got.From != "task.session" {
		t.Errorf("a projection keeps its path: %+v", got)
	}
	if got := ch.InputKeys(); len(got) != 2 || got[0] != "task" || got[1] != "work_session" {
		t.Errorf("InputKeys = %v, want them sorted", got)
	}
}

// One chain id names one chain within its document.
func TestLoadTaskDocuments_RejectsDuplicateChainID(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "pursue.toml"), chainingTaskDoc+`
[[pursue.chains]]
id       = "goal_review"
workflow = "goal_reviewer"

[pursue.chains.when]
all = [{ judge_pending = "goal-met" }]
`)
	cfg := &Config{BaseDir: base}
	if _, err := cfg.LoadTaskDocuments(""); err == nil || !strings.Contains(err.Error(), "declared more than once") {
		t.Fatalf("err = %v, want a duplicate chain id rejection", err)
	}
}

// A chain with no trigger would fire unconditionally.
func TestLoadTaskDocuments_RejectsChainWithNoTrigger(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "pursue.toml"), `
[pursue]
kind              = "task"
description       = "A document whose chain declares no trigger"
resource_observer = "issue_pr"
instructions      = [{ text = "Pursue the goal." }]

[[pursue.chains]]
id       = "goal_review"
workflow = "goal_reviewer"
`)
	cfg := &Config{BaseDir: base}
	if _, err := cfg.LoadTaskDocuments(""); err == nil || !strings.Contains(err.Error(), "declares no facts") {
		t.Fatalf("err = %v, want a triggerless chain rejection", err)
	}
}

const chainingTaskDoc = `
[pursue]
kind              = "task"
description       = "Pursue one goal until an independent reviewer confirms it"
resource_observer = "issue_pr"
instructions      = [{ text = "Pursue the goal at {{ resource.id }}." }]

[pursue.done_when]
all = [{ judge = "the goal is achieved", id = "goal-met" }]

[[pursue.chains]]
id        = "goal_review"
workflow  = "goal_reviewer"
placement = "sibling"

[pursue.chains.when]
all = [{ judge_pending = "goal-met" }]

[pursue.chains.inputs]
task         = "goal_review"
work_session = { from = "task.session" }
`

// The completion predicate's leaf forms parse off a document's declaration,
// and `budget` is a sibling of `done_when` rather than a member of it.
func TestLoadTaskDocuments_ParsesEveryCompletionLeafForm(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "review.toml"), `
[review]
kind              = "task"
description       = "Review a resource"
resource_observer = "issue_pr"
instructions      = [{ text = "Review it." }]

[[review.done_when.all]]
check = "resource.state.revision"
in    = ["merged", "closed"]

[[review.done_when.all]]
check = "resource.state.revision"
gte   = 80

[[review.done_when.all]]
judge = "reviewer approved"
id    = "ac-met"

[review.budget]
max_iterations = 5
`)
	docs, err := (&Config{BaseDir: base}).LoadTaskDocuments("")
	if err != nil {
		t.Fatalf("LoadTaskDocuments: %v", err)
	}
	dw := docs["review"].DoneWhen
	if dw == nil || len(dw.All) != 3 {
		t.Fatalf("done_when not parsed: %+v", dw)
	}
	if dw.All[0].Check != "resource.state.revision" || len(dw.All[0].In) != 2 {
		t.Errorf("in leaf not parsed: %+v", dw.All[0])
	}
	if dw.All[1].Gte == nil || *dw.All[1].Gte != 80 {
		t.Errorf("gte leaf not parsed: %+v", dw.All[1])
	}
	if dw.All[2].Judge == "" || dw.All[2].ID != "ac-met" {
		t.Errorf("judge leaf not parsed: %+v", dw.All[2])
	}
	if dw.Budget["max_iterations"] == nil {
		t.Errorf("budget not joined to the predicate it bounds: %+v", dw.Budget)
	}
}

// A leaf that is both a check and a judge is neither.
func TestLoadTaskDocuments_RejectsAmbiguousCompletionLeaf(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "issue_pr.toml"), minimalObserver)
	writeFile(t, filepath.Join(base, "tasks", "bad.toml"), `
[bad]
kind              = "task"
description       = "A document whose leaf is two leaves"
resource_observer = "issue_pr"
instructions      = [{ text = "Do it." }]

[[bad.done_when.all]]
check = "resource.state.revision"
eq    = "merged"
judge = "both set"
`)
	if _, err := (&Config{BaseDir: base}).LoadTaskDocuments(""); err == nil {
		t.Fatal("expected a load error for a leaf that is both a check and a judge")
	}
}

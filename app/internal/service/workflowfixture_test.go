package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// taskFixture is a terse spec for one task definition. Scope defaults to
// "run". Empty fields are omitted from the generated TOML. extra is raw TOML
// appended verbatim (e.g. an [outputs_schema] block).
//
// attach/capture/sendText/sendKeys build a `[terminal]` table. A verb is
// available on its own, but the writer below still fills any of the four a
// test left unset with a trivial always-succeeds stub, so a case consuming
// one it did not name resolves rather than failing. Most fixtures only care
// about one or two verbs; the
// stubs keep the other verbs inert instead of forcing every attach/capture
// test to also spell out send_text/send_keys it never exercises.
type taskFixture struct {
	id       string
	scope    string
	setup    string
	cleanup  string
	alive    string
	activity string
	attach   string
	capture  string
	sendText string
	sendKeys string
	extra    string
}

// stubTerminalVerb fills an unset [terminal] verb with a trivial
// always-succeeds command, so a fixture that only cares about (say) attach
// can leave capture/send_text/send_keys unset without tripping the
// all-or-nothing [terminal] validation.
func stubTerminalVerb(v string) string {
	if v == "" {
		return "true"
	}
	return v
}

// nodeFixture mirrors one `[[nodes]]` entry for fixture authoring. A fixture
// naming only `id` selects the effect of the same name, which is how most
// cases read.
type nodeFixture struct {
	id     string
	uses   string
	inputs map[string]*lang.Value
}

// effectFixtureDoc renders one effect declaration: the definition table, its
// lifecycle actions and probes as shell actions, its terminal verbs, and
// whatever else the case declares under that table.
func effectFixtureDoc(d taskFixture) string {
	bare, tables := splitFixtureExtra(d.id, d.extra)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\nkind = \"effect\"\n", d.id)
	if d.scope != "" {
		fmt.Fprintf(&b, "scope = %q\n", d.scope)
	}
	b.WriteString(bare)
	alive := d.alive
	if alive == "" && d.setup != "" {
		// Every setup-bearing effect must declare [health.alive]; a fixture
		// that doesn't care about liveness gets the noop filler rather than
		// spelling out an always-true shell probe at every call site.
		alive = "__noop__"
	}
	actions := []struct {
		path   string
		script string
	}{
		{"setup", d.setup},
		{"cleanup", d.cleanup},
		{"health.alive", alive},
		{"health.activity", d.activity},
	}
	if d.attach != "" || d.capture != "" || d.sendText != "" || d.sendKeys != "" {
		actions = append(actions,
			struct {
				path   string
				script string
			}{"terminal.attach", stubTerminalVerb(d.attach)},
			struct {
				path   string
				script string
			}{"terminal.capture", stubTerminalVerb(d.capture)},
			struct {
				path   string
				script string
			}{"terminal.send_text", stubTerminalVerb(d.sendText)},
			struct {
				path   string
				script string
			}{"terminal.send_keys", stubTerminalVerb(d.sendKeys)},
		)
	}
	for _, action := range actions {
		if action.script == "" {
			continue
		}
		if action.script == "__noop__" {
			fmt.Fprintf(&b, "\n[%s.%s]\ntype = \"noop\"\n", d.id, action.path)
			continue
		}
		b.WriteString(shellFixtureAction(d.id, action.path, action.script))
	}
	b.WriteString(tables)
	return b.String()
}

// writeWorkflowFixture writes a workflow file + task definitions under a
// temp config dir, returns the matching *config.Config with BaseDir set so
// cfg.LoadWorkflows / LoadTaskDefinitions pick them up.
//
// The fixture sits at <BaseDir>/workflows/<wfID>.toml plus
// <BaseDir>/tasks/<id>.toml. Service tests that used to declare tasks on
// cfg.Tasks now declare them here and freeze `wfID` onto their session.
func writeWorkflowFixture(t *testing.T, workdirsRoot, wfID string, defs []taskFixture, nodes []nodeFixture) *config.Config {
	t.Helper()
	baseDir := t.TempDir()
	tasksDir := filepath.Join(baseDir, "tasks")
	workflowsDir := filepath.Join(baseDir, "workflows")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	needObserver := false
	for _, d := range defs {
		if !fixtureDeclaresAGate(d) {
			if err := os.WriteFile(filepath.Join(tasksDir, d.id+".toml"), []byte(effectFixtureDoc(d)), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		needObserver = true
		if err := os.WriteFile(filepath.Join(tasksDir, d.id+".toml"), []byte(taskDocumentFixtureDoc(d)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if needObserver {
		resourcesDir := filepath.Join(baseDir, "resources")
		if err := os.MkdirAll(resourcesDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resourcesDir, fixtureObserverID+".toml"), []byte(fixtureObserverDoc(defs)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var w strings.Builder
	fmt.Fprintf(&w, "[%s]\nkind = \"workflow\"\n\n", wfID)
	for _, n := range nodes {
		uses := n.uses
		if uses == "" {
			uses = n.id
		}
		fmt.Fprintf(&w, "[[%s.nodes]]\n", wfID)
		if n.id != "" && n.id != uses {
			fmt.Fprintf(&w, "id = %q\n", n.id)
		}
		fmt.Fprintf(&w, "uses = %q\n", uses)
		if len(n.inputs) > 0 {
			fmt.Fprintf(&w, "[%s.nodes.inputs]\n", wfID)
			for _, k := range sortedInputKeys(n.inputs) {
				fmt.Fprintf(&w, "%s = %s\n", k, n.inputs[k].Source())
			}
		}
		w.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(workflowsDir, wfID+".toml"), []byte(w.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		WorkspaceDirsRoot: workdirsRoot,
		BaseDir:           baseDir,
	}
}

// legacyProjection matches the projections a fixture script writes inline.
// A fixture states its script the way an author does — a shell variable per
// value — but writing the bind table out per case would bury what each case
// is actually about, so these helpers derive it: one binding per distinct
// projection, named after the key it reads.
var legacyProjection = regexp.MustCompile(
	`\{\{\s*(?:get\s+)?\.(Self|Prev|Inputs|Locals|SessionInputs)\s+"([A-Za-z0-9_]+)"(?:\s+"[^"]*")?\s*\}\}` +
		`|\{\{\s*\.(Self|Prev|Inputs|Locals|SessionInputs)\.([A-Za-z0-9_]+)\s*\}\}` +
		`|\{\{\s*\.Nodes\.([A-Za-z0-9_]+)\.outputs\.([A-Za-z0-9_]+)\s*\}\}` +
		`|\{\{\s*\.Workflow\.outputs\.([A-Za-z0-9_]+)\s*\}\}` +
		`|\{\{\s*\.(SessionName|ResourceID|WorkspaceDirPath|Branch|ParentSession)\s*\}\}` +
		`|\{\{\s*(?:terminal|bin)\s+"([A-Za-z0-9_-]+)"[^}]*\}\}`)

var singleQuotedBinding = regexp.MustCompile(`'(\$\{[A-Za-z0-9_]+-\})'`)

var legacyRootPaths = map[string]string{
	"Self":          "self.outputs.",
	"Prev":          "prev.",
	"Inputs":        "inputs.",
	"Locals":        "locals.",
	"SessionInputs": "session.inputs.",
}

var legacyScalarPaths = map[string]struct{ name, path string }{
	"SessionName":      {"session_name", "session.name"},
	"ResourceID":       {"resource_id", "resource.id"},
	"WorkspaceDirPath": {"workspace_dir", "workspace.dir"},
	"Branch":           {"branch", "workspace.branch"},
	"ParentSession":    {"parent_session", "session.parent"},
}

// shellFixtureAction renders one shell action from a fixture's script,
// binding every value the script reads.
func shellFixtureAction(id, path, script string) string {
	binds := map[string]string{}
	rendered := legacyProjection.ReplaceAllStringFunc(script, func(match string) string {
		name, value := fixtureBinding(match)
		binds[name] = value
		return "${" + name + "-}"
	})
	// A fixture wrote its projection inside single quotes, where a shell
	// expands nothing; the binding it became has to be expanded, so the
	// quoting around it changes with it.
	rendered = singleQuotedBinding.ReplaceAllString(rendered, `"$1"`)
	var b strings.Builder
	fmt.Fprintf(&b, "\n[%s.%s]\ntype = \"shell\"\nscript = %q\n", id, path, rendered)
	if len(binds) > 0 {
		fmt.Fprintf(&b, "\n[%s.%s.bind]\n", id, path)
		names := make([]string, 0, len(binds))
		for name := range binds {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "%s = %s\n", name, binds[name])
		}
	}
	return b.String()
}

// fixtureBinding names the binding one projection becomes, and the value it
// resolves. Every binding is optional: a fixture script is shell-defensive
// about what a partial setup left behind, the same way a real one is.
func fixtureBinding(match string) (name, value string) {
	groups := legacyProjection.FindStringSubmatch(match)
	switch {
	case groups[1] != "":
		return groups[2], fmt.Sprintf("{ from = %q, optional = true }", legacyRootPaths[groups[1]]+groups[2])
	case groups[3] != "":
		return groups[4], fmt.Sprintf("{ from = %q, optional = true }", legacyRootPaths[groups[3]]+groups[4])
	case groups[5] != "":
		return groups[5] + "_" + groups[6], fmt.Sprintf("{ from = %q, optional = true }", "nodes."+groups[5]+".outputs."+groups[6])
	case groups[7] != "":
		return "wf_" + groups[7], fmt.Sprintf("{ from = %q, optional = true }", "workflow.outputs."+groups[7])
	case groups[8] != "":
		scalar := legacyScalarPaths[groups[8]]
		return scalar.name, fmt.Sprintf("{ from = %q, optional = true }", scalar.path)
	default:
		if strings.Contains(match, "terminal") {
			return "terminal_" + groups[9], fmt.Sprintf("{ terminal = %q }", groups[9])
		}
		return "bin_" + strings.ReplaceAll(groups[9], "-", "_"), fmt.Sprintf("{ bin = %q }", groups[9])
	}
}

// A fixture that declares a completion predicate or a chain is a task
// document: those fields are the task surface's, and an effect answers for
// neither. The fixture spec stays one struct so a test says what it is about
// rather than which file the helper writes.
const fixtureObserverID = "fixture_resource"

func fixtureDeclaresAGate(d taskFixture) bool {
	return strings.Contains(d.extra, "done_when") || strings.Contains(d.extra, "chains")
}

// fixtureStateKeys collects the keys a fixture's rooted reads name, so the
// contract that declares them can be generated rather than restated in every
// fixture.
func fixtureStateKeys(body, root string) []string {
	re := regexp.MustCompile(root + `\.state\.([A-Za-z0-9_]+)`)
	seen := map[string]bool{}
	var keys []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	sort.Strings(keys)
	return keys
}

// dropEffectOnlyTables removes the fields a fixture states for the effect
// half of what used to be one declaration. A document publishes no outputs,
// so an outputs contract has nothing to describe and the task surface rejects
// it; a fixture keeps stating one only because it also stands in for a node.
func dropEffectOnlyTables(id, tables string) string {
	var kept []string
	dropping := false
	for _, line := range strings.Split(tables, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "[["+id+".outputs]"), strings.HasPrefix(trimmed, "["+id+".outputs_schema"):
			dropping = true
		case strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "[["):
			dropping = false
		}
		if !dropping {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func taskDocumentFixtureDoc(d taskFixture) string {
	bare, tables := splitFixtureExtra(d.id, d.extra)
	tables = dropEffectOnlyTables(d.id, tables)
	bare = strings.Join(slices.DeleteFunc(strings.Split(bare, "\n"), func(line string) bool {
		return strings.HasPrefix(strings.TrimSpace(line), "requires ")
	}), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\nkind = \"task\"\ndescription = %q\nresource_observer = %q\ninstructions = [{ text = %q }]\n",
		d.id, d.id+" fixture", fixtureObserverID, "Carry out "+d.id+".")
	b.WriteString(bare)
	if keys := fixtureStateKeys(d.extra, "self"); len(keys) > 0 {
		fmt.Fprintf(&b, "\n[%s.state_schema]\ntype = \"object\"\n\n[%s.state_schema.properties]\n", d.id, d.id)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = {}\n", k)
		}
	}
	b.WriteString(tables)
	return b.String()
}

// fixtureObserverDoc declares one observer covering every resource key the
// fixtures read, matching any resource so a seeded instance resolves.
func fixtureObserverDoc(defs []taskFixture) string {
	seen := map[string]bool{}
	var keys []string
	for _, d := range defs {
		for _, k := range fixtureStateKeys(d.extra, "resource") {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	// The match claims a scheme no fixture resource uses, so a test that
	// declares its own observer keeps it: what an instance observes with is
	// the observer its document declares, not whichever pattern also fits.
	fmt.Fprintf(&b, "[%s]\nkind = \"resource_observer\"\nmatch = '^fixture://'\n\n", fixtureObserverID)
	// A fixture has no real resource to look at, so observing one fails and
	// the last observation a test seeded stands — which is what lets a test
	// state the facts its predicate reads instead of standing up a source of
	// truth for them.
	fmt.Fprintf(&b, "[%s.observe]\ntype = \"shell\"\nscript = \"echo 'this fixture has no resource to observe' >&2; exit 1\"\n\n", fixtureObserverID)
	fmt.Fprintf(&b, "[%s.state_schema]\ntype = \"object\"\n\n[%s.state_schema.properties]\n", fixtureObserverID, fixtureObserverID)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = {}\n", k)
	}
	return b.String()
}

// stubObservedFacts rewrites the fixture observer so observing a resource
// succeeds and reports the given facts — for a path that re-observes before
// deciding (finalize, instantiation) rather than reading the last
// observation. match is the caller's, because which resources this observer
// claims decides what a test's own observer is left to claim.
func stubObservedFacts(t *testing.T, cfg *config.Config, match string, facts map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(facts))
	for k := range facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\nkind = \"resource_observer\"\nmatch = '%s'\n\n", fixtureObserverID, match)
	fmt.Fprintf(&b, "[%s.observe]\ntype = \"shell\"\nscript = %q\n\n", fixtureObserverID, "cat <<'JSON'\n"+string(encoded)+"\nJSON")
	fmt.Fprintf(&b, "[%s.state_schema]\ntype = \"object\"\n\n[%s.state_schema.properties]\n", fixtureObserverID, fixtureObserverID)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = {}\n", k)
	}
	path := filepath.Join(cfg.BaseDir, "resources", fixtureObserverID+".toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// observedFacts is one instance's last observation of its resource — where a
// completion check and a chain projection read `resource.state.*` from.
func observedFacts(state map[string]any) *contract.ResourceObservation {
	return &contract.ResourceObservation{State: state, At: time.Now()}
}

// sortedInputKeys keeps a generated fixture byte-stable across runs, so a
// failure names the same line every time.
func sortedInputKeys(inputs map[string]*lang.Value) []string {
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// addWorkflowFields inserts fields into an existing fixture workflow document,
// directly after the header its id and kind occupy: a key written past the
// first `[[<id>.nodes]]` table would be parsed into that table instead.
func addWorkflowFields(t *testing.T, cfg *config.Config, wfID, fields string) {
	t.Helper()
	path := filepath.Join(cfg.BaseDir, "workflows", wfID+".toml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header := fmt.Sprintf("[%s]\nkind = \"workflow\"\n", wfID)
	at := strings.Index(string(existing), header)
	if at < 0 {
		t.Fatalf("workflow %s does not carry its definition header:\n%s", wfID, existing)
	}
	at += len(header)
	merged := string(existing[:at]) + fields + string(existing[at:])
	if err := os.WriteFile(path, []byte(merged), 0o644); err != nil {
		t.Fatal(err)
	}
}

// providerAside gives a fixture's workspace provider an id of its own. One
// layer has one definition id namespace across every kind, so a provider
// cannot be named after the workflow that selects it; the body's table name is
// rewritten with it so a caller states the id once.
func providerAside(wfID, body string) (string, string) {
	provID := wfID + "_provider"
	return provID, strings.ReplaceAll(body, "["+wfID, "["+provID)
}

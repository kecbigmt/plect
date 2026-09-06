package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
)

// observers loads a resource-observer document the way a real config home
// would, so these tests exercise the declarations an author writes rather
// than a hand-built runtime struct.
func observers(t *testing.T, body string) map[string]config.ResourceDef {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "resources", "test.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := (&config.Config{BaseDir: dir}).LoadResourceDefs()
	if err != nil {
		t.Fatalf("load observers: %v", err)
	}
	return defs
}

func TestMatchResourceDef_NoMatch(t *testing.T) {
	defs := observers(t, `
[github]
kind  = "resource_observer"
match = '^https://github\.com/'

[github.observe]
type    = "exec"
command = "true"
`)
	_, _, ok, err := MatchResourceDef(defs, "local-okf://kec/goals/x.md")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected no match")
	}
}

func TestMatchResourceDef_Ambiguous(t *testing.T) {
	defs := observers(t, `
[a]
kind  = "resource_observer"
match = '^https://'

[a.observe]
type    = "exec"
command = "true"

[b]
kind  = "resource_observer"
match = 'github\.com'

[b.observe]
type    = "exec"
command = "true"
`)
	_, _, _, err := MatchResourceDef(defs, "https://github.com/o/r/pull/1")
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("expected ambiguous-match error, got %v", err)
	}
}

func TestResourceStatus_NoDefinition(t *testing.T) {
	state, _, ok, err := ResourceStatus(nil, "https://github.com/o/r/pull/1", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok || state != nil {
		t.Errorf("expected ok=false, state=nil; got ok=%v state=%v", ok, state)
	}
}

func TestResourceStatus_ObservesAndValidates(t *testing.T) {
	defs := observers(t, `
[github]
kind  = "resource_observer"
match = '^https://github\.com/'

[github.observe]
type    = "exec"
command = "printf"
args    = ['{"resource_kind":"pull","checks_status":"SUCCESS"}']

[github.state_schema]
type     = "object"
required = ["resource_kind"]

[github.state_schema.properties]
resource_kind = { type = "string" }
checks_status = { type = "string" }
`)
	state, def, ok, err := ResourceStatus(defs, "https://github.com/o/r/pull/5", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || def.ID != "github" {
		t.Fatalf("expected match on github observer, got ok=%v def=%+v", ok, def)
	}
	if state["resource_kind"] != "pull" || state["checks_status"] != "SUCCESS" {
		t.Errorf("state = %v, want resource_kind=pull checks_status=SUCCESS", state)
	}
}

func TestResourceStatus_ObserveReadsItsSurfaceRoots(t *testing.T) {
	tests := []struct {
		name             string
		projection       string
		branch           string
		workspaceDirPath string
		wantKey          string
		want             string
	}{
		{
			name:       "resource.id",
			projection: `{ from = "resource.id" }`,
			wantKey:    "value",
			want:       "local-okf://kec/goals/x.md",
		},
		{
			name:       "workspace.branch",
			projection: `{ from = "workspace.branch" }`,
			branch:     "issue/632+claude",
			wantKey:    "value",
			want:       "issue/632+claude",
		},
		{
			name:             "workspace.dir",
			projection:       `{ from = "workspace.dir" }`,
			workspaceDirPath: "/tmp/wt/issue-632",
			wantKey:          "value",
			want:             "/tmp/wt/issue-632",
		},
		{
			// A standalone observation has no session, so the root is absent
			// rather than empty and the declared default is what arrives.
			name:       "an absent workspace falls back to the declared default",
			projection: `{ from = "workspace.dir", default = "unset" }`,
			wantKey:    "value",
			want:       "unset",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defs := observers(t, `
[echo]
kind  = "resource_observer"
match = '.*'

[echo.observe]
type    = "exec"
command = "printf"
args    = ['{"value":"%s"}', `+tt.projection+`]
`)
			state, _, ok, err := ResourceStatus(defs, "local-okf://kec/goals/x.md", tt.branch, tt.workspaceDirPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || state[tt.wantKey] != tt.want {
				t.Errorf("state = %v, want %s=%q", state, tt.wantKey, tt.want)
			}
		})
	}
}

// A shell observe action reaches its values through the binding transport,
// never through rendered shell source.
func TestResourceStatus_ShellObserveReadsItsBindings(t *testing.T) {
	defs := observers(t, `
[echo]
kind  = "resource_observer"
match = '.*'

[echo.observe]
type   = "shell"
script = '''
printf '{"resource_id":"%s"}' "$resource_id"
'''

[echo.observe.bind]
resource_id = { from = "resource.id" }
`)
	state, _, ok, err := ResourceStatus(defs, "local-okf://kec/goals/x.md", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || state["resource_id"] != "local-okf://kec/goals/x.md" {
		t.Errorf("state = %v, want the observed resource id echoed back", state)
	}
}

func TestResourceStatus_SchemaViolationErrors(t *testing.T) {
	defs := observers(t, `
[github]
kind  = "resource_observer"
match = '.*'

[github.observe]
type    = "exec"
command = "printf"
args    = ['{"resource_kind":123}']

[github.state_schema]
type = "object"

[github.state_schema.properties]
resource_kind = { type = "string" }
`)
	if _, _, _, err := ResourceStatus(defs, "x", "", "", nil); err == nil {
		t.Error("expected a state_schema validation error")
	}
}

func TestResourceStatus_NonZeroExitIsError(t *testing.T) {
	defs := observers(t, `
[x]
kind  = "resource_observer"
match = '.*'

[x.observe]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 1"]
`)
	if _, _, _, err := ResourceStatus(defs, "x", "", "", nil); err == nil {
		t.Error("expected observe failure to error")
	}
}

func TestFinalizeResource_NoDefinition(t *testing.T) {
	ran, _, err := FinalizeResource(nil, FinalizeResourceParams{ResourceID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("expected ran=false when no observer matches")
	}
}

func TestFinalizeResource_NoFinalizeActionIsNoop(t *testing.T) {
	defs := observers(t, `
[github]
kind  = "resource_observer"
match = '.*'

[github.observe]
type    = "exec"
command = "true"
`)
	ran, def, err := FinalizeResource(defs, FinalizeResourceParams{ResourceID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if ran || def.ID != "github" {
		t.Errorf("expected ran=false with the matched observer, got ran=%v def=%+v", ran, def)
	}
}

func TestFinalizeResource_RunsAndSeesEvidence(t *testing.T) {
	out := filepath.Join(t.TempDir(), "finalize.out")
	defs := observers(t, `
[local_okf]
kind  = "resource_observer"
match = '.*'

[local_okf.observe]
type    = "exec"
command = "true"

[local_okf.finalize]
type    = "exec"
command = "sh"
args    = [
  "-c",
  'printf "resource=%s\nsession=%s\nrevision=%s\njudges=" "$1" "$2" "$3" > "$4"; cat >> "$4"',
  "finalize",
  { from = "resource.id" },
  { from = "session.name" },
  { from = "resource.revision" },
  '`+out+`',
]
stdin = { json = { from = "judges" } }
`)
	ran, def, err := FinalizeResource(defs, FinalizeResourceParams{
		ResourceID:  "local-okf://kec/goals/x.md",
		SessionName: "kec/_orchestrator",
		Revision:    "sha256:abc",
		Judges: []FinalizeJudgeEvidence{
			{ID: "goal-met", Reason: "checklist complete", Revision: "sha256:abc", JudgeSession: "kec/_orchestrator+review"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran || def.ID != "local_okf" {
		t.Fatalf("expected ran=true on local_okf, got ran=%v def=%+v", ran, def)
	}
	raw, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	data := string(raw)
	for _, want := range []string{
		"resource=local-okf://kec/goals/x.md",
		"session=kec/_orchestrator",
		"revision=sha256:abc",
		`"id":"goal-met"`,
		`"judge_session":"kec/_orchestrator+review"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("finalize output = %q, want it to contain %q", data, want)
		}
	}
}

// An empty judge set still reaches finalize as an empty array, so a
// finalize action never has to distinguish "no judges" from "no stdin".
func TestFinalizeResource_NoJudgesStillSerializesAnArray(t *testing.T) {
	out := filepath.Join(t.TempDir(), "finalize.out")
	defs := observers(t, `
[local_okf]
kind  = "resource_observer"
match = '.*'

[local_okf.observe]
type    = "exec"
command = "true"

[local_okf.finalize]
type    = "exec"
command = "sh"
args    = ["-c", 'cat > "$1"', "finalize", '`+out+`']
stdin   = { json = { from = "judges" } }
`)
	if _, _, err := FinalizeResource(defs, FinalizeResourceParams{ResourceID: "x", Revision: "r"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "[]" {
		t.Errorf("stdin = %q, want an empty JSON array", got)
	}
}

func TestFinalizeResource_ActionFailureErrors(t *testing.T) {
	defs := observers(t, `
[x]
kind  = "resource_observer"
match = '.*'

[x.observe]
type    = "exec"
command = "true"

[x.finalize]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 1"]
`)
	if _, _, err := FinalizeResource(defs, FinalizeResourceParams{ResourceID: "x"}); err == nil {
		t.Error("expected finalize failure to error")
	}
}

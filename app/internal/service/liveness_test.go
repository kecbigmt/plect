package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestUp_VanishedPaneIsRebuilt(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	pane := filepath.Join(t.TempDir(), "pane")
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{id: "pane", scope: "run", setup: "touch " + pane + "; echo '{}'", alive: "test -f " + pane},
		},
		[]nodeFixture{{id: "pane"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"pane": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if st := result.Tasks["pane"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("pane = %+v, want rebuilt and produced", st)
	}
	if _, statErr := os.Stat(pane); statErr != nil {
		t.Fatalf("Up did not recreate the vanished pane: %v", statErr)
	}
}

func TestUp_VanishedGuardDirectoryIsRebuilt(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{
				id:    "guard",
				scope: "session",
				setup: `DIR=$(mktemp -d); jq -nc --arg dir "$DIR" '{dir:$dir}'`,
				alive: `test -d "{{.Self.dir}}"`,
			},
			{id: "agent", scope: "run", setup: `printf '{"saw":"%s"}' '{{.Nodes.guard.outputs.dir}}'`},
		},
		[]nodeFixture{
			{id: "guard"},
			{id: "agent", inputs: map[string]*lang.Value{"path_prepend": fromValue("nodes.guard.outputs.dir")}},
		},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"guard": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{"dir": filepath.Join(t.TempDir(), "gone")}},
		"agent": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"saw": "stale"}},
	})

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	dir, _ := result.Tasks["guard"].Outputs["dir"].(string)
	if dir == "" {
		t.Fatalf("guard = %+v, want a freshly produced dir", result.Tasks["guard"])
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("guard's rebuilt dir does not exist: %v", statErr)
	}
	if st := result.Tasks["agent"]; st == nil || st.Outputs["saw"] != dir {
		t.Fatalf("agent did not rebuild against guard's rebuilt dir: %+v", st)
	}
}

func TestUp_VanishedSubscriptionIsRebuilt(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	registration := filepath.Join(t.TempDir(), "subscribed")
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{id: "subscription", scope: "run", setup: "touch " + registration + "; echo '{}'", alive: "test -f " + registration},
		},
		[]nodeFixture{{id: "subscription"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"subscription": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if st := result.Tasks["subscription"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("subscription = %+v, want rebuilt and produced", st)
	}
	if _, statErr := os.Stat(registration); statErr != nil {
		t.Fatalf("Up did not re-register the vanished subscription: %v", statErr)
	}
}

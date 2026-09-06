package legacystate

import (
	"fmt"
	"strings"
	"testing"
)

func TestParse_LoadsSessionsAndNormalizesTree(t *testing.T) {
	data := []byte(`{
  "version": 7,
  "sessions": {
    "root": {"session_name": "root"},
    "work": {"session_name": "work", "parent_session": "root"},
    "review": {"session_name": "review", "parent_session": "root"}
  }
}`)
	sf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	root := sf.Sessions["root"]
	if root == nil {
		t.Fatal("root missing")
	}
	if got, want := root.Children, []string{"review", "work"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("root.Children = %v, want %v", got, want)
	}
}

func TestParse_TreatsRootPrefixAsPseudoParent(t *testing.T) {
	data := []byte(`{
  "version": 7,
  "sessions": {
    "x": {"session_name": "x"},
    "reviewer": {"session_name": "reviewer", "parent_session": "root:x"}
  }
}`)
	sf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sf.Sessions["reviewer"].ParentSession != "root:x" {
		t.Fatalf("reviewer.ParentSession = %q, want root:x", sf.Sessions["reviewer"].ParentSession)
	}
	if len(sf.Sessions["x"].Children) != 0 {
		t.Fatalf("x.Children = %v, want empty", sf.Sessions["x"].Children)
	}
}

func TestParse_ClearsDanglingRootPrefix(t *testing.T) {
	data := []byte(`{
  "version": 7,
  "sessions": {
    "reviewer": {"session_name": "reviewer", "parent_session": "root:missing"}
  }
}`)
	sf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sf.Sessions["reviewer"].ParentSession != "" {
		t.Fatalf("reviewer.ParentSession = %q, want empty", sf.Sessions["reviewer"].ParentSession)
	}
}

func TestParse_BackfillsAliasFromResourceID(t *testing.T) {
	data := []byte(`{
  "version": 7,
  "sessions": {
    "case1": {
      "session_name": "case1",
      "resource_id": "https://example.test/org/repo/items/1"
    }
  }
}`)
	sf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := sf.Sessions["case1"]
	if s == nil {
		t.Fatal("session not loaded")
	}
	if s.Alias != "https://example.test/org/repo/items/1" {
		t.Errorf("Alias = %q, want it backfilled from the resource id", s.Alias)
	}
}

func TestParse_CorruptedInputFails(t *testing.T) {
	if _, err := Parse([]byte("{not valid json")); err == nil {
		t.Fatal("Parse() over corrupted input must fail")
	}
}

func TestParse_VersionMismatchFails(t *testing.T) {
	tests := []struct {
		name      string
		version   int
		wantParts []string
	}{
		{
			name:    "older",
			version: 6,
			wantParts: []string{
				"state schema version mismatch",
				"got 6",
				"want 7",
				"go run ./plugins/legacy-migration/cmd/legacy-migration",
			},
		},
		{
			name:    "newer",
			version: 8,
			wantParts: []string{
				"state schema version mismatch",
				"got 8",
				"want 7",
				"newer",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(fmt.Sprintf(`{
  "version": %d,
  "sessions": {
    "case1": {
      "session_name": "case1",
      "workspace_dir_path": "/tmp/workdir"
    }
  }
}`, tt.version))
			_, err := Parse(data)
			if err == nil {
				t.Fatal("Parse() over a mismatched state version must fail")
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), part)
				}
			}
		})
	}
}

func TestParse_UnmigratedLayerTaskIDFailsLoudInsteadOfZeroingEffectID(t *testing.T) {
	tests := []struct {
		name     string
		layerRaw string
	}{
		{
			name:     "pre-rename task_id field still present",
			layerRaw: `{"task_id": "some-effect", "status": "produced"}`,
		},
		{
			name:     "effect_id present but empty",
			layerRaw: `{"effect_id": "", "status": "produced"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte(fmt.Sprintf(`{
  "version": 7,
  "sessions": {
    "case1": {
      "session_name": "case1",
      "workspace_dir_path": "/tmp/workdir",
      "tasks": {
        "nested-task": {
          "scope": "session",
          "status": "produced",
          "layers": [%s]
        }
      }
    }
  }
}`, tt.layerRaw))
			_, err := Parse(data)
			if err == nil {
				t.Fatal("Parse() over a pre-rename layers[].task_id record must fail loud, not load-then-zero effect_id")
			}
			if !strings.Contains(err.Error(), "effect_id") || !strings.Contains(err.Error(), "task-layer-effect-id-migration.md") {
				t.Fatalf("error = %q, want it to name effect_id and the migration doc", err.Error())
			}
		})
	}
}

func TestParse_LoadsPopulationsAndUpReservations(t *testing.T) {
	data := []byte(`{
  "version": 7,
  "sessions": {},
  "populations": {
    "wf/pop": {
      "workflow": "wf",
      "name": "pop",
      "members": {
        "res-1": {"resource_id": "res-1", "generation": 1}
      }
    }
  },
  "up_reservations": {
    "childA": {"parent": "parent1", "pid": 123}
  }
}`)
	sf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pop := sf.Populations["wf/pop"]
	if pop == nil || pop.Workflow != "wf" || pop.Members["res-1"] == nil {
		t.Fatalf("population = %+v", pop)
	}
	res, ok := sf.UpReservations["childA"]
	if !ok || res.Parent != "parent1" || res.PID != 123 {
		t.Fatalf("up reservation = %+v", res)
	}
}

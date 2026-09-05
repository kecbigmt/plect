package webapi

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
)

// A session with every optional field unset must produce a wire struct
// where those fields are nil pointers, not zero-valued non-pointers — the
// contract's documented "absent and empty are equivalent" rule only holds if
// the Go encoder actually omits the key, which requires a nil pointer, not
// an empty string/slice/map.
func TestDetailFromStatus_OmitsUnsetOptionalFields(t *testing.T) {
	createdAt := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	result := &service.StatusResult{
		Identity: service.StatusIdentity{
			SessionName: "team/workspace-a",
			CreatedAt:   createdAt,
		},
		Runtime: service.StatusRuntime{
			Run: domain.RunDown,
		},
	}

	got := detailFromStatus(result)

	if got.SessionName != "team/workspace-a" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("required fields not carried through: %+v", got)
	}
	if got.ResourceId != nil || got.Title != nil || got.Branch != nil || got.Workflow != nil ||
		got.Tag != nil || got.ParentSession != nil || got.Children != nil || got.Inputs != nil {
		t.Errorf("expected every unset optional identity field to be nil, got %+v", got)
	}
	if got.Health != nil || got.LastCheckedAt != nil || got.LastActivityAt != nil ||
		got.Tasks != nil || got.WorkspaceDirPath != nil || got.Message != nil {
		t.Errorf("expected every unset optional runtime field to be nil, got %+v", got)
	}
	if got.Warnings != nil || got.Destroyed != nil || got.DestroyedAt != nil {
		t.Errorf("expected unset top-level fields to be nil, got %+v", got)
	}
}

// The dynamic-JSON `inputs` field must pass arbitrary user-defined values
// through unchanged — this API does not interpret or reshape them.
func TestDetailFromStatus_PreservesArbitraryInputsJSON(t *testing.T) {
	result := &service.StatusResult{
		Identity: service.StatusIdentity{
			SessionName: "team/workspace-a",
			CreatedAt:   time.Now(),
			Inputs: map[string]any{
				"reviewer": "alice",
				"nested":   map[string]any{"retries": float64(3)},
				"tags":     []any{"a", "b"},
			},
		},
		Runtime: service.StatusRuntime{Run: domain.RunUp},
	}

	got := detailFromStatus(result)

	if got.Inputs == nil {
		t.Fatal("Inputs = nil, want the seeded map")
	}
	in := *got.Inputs
	if in["reviewer"] != "alice" {
		t.Errorf("Inputs[\"reviewer\"] = %v, want alice", in["reviewer"])
	}
	nested, ok := in["nested"].(map[string]any)
	if !ok || nested["retries"] != float64(3) {
		t.Errorf("Inputs[\"nested\"] = %v, want {retries: 3}", in["nested"])
	}
}

func TestDetailFromStatus_MapsRunAndHealthIndependently(t *testing.T) {
	result := &service.StatusResult{
		Identity: service.StatusIdentity{SessionName: "s", CreatedAt: time.Now()},
		Runtime: service.StatusRuntime{
			Run:    domain.RunUp,
			Health: domain.HealthUnhealthy,
		},
	}

	got := detailFromStatus(result)

	if got.Run != webapiv1.Up {
		t.Errorf("Run = %q, want up", got.Run)
	}
	if got.Health == nil || *got.Health != webapiv1.Unhealthy {
		t.Errorf("Health = %v, want unhealthy", got.Health)
	}
}

func TestSummaryFromListEntry_MapsRequiredAndOptionalFields(t *testing.T) {
	now := time.Now()
	entry := service.ListEntry{
		SessionName:   "team/workspace-a",
		DisplayStatus: "up · healthy",
		Run:           domain.RunUp,
		Health:        domain.HealthHealthy,
		ResourceID:    "https://github.com/owner/repo/issues/1",
		LastActiveAt:  &now,
		Branch:        "issue/1",
		ParentSession: "team",
		Tasks: []service.TaskInstanceView{
			{Instance: "initial", Status: "produced"},
		},
	}

	got := summaryFromListEntry(entry)

	if got.SessionName != entry.SessionName || got.DisplayStatus != entry.DisplayStatus ||
		got.ResourceId != entry.ResourceID {
		t.Fatalf("required fields not carried through: %+v", got)
	}
	if got.LastActiveAt == nil || !got.LastActiveAt.Equal(now) {
		t.Errorf("LastActiveAt = %v, want %v", got.LastActiveAt, now)
	}
	if got.Tasks == nil || len(*got.Tasks) != 1 || (*got.Tasks)[0].Instance != "initial" {
		t.Errorf("Tasks = %v, want one entry named initial", got.Tasks)
	}
}

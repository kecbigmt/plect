package webapi

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
	"github.com/kecbigmt/plecture/contracts/event"
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

func TestDetailFromStatus_MapsEveryHealthState(t *testing.T) {
	cases := []struct {
		domain domain.HealthState
		wire   webapiv1.SessionHealthState
	}{
		{domain.HealthHealthy, webapiv1.Healthy},
		{domain.HealthUnhealthy, webapiv1.Unhealthy},
		{domain.HealthUndeclared, webapiv1.Undeclared},
		{domain.HealthStalled, webapiv1.Stalled},
	}
	for _, tt := range cases {
		result := &service.StatusResult{
			Identity: service.StatusIdentity{SessionName: "s", CreatedAt: time.Now()},
			Runtime:  service.StatusRuntime{Run: domain.RunUp, Health: tt.domain},
		}
		got := detailFromStatus(result)
		if got.Health == nil || *got.Health != tt.wire {
			t.Errorf("domain health %q -> wire %v, want %v", tt.domain, got.Health, tt.wire)
		}
	}
}

// A destroyed session is neither "missing" (404, no record ever existed) nor
// an ordinary live result — Destroyed/DestroyedAt must round-trip as present,
// non-nil values so a client can tell the two apart.
func TestDetailFromStatus_MapsDestroyedFields(t *testing.T) {
	destroyedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	result := &service.StatusResult{
		Identity:    service.StatusIdentity{SessionName: "team/workspace-a", CreatedAt: time.Now()},
		Runtime:     service.StatusRuntime{Run: domain.RunDown},
		Destroyed:   true,
		DestroyedAt: destroyedAt,
	}

	got := detailFromStatus(result)

	if got.Destroyed == nil || !*got.Destroyed {
		t.Errorf("Destroyed = %v, want true", got.Destroyed)
	}
	if got.DestroyedAt == nil || !got.DestroyedAt.Equal(destroyedAt) {
		t.Errorf("DestroyedAt = %v, want %v", got.DestroyedAt, destroyedAt)
	}
}

// Parent/child identifiers are preserved verbatim — this package does not
// resolve, validate, or otherwise reinterpret the tree service.Status already
// computed from ParentSession.
func TestDetailFromStatus_MapsParentAndChildren(t *testing.T) {
	result := &service.StatusResult{
		Identity: service.StatusIdentity{
			SessionName:   "team/workspace-b",
			CreatedAt:     time.Now(),
			ParentSession: "team/workspace-a",
			Children:      []string{"team/workspace-c", "team/workspace-d"},
		},
		Runtime: service.StatusRuntime{Run: domain.RunUp},
	}

	got := detailFromStatus(result)

	if got.ParentSession == nil || *got.ParentSession != "team/workspace-a" {
		t.Errorf("ParentSession = %v, want team/workspace-a", got.ParentSession)
	}
	if got.Children == nil || len(*got.Children) != 2 || (*got.Children)[0] != "team/workspace-c" || (*got.Children)[1] != "team/workspace-d" {
		t.Errorf("Children = %v, want [team/workspace-c team/workspace-d]", got.Children)
	}
}

// An unrecognized event type and an unknown metadata key are exactly the
// case docs/design/web-ui.md's Conversation section requires this
// projection to survive — passing every field through untouched proves it,
// rather than a producer-defined constant this package would need to know
// about.
func TestEventFromDomain_PreservesUnknownTypeAndMetadataVerbatim(t *testing.T) {
	when := time.Date(2026, 9, 6, 1, 2, 3, 0, time.UTC)
	ev := event.Event{
		ID:          "01JXAMPLE0000000000000000",
		SessionName: "team/workspace-a",
		Time:        when,
		Type:        "acme.custom_provider.widget_moved",
		Source:      "acme-provider",
		Direction:   event.Inbound,
		Summary:     "widget moved",
		Body:        "full detail",
		Metadata: map[string]string{
			"origin_session": "team/other-session",
			"widget_id":      "w-42",
		},
	}

	got := eventFromDomain(ev)

	if got.Id != ev.ID || got.SessionName != ev.SessionName || !got.Time.Equal(when) {
		t.Fatalf("required identity fields not carried through: %+v", got)
	}
	if got.Type != ev.Type {
		t.Errorf("Type = %q, want the unrecognized type preserved verbatim, got %q", got.Type, ev.Type)
	}
	if got.Source != ev.Source {
		t.Errorf("Source = %q, want %q", got.Source, ev.Source)
	}
	if got.Direction != webapiv1.Inbound {
		t.Errorf("Direction = %q, want inbound", got.Direction)
	}
	if got.Body == nil || *got.Body != ev.Body {
		t.Errorf("Body = %v, want %q", got.Body, ev.Body)
	}
	if got.Metadata == nil {
		t.Fatal("Metadata = nil, want the seeded map")
	}
	meta := *got.Metadata
	if meta["origin_session"] != "team/other-session" || meta["widget_id"] != "w-42" {
		t.Errorf("Metadata = %v, want both keys preserved verbatim", meta)
	}
}

// The receiving session_name and the recorded origin_session are two
// different fields on the same event by design (docs/design/web-ui.md:
// "distinguish the receiving session_name from the recorded origin_session")
// — this projection must not collapse or rename either.
func TestEventFromDomain_DistinguishesReceiverFromOrigin(t *testing.T) {
	ev := event.Event{
		ID:          "01JXAMPLE0000000000000001",
		SessionName: "team/parent", // the receiver: a terminal event pushed one hop
		Type:        "plect.terminal.escalate",
		Direction:   event.Inbound,
		Metadata:    map[string]string{event.MetaOriginSession: "team/child"},
	}

	got := eventFromDomain(ev)

	if got.SessionName != "team/parent" {
		t.Errorf("SessionName = %q, want the receiver team/parent", got.SessionName)
	}
	if got.Metadata == nil || (*got.Metadata)[event.MetaOriginSession] != "team/child" {
		t.Errorf("Metadata[origin_session] = %v, want team/child", got.Metadata)
	}
}

func TestEventFromDomain_OmitsUnsetOptionalFields(t *testing.T) {
	got := eventFromDomain(event.Event{
		ID:          "01JXAMPLE0000000000000002",
		SessionName: "team/workspace-a",
		Type:        event.TypeUserNote,
		Direction:   event.Internal,
	})

	if got.Body != nil {
		t.Errorf("Body = %v, want nil for an empty body", got.Body)
	}
	if got.Metadata != nil {
		t.Errorf("Metadata = %v, want nil for no metadata", got.Metadata)
	}
	if got.DeliveryMode != nil {
		t.Errorf("DeliveryMode = %v, want nil for the zero (pull) value", got.DeliveryMode)
	}
}

func TestEventFromDomain_MapsPushDeliveryMode(t *testing.T) {
	got := eventFromDomain(event.Event{
		ID:           "01JXAMPLE0000000000000003",
		SessionName:  "team/parent",
		Type:         event.TypeTerminalDone,
		DeliveryMode: event.DeliveryModePush,
	})

	if got.DeliveryMode == nil || *got.DeliveryMode != webapiv1.Push {
		t.Errorf("DeliveryMode = %v, want push", got.DeliveryMode)
	}
}

func TestEventPageFromResult_MapsEventsAndCursor(t *testing.T) {
	got := eventPageFromResult(service.EventPageResult{
		Events:     []event.Event{{ID: "e1", SessionName: "s", Type: "user.note"}},
		NextCursor: "opaque-token",
	})

	if len(got.Events) != 1 || got.Events[0].Id != "e1" {
		t.Fatalf("Events = %+v, want one event with id e1", got.Events)
	}
	if got.NextCursor == nil || *got.NextCursor != "opaque-token" {
		t.Errorf("NextCursor = %v, want opaque-token", got.NextCursor)
	}
}

// An empty page (no events, no cursor — the "unknown session" and "desc
// order" cases) must still encode `events` as `[]`, never `null`: an absent
// vs. empty distinction the design doc requires ("Missing, unavailable, and
// empty results are distinguishable").
func TestEventPageFromResult_EmptyPageIsAnEmptySliceNotNil(t *testing.T) {
	got := eventPageFromResult(service.EventPageResult{})

	if got.Events == nil {
		t.Error("Events = nil, want an empty (non-nil) slice")
	}
	if len(got.Events) != 0 {
		t.Errorf("Events = %+v, want empty", got.Events)
	}
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil for an empty result", got.NextCursor)
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
	if got.ParentSession == nil || *got.ParentSession != entry.ParentSession {
		t.Errorf("ParentSession = %v, want %s", got.ParentSession, entry.ParentSession)
	}
	if got.Tasks == nil || len(*got.Tasks) != 1 || (*got.Tasks)[0].Instance != "initial" {
		t.Errorf("Tasks = %v, want one entry named initial", got.Tasks)
	}
}

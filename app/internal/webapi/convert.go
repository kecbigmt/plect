package webapi

import (
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
	"github.com/kecbigmt/plecture/contracts/event"
)

// summaryFromListEntry projects one service.List row onto the wire's
// SessionSummary. A separate, smaller conversion from detailFromStatus
// deliberately: the list is built for every session on every poll, and
// carries no Identity/Runtime detail beyond what ListEntry already computed.
func summaryFromListEntry(e service.ListEntry) webapiv1.SessionSummary {
	return webapiv1.SessionSummary{
		SessionName:   e.SessionName,
		Title:         optionalString(e.Title),
		DisplayStatus: e.DisplayStatus,
		Run:           runState(e.Run),
		Health:        healthState(e.Health),
		ResourceId:    e.ResourceID,
		LastActiveAt:  e.LastActiveAt,
		Message:       message(e.Message),
		Branch:        optionalString(e.Branch),
		ParentSession: optionalString(e.ParentSession),
		Tasks:         taskSummariesFromInstances(e.Tasks),
	}
}

// detailFromStatus projects service.Status's Identity and Runtime layers
// onto the wire's SessionDetail. Work (Tasks detail) and Flow (events) are
// out of this slice's scope — see docs/design/web-ui.md's Tasks/Conversation
// sections, planned for a later task.
func detailFromStatus(r *service.StatusResult) webapiv1.SessionDetail {
	id := r.Identity
	rt := r.Runtime
	return webapiv1.SessionDetail{
		SessionName:        id.SessionName,
		ResourceId:         optionalString(id.ResourceID),
		Title:              optionalString(id.Title),
		Branch:             optionalString(id.Branch),
		Workflow:           optionalString(id.Workflow),
		Tag:                optionalString(id.Tag),
		ParentSession:      optionalString(id.ParentSession),
		Children:           optionalStrings(id.Children),
		Inputs:             optionalJSON(id.Inputs),
		CreatedAt:          id.CreatedAt,
		Run:                runState(rt.Run),
		Health:             healthState(rt.Health),
		LastCheckedAt:      optionalTime(rt.LastCheckedAt),
		LastActivityAt:     optionalTime(rt.LastActivityAt),
		Tasks:              taskSummariesFromRuntime(rt.Tasks),
		WorkspaceDirPath:   optionalString(rt.WorkspaceDirPath),
		WorkspaceDirExists: rt.WorkspaceDirExists,
		Message:            message(rt.Message),
		Warnings:           optionalStrings(r.Warnings),
		Destroyed:          optionalBool(r.Destroyed),
		DestroyedAt:        optionalTime(r.DestroyedAt),
	}
}

// eventPageFromResult projects service.EventPageResult onto the wire's
// EventPage. NextCursor is opaque on both sides — this package neither
// decodes nor reconstructs it, only carries it through as a string.
func eventPageFromResult(r service.EventPageResult) webapiv1.EventPage {
	items := make([]webapiv1.Event, len(r.Events))
	for i, ev := range r.Events {
		items[i] = eventFromDomain(ev)
	}
	return webapiv1.EventPage{
		Events:     items,
		NextCursor: optionalString(r.NextCursor),
	}
}

// eventFromDomain projects one contracts/event.Event onto the wire's Event,
// field for field and verbatim — Type and Source stay untyped strings (a
// producer's own namespace, not this API's to enumerate), and Metadata passes
// through whatever keys the log actually holds, known or not. This is the
// entire "unknown event types/metadata survive the projection" contract: pass
// everything through, invent nothing.
func eventFromDomain(ev event.Event) webapiv1.Event {
	return webapiv1.Event{
		Id:           ev.ID,
		SessionName:  ev.SessionName,
		Time:         ev.Time,
		Type:         ev.Type,
		Source:       ev.Source,
		Direction:    webapiv1.EventDirection(ev.Direction),
		Summary:      ev.Summary,
		Body:         optionalString(ev.Body),
		Metadata:     optionalStringMap(ev.Metadata),
		DeliveryMode: deliveryMode(ev.DeliveryMode),
	}
}

// deliveryMode returns nil for the zero DeliveryMode (pull, the default for
// every ordinary progress event) rather than the empty string: the wire field
// is optional, and "" is not one of EventDeliveryMode's members.
func deliveryMode(m event.DeliveryMode) *webapiv1.EventDeliveryMode {
	if m == "" {
		return nil
	}
	v := webapiv1.EventDeliveryMode(m)
	return &v
}

func optionalStringMap(m map[string]string) *map[string]string {
	if len(m) == 0 {
		return nil
	}
	return &m
}

func runState(s domain.RunState) webapiv1.SessionRunState {
	return webapiv1.SessionRunState(s)
}

// healthState returns nil for the zero HealthState rather than the empty
// string: the wire field is optional, and an empty enum value is not one of
// SessionHealthState's members.
func healthState(s domain.HealthState) *webapiv1.SessionHealthState {
	if s == "" {
		return nil
	}
	v := webapiv1.SessionHealthState(s)
	return &v
}

func message(m *domain.Message) *webapiv1.SessionMessage {
	if m == nil {
		return nil
	}
	return &webapiv1.SessionMessage{Text: m.Text, UpdatedAt: m.UpdatedAt}
}

func taskSummariesFromRuntime(tasks []service.StatusRuntimeTask) *[]webapiv1.SessionTaskSummary {
	if len(tasks) == 0 {
		return nil
	}
	out := make([]webapiv1.SessionTaskSummary, len(tasks))
	for i, t := range tasks {
		out[i] = webapiv1.SessionTaskSummary{Instance: t.Instance, Status: t.Status}
	}
	return &out
}

// taskSummariesFromInstances discards TaskInstanceView's outputs/done_when
// detail — the list endpoint's task rollup is bare lifecycle status, matching
// service.StatusRuntimeTask's shape on the Status endpoint.
func taskSummariesFromInstances(tasks []service.TaskInstanceView) *[]webapiv1.SessionTaskSummary {
	if len(tasks) == 0 {
		return nil
	}
	out := make([]webapiv1.SessionTaskSummary, len(tasks))
	for i, t := range tasks {
		out[i] = webapiv1.SessionTaskSummary{Instance: t.Instance, Status: t.Status}
	}
	return &out
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optionalStrings(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}

func optionalBool(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

// optionalTime returns nil for a zero time.Time — the wire field is
// omitempty, and a zero value here means "never observed", not epoch.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// optionalJSON returns nil for an empty/nil map so the wire field is
// omitted rather than rendered as `{}`, matching every other optional field
// on this model.
func optionalJSON(m map[string]any) *map[string]interface{} {
	if len(m) == 0 {
		return nil
	}
	v := map[string]interface{}(m)
	return &v
}

package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
	"github.com/kecbigmt/plecture/contracts/event"
)

type fakeReader struct {
	entries   []service.ListEntry
	listErr   error
	status    *service.StatusResult
	statusErr error
	// gotName records the identifier handleGet actually passed through, so a
	// test can assert on how a slash-containing path was parsed rather than
	// just on the response.
	gotName string

	eventPage    service.EventPageResult
	eventPageErr error
	// gotEventParams records the params handleSessionEvents actually built,
	// so a test can assert on how query strings were parsed.
	gotEventParams *service.EventPageParams
	gotEventName   string
}

func (f *fakeReader) List() ([]service.ListEntry, error) { return f.entries, f.listErr }

func (f *fakeReader) Status(name string) (*service.StatusResult, error) {
	f.gotName = name
	return f.status, f.statusErr
}

func (f *fakeReader) EventPage(name string, p service.EventPageParams) (service.EventPageResult, error) {
	f.gotEventName = name
	f.gotEventParams = &p
	return f.eventPage, f.eventPageErr
}

func TestHandleList_ReturnsEveryEntryAsAnEnvelope(t *testing.T) {
	now := time.Now()
	svc := &fakeReader{entries: []service.ListEntry{
		{SessionName: "team/workspace-a", DisplayStatus: "up", Run: domain.RunUp, ResourceID: "r1", LastActiveAt: &now},
		{SessionName: "team/workspace-b", DisplayStatus: "down", Run: domain.RunDown, ResourceID: "r2"},
	}}

	rec := doRequest(t, svc, http.MethodGet, "/sessions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var got webapiv1.SessionListResponse
	decodeBody(t, rec, &got)
	if got.Count != 2 || len(got.Items) != 2 {
		t.Fatalf("got %+v, want 2 items", got)
	}
	if got.Items[0].SessionName != "team/workspace-a" {
		t.Errorf("Items[0].SessionName = %q", got.Items[0].SessionName)
	}
}

// The contract documents ascending session-name order (service.List's own
// documented and tested guarantee) — this handler's own responsibility is
// narrower: not to disturb whatever order service.List already returns.
// "z/..." before "a/..." here would be wrong input for the real service,
// but exercises the one thing this test can actually prove: encoding to
// JSON preserves slice order regardless of what that order is.
func TestHandleList_DoesNotReorderWhatServiceListReturns(t *testing.T) {
	svc := &fakeReader{entries: []service.ListEntry{
		{SessionName: "z/last", ResourceID: "r1"},
		{SessionName: "a/first", ResourceID: "r2"},
	}}

	rec := doRequest(t, svc, http.MethodGet, "/sessions")

	var got webapiv1.SessionListResponse
	decodeBody(t, rec, &got)
	if len(got.Items) != 2 || got.Items[0].SessionName != "z/last" || got.Items[1].SessionName != "a/first" {
		t.Fatalf("Items = %+v, want service.List's order preserved verbatim", got.Items)
	}
}

func TestHandleList_EmptyStoreReturnsAnEmptyEnvelopeNotAnError(t *testing.T) {
	svc := &fakeReader{entries: nil}

	rec := doRequest(t, svc, http.MethodGet, "/sessions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var got webapiv1.SessionListResponse
	decodeBody(t, rec, &got)
	if got.Count != 0 || len(got.Items) != 0 {
		t.Errorf("got %+v, want an empty envelope", got)
	}
}

func TestHandleList_ServiceFailureBecomesA500ExecutionError(t *testing.T) {
	svc := &fakeReader{listErr: errors.New("state store unavailable")}

	rec := doRequest(t, svc, http.MethodGet, "/sessions")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body)
	}
	var got webapiv1.ExecutionError
	decodeBody(t, rec, &got)
	if got.Category != webapiv1.ExecutionErrorCategoryExecution {
		t.Errorf("Category = %q, want execution", got.Category)
	}
}

func TestHandleGet_ReturnsTheSessionDetail(t *testing.T) {
	svc := &fakeReader{status: &service.StatusResult{
		Identity: service.StatusIdentity{SessionName: "team/workspace-a", CreatedAt: time.Now()},
		Runtime:  service.StatusRuntime{Run: domain.RunUp},
	}}

	rec := doRequest(t, svc, http.MethodGet, "/sessions/team/workspace-a")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if svc.gotName != "team/workspace-a" {
		t.Errorf("Status called with %q, want the full slash-containing name", svc.gotName)
	}
	var got webapiv1.SessionDetail
	decodeBody(t, rec, &got)
	if got.SessionName != "team/workspace-a" {
		t.Errorf("SessionName = %q", got.SessionName)
	}
}

// The absent/empty documentation (web/api/README.md) is a claim about the
// wire bytes a real handler response produces, not about the Go struct
// encoding.Marshal is handed — decoding into the typed SessionDetail (as
// TestDetailFromStatus_OmitsUnsetOptionalFields does) can't tell an omitted
// key from one that was never checked, since a struct field simply defaults
// to its zero value either way. Decoding the actual response body into a
// bare map is the only way to see which keys the encoder actually wrote.
func TestHandleGet_ResponseJSONOmitsEveryUnsetOptionalKey(t *testing.T) {
	svc := &fakeReader{status: &service.StatusResult{
		Identity: service.StatusIdentity{SessionName: "team/workspace-a", CreatedAt: time.Now()},
		Runtime:  service.StatusRuntime{Run: domain.RunDown},
	}}

	rec := doRequest(t, svc, http.MethodGet, "/sessions/team/workspace-a")

	var raw map[string]any
	decodeBody(t, rec, &raw)

	unsetOptionalKeys := []string{
		"resourceId", "title", "branch", "workflow", "tag", "parentSession",
		"children", "inputs", "health", "lastCheckedAt", "lastActivityAt",
		"tasks", "workspaceDirPath", "message", "warnings", "destroyed",
		"destroyedAt",
	}
	for _, key := range unsetOptionalKeys {
		if _, present := raw[key]; present {
			t.Errorf("response JSON has key %q = %v, want it omitted (not even null)", key, raw[key])
		}
	}
	for _, key := range []string{"sessionName", "createdAt", "run", "workspaceDirExists"} {
		if _, present := raw[key]; !present {
			t.Errorf("response JSON is missing required key %q: %v", key, raw)
		}
	}
}

func TestHandleGet_UnknownSessionIsA404NotFoundError(t *testing.T) {
	svc := &fakeReader{statusErr: &service.Error{Code: service.ErrSessionNotFound, Message: "no such session"}}

	rec := doRequest(t, svc, http.MethodGet, "/sessions/team/missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
	var got webapiv1.NotFoundError
	decodeBody(t, rec, &got)
	if got.Code != webapiv1.SessionNotFound {
		t.Errorf("Code = %q, want session_not_found", got.Code)
	}
}

// A trailing slash with nothing after it is the empty-name case, not a
// session named "" — reject it before ever calling the service, the same
// way an absent required path segment would be rejected.
func TestHandleGet_EmptyNameIsRejectedWithoutCallingTheService(t *testing.T) {
	svc := &fakeReader{}

	rec := doRequest(t, svc, http.MethodGet, "/sessions/")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
	if svc.gotName != "" {
		t.Errorf("service.Status must not be called for an empty name, got %q", svc.gotName)
	}
}

func TestHandleSessionEvents_ReturnsThePageAndForwardsParsedParams(t *testing.T) {
	svc := &fakeReader{eventPage: service.EventPageResult{
		Events:     []event.Event{{ID: "e1", SessionName: "team/workspace-a", Type: "user.note"}},
		NextCursor: "opaque-token",
	}}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team%2Fworkspace-a&cursor=prior-cursor&limit=25&order=desc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if svc.gotEventName != "team/workspace-a" {
		t.Errorf("EventPage called with session %q, want team/workspace-a", svc.gotEventName)
	}
	if svc.gotEventParams == nil {
		t.Fatal("EventPage was not called")
	}
	if svc.gotEventParams.Order != event.OrderDesc {
		t.Errorf("Order = %q, want desc", svc.gotEventParams.Order)
	}
	if svc.gotEventParams.Cursor != "prior-cursor" {
		t.Errorf("Cursor = %q, want prior-cursor", svc.gotEventParams.Cursor)
	}
	if svc.gotEventParams.Filter.Limit != 25 {
		t.Errorf("Limit = %d, want 25", svc.gotEventParams.Filter.Limit)
	}
	var got webapiv1.EventPage
	decodeBody(t, rec, &got)
	if len(got.Events) != 1 || got.Events[0].Id != "e1" {
		t.Fatalf("Events = %+v, want one event e1", got.Events)
	}
	if got.NextCursor == nil || *got.NextCursor != "opaque-token" {
		t.Errorf("NextCursor = %v, want opaque-token", got.NextCursor)
	}
}

// order defaults to asc when omitted, matching service.EventPage's own
// documented default.
func TestHandleSessionEvents_OrderDefaultsToAsc(t *testing.T) {
	svc := &fakeReader{}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team/workspace-a")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if svc.gotEventParams == nil || svc.gotEventParams.Order != event.OrderAsc {
		t.Errorf("Order = %v, want asc", svc.gotEventParams)
	}
}

// A missing session is rejected before ever calling the service — the same
// discipline TestHandleGet_EmptyNameIsRejectedWithoutCallingTheService
// already applies to GET /sessions/{name}.
func TestHandleSessionEvents_MissingSessionParamIsRejectedWithoutCallingTheService(t *testing.T) {
	svc := &fakeReader{}

	rec := doRequest(t, svc, http.MethodGet, "/events")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	if svc.gotEventParams != nil {
		t.Error("EventPage must not be called when session is missing")
	}
	var got webapiv1.ValidationError
	decodeBody(t, rec, &got)
	if got.Category != webapiv1.ValidationErrorCategoryValidation {
		t.Errorf("Category = %q, want validation", got.Category)
	}
}

func TestHandleSessionEvents_InvalidOrderIsA400ValidationError(t *testing.T) {
	svc := &fakeReader{}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team/workspace-a&order=sideways")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	if svc.gotEventParams != nil {
		t.Error("EventPage must not be called for an invalid order")
	}
}

func TestHandleSessionEvents_InvalidLimitIsA400ValidationError(t *testing.T) {
	svc := &fakeReader{}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team/workspace-a&limit=not-a-number")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	if svc.gotEventParams != nil {
		t.Error("EventPage must not be called for an invalid limit")
	}
}

// An unknown session is not a 404: service.EventPage answers a session
// nothing was ever recorded for with an empty, successful page (its log
// simply doesn't exist), and this handler passes that straight through — see
// routes/events.tsp's documented rationale.
func TestHandleSessionEvents_ServiceEmptyResultIsA200NotA404(t *testing.T) {
	svc := &fakeReader{eventPage: service.EventPageResult{}}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team/never-created")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var got webapiv1.EventPage
	decodeBody(t, rec, &got)
	if len(got.Events) != 0 {
		t.Errorf("Events = %+v, want empty", got.Events)
	}
	if got.NextCursor != nil {
		t.Errorf("NextCursor = %v, want absent", got.NextCursor)
	}
}

// A cursor rejected by the service (order mismatch, stale generation) is a
// service-layer *service.Error{Code: ErrInvalidInput}, routed through the
// same apiError classification every other operation uses — not a special
// case this handler invents its own response for.
func TestHandleSessionEvents_ServiceValidationErrorBecomesA400ValidationError(t *testing.T) {
	svc := &fakeReader{eventPageErr: &service.Error{Code: service.ErrInvalidInput, Message: "cursor expired"}}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team/workspace-a&cursor=stale-token")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body)
	}
	var got webapiv1.ValidationError
	decodeBody(t, rec, &got)
	if got.Code != webapiv1.InvalidInput {
		t.Errorf("Code = %q, want invalid_input", got.Code)
	}
}

func TestHandleSessionEvents_ServiceFailureBecomesA500ExecutionError(t *testing.T) {
	svc := &fakeReader{eventPageErr: &service.Error{Code: service.ErrExecutionFailed, Message: "log unreadable"}}

	rec := doRequest(t, svc, http.MethodGet, "/events?session=team/workspace-a")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body)
	}
}

func doRequest(t *testing.T, svc SessionReader, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	Routes(svc).ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v (body: %s)", err, rec.Body)
	}
}

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
}

func (f *fakeReader) List() ([]service.ListEntry, error) { return f.entries, f.listErr }

func (f *fakeReader) Status(name string) (*service.StatusResult, error) {
	f.gotName = name
	return f.status, f.statusErr
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

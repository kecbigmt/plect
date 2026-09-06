package webui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/contracts/event"
)

// sampleShow returns a StatusResult exercising every optional section so the
// detail template is rendered against realistic data.
func sampleShow() *service.StatusResult {
	return &service.StatusResult{
		Identity: service.StatusIdentity{
			SessionName: "owner/repo-7",
			ResourceID:  "https://github.com/owner/repo/issues/7",
			Title:       "make the thing work",
			Branch:      "issue/7",
			CreatedAt:   time.Now(),
		},
		Runtime: service.StatusRuntime{
			Run:                domain.RunUp,
			Health:             domain.HealthHealthy,
			WorkspaceDirPath:   "/home/dev/workdirs/owner/repo/issue-7",
			WorkspaceDirExists: true,
			Conversation:       &domain.Conversation{Source: "Slack", URL: "https://slack.example/thread/1"},
			AttachCommand:      "some-runtime attach owner/repo-7",
		},
	}
}

// 1. detail: GET /sessions/<name> renders the session's key fields, 200.
func TestDetail_ShowsSession(t *testing.T) {
	rec := get(t, &fakeService{status: sampleShow()}, "/sessions/owner/repo-7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"owner/repo-7", "issue/7", "make the thing work", "Slack",
		"/home/dev/workdirs/owner/repo/issue-7",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail body missing %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(body), "<html") {
		t.Errorf("detail should be a full HTML page")
	}
}

// detail: a non-URL resource id renders as plain text, not a broken anchor.
func TestDetail_NonURLResourceIsPlainText(t *testing.T) {
	status := sampleShow()
	status.Identity.ResourceID = "my-experiment"
	body := get(t, &fakeService{status: status}, "/sessions/owner/repo-7").Body.String()
	if !strings.Contains(body, "my-experiment") {
		t.Error("resource id missing from detail")
	}
	if strings.Contains(body, `href="my-experiment"`) {
		t.Error("non-URL resource id must not be wrapped in an anchor")
	}
}

// 2. not found: unknown session -> 404 + banner with a link back to the list.
func TestDetail_NotFound(t *testing.T) {
	svc := &fakeService{statusErr: &service.Error{Code: service.ErrSessionNotFound, Message: "no state entry"}}
	rec := get(t, svc, "/sessions/owner/missing-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "not found") {
		t.Error("not-found banner missing")
	}
	if !strings.Contains(body, `href="/"`) {
		t.Error("not-found page should link back to the list")
	}
}

// 3. error: unexpected Status failure -> 500 banner, no panic.
func TestDetail_ErrorReturns500(t *testing.T) {
	svc := &fakeService{statusErr: &service.Error{Code: service.ErrExecutionFailed, Message: "boom"}}
	rec := get(t, svc, "/sessions/owner/repo-7")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "error") {
		t.Error("error banner missing")
	}
}

// The detail route must capture session names that contain "/".
func TestDetail_WildcardCapturesSlashedName(t *testing.T) {
	var got string
	svc := nameCapture{fn: func(name string) { got = name }, status: sampleShow()}
	get(t, svc, "/sessions/owner/repo-7")
	if got != "owner/repo-7" {
		t.Errorf("captured name = %q, want owner/repo-7", got)
	}
}

// nameCapture records the name passed to Status.
type nameCapture struct {
	fn     func(string)
	status *service.StatusResult
}

func (n nameCapture) List() ([]service.ListEntry, error) { return nil, nil }
func (n nameCapture) Status(name string) (*service.StatusResult, error) {
	n.fn(name)
	return n.status, nil
}
func (n nameCapture) Events(string) ([]event.Event, error)        { return nil, nil }
func (n nameCapture) EventsSubtree(string) ([]event.Event, error) { return nil, nil }
func (n nameCapture) EventPage(string, service.EventPageParams) (service.EventPageResult, error) {
	return service.EventPageResult{}, nil
}
func (n nameCapture) EventStreamResume(string, string) (string, int64, error) { return "", 0, nil }
func (n nameCapture) PublishEvent(string, service.EventPublishParams) (event.Event, error) {
	return event.Event{}, nil
}
func (n nameCapture) Create(service.CreateParams) (*service.CreateResult, error)    { return nil, nil }
func (n nameCapture) Up(service.UpParams) (*service.UpResult, error)                { return nil, nil }
func (n nameCapture) Down(service.DownParams) (*service.DownResult, error)          { return nil, nil }
func (n nameCapture) Destroy(service.DestroyParams) (*service.DestroyResult, error) { return nil, nil }

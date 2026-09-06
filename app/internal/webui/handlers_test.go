package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/contracts/event"
)

// fakeService injects canned results so handlers can be tested without
// git/tmux/store side tasks. The capture pointers record the params the
// handler built so mutation tests can assert on them.
type fakeService struct {
	entries   []service.ListEntry
	err       error
	status    *service.StatusResult
	statusErr error
	events    []event.Event
	eventsErr error

	subtreeEvents []event.Event
	subtreeErr    error
	gotSubtree    string

	eventPage    service.EventPageResult
	eventPageErr error
	gotEventPage *service.EventPageParams
	gotEventName string

	resumeGen       string
	resumeOffset    int64
	resumeErr       error
	gotResumeName   string
	gotResumeCursor string
	resumeFn        func(name, cursor string) (string, int64, error)

	createResult  *service.CreateResult
	upResult      *service.UpResult
	upErr         error
	downResult    *service.DownResult
	downErr       error
	destroyResult *service.DestroyResult
	destroyErr    error

	gotUp      *service.UpParams
	gotDown    *service.DownParams
	gotDestroy *service.DestroyParams

	publishResult event.Event
	publishErr    error
	gotPublish    *service.EventPublishParams
	gotPublishFor string
}

func (f fakeService) List() ([]service.ListEntry, error) { return f.entries, f.err }

func (f fakeService) Status(string) (*service.StatusResult, error) { return f.status, f.statusErr }

func (f *fakeService) Events(string) ([]event.Event, error) { return f.events, f.eventsErr }

func (f *fakeService) EventsSubtree(root string) ([]event.Event, error) {
	f.gotSubtree = root
	return f.subtreeEvents, f.subtreeErr
}

func (f *fakeService) EventPage(name string, p service.EventPageParams) (service.EventPageResult, error) {
	f.gotEventName = name
	f.gotEventPage = &p
	return f.eventPage, f.eventPageErr
}

func (f *fakeService) EventStreamResume(name, cursor string) (string, int64, error) {
	f.gotResumeName = name
	f.gotResumeCursor = cursor
	if f.resumeFn != nil {
		return f.resumeFn(name, cursor)
	}
	return f.resumeGen, f.resumeOffset, f.resumeErr
}

func (f *fakeService) PublishEvent(name string, p service.EventPublishParams) (event.Event, error) {
	f.gotPublishFor = name
	f.gotPublish = &p
	return f.publishResult, f.publishErr
}

func (f *fakeService) Create(service.CreateParams) (*service.CreateResult, error) {
	return f.createResult, nil
}

func (f *fakeService) Up(p service.UpParams) (*service.UpResult, error) {
	f.gotUp = &p
	if f.upResult == nil && f.upErr == nil {
		return &service.UpResult{SessionName: p.Identifier}, nil
	}
	return f.upResult, f.upErr
}

func (f *fakeService) Down(p service.DownParams) (*service.DownResult, error) {
	f.gotDown = &p
	if f.downResult == nil && f.downErr == nil {
		return &service.DownResult{SessionName: p.Identifier}, nil
	}
	return f.downResult, f.downErr
}

func (f *fakeService) Destroy(p service.DestroyParams) (*service.DestroyResult, error) {
	f.gotDestroy = &p
	if f.destroyResult == nil && f.destroyErr == nil {
		return &service.DestroyResult{SessionName: p.Identifier}, nil
	}
	return f.destroyResult, f.destroyErr
}

func get(t *testing.T, svc SessionService, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(svc).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// 1. List view: GET / renders session_name, status, branch, github status.
func TestIndex_ShowsSessions(t *testing.T) {
	svc := &fakeService{entries: []service.ListEntry{
		{SessionName: "acme/mm-123", Run: domain.RunUp, Health: domain.HealthHealthy, Branch: "issue/123", DisplayStatus: "open"},
		{SessionName: "acme/mm-456", Run: domain.RunDown, Branch: "issue/456"},
	}}
	rec := get(t, svc, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"acme/mm-123", "acme/mm-456", "issue/123", "open"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(body), "<html") {
		t.Errorf("index should be a full HTML page")
	}
}

// 1. Empty state: 0 sessions -> empty-state, 200.
func TestIndex_Empty(t *testing.T) {
	rec := get(t, &fakeService{entries: nil}, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No sessions") {
		t.Errorf("empty-state text missing")
	}
}

// 1. htmx partial: GET /sessions returns the rows fragment, not a full page.
func TestSessionsPartial_NoLayout(t *testing.T) {
	svc := &fakeService{entries: []service.ListEntry{{SessionName: "a/b-1", Run: domain.RunUp, Health: domain.HealthHealthy}}}
	rec := get(t, svc, "/sessions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "a/b-1") {
		t.Errorf("partial missing session")
	}
	if strings.Contains(strings.ToLower(body), "<html") {
		t.Errorf("partial must not include the full layout")
	}
}

// 1. status visual distinction via data-run / data-health attributes.
func TestStatusBadges_Distinct(t *testing.T) {
	svc := &fakeService{entries: []service.ListEntry{
		{SessionName: "s/up-healthy-1", Run: domain.RunUp, Health: domain.HealthHealthy},
		{SessionName: "s/down-2", Run: domain.RunDown},
		{SessionName: "s/up-unhealthy-3", Run: domain.RunUp, Health: domain.HealthUnhealthy},
	}}
	body := get(t, svc, "/sessions").Body.String()
	for _, want := range []string{`data-run="up"`, `data-run="down"`, `data-health="healthy"`, `data-health="unhealthy"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// 4. error case: List failure -> 500 + error banner, no panic.
func TestSessions_ErrorReturns500(t *testing.T) {
	rec := get(t, &fakeService{err: errors.New("database corrupt")}, "/sessions")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "error") {
		t.Errorf("error banner missing")
	}
}

// 3. healthz.
func TestHealthz(t *testing.T) {
	rec := get(t, &fakeService{}, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// Default order is most-recently-active first.
func TestList_DefaultNewestFirst(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	mid := time.Now().Add(-1 * time.Hour)
	now := time.Now()
	svc := &fakeService{entries: []service.ListEntry{
		{SessionName: "a/old-1", Run: domain.RunDown, LastActiveAt: &old},
		{SessionName: "b/new-3", Run: domain.RunUp, Health: domain.HealthHealthy, LastActiveAt: &now},
		{SessionName: "c/mid-2", Run: domain.RunDown, LastActiveAt: &mid},
	}}
	body := get(t, svc, "/").Body.String()
	inew, imid, iold := strings.Index(body, "b/new-3"), strings.Index(body, "c/mid-2"), strings.Index(body, "a/old-1")
	if !(inew >= 0 && inew < imid && imid < iold) {
		t.Errorf("default not newest-first: new=%d mid=%d old=%d", inew, imid, iold)
	}
}

// order=oldest reverses, on both the page and the htmx fragment.
func TestList_OldestFirstParam(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	now := time.Now()
	svc := &fakeService{entries: []service.ListEntry{
		{SessionName: "b/new", LastActiveAt: &now},
		{SessionName: "a/old", LastActiveAt: &old},
	}}
	for _, path := range []string{"/?order=oldest", "/sessions?order=oldest"} {
		body := get(t, svc, path).Body.String()
		iold, inew := strings.Index(body, "a/old"), strings.Index(body, "b/new")
		if !(iold >= 0 && iold < inew) {
			t.Errorf("%s not oldest-first: old=%d new=%d", path, iold, inew)
		}
	}
}

// Untracked sessions (no timestamp) sort last regardless of direction.
func TestList_UntrackedSortLast(t *testing.T) {
	now := time.Now()
	svc := &fakeService{entries: []service.ListEntry{
		{SessionName: "z/untracked", Run: "untracked"},
		{SessionName: "a/tracked", Run: domain.RunUp, Health: domain.HealthHealthy, LastActiveAt: &now},
	}}
	for _, path := range []string{"/?order=recent", "/?order=oldest"} {
		body := get(t, svc, path).Body.String()
		itr, iun := strings.Index(body, "a/tracked"), strings.Index(body, "z/untracked")
		if !(itr >= 0 && itr < iun) {
			t.Errorf("%s: untracked should be last: tracked=%d untracked=%d", path, itr, iun)
		}
	}
}

// The page offers both order controls and the auto-refresh keeps the chosen order.
func TestList_OrderToggleControls(t *testing.T) {
	now := time.Now()
	svc := &fakeService{entries: []service.ListEntry{{SessionName: "a/b-1", LastActiveAt: &now}}}
	body := get(t, svc, "/").Body.String()
	if !strings.Contains(body, "/?order=recent") || !strings.Contains(body, "/?order=oldest") {
		t.Error("order toggle links missing")
	}
	if !strings.Contains(body, "/sessions?order=recent") {
		t.Error("auto-refresh hx-get should carry the current order")
	}
}

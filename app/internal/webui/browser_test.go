//go:build browser

// Drives a real Chromium against the committed /app/ build over
// acceptance_test.go's own real service/state seam, since fixtures seeded
// through state.Store/service.PublishEvent need no mutation UI.
package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
	"github.com/mxschmitt/playwright-go"
)

func browserOrigin(t *testing.T, store *state.Store, cfg *Config) (string, *LiveService) {
	t.Helper()
	svcCfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	isolateMachineConfig(svcCfg)
	svc := newLiveService(svcCfg, store)
	bus := startEventBusRelay(t, svcCfg, store)

	s := NewWithConfig(svc, cfg)
	s.busClientFn = func() *event.Client {
		return &event.Client{BaseURL: bus.URL, HTTP: http.DefaultClient}
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv.URL, svc
}

func seedSession(t *testing.T, store *state.Store, sess *domain.Session) {
	t.Helper()
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = now
	}
	if err := store.Put(sess); err != nil {
		t.Fatalf("seed session %s: %v", sess.Name, err)
	}
}

func publish(t *testing.T, svc *LiveService, session string, p service.EventPublishParams) {
	t.Helper()
	if _, err := svc.PublishEvent(session, p); err != nil {
		t.Fatalf("publish to %s: %v", session, err)
	}
}

// Acceptance: sign-in, hierarchy navigation, session detail/resource
// inspection, and history reading in one flow.
func TestBrowserAcceptance_LoginNavigatesHierarchyAndShowsHistory(t *testing.T) {
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-root", ResourceID: "https://github.com/browser-accept/issues/1"})
	seedSession(t, store, &domain.Session{
		Name:          "browser-child",
		ParentSession: "browser-root",
		ResourceID:    "https://github.com/browser-accept/issues/2",
		Branch:        "issue/2",
	})

	origin, svc := browserOrigin(t, store, &Config{AuthToken: "s3cr3t-token"})
	publish(t, svc, "browser-child", service.EventPublishParams{
		Type: event.TypeUserEmit, Summary: "utterance one", Body: "hello from the browser test",
	})
	publish(t, svc, "browser-child", service.EventPublishParams{
		Type: "acceptance.custom.unknown", Summary: "an unrecognized event kind", Body: "raw payload",
	})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)

	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}
	if err := expect.Page(page).ToHaveURL(regexp.MustCompile(`/login(\?|$)`)); err != nil {
		t.Fatalf("unauthenticated navigation should redirect to /login: %v", err)
	}

	if err := page.Locator("#login-token").Fill("s3cr3t-token"); err != nil {
		t.Fatalf("fill login token: %v", err)
	}
	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Sign in"}).Click(); err != nil {
		t.Fatalf("submit login: %v", err)
	}
	if err := expect.Page(page).ToHaveURL(regexp.MustCompile(`/app/$`)); err != nil {
		t.Fatalf("successful login should return to /app/: %v", err)
	}

	root := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-root", Exact: playwright.Bool(true)})
	if err := expect.Locator(root).ToBeVisible(); err != nil {
		t.Fatalf("root session should be visible in the tree: %v", err)
	}
	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Expand browser-root"}).Click(); err != nil {
		t.Fatalf("expand root: %v", err)
	}
	child := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-child", Exact: playwright.Bool(true)})
	if err := child.Click(); err != nil {
		t.Fatalf("select child: %v", err)
	}

	if err := expect.Locator(page.GetByRole("heading", playwright.PageGetByRoleOptions{Name: "browser-child", Exact: playwright.Bool(true)})).ToBeVisible(); err != nil {
		t.Fatalf("header should show the selected session name: %v", err)
	}

	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Details", Exact: playwright.Bool(true)}).Click(); err != nil {
		t.Fatalf("open details: %v", err)
	}
	details := page.GetByRole("complementary", playwright.PageGetByRoleOptions{Name: "Details"})
	if err := expect.Locator(details.GetByText("issue/2")).ToBeVisible(); err != nil {
		t.Fatalf("detail pane should show the session's branch: %v", err)
	}
	if err := expect.Locator(details.GetByRole("link", playwright.LocatorGetByRoleOptions{
		Name: "https://github.com/browser-accept/issues/2", Exact: playwright.Bool(true),
	})).ToBeVisible(); err != nil {
		t.Fatalf("detail pane should show the session's resource as a link: %v", err)
	}

	conversation := page.GetByRole("region", playwright.PageGetByRoleOptions{Name: "Conversation"})
	if err := expect.Locator(conversation.GetByText("hello from the browser test")).ToBeVisible(); err != nil {
		t.Fatalf("history should render the recorded utterance body: %v", err)
	}
	if err := expect.Locator(conversation.GetByText("acceptance.custom.unknown")).ToBeVisible(); err != nil {
		t.Fatalf("history should surface an unknown event kind's own type: %v", err)
	}

	requireNoConsoleErrors(t, errs)
}

// Acceptance: an event published directly through the service appears in
// an open session's timeline without a page reload.
func TestBrowserAcceptance_LiveEventArrivesWithoutReload(t *testing.T) {
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-live-1"})
	origin, svc := browserOrigin(t, store, &Config{})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)
	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}
	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-live-1"}).Click(); err != nil {
		t.Fatalf("select session: %v", err)
	}
	if err := expect.Locator(page.GetByText("No events recorded yet.")).ToBeVisible(); err != nil {
		t.Fatalf("empty session should render its empty state before the live event arrives: %v", err)
	}

	publish(t, svc, "browser-live-1", service.EventPublishParams{Type: event.TypeUserNote, Summary: "arrived-live-without-reload"})

	if err := expect.Locator(page.GetByText("arrived-live-without-reload")).ToBeVisible(); err != nil {
		t.Fatalf("live event should appear without a reload: %v", err)
	}
	requireNoConsoleErrors(t, errs)
}

// Acceptance: reconnecting after an interruption delivers an event
// published during the gap exactly once, dropping nothing shown before it.
func TestBrowserAcceptance_ReconnectRecoversWithoutDuplicateOrGap(t *testing.T) {
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-reconnect-1"})
	origin, svc := browserOrigin(t, store, &Config{})
	publish(t, svc, "browser-reconnect-1", service.EventPublishParams{Type: event.TypeUserNote, Summary: "before-interruption-first"})
	publish(t, svc, "browser-reconnect-1", service.EventPublishParams{Type: event.TypeUserNote, Summary: "before-interruption-second"})

	// No consoleErrors collector here: deliberately going offline below
	// produces its own net::ERR_INTERNET_DISCONNECTED console error, which
	// is the expected side effect being tested, not an app bug.
	page := newBrowserPage(t)
	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}
	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-reconnect-1"}).Click(); err != nil {
		t.Fatalf("select session: %v", err)
	}
	if err := expect.Locator(page.GetByText("before-interruption-second")).ToBeVisible(); err != nil {
		t.Fatalf("history should be visible before the interruption: %v", err)
	}

	ctx := page.Context()
	if err := ctx.SetOffline(true); err != nil {
		t.Fatalf("simulate connection interruption: %v", err)
	}

	publish(t, svc, "browser-reconnect-1", service.EventPublishParams{Type: event.TypeUserNote, Summary: "arrived-during-the-gap"})

	if err := ctx.SetOffline(false); err != nil {
		t.Fatalf("restore connection: %v", err)
	}

	gapEvent := page.GetByText("arrived-during-the-gap", playwright.PageGetByTextOptions{Exact: playwright.Bool(true)})
	// The reconnect backoff (docs/design/web-ui-event-history.md) plus
	// Chromium's own offline-detection latency can take several seconds;
	// Playwright's own polling assertion waits for it rather than a fixed
	// sleep.
	longWait := playwright.LocatorAssertionsToBeVisibleOptions{Timeout: playwright.Float(30000)}
	if err := expect.Locator(gapEvent).ToBeVisible(longWait); err != nil {
		t.Fatalf("event published during the outage should appear once reconnected: %v", err)
	}
	if err := expect.Locator(gapEvent).ToHaveCount(1); err != nil {
		t.Fatalf("reconnect must not re-deliver the event as a duplicate: %v", err)
	}
	if err := expect.Locator(page.GetByText("before-interruption-first")).ToBeVisible(); err != nil {
		t.Fatalf("events shown before the interruption must not be dropped by the reconnect: %v", err)
	}
}

// Acceptance: switching away from a session whose history read is still
// pending, and back, isolates each session's content and restores the
// first session's scroll position.
func TestBrowserAcceptance_SwitchingSessionsIsolatesPendingStream(t *testing.T) {
	const scrollableEventCount = 30
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-switch-a"})
	seedSession(t, store, &domain.Session{Name: "browser-switch-b"})

	svcCfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	isolateMachineConfig(svcCfg)
	svc := newLiveService(svcCfg, store)
	bus := startEventBusRelay(t, svcCfg, store)
	s := NewWithConfig(svc, &Config{})
	s.busClientFn = func() *event.Client {
		return &event.Client{BaseURL: bus.URL, HTTP: http.DefaultClient}
	}

	// Holds the first GET /api/v1/events?session=browser-switch-a request
	// open until releaseA runs, so a's read is provably pending, not merely
	// assumed so, at the point the test switches away from it.
	aRequestStarted := make(chan struct{})
	aReleaseCh := make(chan struct{})
	var aBlockOnce sync.Once
	releaseA := func() {
		select {
		case <-aReleaseCh:
		default:
			close(aReleaseCh)
		}
	}
	routes := s.Routes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events" && r.URL.Query().Get("session") == "browser-switch-a" {
			blocked := false
			aBlockOnce.Do(func() {
				blocked = true
				close(aRequestStarted)
			})
			if blocked {
				<-aReleaseCh
			}
		}
		routes.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// Registered after srv's own cleanup above, so it runs first (t.Cleanup
	// is LIFO): a failure anywhere below must not leave the intercepted
	// handler permanently blocked while srv.Close waits for it to return.
	t.Cleanup(releaseA)
	origin := srv.URL

	for i := range scrollableEventCount {
		publish(t, svc, "browser-switch-a", service.EventPublishParams{
			Type: event.TypeUserNote, Summary: fmt.Sprintf("only-in-a-%02d", i),
		})
	}
	publish(t, svc, "browser-switch-b", service.EventPublishParams{Type: event.TypeUserNote, Summary: "only-in-b"})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)

	// A hard synchronization point on the network layer itself: once this
	// fires, a's (late) response has actually reached the browser, not
	// merely "probably already arrived by now".
	aResponseReceived := make(chan struct{})
	var aResponseOnce sync.Once
	page.OnResponse(func(resp playwright.Response) {
		if strings.Contains(resp.URL(), "/api/v1/events?") && strings.Contains(resp.URL(), "session=browser-switch-a") {
			aResponseOnce.Do(func() { close(aResponseReceived) })
		}
	})

	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}

	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-switch-a"}).Click(); err != nil {
		t.Fatalf("select a: %v", err)
	}
	<-aRequestStarted

	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-switch-b"}).Click(); err != nil {
		t.Fatalf("select b while a's read is still genuinely pending: %v", err)
	}
	if err := expect.Locator(page.GetByText("only-in-b")).ToBeVisible(); err != nil {
		t.Fatalf("b's own event should render: %v", err)
	}
	if err := expect.Locator(page.GetByText("only-in-a-00")).Not().ToBeVisible(); err != nil {
		t.Fatalf("switching away from a while its read is pending must not leak a's content into b's view: %v", err)
	}

	releaseA()
	<-aResponseReceived
	// Force the event loop past whatever a's now-arrived (but abandoned)
	// response scheduled, using a positive assertion as the fence: this
	// marker is delivered through b's own live subscription, which shares
	// the same JS task queue, so waiting for it to render only succeeds
	// after a's late response has already been handled one way or another.
	publish(t, svc, "browser-switch-b", service.EventPublishParams{Type: event.TypeUserNote, Summary: "b-marker-after-late-a-response"})
	if err := expect.Locator(page.GetByText("b-marker-after-late-a-response")).ToBeVisible(); err != nil {
		t.Fatalf("b's live marker should still arrive normally: %v", err)
	}
	if err := expect.Locator(page.GetByText("only-in-a-00")).Not().ToBeVisible(); err != nil {
		t.Fatalf("a's late, released history response must not leak into b's now-settled view: %v", err)
	}

	publish(t, svc, "browser-switch-a", service.EventPublishParams{Type: event.TypeUserNote, Summary: "published-to-a-while-viewing-b"})
	if err := expect.Locator(page.GetByText("published-to-a-while-viewing-b")).Not().ToBeVisible(); err != nil {
		t.Fatalf("an event published to a's now-unselected stream must not appear while b is selected: %v", err)
	}

	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-switch-a"}).Click(); err != nil {
		t.Fatalf("switch back to a: %v", err)
	}
	conversation := page.GetByRole("region", playwright.PageGetByRoleOptions{Name: "Conversation"})
	lastAEvent := fmt.Sprintf("only-in-a-%02d", scrollableEventCount-1)
	if err := expect.Locator(conversation.GetByText(lastAEvent)).ToBeVisible(); err != nil {
		t.Fatalf("a's own history should still be there after switching back: %v", err)
	}
	if err := expect.Locator(conversation.GetByText("published-to-a-while-viewing-b")).ToBeVisible(); err != nil {
		t.Fatalf("a's event published while it was unselected should show up once a is reselected: %v", err)
	}

	// The scroll event is dispatched explicitly: a native "scroll" event
	// does not bubble, so setting scrollTop alone is not guaranteed to have
	// already invoked Conversation.tsx's onScroll handler by the time the
	// next line reads it back.
	if _, err := conversation.Evaluate(`el => { el.scrollTop = el.scrollHeight; el.dispatchEvent(new Event("scroll")); }`, nil); err != nil {
		t.Fatalf("scroll a's conversation: %v", err)
	}
	scrollBefore, err := conversation.Evaluate("el => el.scrollTop", nil)
	if err != nil {
		t.Fatalf("read scrollTop before switching away: %v", err)
	}
	if fmt.Sprint(scrollBefore) == "0" {
		t.Fatal("test setup did not produce a scrollable conversation; seed more events")
	}

	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-switch-b"}).Click(); err != nil {
		t.Fatalf("switch away from a again: %v", err)
	}
	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-switch-a"}).Click(); err != nil {
		t.Fatalf("switch back to a again: %v", err)
	}
	if err := expect.Locator(conversation.GetByText(lastAEvent)).ToBeVisible(); err != nil {
		t.Fatalf("a's history should render again after the second switch back: %v", err)
	}
	scrollAfter, err := conversation.Evaluate("el => el.scrollTop", nil)
	if err != nil {
		t.Fatalf("read scrollTop after switching back: %v", err)
	}
	if fmt.Sprint(scrollAfter) != fmt.Sprint(scrollBefore) {
		t.Fatalf("scroll position not restored across the switch: before=%v after=%v", scrollBefore, scrollAfter)
	}

	requireNoConsoleErrors(t, errs)
}

// Acceptance: arrow keys move the roving tab stop between tree rows and
// Enter selects the focused row (SessionTree.tsx's WAI-ARIA treeview).
func TestBrowserAcceptance_KeyboardNavigatesSessionTree(t *testing.T) {
	store := state.NewStore(t.TempDir())
	for _, name := range []string{"browser-kb-a", "browser-kb-b", "browser-kb-c"} {
		seedSession(t, store, &domain.Session{Name: name})
	}
	origin, _ := browserOrigin(t, store, &Config{})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)
	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}

	first := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-kb-a"})
	if err := first.Click(); err != nil {
		t.Fatalf("focus the first row: %v", err)
	}
	if err := first.Press("ArrowDown"); err != nil {
		t.Fatalf("ArrowDown: %v", err)
	}
	second := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-kb-b"})
	if err := expect.Locator(second).ToBeFocused(); err != nil {
		t.Fatalf("ArrowDown should move the roving tab stop to the next row: %v", err)
	}

	if err := second.Press("Enter"); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if err := expect.Locator(page.GetByRole("heading", playwright.PageGetByRoleOptions{Name: "browser-kb-b", Exact: playwright.Bool(true)})).ToBeVisible(); err != nil {
		t.Fatalf("Enter should select the focused row: %v", err)
	}

	if err := second.Press("End"); err != nil {
		t.Fatalf("End: %v", err)
	}
	last := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-kb-c"})
	if err := expect.Locator(last).ToBeFocused(); err != nil {
		t.Fatalf("End should move the roving tab stop to the last row: %v", err)
	}
	requireNoConsoleErrors(t, errs)
}

// Acceptance: a narrow viewport renders the sidebar and detail pane as
// overlays instead of persistent columns (docs/design/web-ui.md).
func TestBrowserAcceptance_NarrowViewportUsesOverlayDetailPane(t *testing.T) {
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-narrow-1"})
	origin, _ := browserOrigin(t, store, &Config{})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)
	if err := page.SetViewportSize(390, 844); err != nil {
		t.Fatalf("set narrow viewport: %v", err)
	}
	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}

	if err := expect.Locator(page.GetByRole("navigation", playwright.PageGetByRoleOptions{Name: "Sessions"})).Not().ToBeVisible(); err != nil {
		t.Fatalf("a narrow viewport must not show the persistent sidebar column: %v", err)
	}
	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Open sessions"}).Click(); err != nil {
		t.Fatalf("open the sidebar overlay: %v", err)
	}
	overlaySession := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-narrow-1"})
	if err := expect.Locator(overlaySession).ToBeVisible(); err != nil {
		t.Fatalf("the session tree should render inside the narrow-layout overlay: %v", err)
	}
	if err := overlaySession.Click(); err != nil {
		t.Fatalf("select from the overlay: %v", err)
	}
	if err := expect.Locator(page.GetByRole("navigation", playwright.PageGetByRoleOptions{Name: "Sessions"})).Not().ToBeVisible(); err != nil {
		t.Fatalf("selecting a session should close the sidebar overlay: %v", err)
	}

	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Toggle details"}).Click(); err != nil {
		t.Fatalf("open details on narrow layout: %v", err)
	}
	if err := expect.Locator(page.GetByRole("heading", playwright.PageGetByRoleOptions{Name: "Details"})).ToBeVisible(); err != nil {
		t.Fatalf("details should render as its own overlay, not a persistent column: %v", err)
	}
	requireNoConsoleErrors(t, errs)
}

// Acceptance: a bounded dense tree and long timeline (40 siblings, 40
// events) render completely.
func TestBrowserAcceptance_DenseTreeAndLongTimelineRenderBounded(t *testing.T) {
	const siblingCount = 40
	const eventCount = 40

	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-dense-parent"})
	for i := range siblingCount {
		seedSession(t, store, &domain.Session{
			Name:          fmt.Sprintf("browser-dense-child-%02d", i),
			ParentSession: "browser-dense-parent",
		})
	}
	origin, svc := browserOrigin(t, store, &Config{})
	for i := range eventCount {
		publish(t, svc, "browser-dense-child-00", service.EventPublishParams{
			Type: event.TypeUserNote, Summary: fmt.Sprintf("dense-event-%02d", i),
		})
	}

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)
	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}
	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Expand browser-dense-parent"}).Click(); err != nil {
		t.Fatalf("expand the dense parent: %v", err)
	}
	if err := expect.Locator(page.GetByRole("treeitem")).ToHaveCount(siblingCount + 1); err != nil {
		t.Fatalf("every sibling should render in the tree, none dropped or duplicated: %v", err)
	}

	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-dense-child-00"}).Click(); err != nil {
		t.Fatalf("select the session with the long timeline: %v", err)
	}
	if err := expect.Locator(page.GetByText("dense-event-00")).ToBeVisible(); err != nil {
		t.Fatalf("earliest event in the long timeline should be present: %v", err)
	}
	if err := expect.Locator(page.GetByText(fmt.Sprintf("dense-event-%02d", eventCount-1))).ToBeVisible(); err != nil {
		t.Fatalf("most recent event in the long timeline should be present: %v", err)
	}
	requireNoConsoleErrors(t, errs)
}

// Acceptance: against a list large enough to matter (~90 sessions), the list
// is fetched once per page load; a burst of self-reported status-message
// events never triggers another request; and a burst of lifecycle events
// coalesces into exactly one more.
func TestBrowserAcceptance_SessionListFetchesOnceDespiteEventStorm(t *testing.T) {
	const sessionCount = 90
	const statusMessageBurst = 50
	const target = "browser-storm-00"

	store := state.NewStore(t.TempDir())
	for i := range sessionCount {
		seedSession(t, store, &domain.Session{Name: fmt.Sprintf("browser-storm-%02d", i)})
	}
	origin, svc := browserOrigin(t, store, &Config{})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)

	var mu sync.Mutex
	sessionListRequests := 0
	page.OnRequest(func(req playwright.Request) {
		if req.Method() != http.MethodGet {
			return
		}
		u, err := url.Parse(req.URL())
		if err != nil || u.Path != "/api/v1/sessions" {
			return
		}
		mu.Lock()
		sessionListRequests++
		mu.Unlock()
	})
	countSessionListRequests := func() int {
		mu.Lock()
		defer mu.Unlock()
		return sessionListRequests
	}

	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}
	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: target}).Click(); err != nil {
		t.Fatalf("select session: %v", err)
	}
	if err := page.GetByRole("button", playwright.PageGetByRoleOptions{Name: "Details", Exact: playwright.Bool(true)}).Click(); err != nil {
		t.Fatalf("open details: %v", err)
	}
	details := page.GetByRole("complementary", playwright.PageGetByRoleOptions{Name: "Details"})
	if err := expect.Locator(details.GetByRole("heading", playwright.LocatorGetByRoleOptions{Name: target, Exact: playwright.Bool(true)})).ToBeVisible(); err != nil {
		t.Fatalf("detail pane should have loaded before the burst starts: %v", err)
	}
	if got := countSessionListRequests(); got != 1 {
		t.Fatalf("expected exactly one session-list request from page load, got %d", got)
	}

	for i := range statusMessageBurst {
		publish(t, svc, target, service.EventPublishParams{
			Type:    event.TypeStatusMessage,
			Summary: fmt.Sprintf("status %02d", i),
			Metadata: map[string]string{
				"text": fmt.Sprintf("status %02d", i), "cleared": "false", "previous": "",
			},
		})
	}
	lastStatus := details.GetByText(fmt.Sprintf("status %02d", statusMessageBurst-1))
	if err := expect.Locator(lastStatus).ToBeVisible(); err != nil {
		t.Fatalf("detail pane should reflect the last status message: %v", err)
	}
	if got := countSessionListRequests(); got != 1 {
		t.Fatalf("a status-message burst must not trigger any session-list request, got %d total", got)
	}

	for i := range 3 {
		publish(t, svc, target, service.EventPublishParams{
			Type: event.TypeLifecyclePrefix + "task_setup", Summary: fmt.Sprintf("node-%d", i),
		})
	}
	publish(t, svc, target, service.EventPublishParams{Type: event.TypeLifecyclePrefix + "up", Summary: "up"})

	// The client-side coalescing debounce delays the refetch by a few hundred
	// milliseconds; poll for it rather than assuming a fixed sleep covers it.
	deadline := time.Now().Add(5 * time.Second)
	for countSessionListRequests() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := countSessionListRequests(); got != 2 {
		t.Fatalf("a burst of lifecycle events should coalesce into exactly one more session-list request, got %d total", got)
	}

	requireNoConsoleErrors(t, errs)
}

// Acceptance: a lifecycle event on a session that is not selected still
// refreshes its own row in the tree — the selected session's own live
// stream cannot observe it, so the list needs its own, session-independent
// source (useSessionListLiveFacts).
func TestBrowserAcceptance_UnselectedSessionRowUpdatesLive(t *testing.T) {
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-cross-selected"})
	seedSession(t, store, &domain.Session{Name: "browser-cross-other"})
	origin, svc := browserOrigin(t, store, &Config{})

	page := newBrowserPage(t)
	errs := consoleErrors(t, page)
	if _, err := page.Goto(origin + "/app/"); err != nil {
		t.Fatalf("goto /app/: %v", err)
	}
	if err := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-cross-selected"}).Click(); err != nil {
		t.Fatalf("select a session other than the one that will change: %v", err)
	}

	otherDot := page.GetByRole("treeitem", playwright.PageGetByRoleOptions{Name: "browser-cross-other"}).Locator("[aria-hidden='true']")
	if err := expect.Locator(otherDot).ToHaveClass(regexp.MustCompile(`bg-muted-foreground/40`)); err != nil {
		t.Fatalf("unselected session should render its seeded down state: %v", err)
	}

	// Run is derived from a produced run-scoped task-state entry (RunScopeUp),
	// not a stored field; a real `up` records both this and the event below,
	// so publish() alone would record the event but not the fact the list
	// actually renders.
	sess, err := store.GetE("browser-cross-other")
	if err != nil {
		t.Fatal(err)
	}
	sess.Tasks = map[string]*contract.TaskState{
		"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
	}
	if err := store.Put(sess); err != nil {
		t.Fatal(err)
	}
	publish(t, svc, "browser-cross-other", service.EventPublishParams{Type: event.TypeLifecyclePrefix + "up", Summary: "up"})

	longWait := playwright.LocatorAssertionsToHaveClassOptions{Timeout: playwright.Float(5000)}
	if err := expect.Locator(otherDot).ToHaveClass(regexp.MustCompile(`bg-green-500`), longWait); err != nil {
		t.Fatalf("a lifecycle event for an unselected session should still refresh its row: %v", err)
	}
	requireNoConsoleErrors(t, errs)
}

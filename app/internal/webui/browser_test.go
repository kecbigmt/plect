//go:build browser

// Browser-level acceptance for the /app/ shell's read/live milestone: a
// real Chromium against the committed /app/ build, served by the real
// service/state stack over httptest.NewServer, exactly as
// acceptance_test.go's HTTP-level suite already does — this file adds the
// browser on top of that same seam. Fixtures are seeded directly through
// state.Store/service.PublishEvent, never through a mutation UI, matching
// this milestone's read/live-only scope.
package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/mxschmitt/playwright-go"
)

// browserOrigin wires a real service/state stack, a fake event-bus relay
// (startEventBusRelay, shared with the HTTP-level acceptance suite), and the
// production Routes() — including the real, committed /app/ build — behind
// an httptest server. cfg controls auth; pass &Config{} for the network-trust
// default.
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

// blockFirstHistoryRequest returns a middleware that holds up the first
// GET /api/v1/events?session=<session> request until release is called,
// so a test can force that session's read to be genuinely in flight (not
// merely hope it hasn't settled yet) at a chosen point. Only the first
// matching request blocks; every later request for the same session (e.g.
// after switching back to it) passes straight through.
func blockFirstHistoryRequest(session string) (mw func(http.Handler) http.Handler, started <-chan struct{}, release func()) {
	startedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var once sync.Once
	mw = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/events" && r.URL.Query().Get("session") == session {
				blocked := false
				once.Do(func() {
					blocked = true
					close(startedCh)
				})
				if blocked {
					<-releaseCh
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	release = func() {
		select {
		case <-releaseCh:
		default:
			close(releaseCh)
		}
	}
	return mw, startedCh, release
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

// Given a fresh Go-served build with a real session hierarchy, recorded
// utterance and unknown-type events, and auth_token configured,
// When a browser signs in and navigates the tree into a child session,
// Then it can inspect that session's resource and read its history —
// covering the milestone's login, hierarchy navigation, session
// detail/resource inspection, and history reading criteria in one flow.
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

// Given a session already open in the browser with no prior history,
// When an event is published directly through the service (not the UI),
// Then it appears in the timeline without a page reload.
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

// Given a live timeline that is interrupted mid-connection,
// When an event is published during the outage and the connection recovers,
// Then that event appears exactly once, and the events shown before the
// interruption remain visible — no duplicate, no silent gap.
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

	// Activity created while the browser is offline — the client cannot see
	// this until it reconnects.
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

// Given a first session whose history read is deliberately held pending on
// the server (blockFirstHistoryRequest), and a second, unrelated session,
// When the browser switches to the second session while that read is still
// in flight, and the first session's read is only then allowed to reach
// the browser,
// Then the second session's view never shows the first session's content —
// neither before the pending read resolves nor once its late response
// finally arrives — and an event published to the first session's
// now-unselected stream does not leak in either. Switching back restores
// the first session's own history, including what arrived while it was
// unselected, and its prior scroll position.
func TestBrowserAcceptance_SwitchingSessionsIsolatesPendingStream(t *testing.T) {
	const scrollableEventCount = 30
	store := state.NewStore(t.TempDir())
	seedSession(t, store, &domain.Session{Name: "browser-switch-a"})
	seedSession(t, store, &domain.Session{Name: "browser-switch-b"})

	// Only this test needs a's history read held pending, so the gate wraps
	// the handler directly here rather than becoming a second, one-consumer
	// parameter on the shared browserOrigin helper.
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
	blockA, aRequestStarted, releaseA := blockFirstHistoryRequest("browser-switch-a")
	srv := httptest.NewServer(blockA(s.Routes()))
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

// Given a session tree with several siblings,
// When the browser drives it by keyboard alone,
// Then arrow keys move the roving tab stop between rows and Enter selects
// the focused row — the WAI-ARIA treeview pattern SessionTree.tsx implements.
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

// Given a narrow viewport,
// When a session and its details are opened,
// Then the sidebar and detail pane render as a temporary overlay rather than
// a persistent column (docs/design/web-ui.md's narrow-screen rule).
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
	// Selecting a session closes the sidebar overlay on a narrow layout
	// (AppShell.tsx's selectSession), so the tree it came from is gone.
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

// A bounded dense-tree/long-timeline example: enough siblings and history
// that the tree and the timeline are not trivially small, small enough to
// stay a fast, deterministic test.
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

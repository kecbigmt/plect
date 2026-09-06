//go:build browser

package webui

import (
	"log/slog"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// sharedBrowser is launched once for the whole package's browser-tagged test
// run: a fresh browser.NewContext() per test still isolates cookies/storage
// (see newBrowserPage), so nothing is shared across tests except the
// expensive one-time process launch.
var sharedBrowser playwright.Browser

func runTestMain(m *testing.M) int {
	pw, err := playwright.Run()
	if err != nil {
		slog.Error("could not start the Playwright driver; run 'go run github.com/mxschmitt/playwright-go/cmd/playwright install --with-deps chromium' first", "error", err)
		return 1
	}
	defer func() {
		if err := pw.Stop(); err != nil {
			slog.Error("playwright.Stop", "error", err)
		}
	}()

	browser, err := pw.Chromium.Launch()
	if err != nil {
		slog.Error("could not launch chromium", "error", err)
		return 1
	}
	defer func() {
		if err := browser.Close(); err != nil {
			slog.Error("browser.Close", "error", err)
		}
	}()
	sharedBrowser = browser

	return m.Run()
}

func newBrowserPage(t *testing.T) playwright.Page {
	t.Helper()
	ctx, err := sharedBrowser.NewContext()
	if err != nil {
		t.Fatalf("browser.NewContext: %v", err)
	}
	t.Cleanup(func() {
		if err := ctx.Close(); err != nil {
			t.Errorf("context.Close: %v", err)
		}
	})
	page, err := ctx.NewPage()
	if err != nil {
		t.Fatalf("context.NewPage: %v", err)
	}
	return page
}

// consoleErrors collects the browser page's own console.error output so a
// scenario that passes on the DOM but is quietly throwing in the client can
// still fail the test.
func consoleErrors(t *testing.T, page playwright.Page) *[]string {
	t.Helper()
	var errs []string
	page.OnConsole(func(msg playwright.ConsoleMessage) {
		if msg.Type() == "error" {
			errs = append(errs, msg.Text())
		}
	})
	return &errs
}

func requireNoConsoleErrors(t *testing.T, errs *[]string) {
	t.Helper()
	if len(*errs) > 0 {
		t.Errorf("browser console reported error(s): %v", *errs)
	}
}

var expect = playwright.NewPlaywrightAssertions()

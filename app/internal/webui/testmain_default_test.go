//go:build !browser

package webui

import "testing"

// runTestMain is a no-op passthrough for every build that does not carry the
// browser tag: the browser-tagged variant (testmain_browser_test.go) replaces
// this to launch and tear down the shared Chromium instance around m.Run().
func runTestMain(m *testing.M) int {
	return m.Run()
}

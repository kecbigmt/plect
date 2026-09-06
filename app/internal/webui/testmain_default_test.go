//go:build !browser

package webui

import "testing"

// testmain_browser_test.go's browser-tagged runTestMain replaces this one to
// launch and tear down the shared Chromium instance around m.Run().
func runTestMain(m *testing.M) int {
	return m.Run()
}

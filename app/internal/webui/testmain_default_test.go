//go:build !browser

package webui

import "testing"

// Every default go test/go vet run — including every non-browser CI job —
// must stay usable on a host with no Playwright driver or Chromium
// installed at all, so this stub must never attempt to launch one.
func runTestMain(m *testing.M) int {
	return m.Run()
}

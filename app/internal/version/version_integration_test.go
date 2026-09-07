//go:build integration

package version

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIntegration_LdflagsInjectsCurrent exercises the exact -ldflags -X
// string a packager (the release pipeline, a Nix package, ...) must inject,
// so a typo in that import path — a rename this package's own tests would
// not otherwise catch — fails here instead of only surfacing as every
// packaged binary silently staying classified as a development build.
func TestIntegration_LdflagsInjectsCurrent(t *testing.T) {
	const injected = "9.9.9"
	bin := filepath.Join(t.TempDir(), "versionprobe")
	cmd := exec.Command("go", "build",
		"-ldflags", "-X github.com/kecbigmt/plecture/app/internal/version.Current="+injected,
		"-o", bin, "./testdata/versionprobe")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build versionprobe: %v\n%s", err, out)
	}

	out, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatalf("run versionprobe: %v", err)
	}
	if got := string(out); got != injected {
		t.Errorf("versionprobe printed %q, want %q", got, injected)
	}
}

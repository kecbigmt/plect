package version

import "testing"

func TestIsDevelopmentBuild_TrueForTheUnstampedPlaceholder(t *testing.T) {
	if Current != "0.0.0-dev" {
		t.Fatalf("Current = %q, want the dev placeholder for this test to be meaningful", Current)
	}
	if !IsDevelopmentBuild() {
		t.Error("IsDevelopmentBuild() = false, want true for the unstamped placeholder")
	}
}

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

func TestIsDevelopmentBuild_FalseOnceStamped(t *testing.T) {
	original := Current
	t.Cleanup(func() { Current = original })

	Current = "1.2.3"
	if IsDevelopmentBuild() {
		t.Error("IsDevelopmentBuild() = true after stamping Current, want false")
	}
}

// -ldflags -X can only overwrite a package-level string variable, not a
// constant; this test compiles only if Current is a var, so reverting it to
// a const fails the build, not just the assertion below.
func TestCurrent_IsAssignable(t *testing.T) {
	original := Current
	t.Cleanup(func() { Current = original })

	Current = "1.2.3"

	if Current != "1.2.3" {
		t.Fatalf("Current = %q after assignment, want %q", Current, "1.2.3")
	}
}

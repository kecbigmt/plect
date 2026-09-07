package version

import "testing"

func TestCurrent_DefaultsToDevPlaceholder(t *testing.T) {
	if Current != "0.0.0-dev" {
		t.Errorf("Current = %q, want the dev placeholder", Current)
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

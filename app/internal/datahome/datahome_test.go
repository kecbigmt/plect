package datahome

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolve_DefaultsToXDGDataSharePlect(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(EnvVar, "")
	t.Setenv(XDGEnvVar, "")

	want := filepath.Join(tmpHome, ".local", "share", "plect")
	if got := Resolve(); got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolve_XDGDataHomeOverridesDefaultWithPlectSuffix(t *testing.T) {
	t.Setenv(EnvVar, "")
	xdgDataHome := t.TempDir()
	t.Setenv(XDGEnvVar, xdgDataHome)

	want := filepath.Join(xdgDataHome, "plect")
	if got := Resolve(); got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolve_EnvVarOverridesXDGDataHomeWithNoSuffix(t *testing.T) {
	t.Setenv(XDGEnvVar, t.TempDir())
	override := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv(EnvVar, override)

	if got := Resolve(); got != override {
		t.Fatalf("Resolve() = %q, want %q (no /plect suffix appended)", got, override)
	}
}

// TestInheritableEnv_StripsPlectDataHomeButKeepsXDGDataHome pins the
// asymmetry: EnvVar is plect-specific and always stripped, while XDGEnvVar
// is a general-purpose variable existing declarations (a resource
// observer, a plugin service) already rely on inheriting for their own
// unrelated on-disk state — see the doc comment on InheritableEnv.
func TestInheritableEnv_StripsPlectDataHomeButKeepsXDGDataHome(t *testing.T) {
	t.Setenv(EnvVar, "/x")
	t.Setenv(XDGEnvVar, "/shared")
	t.Setenv("UNRELATED_VAR", "kept")

	env := InheritableEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, EnvVar+"=") {
			t.Fatalf("InheritableEnv() kept %q, want it stripped", kv)
		}
	}
	if !slices.Contains(env, XDGEnvVar+"=/shared") {
		t.Fatalf("InheritableEnv() stripped %s, want it kept: %v", XDGEnvVar, env)
	}
	if !slices.Contains(env, "UNRELATED_VAR=kept") {
		t.Fatalf("InheritableEnv() dropped an unrelated variable, got %v", env)
	}
}

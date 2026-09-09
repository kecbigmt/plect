package cachehome

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolve_DefaultsToXDGCachePlectCatalogs(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv(EnvVar, "")
	t.Setenv(XDGEnvVar, "")

	want := filepath.Join(tmpHome, ".cache", "plect", "catalogs")
	if got := Resolve(); got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolve_XDGCacheHomeOverridesDefaultWithPlectCatalogsSuffix(t *testing.T) {
	t.Setenv(EnvVar, "")
	xdgCacheHome := t.TempDir()
	t.Setenv(XDGEnvVar, xdgCacheHome)

	want := filepath.Join(xdgCacheHome, "plect", "catalogs")
	if got := Resolve(); got != want {
		t.Fatalf("Resolve() = %q, want %q", got, want)
	}
}

func TestResolve_EnvVarOverridesXDGCacheHomeWithNoSuffix(t *testing.T) {
	t.Setenv(XDGEnvVar, t.TempDir())
	override := filepath.Join(t.TempDir(), "custom-cache")
	t.Setenv(EnvVar, override)

	if got := Resolve(); got != override {
		t.Fatalf("Resolve() = %q, want %q (no /plect/catalogs suffix appended)", got, override)
	}
}

func TestStrip_RemovesPlectCacheHomeButKeepsOtherVars(t *testing.T) {
	env := []string{
		EnvVar + "=/x",
		XDGEnvVar + "=/shared",
		"UNRELATED_VAR=kept",
	}

	got := Strip(env)
	for _, kv := range got {
		if strings.HasPrefix(kv, EnvVar+"=") {
			t.Fatalf("Strip() kept %q, want it stripped", kv)
		}
	}
	if !slices.Contains(got, XDGEnvVar+"=/shared") {
		t.Fatalf("Strip() removed %s, want it kept: %v", XDGEnvVar, got)
	}
	if !slices.Contains(got, "UNRELATED_VAR=kept") {
		t.Fatalf("Strip() dropped an unrelated variable, got %v", got)
	}
}

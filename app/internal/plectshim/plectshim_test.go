package plectshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFakeBin(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// One argv element per line: unambiguous to split back into a slice.
const echoArgvScript = "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"

func TestBuild_ShimForwardsArgvToTheDaemonBinaryWithDataHome(t *testing.T) {
	dataHome := t.TempDir()
	bin := writeFakeBin(t, t.TempDir(), "fake-plect", echoArgvScript)

	shimDir, err := Build(bin, dataHome, "", "")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantDir := filepath.Join(dataHome, "bin")
	if shimDir != wantDir {
		t.Fatalf("shimDir = %q, want %q", shimDir, wantDir)
	}

	out, err := exec.Command(filepath.Join(shimDir, shimName), "event", "publish", "foo").Output()
	if err != nil {
		t.Fatalf("shim exec: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	want := []string{"--data-home", dataHome, "event", "publish", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shim forwarded argv = %v, want %v", got, want)
	}
}

func TestBuild_IncludesConfigAndCacheHomeOnlyWhenGiven(t *testing.T) {
	dataHome := t.TempDir()
	bin := writeFakeBin(t, t.TempDir(), "fake-plect", echoArgvScript)

	shimDir, err := Build(bin, dataHome, "/config-home", "/cache-home")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := exec.Command(filepath.Join(shimDir, shimName)).Output()
	if err != nil {
		t.Fatalf("shim exec: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	want := []string{"--data-home", dataHome, "--config-home", "/config-home", "--cache-home", "/cache-home"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shim forwarded argv = %v, want %v", got, want)
	}
}

func TestBuild_OverwritesAStaleShimIdempotently(t *testing.T) {
	dataHome := t.TempDir()
	binDir := t.TempDir()
	first := writeFakeBin(t, binDir, "first", echoArgvScript)
	second := writeFakeBin(t, binDir, "second", echoArgvScript)

	if _, err := Build(first, dataHome, "", ""); err != nil {
		t.Fatalf("Build (first): %v", err)
	}
	shimDir, err := Build(second, dataHome, "", "")
	if err != nil {
		t.Fatalf("Build (second): %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(shimDir, shimName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), first) {
		t.Fatalf("shim still points at the stale binary %q:\n%s", first, raw)
	}
	if !strings.Contains(string(raw), second) {
		t.Fatalf("shim does not point at the rebuilt binary %q:\n%s", second, raw)
	}
}

func TestBuild_RejectsAnEmptyBinOrDataHome(t *testing.T) {
	if _, err := Build("", "/data", "", ""); err == nil {
		t.Fatal("Build with empty bin: want error, got nil")
	}
	if _, err := Build("/bin/plect", "", "", ""); err == nil {
		t.Fatal("Build with empty dataHome: want error, got nil")
	}
}

func TestForCurrentProcess_RefusesUnderGoTestUnlessOverridden(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("PLECT_DATA_HOME", dataHome)

	// No UseExecutableForTest override: this test itself runs under `go
	// test`, the exact case that must refuse, so a real script under test
	// can never recurse into re-invoking the test binary.
	if _, err := ForCurrentProcess(); err == nil {
		t.Fatal("ForCurrentProcess: want an error under go test with no override, got nil")
	}
	if _, err := os.Stat(filepath.Join(dataHome, "bin")); !os.IsNotExist(err) {
		t.Fatalf("ForCurrentProcess wrote a shim despite refusing: stat = %v", err)
	}
}

func TestForCurrentProcess_PinsDataHomeAloneWhenNothingElseIsOverridden(t *testing.T) {
	dataHome := t.TempDir()
	fakeBin := filepath.Join(t.TempDir(), "plect")
	defer UseExecutableForTest(fakeBin)()
	t.Setenv("PLECT_DATA_HOME", dataHome)
	t.Setenv("PLECT_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PLECT_CACHE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	shimDir, err := ForCurrentProcess()
	if err != nil {
		t.Fatalf("ForCurrentProcess: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(shimDir, shimName))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if !strings.Contains(script, fakeBin) {
		t.Errorf("shim script does not exec the running binary %q:\n%s", fakeBin, script)
	}
	if !strings.Contains(script, "--data-home") || !strings.Contains(script, dataHome) {
		t.Errorf("shim script does not pin --data-home %q:\n%s", dataHome, script)
	}
	if strings.Contains(script, "--config-home") || strings.Contains(script, "--cache-home") {
		t.Errorf("shim script pins a config/cache-home flag with neither overridden:\n%s", script)
	}
}

func TestForCurrentProcess_PinsConfigAndCacheHomeWhenOverridden(t *testing.T) {
	dataHome := t.TempDir()
	configHome := t.TempDir()
	cacheHome := t.TempDir()
	defer UseExecutableForTest(filepath.Join(t.TempDir(), "plect"))()
	t.Setenv("PLECT_DATA_HOME", dataHome)
	t.Setenv("PLECT_CONFIG_HOME", configHome)
	t.Setenv("PLECT_CACHE_HOME", cacheHome)

	shimDir, err := ForCurrentProcess()
	if err != nil {
		t.Fatalf("ForCurrentProcess: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(shimDir, shimName))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if !strings.Contains(script, "--config-home") || !strings.Contains(script, configHome) {
		t.Errorf("shim script does not pin --config-home %q:\n%s", configHome, script)
	}
	if !strings.Contains(script, "--cache-home") || !strings.Contains(script, cacheHome) {
		t.Errorf("shim script does not pin --cache-home %q:\n%s", cacheHome, script)
	}
}

// plect-web has no CLI subcommand tree of its own; the shim must exec its
// "plect" sibling, not the running plect-web binary itself.
func TestForCurrentProcess_ResolvesTheSiblingPlectBinaryForANonCLIHost(t *testing.T) {
	dir := t.TempDir()
	webBin := writeFakeBin(t, dir, "plect-web", "#!/bin/sh\nexit 2\n")
	writeFakeBin(t, dir, "plect", echoArgvScript)
	defer UseExecutableForTest(webBin)()

	dataHome := t.TempDir()
	t.Setenv("PLECT_DATA_HOME", dataHome)

	shimDir, err := ForCurrentProcess()
	if err != nil {
		t.Fatalf("ForCurrentProcess: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(shimDir, shimName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), filepath.Join(dir, "plect")) {
		t.Fatalf("shim script does not exec the plect sibling:\n%s", raw)
	}
	if strings.Contains(string(raw), webBin) {
		t.Fatalf("shim script execs plect-web itself instead of its sibling:\n%s", raw)
	}
}

// A standalone dev build with no shipped sibling still needs an operator
// to name the exact matching CLI build explicitly via EnvVar -- resolveBin
// never guesses one from PATH, which could silently pick an unrelated,
// differently-versioned installed plect.
func TestForCurrentProcess_UsesEnvVarWhenNoSiblingPlectExists(t *testing.T) {
	webBin := filepath.Join(t.TempDir(), "plect-web")
	if err := os.WriteFile(webBin, []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	namedBin := writeFakeBin(t, t.TempDir(), "plect", echoArgvScript)
	defer UseExecutableForTest(webBin)()
	t.Setenv(EnvVar, namedBin)
	t.Setenv("PLECT_DATA_HOME", t.TempDir())

	shimDir, err := ForCurrentProcess()
	if err != nil {
		t.Fatalf("ForCurrentProcess: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(shimDir, shimName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), namedBin) {
		t.Fatalf("shim script does not exec the %s-named plect:\n%s", EnvVar, raw)
	}
}

// A relative EnvVar value must not reach the shim script verbatim: the
// script runs later, from an isolated child's own working directory.
func TestForCurrentProcess_NormalizesARelativeEnvVarToAbsolute(t *testing.T) {
	webBin := filepath.Join(t.TempDir(), "plect-web")
	if err := os.WriteFile(webBin, []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeFakeBin(t, binDir, "myplect", echoArgvScript)
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(binDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)
	defer UseExecutableForTest(webBin)()
	t.Setenv(EnvVar, "myplect")
	t.Setenv("PLECT_DATA_HOME", t.TempDir())

	shimDir, err := ForCurrentProcess()
	if err != nil {
		t.Fatalf("ForCurrentProcess: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(shimDir, shimName))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(binDir, "myplect")
	if !strings.Contains(string(raw), want) {
		t.Fatalf("shim script does not exec the absolute path %q:\n%s", want, raw)
	}
	if strings.Contains(string(raw), "'myplect'") {
		t.Fatalf("shim script embeds the relative %s value verbatim:\n%s", EnvVar, raw)
	}
}

func TestForCurrentProcess_RefusesWhenNoSiblingPlectExistsAndEnvVarUnset(t *testing.T) {
	webBin := filepath.Join(t.TempDir(), "plect-web")
	if err := os.WriteFile(webBin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer UseExecutableForTest(webBin)()
	t.Setenv(EnvVar, "")
	t.Setenv("PLECT_DATA_HOME", t.TempDir())

	if _, err := ForCurrentProcess(); err == nil {
		t.Fatal("ForCurrentProcess: want an error with no plect sibling, got nil")
	}
}

// An unrelated plect elsewhere on the process's own PATH must never win by
// coincidence: only a same-directory sibling or an explicit EnvVar counts.
func TestForCurrentProcess_IgnoresAnUnrelatedAmbientPlectOnPATH(t *testing.T) {
	webBin := filepath.Join(t.TempDir(), "plect-web")
	if err := os.WriteFile(webBin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := writeFakeBin(t, t.TempDir(), "plect", echoArgvScript)
	defer UseExecutableForTest(webBin)()
	t.Setenv("PATH", filepath.Dir(unrelated))
	t.Setenv(EnvVar, "")
	t.Setenv("PLECT_DATA_HOME", t.TempDir())

	if _, err := ForCurrentProcess(); err == nil {
		t.Fatal("ForCurrentProcess: want an error rather than picking the ambient PATH plect, got nil")
	}
}

func TestPatchPath_PrependsShimDirAheadOfExistingPath(t *testing.T) {
	env := []string{"HOME=/home/x", "PATH=/usr/bin:/bin"}
	got := PatchPath(env, "/shim")
	want := append(append([]string{}, env...), "PATH=/shim"+string(os.PathListSeparator)+"/usr/bin:/bin")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PatchPath = %v, want %v", got, want)
	}
}

func TestPatchPath_AddsPathWhenAbsent(t *testing.T) {
	env := []string{"HOME=/home/x"}
	got := PatchPath(env, "/shim")
	want := []string{"HOME=/home/x", "PATH=/shim"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PatchPath = %v, want %v", got, want)
	}
}

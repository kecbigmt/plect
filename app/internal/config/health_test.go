package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

func TestHealthConfigValidate(t *testing.T) {
	probe := &lang.Action{Type: lang.ActionShell, Script: "kill -0 1"}
	tests := []struct {
		name    string
		health  *HealthConfig
		wantErr bool
	}{
		{name: "nil is valid (no probes declared)", health: nil},
		{name: "alive only", health: &HealthConfig{Alive: probe}},
		{name: "activity only", health: &HealthConfig{Activity: probe}},
		{name: "both", health: &HealthConfig{Alive: probe, Activity: probe}},
		{name: "bare table declares nothing", health: &HealthConfig{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.health.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "[health]") {
				t.Fatalf("err = %q, want it to name the [health] table", err.Error())
			}
		})
	}
}

// writeEffectDoc writes one definition document under the effect root of a
// base layer, mirroring writeChannelDoc / writeProviderDoc.
func writeEffectDoc(t *testing.T, baseDir, name, body string) *Config {
	t.Helper()
	writeFile(t, filepath.Join(baseDir, "tasks", name+".toml"), body)
	return &Config{BaseDir: baseDir}
}

// effectSetupHook is a setup every fixture that only cares about another
// field can share.
const effectSetupHook = `
[runtime.setup]
type   = "shell"
script = "echo '{}'"
`

func TestLoadTaskDefinitions_HealthProbesAreIndependent(t *testing.T) {
	tests := []struct {
		name         string
		table        string
		wantAlive    string
		wantActivity string
	}{
		{
			name:      "alive only",
			table:     "[runtime.health.alive]\ntype = \"shell\"\nscript = \"kill -0 $pid\"\n",
			wantAlive: "kill -0 $pid",
		},
		{
			// A setup-bearing effect always declares alive, so the
			// activity-only shape this test targets is exercised alongside
			// a noop alive rather than with alive absent entirely.
			name:         "activity, alongside a noop alive",
			table:        "[runtime.health.alive]\ntype = \"noop\"\n\n[runtime.health.activity]\ntype = \"exec\"\ncommand = \"agent-activity\"\nargs = [\"probe\"]\n",
			wantAlive:    "noop",
			wantActivity: "agent-activity probe",
		},
		{
			name:         "both",
			table:        "[runtime.health.alive]\ntype = \"shell\"\nscript = \"kill -0 1\"\n\n[runtime.health.activity]\ntype = \"exec\"\ncommand = \"agent-activity\"\nargs = [\"probe\"]\n",
			wantAlive:    "kill -0 1",
			wantActivity: "agent-activity probe",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := writeEffectDoc(t, t.TempDir(), "runtime", "[runtime]\nkind = \"effect\"\nscope = \"run\"\n"+effectSetupHook+"\n"+tt.table)
			defs, err := cfg.LoadTaskDefinitions("")
			if err != nil {
				t.Fatalf("LoadTaskDefinitions: %v", err)
			}
			got := defs["runtime"].Health
			if got.AliveProbe().Source() != tt.wantAlive {
				t.Errorf("alive = %q, want %q", got.AliveProbe().Source(), tt.wantAlive)
			}
			if got.ActivityProbe().Source() != tt.wantActivity {
				t.Errorf("activity = %q, want %q", got.ActivityProbe().Source(), tt.wantActivity)
			}
		})
	}
}

func TestLoadTaskDefinitions_HealthUnknownFieldRejected(t *testing.T) {
	cfg := writeEffectDoc(t, t.TempDir(), "runtime", `
[runtime]
kind  = "effect"
scope = "run"
`+effectSetupHook+`
[runtime.health.alive]
type   = "shell"
script = "true"

[runtime.health.readiness]
type   = "shell"
script = "true"
`)
	_, err := cfg.LoadTaskDefinitions("")
	if err == nil {
		t.Fatal("expected an error for an unknown [health] field")
	}
	if !strings.Contains(err.Error(), "readiness") {
		t.Errorf("error %q should name the unknown field", err.Error())
	}
}

func TestLoadTaskDefinitions_HealthBareTableRejected(t *testing.T) {
	// No setup: a setup-bearing effect's own mandatory-alive rule would fire
	// first and mask the bare-table rule this test targets.
	cfg := writeEffectDoc(t, t.TempDir(), "runtime", "[runtime]\nkind = \"effect\"\nscope = \"run\"\n\n[runtime.health]\n")
	_, err := cfg.LoadTaskDefinitions("")
	if err == nil {
		t.Fatal("expected an error for a [health] table declaring no probe")
	}
	if !strings.Contains(err.Error(), "alive") || !strings.Contains(err.Error(), "activity") {
		t.Errorf("error %q should name both probes", err.Error())
	}
}

// A completion field on an effect is the closed-grammar rule read from one
// direction: the effect surface certifies no completion contract, so the
// declaration that owns it has to be a task document.
func TestLoadTaskDefinitions_UnknownFieldRejected(t *testing.T) {
	cfg := writeEffectDoc(t, t.TempDir(), "runtime", "[runtime]\nkind = \"effect\"\nscope = \"run\"\nhealthcheck = \"true\"\n"+effectSetupHook)
	_, err := cfg.LoadTaskDefinitions("")
	if err == nil {
		t.Fatal("expected an error for a field outside the effect surface")
	}
	if !strings.Contains(err.Error(), "healthcheck") {
		t.Errorf("error %q should name the offending field", err.Error())
	}
}

func TestLoadWorkflows_RetiredTickMovementSourceRejected(t *testing.T) {
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "workflows", "default.toml"), `
[default]
kind = "workflow"
[[default.nodes]]
id   = "initial"
uses = "runtime"

[default.tick.movement_source]
name   = "fingerprint"
script = "echo hi"
`)
	cfg := &Config{BaseDir: baseDir}
	_, err := cfg.LoadWorkflows("")
	if err == nil {
		t.Fatal("expected an error for the retired [tick.movement_source] table")
	}
	if !strings.Contains(err.Error(), "movement_source") {
		t.Errorf("error %q should name the retired key", err.Error())
	}
}

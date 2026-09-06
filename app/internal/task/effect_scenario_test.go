package task

import (
	"fmt"
	"testing"
)

// effectScenario is one run condition a declaration is exercised under, read
// from the plugin's own testdata. An id can name more than one — e.g. the
// normal case plus an optional dependency's absence — in which case every
// variant needs a distinct Name to tell their recorded sections apart.
//
// This type (and the variant-name validation below) has no dependency on the
// shell/filesystem runtime the rest of the effect-scenario harness drives, so
// it lives in its own untagged file: the validation gets a fast, always-run
// unit test instead of only ever executing as a side effect of the
// `-tags integration` corpus test.
type effectScenario struct {
	// Name distinguishes one variant from another under the same id. Only
	// required when an id declares more than one variant.
	Name string `toml:"name"`
	// Inputs are the node inputs this effect is set up with, and Prev the
	// outputs a prior run of it left behind.
	Inputs map[string]string `toml:"inputs"`
	Prev   map[string]string `toml:"prev"`
	// Self stands in for setup's outputs when cleanup and the probes must be
	// exercised against an instance this scenario does not set up.
	Self map[string]string `toml:"self"`
	// Hooks selects what to run, defaulting to every action the declaration
	// carries.
	Hooks []string `toml:"hooks"`
	// Files are seeded into the sandbox home before the run, for a script
	// that discovers its own subject by reading the filesystem.
	Files []effectScenarioFile `toml:"files"`
	// Capture is what the terminal's capture verb reports, for a script that
	// waits on what the endpoint displays.
	Capture string `toml:"capture"`
	// Artifacts name files a setup generated rather than invoked, each
	// located by the output key that carries its directory. A generated
	// wrapper script is as much the effect's product as any call it made.
	Artifacts []effectScenarioArtifact `toml:"artifacts"`
	// AbsentExecutables names this plugin's own declared executables to
	// remove from the mounted copy before this variant runs, so a script's
	// presence check on an optional companion is exercised against it
	// actually being missing rather than always being the spy this harness
	// would otherwise install for every declared executable.
	AbsentExecutables []string `toml:"absent_executables"`
	// ExpectLiveProcessDead asserts, once every hook this variant runs has
	// finished, that the sandbox's own live process — the harness's real,
	// owned stand-in for whatever an agent launch would have started — has
	// actually been terminated. This is what lets a script's kill-on-failure
	// or cleanup path be verified by a real process's death rather than by
	// trusting that a recorded call did what it claims.
	ExpectLiveProcessDead bool `toml:"expect_live_process_dead"`
	// ExpectWorkerProcessDead asserts effectHarness.workerProcess died while
	// liveProcess (the endpoint's own root process) survived — the proof a
	// kill-on-failure path needs when the launched thing is not the
	// endpoint's own root process.
	ExpectWorkerProcessDead bool `toml:"expect_worker_process_dead"`
	// RetryInputs reruns setup with these inputs against the same sandbox
	// and live processes a first attempt (using Inputs) left behind, so a
	// retry-succeeds claim is checked against that exact state rather than
	// a separately-started scenario that only shares on-disk paths.
	RetryInputs map[string]string `toml:"retry_inputs"`
	// RetryCapture is RetryInputs' counterpart for a Capture-based endpoint.
	RetryCapture string `toml:"retry_capture"`
	// FailOutput makes each plugin's own jq stand-in fail the one call that
	// assembles setup's final output line, once everything else — the
	// launch, its detection, its readiness — has already succeeded. This is
	// the only reachable way to exercise "a non-zero exit after the process
	// was started" that is not the detection timeout: every value that call
	// assembles is already validated by the time setup reaches it, so no
	// scenario input can make it fail on its own.
	FailOutput bool `toml:"fail_output"`
}

type effectScenarioFile struct {
	Path    string `toml:"path"`
	Content string `toml:"content"`
}

type effectScenarioArtifact struct {
	Output string `toml:"output"`
	Path   string `toml:"path"`
}

// validateScenarioVariantNames rejects the two ways a multi-variant id's
// names could fail to tell their recorded sections apart: a variant left
// unnamed, or two variants sharing a name.
func validateScenarioVariantNames(scenarios map[string][]effectScenario) error {
	for id, variants := range scenarios {
		if len(variants) <= 1 {
			continue
		}
		seen := make(map[string]bool, len(variants))
		for _, variant := range variants {
			if variant.Name == "" {
				return fmt.Errorf("%q declares %d scenario variants; every variant needs a distinct name", id, len(variants))
			}
			if seen[variant.Name] {
				return fmt.Errorf("%q declares more than one scenario variant named %q; names must be distinct", id, variant.Name)
			}
			seen[variant.Name] = true
		}
	}
	return nil
}

func TestValidateScenarioVariantNames_RejectsUnnamedVariant(t *testing.T) {
	scenarios := map[string][]effectScenario{
		"runtime": {{Name: "present"}, {}},
	}
	err := validateScenarioVariantNames(scenarios)
	if err == nil {
		t.Fatal("want an error for an unnamed variant alongside a named one, got nil")
	}
	const want = `"runtime" declares 2 scenario variants; every variant needs a distinct name`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestValidateScenarioVariantNames_RejectsDuplicateName(t *testing.T) {
	scenarios := map[string][]effectScenario{
		"runtime": {{Name: "absent"}, {Name: "absent"}},
	}
	err := validateScenarioVariantNames(scenarios)
	if err == nil {
		t.Fatal("want an error for two variants sharing a name, got nil")
	}
	const want = `"runtime" declares more than one scenario variant named "absent"; names must be distinct`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestValidateScenarioVariantNames_AllowsDistinctNames(t *testing.T) {
	scenarios := map[string][]effectScenario{
		"runtime": {{Name: "present"}, {Name: "absent"}},
	}
	if err := validateScenarioVariantNames(scenarios); err != nil {
		t.Errorf("distinct variant names rejected: %v", err)
	}
}

func TestValidateScenarioVariantNames_AllowsASoleUnnamedVariant(t *testing.T) {
	// The common case every other plugin's scenarios.toml uses today: one
	// variant per id, with no name at all.
	scenarios := map[string][]effectScenario{
		"runtime": {{}},
	}
	if err := validateScenarioVariantNames(scenarios); err != nil {
		t.Errorf("a lone unnamed variant rejected: %v", err)
	}
}

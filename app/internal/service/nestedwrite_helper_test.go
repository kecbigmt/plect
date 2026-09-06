package service

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
	"testing"
)

// nestedWritePatch is the shape a nested-write test plants as a JSON file
// and hands to TestServiceNestedWriteHelperProcess: a partial session
// update applied through the real store.Update path, from a real separate
// process, simulating a nested `plect task setup` (or any other session
// process) racing the lifecycle command under test.
type nestedWritePatch struct {
	Tasks       map[string]*contract.TaskState `json:"tasks,omitempty"`
	Health      *contract.HealthState          `json:"health,omitempty"`
	TickBackoff *contract.TickBackoff          `json:"tick_backoff,omitempty"`
}

// TestServiceNestedWriteHelperProcess is not a real test: it is invoked as
// a subprocess (os.Args[0], the compiled test binary) by a task hook shell
// script under PLECT_SERVICE_NESTED_WRITE_HELPER=1, applying a
// nestedWritePatch to one session through store.Update. It replaced a
// jq-against-state.json shell one-liner that stopped meaning anything once
// state.json was no longer a live persistence format; going through the
// real store.Update here is a closer simulation of a real racing writer
// than editing a file the running binary never reads, not a weaker one.
func TestServiceNestedWriteHelperProcess(t *testing.T) {
	if os.Getenv("PLECT_SERVICE_NESTED_WRITE_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: -- <dir> <session-name> <patch.json>")
		os.Exit(2)
	}
	dir, sessionName, patchPath := args[1], args[2], args[3]

	data, err := os.ReadFile(patchPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read patch:", err)
		os.Exit(1)
	}
	var patch nestedWritePatch
	if err := json.Unmarshal(data, &patch); err != nil {
		fmt.Fprintln(os.Stderr, "parse patch:", err)
		os.Exit(1)
	}

	store := state.NewStore(dir)
	err = store.Update(sessionName, func(s *domain.Session) error {
		if len(patch.Tasks) > 0 {
			if s.Tasks == nil {
				s.Tasks = make(map[string]*contract.TaskState)
			}
			for k, v := range patch.Tasks {
				s.Tasks[k] = v
			}
		}
		if patch.Health != nil {
			s.Health = patch.Health
		}
		if patch.TickBackoff != nil {
			s.TickBackoff = patch.TickBackoff
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// writeNestedWritePatch serializes patch to a temp file under dir and
// returns its path, for a shell script argument.
func writeNestedWritePatch(t *testing.T, dir string, patch nestedWritePatch) string {
	t.Helper()
	data, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("marshal nested write patch: %v", err)
	}
	path := dir + "/nested-write-patch.json"
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write nested write patch: %v", err)
	}
	return path
}

// nestedWriteCommand returns the shell command a task hook runs to invoke
// TestServiceNestedWriteHelperProcess against storeDir/sessionName with
// patch, and sets the environment the subprocess needs (the compiled test
// binary path and the helper-activation env var; PLECT_SERVICE_NESTED_WRITE_HELPER
// is set via t.Setenv by the caller so the outer test process itself does
// not recurse into the helper body).
func nestedWriteCommand(t *testing.T, storeDir, sessionName string, patch nestedWritePatch) string {
	t.Helper()
	patchPath := writeNestedWritePatch(t, t.TempDir(), patch)
	return fmt.Sprintf("%q -test.run=TestServiceNestedWriteHelperProcess -- %q %q %q",
		os.Args[0], storeDir, sessionName, patchPath)
}

// TestServiceDeleteSessionHelperProcess is TestServiceNestedWriteHelperProcess's
// counterpart for simulating a concurrent Delete (e.g. a racing `plect
// destroy`) rather than a nested write — used by a test proving a later
// persist attempt against a since-deleted session fails and that failure is
// not silently discarded.
func TestServiceDeleteSessionHelperProcess(t *testing.T) {
	if os.Getenv("PLECT_SERVICE_DELETE_SESSION_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: -- <dir> <session-name>")
		os.Exit(2)
	}
	dir, sessionName := args[1], args[2]
	if err := state.NewStore(dir).Delete(sessionName); err != nil {
		fmt.Fprintln(os.Stderr, "delete:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// deleteSessionCommand returns the shell command a task hook runs to invoke
// TestServiceDeleteSessionHelperProcess against storeDir/sessionName; the
// caller sets PLECT_SERVICE_DELETE_SESSION_HELPER=1 via t.Setenv.
func deleteSessionCommand(storeDir, sessionName string) string {
	return fmt.Sprintf("%q -test.run=TestServiceDeleteSessionHelperProcess -- %q %q",
		os.Args[0], storeDir, sessionName)
}

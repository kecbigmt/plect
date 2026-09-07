package service

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// writePostMigrationState seeds a store with sessions in the shape the
// one-time identity migration leaves behind: canonical resource id and
// create-time alias, with no legacy provider-shaped identity keys at all.
// It builds these through the store's own Put, the same as any real
// session write — the fixture predates the SQLite storage cutover as a
// literal legacy state.json blob, but its subject (a post-migration
// session's identity shape) has nothing to do with the storage format
// underneath, so it survives the cutover as ordinary seeded sessions.
func writePostMigrationState(t *testing.T) *state.Store {
	t.Helper()
	store := state.NewStore(t.TempDir())
	seedPostMigrationSession(t, store, "acme/widgets-1+claude", "https://example.test/acme/widgets/items/1",
		"item/1+claude", "coding", "/tmp/workdirs/acme-widgets-1-claude")
	seedPostMigrationSession(t, store, "standalone", "standalone", "", "", "/tmp/workdirs/standalone")
	return store
}

func seedPostMigrationSession(t *testing.T, store *state.Store, name, resourceID, branch, workflow, workspaceDirPath string) {
	t.Helper()
	createdAt := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	session := &domain.Session{
		Name:             name,
		ResourceID:       resourceID,
		Alias:            resourceID,
		Workflow:         workflow,
		WorkspaceDirPath: workspaceDirPath,
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	}
	if workflow != "" {
		session.Tasks = map[string]*contract.TaskState{
			contract.WorkflowPseudoNodeID: {
				Scope:   contract.TaskScopeSession,
				Status:  contract.TaskStatusProduced,
				Outputs: map[string]any{contract.OutputKeyWorkspaceDir: workspaceDirPath},
			},
		}
	}
	if branch != "" {
		if session.Tasks == nil {
			session.Tasks = map[string]*contract.TaskState{}
		}
		wf := session.Tasks[contract.WorkflowPseudoNodeID]
		if wf == nil {
			wf = &contract.TaskState{Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced}
			session.Tasks[contract.WorkflowPseudoNodeID] = wf
		}
		if wf.Outputs == nil {
			wf.Outputs = map[string]any{}
		}
		wf.Outputs["branch"] = branch
	}
	if err := store.Put(session); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
}

// TestPostMigrationState_LoadsIdentityFields pins that a session with no
// legacy identity keys loads with its canonical identity intact.
func TestPostMigrationState_LoadsIdentityFields(t *testing.T) {
	store := writePostMigrationState(t)

	s := store.Get("acme/widgets-1+claude")
	if s == nil {
		t.Fatal("session not loaded from post-migration state")
	}
	if s.ResourceID != "https://example.test/acme/widgets/items/1" {
		t.Errorf("ResourceID = %q", s.ResourceID)
	}
	if s.Alias != "https://example.test/acme/widgets/items/1" {
		t.Errorf("Alias = %q", s.Alias)
	}
	if domain.SessionBranch(s) != "item/1+claude" {
		t.Errorf("Branch = %q", domain.SessionBranch(s))
	}
	if s.WorkspaceDirPath != "/tmp/workdirs/acme-widgets-1-claude" {
		t.Errorf("WorkspaceDirPath = %q", s.WorkspaceDirPath)
	}
}

// TestPostMigrationState_ResolveSession pins every lookup order a
// post-migration session must still support: by name, by alias, and by
// canonical resource id.
func TestPostMigrationState_ResolveSession(t *testing.T) {
	store := writePostMigrationState(t)

	tests := []struct {
		name       string
		identifier string
		want       string
	}{
		{"by session name", "acme/widgets-1+claude", "acme/widgets-1+claude"},
		{"by alias", "https://example.test/acme/widgets/items/1", "acme/widgets-1+claude"},
		{"identity session by name", "standalone", "standalone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, sess, err := ResolveSession(nil, store, tt.identifier)
			if err != nil {
				t.Fatalf("ResolveSession(%q): %v", tt.identifier, err)
			}
			if name != tt.want || sess == nil || sess.Name != tt.want {
				t.Errorf("resolved to %q, want %q", name, tt.want)
			}
		})
	}
}

// TestPostMigrationState_UnknownIdentifier pins the failure path: an
// identifier with no state entry and no resolver must report not-found
// rather than inventing a session.
func TestPostMigrationState_UnknownIdentifier(t *testing.T) {
	store := writePostMigrationState(t)

	if _, _, err := ResolveSession(nil, store, "no-such-session"); err == nil {
		t.Fatal("expected an error for an identifier with no state entry")
	} else if svcErr, ok := err.(*Error); !ok || svcErr.Code != ErrSessionNotFound {
		t.Errorf("error = %v, want ErrSessionNotFound", err)
	}
}

// TestPostMigrationState_WorkspaceDirFromSession pins that the working directory
// lookup reads the session's recorded workspace directory, with no
// resource-shape
// derivation involved.
func TestPostMigrationState_WorkspaceDirFromSession(t *testing.T) {
	store := writePostMigrationState(t)

	got, err := WorkspaceDir(nil, store, "acme/widgets-1+claude")
	if err != nil {
		t.Fatalf("WorkspaceDir: %v", err)
	}
	if got != "/tmp/workdirs/acme-widgets-1-claude" {
		t.Errorf("WorkspaceDir = %q", got)
	}
}

// TestPostMigrationState_ResolverDispatchOverPostMigrationState is the
// end-to-end check that the current code operates correctly against a
// session in post-migration shape: a session recorded there is found by the
// same resolver dispatch a fresh create would use, so a migrated session is
// reused rather than duplicated.
func TestPostMigrationState_ResolverDispatchOverPostMigrationState(t *testing.T) {
	store := state.NewStore(t.TempDir())
	seedPostMigrationSession(t, store, "acme/widgets-42+gh", "https://github.com/acme/widgets/issues/42",
		"issue/42+gh", "gh", "/tmp/workdirs/issue-42-gh")

	cfg := writeWorkflowFixture(t, t.TempDir(), "gh",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "gh", providerEchoingOutputs("gh", `{"workspace_dir":"/tmp/x"}`))

	disp, matched, err := dispatchResource(cfg, "", "https://github.com/acme/widgets/issues/42")
	if err != nil || !matched {
		t.Fatalf("dispatch: matched=%v err=%v", matched, err)
	}
	name := disp.Name + "+gh"
	if store.Get(name) == nil {
		t.Fatalf("resolver-derived name %q does not address the migrated session", name)
	}

	// Every identity lookup a command performs must land on the same session.
	for _, identifier := range []string{name, "https://github.com/acme/widgets/issues/42"} {
		got, _, err := ResolveSession(cfg, store, identifier)
		if err != nil {
			t.Fatalf("ResolveSession(%q): %v", identifier, err)
		}
		if got != name {
			t.Errorf("ResolveSession(%q) = %q, want %q", identifier, got, name)
		}
	}
}

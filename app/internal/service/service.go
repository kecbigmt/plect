package service

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

var validTag = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validateTagFormat returns ErrInvalidTag for non-empty tags that don't match
// the allowed character set. Empty tags are caller-validated (the "no tag"
// case is legal at the API surface).
func validateTagFormat(tag string) *Error {
	if !validTag.MatchString(tag) {
		return &Error{Code: ErrInvalidTag, Message: fmt.Sprintf("invalid tag %q: must match [a-zA-Z0-9_-]+", tag)}
	}
	return nil
}

// effectiveTag resolves the session-identity tag that becomes part of a
// session name. An explicit --tag wins; otherwise the workflow id is the
// default, so two workflows acting on one resource (a work workflow, a
// review workflow) materialize distinct sessions instead of racing for one
// branch/workspace.
// The tag is never empty on the workspace-provider-dispatch paths — session
// identity always carries a label.
func effectiveTag(tag, workflowID string) (string, *Error) {
	if tag != "" {
		if err := validateTagFormat(tag); err != nil {
			return "", err
		}
		return tag, nil
	}
	if err := validateTagFormat(workflowID); err != nil {
		return "", &Error{Code: ErrInvalidTag, Message: fmt.Sprintf("workflow id %q cannot seed a session tag (must match [a-zA-Z0-9_-]+); pass --tag explicitly", workflowID)}
	}
	return workflowID, nil
}

// resolveSession resolves an identifier (session name, create-time alias, or
// resource id) to a session from the store. Lookup order:
//
//  1. exact session name
//  2. create-time alias (survives resolver rule changes; ambiguous when tag
//     variants share the alias)
//  3. resolver derivation (pure, offline — works during workspace provider
//     outages)
func resolveSession(cfg *config.Config, store *state.Store, identifier string) (string, *domain.Session, error) {
	if session, err := store.GetE(identifier); err != nil {
		return "", nil, err
	} else if session != nil {
		return identifier, session, nil
	}

	if hits, err := store.FindByAliasE(identifier); err != nil {
		return "", nil, err
	} else if len(hits) == 1 {
		return hits[0].Name, hits[0], nil
	} else if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.Name
		}
		slices.Sort(names)
		return "", nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("identifier %q matches multiple sessions (%s); use the session name", identifier, strings.Join(names, ", "))}
	}

	sessionName := identifier
	if cfg != nil {
		if disp, matched, err := dispatchResource(cfg, "", identifier); err == nil && matched {
			if session, err := store.GetE(disp.Name); err != nil {
				return "", nil, err
			} else if session != nil {
				return disp.Name, session, nil
			}
			sessionName = disp.Name
		}
	}

	return "", nil, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("no state entry for session %q", sessionName)}
}

// ResolveSession is the exported entry point for resolving an identifier
// (session name, create-time alias, or resource id) to a session, using the
// same lookup order as the internal resolver. It hands back the raw
// mutating-lifecycle-owned session, so a caller that mutates the session
// (e.g. to update its message) can call this directly, but a
// caller that only needs the canonical name should call ResolveSessionName
// instead, and one that needs read-only session fields should call a
// dedicated projection function instead of reading the raw struct.
func ResolveSession(cfg *config.Config, store *state.Store, identifier string) (string, *domain.Session, error) {
	return resolveSession(cfg, store, identifier)
}

// ResolveSessionName resolves an identifier to its canonical session name,
// using the same lookup order as ResolveSession, without handing back the
// raw session for callers that only need the name (e.g. `plect ls --parent`
// filtering entries by ParentSession).
func ResolveSessionName(cfg *config.Config, store *state.Store, identifier string) (string, error) {
	name, _, err := resolveSession(cfg, store, identifier)
	return name, err
}

// TaskInstanceView is the display projection of one task instance: its
// identity, scope, and status, plus — when the task declares a done_when —
// the per-instance evaluation against the instance's own outputs.
// Only dynamic instances and done_when-bearing tasks are projected; pure
// lifecycle-only static nodes (the runtime task, the agent launcher) are
// omitted to keep show/ls focused.
type TaskInstanceView struct {
	Instance          string                  `json:"instance"`
	TaskID            string                  `json:"task_id,omitempty"`
	Scope             string                  `json:"scope"`
	Status            string                  `json:"status"`
	Dynamic           bool                    `json:"dynamic,omitempty"`
	Name              string                  `json:"name,omitempty"`
	Resource          string                  `json:"resource,omitempty"`
	DoneWhen          *task.DoneWhenResult    `json:"done_when,omitempty"`
	Finalized         bool                    `json:"finalized,omitempty"` // set once `plect task finalize` has recorded completion; cleanup still pending
	Outputs           map[string]any          `json:"outputs,omitempty"`
	PersistedDoneWhen *contract.DoneWhenState `json:"persisted_done_when,omitempty"`
}

// sessionTaskItem is the shared per-instance projection both taskViews
// (ls/List's legacy `tasks` display) and statusTaskViews (plect status's `work`
// layer) build from — instance identity, the dynamic-or-done_when filter, and
// the done_when evaluation itself live in exactly one place so the two
// display surfaces cannot silently drift apart.
type sessionTaskItem struct {
	seq       int
	instance  string
	taskID    string
	scope     string
	status    string
	dynamic   bool
	name      string
	resource  string
	outputs   map[string]any
	state     map[string]any
	observed  *contract.ResourceObservation
	doneWhen  *task.DoneWhenResult
	finalized bool
}

// sessionTaskItems projects a session's task instances, ordered by
// instantiation Seq so display matches the instantiation stack. defs supplies
// the done_when predicates (loaded from the trusted config layers); a nil defs
// degrades to dynamic-instance identity only. cached supplies a done_when
// result already evaluated elsewhere for the same (def, outputs, context) —
// e.g. evaluateSessionActions, which Status calls first — so this projection
// doesn't redundantly re-evaluate it; a cache miss (not produced, or no
// done_when-bearing evaluation ran) still evaluates it directly.
func sessionTaskItems(cfg *config.Config, declarations taskDeclarations, session *domain.Session, sessions map[string]*domain.Session, cached map[string]task.DoneWhenResult) []sessionTaskItem {
	if session == nil || len(session.Tasks) == 0 {
		return nil
	}
	var items []sessionTaskItem
	for key, st := range session.Tasks {
		if st == nil {
			continue
		}
		if key == contract.WorkflowPseudoNodeID {
			// The workflow pseudo-node carries session-level outputs (title,
			// branch, ...) but no done_when or lifecycle status of its own —
			// still one Task line + outputs, per the unified display rule.
			if len(st.Outputs) > 0 {
				items = append(items, sessionTaskItem{seq: st.Seq, instance: key, outputs: st.Outputs})
			}
			continue
		}
		taskID := taskIDForInstance(key, st)
		var dwResult *task.DoneWhenResult
		if r, ok := cached[key]; ok {
			rc := r
			dwResult = &rc
		} else if declarations.declares(taskID) {
			dw, live, err := declarations.gate(key, st)
			if err == nil && dw != nil {
				res := task.EvaluateTaskDoneWhenWithContext(dw, live, doneWhenEvalContext(session.Name, st, sessions))
				dwResult = &res
			}
		}
		if st.Status != contract.TaskStatusProduced && !st.Dynamic && dwResult == nil {
			continue // not produced, not dynamically named, no done_when to report — nothing to show yet
		}
		items = append(items, sessionTaskItem{
			seq: st.Seq, instance: key, taskID: st.TaskID, scope: st.Scope, status: st.Status,
			dynamic: st.Dynamic, name: st.Name, resource: st.Resource, outputs: st.Outputs,
			state: st.State, observed: st.Observed,
			doneWhen: dwResult, finalized: !st.FinalizedAt.IsZero(),
		})
	}
	slices.SortStableFunc(items, func(a, b sessionTaskItem) int {
		if a.seq != b.seq {
			return a.seq - b.seq
		}
		return strings.Compare(a.instance, b.instance)
	})
	return items
}

// taskViews projects a session's task instances for display. See
// sessionTaskItems for the shared projection logic.
func taskViews(cfg *config.Config, declarations taskDeclarations, session *domain.Session, sessions map[string]*domain.Session) []TaskInstanceView {
	items := sessionTaskItems(cfg, declarations, session, sessions, nil)
	if items == nil {
		return nil
	}
	out := make([]TaskInstanceView, len(items))
	for i, it := range items {
		out[i] = TaskInstanceView{
			Instance:  it.instance,
			TaskID:    it.taskID,
			Scope:     it.scope,
			Status:    it.status,
			Dynamic:   it.dynamic,
			Name:      it.name,
			Resource:  it.resource,
			DoneWhen:  it.doneWhen,
			Finalized: it.finalized,
		}
	}
	return out
}

// loadDisplayTasks loads the trusted-layer task declarations once for
// done_when display across sessions. Declarations are trusted-layer-only (the
// workdir layer cannot contribute shell), so the workdir-independent load is
// sufficient — mirroring loadDisplayWorkflows. A load failure leaves display
// without predicates rather than failing a listing: nothing here decides
// anything.
func loadDisplayTasks(cfg *config.Config) taskDeclarations {
	if cfg == nil {
		return taskDeclarations{}
	}
	docs, effects, err := cfg.LoadTaskDeclarations("")
	if err != nil {
		return taskDeclarations{}
	}
	return taskDeclarations{docs: docs, effects: effects}
}

// lookupTombstone resolves identifier to a session name the same way
// resolveSession does (but without requiring a live state entry — the state
// entry is exactly what's gone by the time a tombstone matters) and reads
// that session's tombstone from the event log, if any. A nil, nil result
// means no tombstone exists.
func lookupTombstone(cfg *config.Config, store *state.Store, identifier string) (*contract.Tombstone, error) {
	sessionName, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return nil, err
	}
	data, ok, err := eventlog.NewStore(store.Dir()).ReadTombstone(sessionName)
	if err != nil || !ok {
		return nil, err
	}
	var tomb contract.Tombstone
	if err := json.Unmarshal(data, &tomb); err != nil {
		return nil, fmt.Errorf("tombstone: unmarshal: %w", err)
	}
	return &tomb, nil
}

// childNames lists the sessions whose ParentSession is name, sorted. Derived
// from the parent pointers (the tree fact Subtree walks), not the Children
// slice, so the projection cannot drift from subtree membership.
func childNames(sessions map[string]*domain.Session, name string) []string {
	var out []string
	for n, s := range sessions {
		if s != nil && s.ParentSession == name {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// ListEntry represents a single entry in the session list.
type ListEntry struct {
	SessionName      string             `json:"session_name"`
	Title            string             `json:"title,omitempty"`
	Run              domain.RunState    `json:"run"`
	Health           domain.HealthState `json:"health,omitempty"`
	DisplayStatus    string             `json:"display_status"`
	ResourceID       string             `json:"resource_id"`
	Tracked          bool               `json:"tracked"`
	LastActiveAt     *time.Time         `json:"last_active_at,omitempty"`
	Message          *domain.Message    `json:"message,omitempty"`
	Branch           string             `json:"branch,omitempty"`
	WorkspaceDirPath string             `json:"workspace_dir_path,omitempty"`
	ParentSession    string             `json:"parent_session,omitempty"`
	// Tasks projects the session's dynamic instances and done_when-bearing
	// tasks with their per-instance done_when status.
	Tasks []TaskInstanceView `json:"tasks,omitempty"`
}

// List returns all sessions with their statuses.
func List(cfg *config.Config, store *state.Store) ([]ListEntry, error) {
	sessions, err := store.AllE()
	if err != nil {
		return nil, err
	}
	displayWorkflows := loadDisplayWorkflows(cfg)
	displayTasks := loadDisplayTasks(cfg)

	// Each session may need a health probe subprocess. Doing 50+
	// serially is the dominant cost, so fan out over a bounded pool and fill a
	// preallocated slice by index; the sort below restores order.
	tracked := make([]*domain.Session, 0, len(sessions))
	for _, s := range sessions {
		tracked = append(tracked, s)
	}
	entries := make([]ListEntry, len(tracked))
	sem := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	for i := range tracked {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			entries[i] = buildListEntry(cfg, store, displayWorkflows, displayTasks, tracked[i], sessions)
		}(i)
	}
	wg.Wait()

	// store.All ranges a map, so sort by name to make List deterministic —
	// callers (plect ls, MCP, web UI auto-refresh) get a stable order.
	slices.SortFunc(entries, func(a, b ListEntry) int {
		return strings.Compare(a.SessionName, b.SessionName)
	})

	return entries, nil
}

// listConcurrency bounds the per-session health-probe fan-out in List.
const listConcurrency = 16

// buildListEntry gathers one session's runtime status. Safe to call
// concurrently: it only reads the shared workflows and spawns its own
// subprocesses.
func buildListEntry(cfg *config.Config, store *state.Store, displayWorkflows map[string]config.WorkflowFile, displayTasks taskDeclarations, s *domain.Session, sessions map[string]*domain.Session) ListEntry {
	var cached cachedInfo
	applyDisplay(displayWorkflows, s, &cached)

	entry := ListEntry{
		SessionName:      s.Name,
		Title:            cached.Title,
		Run:              sessionRunState(cfg, s),
		Health:           sessionHealthState(cfg, store, s.Name),
		DisplayStatus:    cached.DisplayStatus,
		ResourceID:       s.ResourceID,
		Tracked:          true,
		LastActiveAt:     &s.UpdatedAt,
		Message:          s.Message,
		Branch:           s.Branch,
		WorkspaceDirPath: s.WorkspaceDirPath,
		ParentSession:    s.ParentSession,
		Tasks:            taskViews(cfg, displayTasks, s, sessions),
	}

	return entry
}

// sessionRunState reports the "run" fact: whether a current-plan run-scoped
// task instance has produced.
func sessionRunState(cfg *config.Config, s *domain.Session) domain.RunState {
	if s != nil && cfg.RunScopeUp(s) {
		return domain.RunUp
	}
	return domain.RunDown
}

// sessionHealthState reports the "health" fact: the declared-alive-probe
// evaluation, independent of run state. An evaluation error surfaces as
// unhealthy — the same treatment the old three-value collapse gave it.
func sessionHealthState(cfg *config.Config, store *state.Store, name string) domain.HealthState {
	_, state := sessionHealthReport(cfg, store, name)
	return state
}

func sessionHealthReport(cfg *config.Config, store *state.Store, name string) (HealthReport, domain.HealthState) {
	report, err := EvaluateHealth(cfg, store, name)
	if err != nil {
		return HealthReport{}, domain.HealthUnhealthy
	}
	return report, report.State()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// taskIDForInstance resolves a task instance's definition ID. Empty TaskID
// preserves the older static-node state shape.
func taskIDForInstance(key string, st *contract.TaskState) string {
	if st.TaskID != "" {
		return st.TaskID
	}
	return key
}

// cachedInfo holds a session's display projection (title, status line).
type cachedInfo struct {
	Title         string
	DisplayStatus string
}

// WorkspaceDir resolves an identifier to the session's workspace directory
// using the full lookup order (name -> alias -> resolver derivation). The
// workspace directory is whatever the workspace provider's setup recorded;
// plect never recomputes it from the shape of the identifier.
func WorkspaceDir(cfg *config.Config, store *state.Store, identifier string) (string, error) {
	if _, session, err := resolveSession(cfg, store, identifier); err == nil {
		if session.WorkspaceDirPath == "" {
			return "", &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q has no workspace directory recorded", session.Name)}
		}
		return session.WorkspaceDirPath, nil
	} else if svcErr, ok := err.(*Error); ok && svcErr.Code != ErrSessionNotFound {
		// An ambiguous alias must surface rather than fall through.
		return "", err
	}

	return "", &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("no state entry for session %q", identifier)}
}

// loadDisplayWorkflows loads the trusted base layers once for [display]
// lookup across sessions. Display is a trusted-layer-only field, so the
// per-session ancestor overlays can't change it; one load serves the whole
// listing.
func loadDisplayWorkflows(cfg *config.Config) map[string]config.WorkflowFile {
	if cfg == nil {
		return nil
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		return nil
	}
	return workflows
}

// applyDisplay resolves the workflow's [display] values into cached when they
// produce a non-empty result. Evaluation is state-only (the workflow's own
// persisted outputs and the session's inputs) — no network. A value that does
// not resolve leaves the field at what it already shows: a listing reporting
// a blank title would read as a session that has none.
func applyDisplay(workflows map[string]config.WorkflowFile, s *domain.Session, cached *cachedInfo) {
	if s.Workflow == "" {
		return
	}
	wf, ok := workflows[s.Workflow]
	if !ok || len(wf.Display) == 0 {
		return
	}
	outputs := workflowDisplayOutputs(s)
	for _, shown := range []struct {
		key  string
		into *string
	}{
		{"title", &cached.Title},
		{"status", &cached.DisplayStatus},
	} {
		value, declared := wf.Display[shown.key]
		if !declared {
			continue
		}
		if resolved, err := task.ResolveDisplay(value, outputs, s.Inputs); err == nil && resolved != "" {
			*shown.into = resolved
		}
	}
}

func workflowDisplayOutputs(s *domain.Session) map[string]any {
	out := map[string]any{}
	if ws, ok := s.Tasks[contract.WorkflowPseudoNodeID]; ok && ws != nil {
		maps.Copy(out, ws.Outputs)
	}
	return out
}

// SetMessage updates the session-level self-reported status message. An empty
// text unsets it (Session.Message becomes nil) rather than persisting a
// blank, since a blank line would look identical to "message never set" in
// display but consume a stored object.
func SetMessage(cfg *config.Config, store *state.Store, identifier string, text string) error {
	sessionName, session, err := resolveSession(cfg, store, identifier)
	if err != nil {
		return err
	}
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return guardErr
	}
	previous := ""
	if session.Message != nil {
		previous = session.Message.Text
	}
	reported := session.Message != nil
	if !reported {
		events, tailErr := eventlog.NewStore(store.Dir()).Tail(sessionName, event.Filter{Types: []string{event.TypeStatusMessage}}, 1)
		if tailErr != nil {
			return &Error{Code: ErrExecutionFailed, Message: tailErr.Error()}
		}
		reported = len(events) > 0
	}
	changed := previous != text || !reported
	now := time.Now()
	if text == "" {
		session.Message = nil
	} else {
		session.Message = &domain.Message{Text: text, UpdatedAt: now}
	}
	session.UpdatedAt = now
	if err := store.Put(session); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	cleared := "false"
	if text == "" {
		cleared = "true"
	}
	_, _, _, err = eventlog.NewStore(store.Dir()).Append(event.Event{
		SessionName: sessionName,
		Type:        event.TypeStatusMessage,
		Source:      event.SourcePlect,
		Direction:   event.Outbound,
		Summary:     text,
		Metadata: map[string]string{
			"text":     text,
			"cleared":  cleared,
			"previous": previous,
		},
	})
	if err != nil {
		return &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	return nil
}

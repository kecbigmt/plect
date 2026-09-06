// Package migrate implements the one-time rewrite of on-disk plect state
// and config data from legacy, GitHub-shaped forms to their current forms,
// ahead of the removal of the legacy compatibility code that reads those
// old forms.
//
// This is a standalone operator tool, not runtime dispatch code: it runs
// once, out of band, against a data directory. It is deliberately narrow —
// it only knows the specific old-form shapes named by the refactor plans
// that produced it (see docs/migrations/) — rather than a generic pluggable
// migration framework, because no second use case exists yet. Its
// integration-specific field mapping (GitHub identity fields, GitHub-shaped
// repo allowlist entries) lives here, outside plect's core, because that
// knowledge is retired together with this tool once the migration has run.
package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

const preGuardStateVersion = 5

// workdirEraStateVersion is the schema version the prior (now-superseded)
// worktree→workdir vocabulary migration produced. This tool accepts input at
// either preGuardStateVersion or workdirEraStateVersion so an operator who
// already ran the old migration does not have to chain two rewrites — one
// pass carries either starting point straight to the current workspace
// vocabulary.
const workdirEraStateVersion = 6

// targetStateVersion is the last legacy state.json envelope version this
// tool's rewrites ever produced (7). It is a frozen historical fact this
// tool owns, not a live authority: contracts/state no longer defines a
// schema version, since state.json is no longer the runtime persistence
// format.
const targetStateVersion = 7

// supportedOldStateVersions are every schema version this tool knows how to
// bring forward to targetStateVersion. Deliberately an explicit set, not
// a "less than current" range: each entry names a real historical release
// shape this tool's field-rewrite functions were written against, not an
// assumption that every older version happens to need the same rewrites.
var supportedOldStateVersions = map[int]bool{
	preGuardStateVersion:   true,
	workdirEraStateVersion: true,
}

// Options configures a migration run.
type Options struct {
	// StatePath is the path to state.json. A missing file is treated as
	// already in the new form (nothing to migrate).
	StatePath string
	// ConfigPath is the path to config.toml. A missing file is treated as
	// already in the new form.
	ConfigPath string
	// BackupDir is the directory under which a timestamped backup
	// subdirectory is created before any rewrite. Defaults to a
	// "migration-backups" directory next to StatePath.
	BackupDir string
	// Now returns the timestamp used to name the backup subdirectory.
	// Defaults to time.Now. Tests inject a deterministic or
	// monotonically-advancing clock so consecutive runs land in distinct
	// backup directories.
	Now func() time.Time
}

// Report summarizes what a migration run did.
type Report struct {
	// Changed is true if either file was rewritten.
	Changed bool
	// BackupDir is the timestamped directory holding pre-rewrite copies of
	// every file that existed on disk. Empty when Changed is false, since a
	// no-op run leaves nothing to back up.
	BackupDir string
	// StateChanged and ConfigChanged report which of the two files were
	// rewritten, so callers/logs can be specific about what happened.
	StateChanged  bool
	ConfigChanged bool
	// Notes describes each rewrite performed, in the order applied. Empty
	// when Changed is false.
	Notes []string
}

// Run performs one migration pass: it computes the new form of state.json
// and config.toml in memory, and only if either differs from the on-disk
// form does it back up the originals and write the new forms. Running Run
// again against its own output is a no-op (Report.Changed is false) because
// the new form has nothing left to rewrite.
func Run(opts Options) (Report, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	var report Report

	stateOld, stateHadFile, err := readFileIfExists(opts.StatePath)
	if err != nil {
		return Report{}, fmt.Errorf("migrate: read state file: %w", err)
	}
	stateNew, stateNotes, err := migrateStateJSON(stateOld)
	if err != nil {
		return Report{}, fmt.Errorf("migrate: rewrite state file: %w", err)
	}
	stateChanged := stateHadFile && len(stateNotes) > 0

	configOld, configHadFile, err := readFileIfExists(opts.ConfigPath)
	if err != nil {
		return Report{}, fmt.Errorf("migrate: read config file: %w", err)
	}
	configNew, configNotes, err := migrateConfigTOML(configOld)
	if err != nil {
		return Report{}, fmt.Errorf("migrate: rewrite config file: %w", err)
	}
	configChanged := configHadFile && len(configNotes) > 0

	if !stateChanged && !configChanged {
		return Report{Changed: false}, nil
	}

	backupDir := opts.BackupDir
	if backupDir == "" {
		backupDir = filepath.Join(filepath.Dir(opts.StatePath), "migration-backups")
	}
	runDir := filepath.Join(backupDir, now().UTC().Format("20060102T150405.000000000"))
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return Report{}, fmt.Errorf("migrate: create backup dir: %w", err)
	}

	if stateHadFile {
		if err := backupFile(runDir, filepath.Base(opts.StatePath), stateOld); err != nil {
			return Report{}, fmt.Errorf("migrate: back up state file: %w", err)
		}
	}
	if configHadFile {
		if err := backupFile(runDir, filepath.Base(opts.ConfigPath), configOld); err != nil {
			return Report{}, fmt.Errorf("migrate: back up config file: %w", err)
		}
	}

	if stateChanged {
		if err := writeFile(opts.StatePath, stateNew); err != nil {
			return Report{}, fmt.Errorf("migrate: write state file: %w", err)
		}
	}
	if configChanged {
		if err := writeFile(opts.ConfigPath, configNew); err != nil {
			return Report{}, fmt.Errorf("migrate: write config file: %w", err)
		}
	}

	report.Changed = true
	report.BackupDir = runDir
	report.StateChanged = stateChanged
	report.ConfigChanged = configChanged
	report.Notes = append(append([]string{}, stateNotes...), configNotes...)
	return report, nil
}

func readFileIfExists(path string) ([]byte, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func backupFile(dir, name string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, name), data, 0644)
}

// migrateStateJSON rewrites state.json's legacy forms into their current
// equivalents:
//
//   - a session-level legacy "slack" field becomes "conversation"
//   - a session-level legacy "effects" map becomes entries in "tasks"
//   - legacy GitHub identity fields (url/url_type/owner_repo/number) are
//     folded into resource_id/alias and then dropped
//   - for a session with no "workflow" (the retired inline `[[tasks]]`
//     path, which had no separate task-id concept), each of its task
//     entries gets an explicit task_id equal to its map key, so it no
//     longer depends on the "empty task_id means node id == task id"
//     round-tripping convention once that convention's supporting code is
//     removed
//
// It returns the rewritten bytes and a human-readable note per change
// category actually applied; an empty notes slice means the input was
// already in the new form (or absent), so the caller treats it as a no-op.
func migrateStateJSON(data []byte) ([]byte, []string, error) {
	if len(data) == 0 {
		return nil, nil, nil
	}

	var before map[string]any
	if err := json.Unmarshal(data, &before); err != nil {
		return nil, nil, fmt.Errorf("parse state.json: %w", err)
	}

	after := deepCopyJSON(before).(map[string]any)
	var notes []string

	sessions, _ := after["sessions"].(map[string]any)
	for name, raw := range sessions {
		session, ok := raw.(map[string]any)
		if !ok || session == nil {
			continue
		}

		if migrateSlackField(session) {
			notes = append(notes, fmt.Sprintf("session %q: migrated legacy slack field to conversation", name))
		}
		if migrateEffectsField(session) {
			notes = append(notes, fmt.Sprintf("session %q: migrated legacy effects map into tasks", name))
		}
		if migrateLegacyIdentity(session) {
			notes = append(notes, fmt.Sprintf("session %q: folded legacy url/owner_repo/number identity fields into resource_id/alias", name))
		}
		if migrateWorkspaceDirPath(session) {
			notes = append(notes, fmt.Sprintf("session %q: renamed worktree_path/workdir_path to workspace_dir_path", name))
		}
		if migrateWorkflowOutputsWorkdirKey(session) {
			notes = append(notes, fmt.Sprintf("session %q: renamed @workflow output key workdir to workspace_dir", name))
		}
		if backfillInlineTaskIDs(session) {
			notes = append(notes, fmt.Sprintf("session %q: backfilled task_id for legacy inline tasks", name))
		}
	}

	if note, err := stampStateVersion(after); err != nil {
		return nil, nil, err
	} else if note != "" {
		notes = append(notes, note)
	}

	if len(notes) == 0 {
		return nil, nil, nil
	}

	// Every migration function above must report a note if and only if it
	// actually changed the map; this guards against one of them drifting
	// out of sync (e.g. reporting a change that didn't happen), which would
	// otherwise silently break the "no-op on already-migrated data"
	// acceptance criterion.
	if reflect.DeepEqual(before, after) {
		return nil, nil, fmt.Errorf("state.json: notes reported but no change was made")
	}

	out, err := json.MarshalIndent(after, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal migrated state.json: %w", err)
	}
	out = append(out, '\n')
	return out, notes, nil
}

func stampStateVersion(state map[string]any) (string, error) {
	raw, ok := state["version"]
	if !ok {
		state["version"] = float64(targetStateVersion)
		return fmt.Sprintf("state.json: stamped schema version %d", targetStateVersion), nil
	}
	version, ok := raw.(float64)
	if !ok || version != float64(int(version)) {
		return "", fmt.Errorf("state.json: invalid schema version %v", raw)
	}
	got := int(version)
	if got == targetStateVersion {
		return "", nil
	}
	if got > targetStateVersion {
		return "", fmt.Errorf("state.json: schema version %d is newer than this migration tool understands (%d)", got, targetStateVersion)
	}
	if !supportedOldStateVersions[got] {
		return "", fmt.Errorf("state.json: schema version %d is not supported by this migration tool", got)
	}
	state["version"] = float64(targetStateVersion)
	return fmt.Sprintf("state.json: bumped schema version from %d to %d", got, targetStateVersion), nil
}

func migrateSlackField(session map[string]any) bool {
	slackRaw, ok := session["slack"]
	if !ok || slackRaw == nil {
		return false
	}
	delete(session, "slack")
	if _, hasConversation := session["conversation"]; hasConversation {
		return true
	}
	slack, ok := slackRaw.(map[string]any)
	if !ok {
		return true
	}
	metadata := map[string]any{}
	if v, ok := slack["thread_ts"]; ok {
		metadata["thread_ts"] = v
	}
	if v, ok := slack["channel_id"]; ok {
		metadata["channel_id"] = v
	}
	session["conversation"] = map[string]any{
		"source":   "Slack",
		"metadata": metadata,
	}
	return true
}

func migrateEffectsField(session map[string]any) bool {
	effectsRaw, ok := session["effects"]
	if !ok || effectsRaw == nil {
		return false
	}
	delete(session, "effects")
	effects, ok := effectsRaw.(map[string]any)
	if !ok || len(effects) == 0 {
		return true
	}

	tasks, _ := session["tasks"].(map[string]any)
	if tasks == nil {
		tasks = map[string]any{}
	}
	for key, raw := range effects {
		if _, exists := tasks[key]; exists {
			continue
		}
		task, ok := raw.(map[string]any)
		if !ok || task == nil {
			continue
		}
		if effectID, ok := task["effect_id"]; ok {
			if _, hasTaskID := task["task_id"]; !hasTaskID {
				task["task_id"] = effectID
			}
			delete(task, "effect_id")
		}
		tasks[key] = task
	}
	session["tasks"] = tasks
	return true
}

// legacyIdentityKeys are the GitHub-shaped fields the pre-v3 identity model
// always serialized. Their presence (regardless of value) marks a session as
// old-form, since the current form never writes them.
var legacyIdentityKeys = []string{"url", "url_type", "owner_repo", "number"}

func migrateLegacyIdentity(session map[string]any) bool {
	hasLegacy := false
	for _, key := range legacyIdentityKeys {
		if _, ok := session[key]; ok {
			hasLegacy = true
			break
		}
	}
	if !hasLegacy {
		return false
	}

	if url, _ := session["url"].(string); url != "" {
		if rid, _ := session["resource_id"].(string); rid == "" {
			session["resource_id"] = url
		}
	}
	if rid, _ := session["resource_id"].(string); rid != "" {
		if alias, _ := session["alias"].(string); alias == "" {
			session["alias"] = rid
		}
	}
	for _, key := range legacyIdentityKeys {
		delete(session, key)
	}
	return true
}

// migrateWorkspaceDirPath folds either legacy path field — the oldest
// "worktree_path" or the workdir-era "workdir_path" a prior migration run
// may have left — into the current "workspace_dir_path", in one pass. Both
// legacy keys are checked (not just whichever the input schema version
// implies) because a session can carry a stale worktree_path alongside an
// empty workdir_path when an earlier migration ran against data missing its
// value — see the fallback below.
func migrateWorkspaceDirPath(session map[string]any) bool {
	_, hadWorktreePath := session["worktree_path"]
	_, hadWorkdirPath := session["workdir_path"]
	if !hadWorktreePath && !hadWorkdirPath {
		return false
	}

	value := ""
	if v, ok := session["workdir_path"].(string); ok && v != "" {
		value = v
	}
	if value == "" {
		if v, ok := session["worktree_path"].(string); ok {
			value = v
		}
	}
	delete(session, "worktree_path")
	delete(session, "workdir_path")
	if current, exists := session["workspace_dir_path"]; !exists || current == "" {
		session["workspace_dir_path"] = value
	}
	return true
}

// migrateWorkflowOutputsWorkdirKey renames the @workflow pseudo-node's
// reserved setup output key from "workdir" to "workspace_dir", mirroring
// contract.OutputKeyWorkspaceDir. Other tasks' outputs are left alone: only
// the @workflow pseudo-node's outputs carry a plect-reserved key with
// contract meaning.
func migrateWorkflowOutputsWorkdirKey(session map[string]any) bool {
	tasks, _ := session["tasks"].(map[string]any)
	if tasks == nil {
		return false
	}
	wf, _ := tasks[contract.WorkflowPseudoNodeID].(map[string]any)
	if wf == nil {
		return false
	}
	outputs, _ := wf["outputs"].(map[string]any)
	if outputs == nil {
		return false
	}
	value, ok := outputs["workdir"]
	if !ok {
		return false
	}
	delete(outputs, "workdir")
	if current, exists := outputs["workspace_dir"]; !exists || current == "" {
		outputs["workspace_dir"] = value
	}
	return true
}

// backfillInlineTaskIDs makes task_id explicit for sessions created via the
// retired inline `[[tasks]]` config path (workflow unset). Those sessions
// have no compiled workflow node to fall back on once the "empty task_id
// means node id == task id" round-tripping convention is removed, so the map
// key (the only durable identity such a task ever had) is stamped in
// explicitly.
func backfillInlineTaskIDs(session map[string]any) bool {
	workflow, _ := session["workflow"].(string)
	if workflow != "" {
		return false
	}
	tasks, _ := session["tasks"].(map[string]any)
	if len(tasks) == 0 {
		return false
	}
	changed := false
	for key, raw := range tasks {
		task, ok := raw.(map[string]any)
		if !ok || task == nil {
			continue
		}
		if id, ok := task["task_id"].(string); ok && id != "" {
			continue
		}
		task["task_id"] = key
		changed = true
	}
	return changed
}

// githubOwnerRepoRE matches the exact "owner/repo" shape legacy
// repo_allowlist entries used.
var githubOwnerRepoRE = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)

// migrateConfigTOML folds the legacy repo_allowlist (exact "owner/repo"
// matches) into resource_allowlist (regex patterns), which is the only
// allowlist form the follow-up removal leaves in place.
func migrateConfigTOML(data []byte) ([]byte, []string, error) {
	if len(data) == 0 {
		return nil, nil, nil
	}

	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, nil, fmt.Errorf("parse config.toml: %w", err)
	}

	var notes []string
	// Either legacy root key may be present (worktrees_root from the oldest
	// form, workdirs_root from a workdir-era config a prior migration already
	// produced) — both fold into workspace_dirs_root in one pass, mirroring
	// migrateWorkspaceDirPath's same-shaped fallback in state.json.
	root, hadWorktreesRoot := doc["worktrees_root"]
	workdirsRoot, hadWorkdirsRoot := doc["workdirs_root"]
	if hadWorkdirsRoot {
		root = workdirsRoot
	}
	if hadWorktreesRoot || hadWorkdirsRoot {
		delete(doc, "worktrees_root")
		delete(doc, "workdirs_root")
		if _, exists := doc["workspace_dirs_root"]; !exists {
			doc["workspace_dirs_root"] = root
		}
		notes = append(notes, "config.toml: renamed worktrees_root/workdirs_root to workspace_dirs_root")
	}

	rawAllowlist, ok := doc["repo_allowlist"]
	if !ok {
		if len(notes) == 0 {
			return nil, nil, nil
		}
		return marshalTOMLIfChanged(doc, notes)
	}
	entries, _ := rawAllowlist.([]any)
	if len(entries) == 0 {
		// An empty repo_allowlist still counts as old form: its mere
		// presence is the legacy key the follow-up removal drops support
		// for, so it is removed even though there is nothing to fold in.
		delete(doc, "repo_allowlist")
		notes = append(notes, "config.toml: removed empty legacy repo_allowlist key")
		return marshalTOMLIfChanged(doc, notes)
	}

	existing, _ := doc["resource_allowlist"].([]any)
	existingSet := map[string]bool{}
	for _, v := range existing {
		if s, ok := v.(string); ok {
			existingSet[s] = true
		}
	}

	var added []string
	for _, v := range entries {
		ownerRepo, ok := v.(string)
		if !ok || !githubOwnerRepoRE.MatchString(ownerRepo) {
			continue
		}
		parts := splitOnce(ownerRepo, '/')
		pattern := fmt.Sprintf(`^https://github\.com/%s/%s(/|$)`, regexp.QuoteMeta(parts[0]), regexp.QuoteMeta(parts[1]))
		if existingSet[pattern] {
			continue
		}
		existingSet[pattern] = true
		existing = append(existing, pattern)
		added = append(added, pattern)
	}

	delete(doc, "repo_allowlist")
	doc["resource_allowlist"] = existing

	notes = append(notes, fmt.Sprintf("config.toml: folded %d legacy repo_allowlist entries into resource_allowlist", len(entries)))
	if len(added) > 0 {
		sort.Strings(added)
		notes = append(notes, fmt.Sprintf("config.toml: added resource_allowlist patterns: %v", added))
	}
	return marshalTOMLIfChanged(doc, notes)
}

func marshalTOMLIfChanged(doc map[string]any, notes []string) ([]byte, []string, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return nil, nil, fmt.Errorf("marshal migrated config.toml: %w", err)
	}
	return buf.Bytes(), notes, nil
}

func splitOnce(s string, sep byte) [2]string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return [2]string{s[:i], s[i+1:]}
		}
	}
	return [2]string{s, ""}
}

func deepCopyJSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, sub := range val {
			out[k] = deepCopyJSON(sub)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, sub := range val {
			out[i] = deepCopyJSON(sub)
		}
		return out
	default:
		return val
	}
}

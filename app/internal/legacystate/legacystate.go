// Package legacystate parses the state.json envelope core wrote before the
// SQLite cutover (see docs/design/sqlite-persistence.md); app/internal/
// legacyimport is its one live consumer. This is a byte-slice parser, not a
// file reader: the importer owns locating and reading the operator-supplied
// backup.
package legacystate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// SupportedVersion is the last state.json envelope version core ever wrote.
// It is a frozen historical fact this package owns, not a live schema
// authority — contracts/state.SchemaVersion was retired with the cutover.
const SupportedVersion = 7

// StateFile is the parsed, validated form of a state.json envelope.
type StateFile struct {
	Sessions              map[string]*domain.Session
	Populations           map[string]*domain.PopulationState
	UpReservations        map[string]domain.UpReservation
	HeartbeatLogPositions map[string]int64 // see parseHeartbeatLogPositions
}

// Parse decodes and validates a complete state.json byte slice: it rejects
// an unsupported envelope version and a pre-rename layers[].task_id record
// exactly as the live store did before the cutover, then normalizes the
// session tree (parent/child links, "root:<session>" pseudo-parents) the
// same way. It never treats malformed or unreadable input as an empty
// store — every error is returned rather than silently swallowed.
func Parse(data []byte) (*StateFile, error) {
	if err := checkLayerIdentityMigrated(data); err != nil {
		return nil, err
	}

	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	if err := ValidateVersion(header.Version); err != nil {
		return nil, err
	}

	var parsed struct {
		Version        int                                `json:"version"`
		Sessions       map[string]*domain.Session         `json:"sessions"`
		Populations    map[string]*domain.PopulationState `json:"populations,omitempty"`
		UpReservations map[string]domain.UpReservation    `json:"up_reservations,omitempty"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	if err := ValidateVersion(parsed.Version); err != nil {
		return nil, err
	}

	sf := &StateFile{
		Sessions:              make(map[string]*domain.Session, len(parsed.Sessions)),
		Populations:           parsed.Populations,
		UpReservations:        parsed.UpReservations,
		HeartbeatLogPositions: parseHeartbeatLogPositions(data),
	}
	for name, session := range parsed.Sessions {
		if session == nil {
			continue
		}
		session.Name = name // ensure name is set from map key
		migrateResourceID(session)
		sf.Sessions[name] = session
	}
	splitLegacyTasks(sf.Sessions, legacyDynamicFlags(data))
	normalizeSessionTree(sf.Sessions)

	return sf, nil
}

// legacyDynamicFlags re-derives Dynamic — a per-entry field the version-7
// envelope wrote but contract.TaskState no longer declares — from the raw
// bytes, keyed by session name then task/node key, so Parse can split a flat
// legacy "tasks" map into today's Nodes/Tasks collections.
func legacyDynamicFlags(data []byte) map[string]map[string]bool {
	var raw struct {
		Sessions map[string]struct {
			Tasks map[string]struct {
				Dynamic bool `json:"dynamic"`
			} `json:"tasks"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	out := make(map[string]map[string]bool, len(raw.Sessions))
	for name, session := range raw.Sessions {
		flags := make(map[string]bool, len(session.Tasks))
		for key, t := range session.Tasks {
			flags[key] = t.Dynamic
		}
		out[name] = flags
	}
	return out
}

// splitLegacyTasks partitions each session's flat legacy task map (decoded
// wholesale into Tasks, since Nodes did not exist in the version-7 envelope)
// into today's Nodes/Tasks by dynamicFlagsBySession; an unflagged key
// defaults to a node, matching the retired field's own zero value.
func splitLegacyTasks(sessions map[string]*domain.Session, dynamicFlagsBySession map[string]map[string]bool) {
	for name, session := range sessions {
		if session == nil || len(session.Tasks) == 0 {
			continue
		}
		dynamicFlags := dynamicFlagsBySession[name]
		nodes := make(map[string]*contract.TaskState)
		tasks := make(map[string]*contract.TaskState)
		for key, st := range session.Tasks {
			if dynamicFlags[key] {
				tasks[key] = st
			} else {
				nodes[key] = st
			}
		}
		if len(nodes) == 0 {
			nodes = nil
		}
		if len(tasks) == 0 {
			tasks = nil
		}
		session.Nodes = nodes
		session.Tasks = tasks
	}
}

// parseHeartbeatLogPositions re-scans data for a field contract.TickBackoff no longer declares.
func parseHeartbeatLogPositions(data []byte) map[string]int64 {
	var raw struct {
		Sessions map[string]struct {
			TickBackoff *struct {
				LastLogPosition int64 `json:"last_log_position"`
			} `json:"tick_backoff"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var positions map[string]int64
	for name, session := range raw.Sessions {
		if session.TickBackoff == nil {
			continue
		}
		if positions == nil {
			positions = make(map[string]int64)
		}
		positions[name] = session.TickBackoff.LastLogPosition
	}
	return positions
}

// ValidateVersion reports whether got is the one legacy envelope version
// this package understands.
func ValidateVersion(got int) error {
	if got == SupportedVersion {
		return nil
	}
	if got > SupportedVersion {
		return fmt.Errorf("state schema version mismatch: got %d, want %d; this state was written by a newer plect binary, so use a matching binary or migrate explicitly", got, SupportedVersion)
	}
	return fmt.Errorf("state schema version mismatch: got %d, want %d; run `go run ./plugins/legacy-migration/cmd/legacy-migration` before importing this state", got, SupportedVersion)
}

// json.Unmarshal silently drops unknown struct fields, so without this
// check an unmigrated task_id would decode as a zero-value EffectID and a
// later import would persist that zero value, destroying the layer's
// identity. See docs/migrations/task-layer-effect-id-migration.md.
func checkLayerIdentityMigrated(data []byte) error {
	var raw struct {
		Sessions map[string]struct {
			Tasks map[string]struct {
				Layers []map[string]json.RawMessage `json:"layers"`
			} `json:"tasks"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		// Malformed JSON is reported by the caller's own parse step.
		return nil
	}

	for sessionName, session := range raw.Sessions {
		for taskID, task := range session.Tasks {
			for i, layer := range task.Layers {
				if _, hasLegacyField := layer["task_id"]; hasLegacyField {
					return fmt.Errorf("state file: session %q task %q layer %d still has the pre-rename task_id field instead of effect_id; run the migration in docs/migrations/task-layer-effect-id-migration.md before importing this state", sessionName, taskID, i)
				}
				var effectID string
				if effectIDRaw, ok := layer["effect_id"]; ok {
					_ = json.Unmarshal(effectIDRaw, &effectID)
				}
				if effectID == "" {
					return fmt.Errorf("state file: session %q task %q layer %d has no effect_id; run the migration in docs/migrations/task-layer-effect-id-migration.md before importing this state", sessionName, taskID, i)
				}
			}
		}
	}
	return nil
}

// migrateResourceID backfills the create-time alias from the canonical
// resource id, which is what a session created without an explicit alias
// was looked up by.
func migrateResourceID(session *domain.Session) {
	if session.Alias == "" && session.ResourceID != "" {
		session.Alias = session.ResourceID
	}
}

// normalizeSessionTree derives Children from ParentSession, silently
// clearing a self-reference, a cycle, or a "root:<session>" pseudo-parent
// naming a session that does not exist — the same rules the live SQLite
// store enforces on write (app/internal/persistence's resolveParentColumns),
// so an imported session tree behaves identically either way.
func normalizeSessionTree(sessions map[string]*domain.Session) {
	for parentName, parent := range sessions {
		if parent == nil {
			continue
		}
		parent.Children = uniqueSessionNames(parent.Children)
		for _, childName := range parent.Children {
			child := sessions[childName]
			if child == nil || child.ParentSession != "" {
				continue
			}
			if childName != parentName && !wouldCreateCycle(sessions, childName, parentName) {
				child.ParentSession = parentName
			}
		}
	}

	for _, session := range sessions {
		if session != nil {
			session.Children = nil
		}
	}

	for childName, child := range sessions {
		if child == nil || child.ParentSession == "" {
			continue
		}
		if rootTarget, ok := strings.CutPrefix(child.ParentSession, "root:"); ok {
			// A "root:<session>" parent is a pseudo-node (that session's own
			// implicit root, domain.ImplicitRootParent) — not a Session in
			// this map, so it has no Children slot to append into. It is
			// valid as long as the named session actually exists.
			if rootTarget == "" || rootTarget == childName || sessions[rootTarget] == nil {
				child.ParentSession = ""
			}
			continue
		}
		parent := sessions[child.ParentSession]
		if parent == nil || child.ParentSession == childName || wouldCreateCycle(sessions, childName, child.ParentSession) {
			child.ParentSession = ""
			continue
		}
		parent.Children = append(parent.Children, childName)
	}

	for _, session := range sessions {
		if session != nil {
			session.Children = uniqueSessionNames(session.Children)
		}
	}
}

func wouldCreateCycle(sessions map[string]*domain.Session, childName, parentName string) bool {
	seen := map[string]bool{}
	for cur := parentName; cur != ""; {
		if cur == childName {
			return true
		}
		if seen[cur] {
			return true
		}
		seen[cur] = true
		parent := sessions[cur]
		if parent == nil {
			return false
		}
		cur = parent.ParentSession
	}
	return false
}

func uniqueSessionNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := names[:0]
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

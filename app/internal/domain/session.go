package domain

import (
	"slices"

	contract "github.com/kecbigmt/plecture/contracts/state"
)

// RunState reports whether a session's run-scoped task is produced — a plain
// runtime fact, not an interpretation of what the session is doing.
type RunState string

const (
	RunUp   RunState = "up"
	RunDown RunState = "down"
)

// HealthState reports the outcome of evaluating a session's declared
// `[health]` probes: the alive probe plus, when an activity probe is
// declared, whether the session is actually moving rather than merely
// presenting a live surface.
//
//   - Unhealthy: a declared alive probe failed.
//   - Undeclared: there is no basis to judge liveness or activity because no
//     alive probe is declared, or activity is expected but no activity probe
//     is declared to judge it.
//   - Healthy: the alive probe passes and either no activity is currently
//     expected, or fresh activity evidence exists within the stall threshold.
//   - Stalled: the surface is present, activity is expected, but no fresh
//     activity evidence exists.
type HealthState string

const (
	HealthHealthy    HealthState = "healthy"
	HealthUnhealthy  HealthState = "unhealthy"
	HealthUndeclared HealthState = "undeclared"
	// HealthStalled means the execution surface is present and activity was
	// expected, but no fresh activity evidence was found within the stall
	// threshold.
	HealthStalled HealthState = "stalled"
)

// Message is an alias for the shared contract type.
type Message = contract.Message

// Session is an alias for the shared contract type.
// Domain-specific logic is provided by package-level functions to avoid field
// drift between domain and contracts.
type Session = contract.Session

// SessionRelation describes another session's position from a target session.
type SessionRelation string

const (
	RelationSelf       SessionRelation = "self"
	RelationParent     SessionRelation = "parent"
	RelationChild      SessionRelation = "child"
	RelationSibling    SessionRelation = "sibling"
	RelationAncestor   SessionRelation = "ancestor"
	RelationDescendant SessionRelation = "descendant"
	RelationUnrelated  SessionRelation = "unrelated"
)

// ImplicitRootParent returns the parent key a parentless session is deemed to
// have: a root scoped to that session alone, so sibling placement is opt-in
// per root rather than shared across every parentless session in the forest.
func ImplicitRootParent(name string) string {
	return "root:" + name
}

// effectiveParent returns s's parent key for sibling comparison: its real
// ParentSession if set, otherwise its own implicit root. This is the only
// place "no parent" gets normalized — parent/child checks still compare raw
// ParentSession, since an implicit root is not an addressable session that
// can itself be another session's parent or child.
func effectiveParent(s *Session, name string) string {
	if s.ParentSession != "" {
		return s.ParentSession
	}
	return ImplicitRootParent(name)
}

// RelationFromTarget returns otherName's tree relation from targetName's point
// of view. Missing sessions are unrelated so callers can treat stale names as
// non-authoritative facts.
func RelationFromTarget(sessions map[string]*Session, targetName, otherName string) SessionRelation {
	target := sessions[targetName]
	other := sessions[otherName]
	if target == nil || other == nil {
		return RelationUnrelated
	}
	if targetName == otherName {
		return RelationSelf
	}
	if target.ParentSession == otherName {
		return RelationParent
	}
	if other.ParentSession == targetName {
		return RelationChild
	}
	if effectiveParent(target, targetName) == effectiveParent(other, otherName) {
		return RelationSibling
	}
	if isAncestor(sessions, otherName, targetName) {
		return RelationAncestor
	}
	if isAncestor(sessions, targetName, otherName) {
		return RelationDescendant
	}
	return RelationUnrelated
}

// defaultJudgeRelations is the relation policy a judge leaf uses when it
// declares none: an independent sibling reviewer, or a parent / orchestrator.
var defaultJudgeRelations = []SessionRelation{RelationSibling, RelationParent}

// DefaultJudgeRelations returns the relation policy applied to a judge leaf that
// declares no relation of its own.
func DefaultJudgeRelations() []SessionRelation {
	return slices.Clone(defaultJudgeRelations)
}

// AssignableJudgeRelation reports whether r may appear in a judge leaf's
// relation policy. self is never assignable — a session cannot satisfy its own
// judge leaf — and ancestor/descendant/unrelated are not placements a reviewer
// is spawned into, so only the tree positions a reviewer can occupy are listable.
func AssignableJudgeRelation(r SessionRelation) bool {
	switch r {
	case RelationParent, RelationChild, RelationSibling:
		return true
	default:
		return false
	}
}

// JudgeRelationAccepted reports whether a verdict recorded under rel is admitted
// by a judge leaf whose policy is accepted (empty = DefaultJudgeRelations). self
// is rejected regardless of policy, so independence is structural rather than a
// reviewer-discipline convention.
func JudgeRelationAccepted(accepted []SessionRelation, rel SessionRelation) bool {
	if rel == RelationSelf || rel == "" {
		return false
	}
	if len(accepted) == 0 {
		return slices.Contains(defaultJudgeRelations, rel)
	}
	return slices.Contains(accepted, rel)
}

// Subtree returns the root session plus every descendant, sorted for
// deterministic iteration. Membership is derived from ParentSession (the same
// fact RelationFromTarget walks), not from Children, so it holds even on a map
// a caller built with only parent links set. A missing root yields nil.
func Subtree(sessions map[string]*Session, root string) []string {
	if sessions[root] == nil {
		return nil
	}
	out := []string{root}
	for name, s := range sessions {
		if name == root || s == nil {
			continue
		}
		if isAncestor(sessions, root, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func isAncestor(sessions map[string]*Session, ancestor, child string) bool {
	seen := map[string]bool{}
	for cur := child; cur != ""; {
		if seen[cur] {
			return false
		}
		seen[cur] = true
		s := sessions[cur]
		if s == nil {
			return false
		}
		parent := s.ParentSession
		if parent == "" {
			return false
		}
		if parent == ancestor {
			return true
		}
		cur = parent
	}
	return false
}

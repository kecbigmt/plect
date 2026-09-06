package task

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/lang"
)

// DoneStatus is the evaluation result of a done_when leaf (and, aggregated,
// of a whole done_when).
//
//   - DoneSatisfied   — the predicate holds (✓).
//   - DoneUnsatisfied — the fact is present and the predicate is false (✗).
//   - DonePending     — the predicate cannot be evaluated yet: a check leaf
//     reads a key nothing has reported, an expression reads one, or a judge
//     leaf has no judge action.
type DoneStatus string

const (
	DoneSatisfied   DoneStatus = "satisfied"
	DoneUnsatisfied DoneStatus = "unsatisfied"
	DonePending     DoneStatus = "pending"
)

const (
	JudgeActionApprove        = "approve"
	JudgeActionRequestChanges = "request_changes"
)

// Judge is the read-only judge action consumed by the done_when evaluator.
// Callers own persistence and convert from their state representation at the
// boundary.
type Judge struct {
	LeafID        string `json:"leaf_id"`
	Action        string `json:"action"`
	Reason        string `json:"reason,omitempty"`
	Revision      string `json:"revision,omitempty"`
	JudgeSession  string `json:"judge_session,omitempty"`
	JudgeWorkflow string `json:"judge_workflow,omitempty"`
	Relation      string `json:"relation,omitempty"`
}

// DoneWhenEvalContext supplies judge actions and the current opaque revision
// for the task instance under evaluation.
type DoneWhenEvalContext struct {
	WorkSession     string
	CurrentRevision string
	Judges          map[string]Judge
}

// DoneLeafResult is one leaf's evaluation. A check leaf also carries the read
// output's last-fetched Value (Observed=false until produced) — acquisition and
// evaluation are separate, so the value is displayable on its own.
type DoneLeafResult struct {
	Kind            string     `json:"kind"` // "check" | "judge"
	Expr            string     `json:"expr"` // human-readable rendering of the leaf
	Status          DoneStatus `json:"status"`
	ID              string     `json:"id,omitempty"`
	Output          string     `json:"output,omitempty"`
	Value           string     `json:"value,omitempty"`
	Observed        bool       `json:"observed,omitempty"`
	Action          string     `json:"action,omitempty"`
	Reason          string     `json:"reason,omitempty"`
	Revision        string     `json:"revision,omitempty"`
	CurrentRevision string     `json:"current_revision,omitempty"`
	JudgeSession    string     `json:"judge_session,omitempty"`
	JudgeWorkflow   string     `json:"judge_workflow,omitempty"`
	Relation        string     `json:"relation,omitempty"`
	PendingReason   string     `json:"pending_reason,omitempty"`
}

// DoneWhenResult is the per-instance evaluation of a task's done_when.
// Overall is satisfied only when every leaf is satisfied; it is unsatisfied
// when any leaf is unsatisfied; otherwise (some leaves still pending) it is
// pending.
type DoneWhenResult struct {
	Overall DoneStatus       `json:"overall"`
	Leaves  []DoneLeafResult `json:"leaves,omitempty"`
}

// CompletionState is the pair of live roots a completion predicate reads:
// what the declared observer publishes about the resource, and what this task
// holds about itself. Both are current as of the evaluation that reads them —
// re-evaluation rides on that, so nothing here declares itself dynamic.
type CompletionState struct {
	Resource map[string]any
	Self     map[string]any
}

// Lookup resolves one completion key path. Absence is the pending case, so it
// is reported rather than defaulted. A key that names no root resolves to
// nothing: every completion key names the contract it reads from.
func (s CompletionState) Lookup(path string) (any, bool) {
	root, key, ok := splitCompletionKey(path)
	if !ok {
		return nil, false
	}
	var from map[string]any
	switch root {
	case "resource":
		from = s.Resource
	case "self":
		from = s.Self
	default:
		return nil, false
	}
	val, ok := from[key]
	return val, ok && val != nil
}

// Roots renders the two live roots as the tree an expression leaf is
// evaluated against.
func (s CompletionState) Roots() lang.Roots {
	return lang.Roots{
		"resource": map[string]any{"state": orEmpty(normalizeOutputs(s.Resource))},
		"self":     map[string]any{"state": orEmpty(normalizeOutputs(s.Self))},
	}
}

func splitCompletionKey(path string) (root, key string, ok bool) {
	segments := strings.Split(path, ".")
	if len(segments) != 3 || segments[1] != "state" {
		return "", "", false
	}
	return segments[0], segments[2], true
}

// EvaluateTaskDoneWhen evaluates a task's done_when against one instance's
// live state. Each check leaf compares the key it names with the leaf's
// operator, an expression leaf states its whole predicate, and a judge leaf
// reads persisted reviewer action.
func EvaluateTaskDoneWhen(dw *config.DoneWhen, state CompletionState) DoneWhenResult {
	return EvaluateTaskDoneWhenWithContext(dw, state, DoneWhenEvalContext{})
}

// EvaluateTaskDoneWhenWithContext evaluates a done_when with judge action
// context for judge leaves. Evaluation is read-only: stale, missing, or
// self-review action is pending; current request_changes is unsatisfied.
func EvaluateTaskDoneWhenWithContext(dw *config.DoneWhen, state CompletionState, ctx DoneWhenEvalContext) DoneWhenResult {
	if dw == nil || len(dw.All) == 0 {
		return DoneWhenResult{}
	}
	state.Resource = orEmpty(normalizeOutputs(state.Resource))
	state.Self = orEmpty(normalizeOutputs(state.Self))

	leaves := make([]DoneLeafResult, 0, len(dw.All))
	satisfied, unsatisfied := 0, 0
	for i, leaf := range dw.All {
		var res DoneLeafResult
		switch {
		case leaf.IsJudge():
			res = evalJudgeLeaf(i, leaf, ctx)
		case leaf.IsExpr():
			res = evalExprLeaf(leaf, state)
		default:
			val, observed := state.Lookup(leaf.Check)
			res = DoneLeafResult{
				Kind:     "check",
				Expr:     checkExpr(leaf),
				Status:   evalCheckLeaf(leaf, state),
				Output:   leaf.Check,
				Observed: observed,
			}
			if observed {
				res.Value = toString(val)
			}
		}
		switch res.Status {
		case DoneSatisfied:
			satisfied++
		case DoneUnsatisfied:
			unsatisfied++
		}
		leaves = append(leaves, res)
	}

	overall := DonePending
	switch {
	case unsatisfied > 0:
		overall = DoneUnsatisfied
	case satisfied == len(leaves):
		overall = DoneSatisfied
	}
	return DoneWhenResult{Overall: overall, Leaves: leaves}
}

func evalJudgeLeaf(i int, leaf config.DoneWhenLeaf, ctx DoneWhenEvalContext) DoneLeafResult {
	id := JudgeLeafID(i, leaf)
	res := DoneLeafResult{
		Kind:            "judge",
		ID:              id,
		Expr:            leaf.Judge,
		Status:          DonePending,
		CurrentRevision: ctx.CurrentRevision,
	}
	judge, ok := ctx.Judges[id]
	if !ok {
		res.PendingReason = "missing_judge"
		return res
	}
	res.Action = judge.Action
	res.Reason = judge.Reason
	res.Revision = judge.Revision
	res.JudgeSession = judge.JudgeSession
	res.JudgeWorkflow = judge.JudgeWorkflow
	res.Relation = judge.Relation
	rel := domain.SessionRelation(judge.Relation)
	// self-review is structurally rejected: a verdict from the work session (or
	// one with no attributable judge) can never satisfy a judge leaf.
	if judge.JudgeSession == "" || rel == domain.RelationSelf || (ctx.WorkSession != "" && judge.JudgeSession == ctx.WorkSession) {
		res.PendingReason = "self_review"
		return res
	}
	if !domain.JudgeRelationAccepted(acceptedJudgeRelations(leaf), rel) {
		res.PendingReason = "relation_not_accepted"
		return res
	}
	if ctx.CurrentRevision == "" {
		res.PendingReason = "missing_current_revision"
		return res
	}
	if judge.Revision != ctx.CurrentRevision {
		res.PendingReason = "stale_judge"
		return res
	}
	switch judge.Action {
	case JudgeActionApprove:
		res.Status = DoneSatisfied
	case JudgeActionRequestChanges:
		res.Status = DoneUnsatisfied
	default:
		res.PendingReason = "unknown_judge"
	}
	return res
}

// acceptedJudgeRelations converts a leaf's declared relation policy to domain
// values; nil (leaf declares none) lets JudgeRelationAccepted apply the default.
func acceptedJudgeRelations(leaf config.DoneWhenLeaf) []domain.SessionRelation {
	if len(leaf.Relation) == 0 {
		return nil
	}
	out := make([]domain.SessionRelation, len(leaf.Relation))
	for i, r := range leaf.Relation {
		out[i] = domain.SessionRelation(r)
	}
	return out
}

// JudgeLeafID returns the stable storage key for a judge leaf. Configs should
// set explicit ids; the index fallback keeps older configs addressable.
func JudgeLeafID(i int, leaf config.DoneWhenLeaf) string {
	if strings.TrimSpace(leaf.ID) != "" {
		return strings.TrimSpace(leaf.ID)
	}
	return fmt.Sprintf("judge:%d", i)
}

// CheckLeafStatus evaluates a single check leaf against one instance's live
// state. It is the check-fact primitive the chaining engine's `when` shares
// with done_when, so a chain `{ check = ..., in = [...] }` reads identically to
// the same done_when leaf.
func CheckLeafStatus(leaf config.DoneWhenLeaf, state CompletionState) DoneStatus {
	state.Resource = orEmpty(normalizeOutputs(state.Resource))
	state.Self = orEmpty(normalizeOutputs(state.Self))
	if leaf.IsExpr() {
		return evalExprLeaf(leaf, state).Status
	}
	return evalCheckLeaf(leaf, state)
}

// evalCheckLeaf applies the leaf's comparison operator to the key it names. A
// key nothing has reported is pending (no fact yet); a present one that fails
// the predicate is unsatisfied. gte/lte against a non-numeric value reads as
// unsatisfied — the value exists but cannot satisfy a numeric bound.
// evalExprLeaf evaluates one expression leaf. A predicate that cannot be
// evaluated is pending, not unsatisfied: an expression comparing a recorded
// value against a live one reads as "nothing recorded yet" before it reads as
// "they differ", and only the second is a reason to stop waiting. A
// non-boolean result is a declaration error the language rejects at load, so
// it is pending here rather than guessed at.
func evalExprLeaf(leaf config.DoneWhenLeaf, state CompletionState) DoneLeafResult {
	res := DoneLeafResult{Kind: "expr", Expr: leaf.Expr, Status: DonePending}
	eval := lang.Eval{Roots: state.Roots()}
	resolved, _, err := eval.Value(&lang.Value{Form: lang.FormExpr, Expr: leaf.Expr})
	if err != nil {
		res.PendingReason = "unevaluable_expression"
		return res
	}
	held, ok := resolved.(bool)
	if !ok {
		res.PendingReason = "non_boolean_expression"
		return res
	}
	res.Observed = true
	res.Value = toString(held)
	res.Status = boolStatus(held)
	return res
}

func evalCheckLeaf(leaf config.DoneWhenLeaf, state CompletionState) DoneStatus {
	val, ok := state.Lookup(leaf.Check)
	if !ok {
		return DonePending
	}
	switch {
	case leaf.Eq != nil:
		return boolStatus(toString(val) == *leaf.Eq)
	case leaf.Ne != nil:
		return boolStatus(toString(val) != *leaf.Ne)
	case leaf.In != nil:
		target := toString(val)
		for _, item := range leaf.In {
			if toString(item) == target {
				return DoneSatisfied
			}
		}
		return DoneUnsatisfied
	case leaf.Gte != nil:
		f, ok := toFloat(val)
		if !ok {
			return DoneUnsatisfied
		}
		return boolStatus(f >= *leaf.Gte)
	case leaf.Lte != nil:
		f, ok := toFloat(val)
		if !ok {
			return DoneUnsatisfied
		}
		return boolStatus(f <= *leaf.Lte)
	}
	// No operator — config validation rejects this; treat defensively as pending.
	return DonePending
}

// checkExpr renders a check leaf for display, e.g. `coverage gte 80`.
func checkExpr(leaf config.DoneWhenLeaf) string {
	switch {
	case leaf.Eq != nil:
		return fmt.Sprintf("%s eq %q", leaf.Check, *leaf.Eq)
	case leaf.Ne != nil:
		return fmt.Sprintf("%s ne %q", leaf.Check, *leaf.Ne)
	case leaf.In != nil:
		return fmt.Sprintf("%s in %v", leaf.Check, leaf.In)
	case leaf.Gte != nil:
		return fmt.Sprintf("%s gte %v", leaf.Check, *leaf.Gte)
	case leaf.Lte != nil:
		return fmt.Sprintf("%s lte %v", leaf.Check, *leaf.Lte)
	}
	return leaf.Check
}

func boolStatus(b bool) DoneStatus {
	if b {
		return DoneSatisfied
	}
	return DoneUnsatisfied
}

// toString renders an output value for string-equality / membership checks.
// Numbers arrive normalized (int64 for integral values), so 3 → "3".
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

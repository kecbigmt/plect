package task

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
)

func strp(s string) *string { return &s }
func fp(f float64) *float64 { return &f }

func TestEvaluateTaskDoneWhen(t *testing.T) {
	tests := []struct {
		name    string
		dw      *config.DoneWhen
		outputs map[string]any
		want    DoneStatus
	}{
		{
			name:    "nil done_when is empty",
			dw:      nil,
			outputs: nil,
			want:    "",
		},
		{
			name:    "eq satisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.checks_status", Eq: strp("SUCCESS")}}},
			outputs: map[string]any{"checks_status": "SUCCESS"},
			want:    DoneSatisfied,
		},
		{
			name:    "eq present but false is unsatisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.checks_status", Eq: strp("SUCCESS")}}},
			outputs: map[string]any{"checks_status": "FAILURE"},
			want:    DoneUnsatisfied,
		},
		{
			name:    "missing output is pending, not unsatisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.checks_status", Eq: strp("SUCCESS")}}},
			outputs: map[string]any{},
			want:    DonePending,
		},
		{
			name:    "ne satisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.pr_state", Ne: strp("open")}}},
			outputs: map[string]any{"pr_state": "merged"},
			want:    DoneSatisfied,
		},
		{
			name:    "in satisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.pr_state", In: []any{"merged", "closed"}}}},
			outputs: map[string]any{"pr_state": "closed"},
			want:    DoneSatisfied,
		},
		{
			name:    "in not a member is unsatisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.pr_state", In: []any{"merged", "closed"}}}},
			outputs: map[string]any{"pr_state": "open"},
			want:    DoneUnsatisfied,
		},
		{
			name:    "gte satisfied (numeric, normalized int)",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.coverage", Gte: fp(80)}}},
			outputs: map[string]any{"coverage": float64(85)},
			want:    DoneSatisfied,
		},
		{
			name:    "gte below bound is unsatisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.coverage", Gte: fp(80)}}},
			outputs: map[string]any{"coverage": float64(70)},
			want:    DoneUnsatisfied,
		},
		{
			name:    "lte satisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.failures", Lte: fp(0)}}},
			outputs: map[string]any{"failures": float64(0)},
			want:    DoneSatisfied,
		},
		{
			name:    "gte against non-numeric value is unsatisfied",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.coverage", Gte: fp(80)}}},
			outputs: map[string]any{"coverage": "lots"},
			want:    DoneUnsatisfied,
		},
		{
			name: "conjunction: all satisfied",
			dw: &config.DoneWhen{All: []config.DoneWhenLeaf{
				{Check: "self.state.pr_state", Eq: strp("merged")},
				{Check: "self.state.checks_status", Eq: strp("SUCCESS")},
			}},
			outputs: map[string]any{"pr_state": "merged", "checks_status": "SUCCESS"},
			want:    DoneSatisfied,
		},
		{
			name: "conjunction: one unsatisfied wins over pending",
			dw: &config.DoneWhen{All: []config.DoneWhenLeaf{
				{Check: "self.state.pr_state", Eq: strp("merged")},       // unsatisfied
				{Check: "self.state.checks_status", Eq: strp("SUCCESS")}, // pending (missing)
			}},
			outputs: map[string]any{"pr_state": "open"},
			want:    DoneUnsatisfied,
		},
		{
			name: "conjunction: satisfied + pending is pending",
			dw: &config.DoneWhen{All: []config.DoneWhenLeaf{
				{Check: "self.state.pr_state", Eq: strp("merged")},       // satisfied
				{Check: "self.state.checks_status", Eq: strp("SUCCESS")}, // pending (missing)
			}},
			outputs: map[string]any{"pr_state": "merged"},
			want:    DonePending,
		},
		{
			name:    "judge leaf is pending without a judge",
			dw:      &config.DoneWhen{All: []config.DoneWhenLeaf{{Judge: "reviewer approved", ID: "ac-met"}}},
			outputs: map[string]any{},
			want:    DonePending,
		},
		{
			name: "judge leaf keeps a satisfied check pending",
			dw: &config.DoneWhen{All: []config.DoneWhenLeaf{
				{Check: "self.state.pr_state", Eq: strp("merged")},
				{Judge: "reviewer approved"},
			}},
			outputs: map[string]any{"pr_state": "merged"},
			want:    DonePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateTaskDoneWhen(tt.dw, CompletionState{Self: tt.outputs})
			if got.Overall != tt.want {
				t.Errorf("overall = %q, want %q (leaves: %+v)", got.Overall, tt.want, got.Leaves)
			}
		})
	}
}

func TestEvaluateTaskDoneWhen_Judges(t *testing.T) {
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{{Judge: "acceptance criteria", ID: "ac-met"}}}
	outputs := map[string]any{"revision": "sha1"}

	t.Run("approve action from current independent review satisfies", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "review", Relation: "sibling", Reason: "tests pass"},
			},
		})
		if got.Overall != DoneSatisfied {
			t.Fatalf("overall = %q, want satisfied: %+v", got.Overall, got.Leaves)
		}
	})

	t.Run("changes requested current judge is unsatisfied", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionRequestChanges, Revision: "sha1", JudgeSession: "review", Relation: "sibling", Reason: "missing AC"},
			},
		})
		if got.Overall != DoneUnsatisfied || got.Leaves[0].Reason != "missing AC" {
			t.Fatalf("got %+v, want unsatisfied with reason", got)
		}
	})

	t.Run("stale judge is pending", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha2",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "review", Relation: "sibling"},
			},
		})
		if got.Overall != DonePending || got.Leaves[0].PendingReason != "stale_judge" {
			t.Fatalf("got %+v, want stale pending", got)
		}
	})

	t.Run("self review is pending", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "work"},
			},
		})
		if got.Overall != DonePending || got.Leaves[0].PendingReason != "self_review" {
			t.Fatalf("got %+v, want self-review pending", got)
		}
	})

	t.Run("empty reviewer is self review pending", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1"},
			},
		})
		if got.Overall != DonePending || got.Leaves[0].PendingReason != "self_review" {
			t.Fatalf("got %+v, want empty-reviewer pending", got)
		}
	})

	t.Run("default policy accepts a parent reviewer", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "orchestrator", Relation: "parent"},
			},
		})
		if got.Overall != DoneSatisfied {
			t.Fatalf("got %+v, want parent reviewer satisfied", got)
		}
	})

	t.Run("default policy rejects a child reviewer as relation_not_accepted", func(t *testing.T) {
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "work+child", Relation: "child"},
			},
		})
		if got.Overall != DonePending || got.Leaves[0].PendingReason != "relation_not_accepted" {
			t.Fatalf("got %+v, want child reviewer relation_not_accepted", got)
		}
	})

	t.Run("explicit child policy accepts a child reviewer", func(t *testing.T) {
		dw := &config.DoneWhen{All: []config.DoneWhenLeaf{{Judge: "acceptance criteria", ID: "ac-met", Relation: []string{"child"}}}}
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "work+child", Relation: "child"},
			},
		})
		if got.Overall != DoneSatisfied {
			t.Fatalf("got %+v, want explicitly-allowed child reviewer satisfied", got)
		}
	})

	t.Run("explicit policy that omits sibling rejects a sibling reviewer", func(t *testing.T) {
		dw := &config.DoneWhen{All: []config.DoneWhenLeaf{{Judge: "acceptance criteria", ID: "ac-met", Relation: []string{"parent"}}}}
		got := EvaluateTaskDoneWhenWithContext(dw, CompletionState{Self: outputs}, DoneWhenEvalContext{
			WorkSession:     "work",
			CurrentRevision: "sha1",
			Judges: map[string]Judge{
				"ac-met": {Action: JudgeActionApprove, Revision: "sha1", JudgeSession: "review", Relation: "sibling"},
			},
		})
		if got.Overall != DonePending || got.Leaves[0].PendingReason != "relation_not_accepted" {
			t.Fatalf("got %+v, want sibling rejected under parent-only policy", got)
		}
	})
}

// TestEvaluateTaskDoneWhen_TaskResourceStatus pins the truth table the shipped
// work/review/respond/investigate done_when encodes: resource_kind gates
// which resource kinds are eligible at all, and FAILURE is distinguishable
// from PENDING (not collapsed). issue_status is deliberately absent: a
// task's own completion no longer waits on the issue actually being
// closed — that is the integrator's responsibility, not this done_when's — so
// an issue resource with no linked PR yet (checks_status NULL) is done as
// soon as its own judges are satisfied.
func TestEvaluateTaskDoneWhen_TaskResourceStatus(t *testing.T) {
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{
		{Check: "self.state.resource_kind", In: []any{"pull", "issue"}},
		{Check: "self.state.checks_status", In: []any{"SUCCESS", "NULL"}},
	}}
	out := func(kind, checks string) map[string]any {
		return map[string]any{"resource_kind": kind, "checks_status": checks}
	}
	tests := []struct {
		name    string
		outputs map[string]any
		want    DoneStatus
	}{
		{"pr checks success is done", out("pull", "SUCCESS"), DoneSatisfied},
		{"pr checks failure is unsatisfied", out("pull", "FAILURE"), DoneUnsatisfied},
		{"pr checks pending is not done", out("pull", "PENDING"), DoneUnsatisfied},
		{"issue with no linked PR is done", out("issue", "NULL"), DoneSatisfied},
		{"issue with a green linked PR is done", out("issue", "SUCCESS"), DoneSatisfied},
		{"unknown resource never done", out("unknown", "NULL"), DoneUnsatisfied},
		{"unfetched outputs stay pending", map[string]any{}, DonePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateTaskDoneWhen(dw, CompletionState{Self: tt.outputs}); got.Overall != tt.want {
				t.Errorf("overall = %q, want %q (leaves: %+v)", got.Overall, tt.want, got.Leaves)
			}
		})
	}
}

func TestEvaluateTaskDoneWhen_NormalizesIntegers(t *testing.T) {
	// JSON unmarshal leaves integers as float64; eq compares via the normalized
	// int string form, so an integral coverage value still matches "3".
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{{Check: "self.state.count", Eq: strp("3")}}}
	if got := EvaluateTaskDoneWhen(dw, CompletionState{Self: map[string]any{"count": float64(3)}}); got.Overall != DoneSatisfied {
		t.Errorf("overall = %q, want satisfied", got.Overall)
	}
}

func TestEvaluateTaskDoneWhen_LeafCarriesObservedValue(t *testing.T) {
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{
		{Check: "self.state.workdir_dirty", Eq: strp("0")},
		{Check: "self.state.checks_status", Eq: strp("SUCCESS")},
		{Judge: "reviewer approved"},
	}}
	got := EvaluateTaskDoneWhen(dw, CompletionState{Self: map[string]any{"workdir_dirty": "2"}})

	dirty := got.Leaves[0]
	if !dirty.Observed || dirty.Value != "2" || dirty.Output != "self.state.workdir_dirty" {
		t.Errorf("observed leaf = %+v, want output=self.state.workdir_dirty value=2 observed", dirty)
	}
	if dirty.Status != DoneUnsatisfied {
		t.Errorf("workdir_dirty=2 eq 0 → %q, want unsatisfied", dirty.Status)
	}

	unobserved := got.Leaves[1]
	if unobserved.Observed || unobserved.Value != "" || unobserved.Status != DonePending {
		t.Errorf("unobserved leaf = %+v, want pending with no value", unobserved)
	}

	if judge := got.Leaves[2]; judge.Output != "" || judge.Observed {
		t.Errorf("judge leaf = %+v, want no output/value", judge)
	}
}

func TestEvaluateTaskDoneWhen_RootedKeys(t *testing.T) {
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{
		{Check: "resource.state.checks_status", Eq: strp("SUCCESS")},
		{Check: "self.state.verdict_revision", Eq: strp("sha1")},
	}}
	tests := []struct {
		name  string
		state CompletionState
		want  DoneStatus
	}{
		{
			name: "each root reads its own key",
			state: CompletionState{
				Resource: map[string]any{"checks_status": "SUCCESS"},
				Self:     map[string]any{"verdict_revision": "sha1"},
			},
			want: DoneSatisfied,
		},
		{
			name: "a key present in the other root is not visible",
			state: CompletionState{
				Resource: map[string]any{"checks_status": "SUCCESS", "verdict_revision": "sha1"},
			},
			want: DonePending,
		},
		{
			name: "an unknown root is pending, not a lookup into nothing",
			state: CompletionState{
				Resource: map[string]any{"checks_status": "SUCCESS"},
				Self:     map[string]any{"verdict_revision": "sha0"},
			},
			want: DoneUnsatisfied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateTaskDoneWhen(dw, tt.state); got.Overall != tt.want {
				t.Errorf("overall = %q, want %q", got.Overall, tt.want)
			}
		})
	}
}

// The verdict flow is the case the expression leaf exists for: a recorded
// revision compared against the live one, with no key of its own to hang on.
func TestEvaluateTaskDoneWhen_ExprLeaf(t *testing.T) {
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{
		{Expr: "self.state.verdict_revision == resource.state.revision"},
	}}
	tests := []struct {
		name  string
		state CompletionState
		want  DoneStatus
	}{
		{
			name: "nothing recorded yet is pending, not a difference",
			state: CompletionState{
				Resource: map[string]any{"revision": "sha2"},
			},
			want: DonePending,
		},
		{
			name: "a verdict against an older revision is unsatisfied",
			state: CompletionState{
				Resource: map[string]any{"revision": "sha2"},
				Self:     map[string]any{"verdict_revision": "sha1"},
			},
			want: DoneUnsatisfied,
		},
		{
			name: "a verdict against the live revision satisfies",
			state: CompletionState{
				Resource: map[string]any{"revision": "sha2"},
				Self:     map[string]any{"verdict_revision": "sha2"},
			},
			want: DoneSatisfied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateTaskDoneWhen(dw, tt.state)
			if got.Overall != tt.want {
				t.Errorf("overall = %q, want %q (leaves: %+v)", got.Overall, tt.want, got.Leaves)
			}
			if got.Leaves[0].Kind != "expr" {
				t.Errorf("leaf kind = %q, want expr", got.Leaves[0].Kind)
			}
		})
	}
}

func TestEvaluateTaskDoneWhen_ExprLeafNonBooleanIsPending(t *testing.T) {
	dw := &config.DoneWhen{All: []config.DoneWhenLeaf{{Expr: "resource.state.revision"}}}
	got := EvaluateTaskDoneWhen(dw, CompletionState{Resource: map[string]any{"revision": "sha2"}})
	if got.Overall != DonePending {
		t.Fatalf("overall = %q, want %q", got.Overall, DonePending)
	}
	if got.Leaves[0].PendingReason != "non_boolean_expression" {
		t.Errorf("pending reason = %q, want non_boolean_expression", got.Leaves[0].PendingReason)
	}
}

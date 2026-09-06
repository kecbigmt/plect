package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// loadTasks assembles a session's Tasks map from two tables: node_instances
// (static workflow-DAG nodes, including the @workflow pseudo-node) and
// task_instances (dynamic instances created via `plect task setup`), joined
// in Go with task_done_when_states/task_done_when_judges by the dynamic
// instance's id. Dynamic is derived from which table a record came from,
// never stored.
func loadTasks(ctx context.Context, q sqlcgen.DBTX, sessionName string) (map[string]*contract.TaskState, error) {
	queries := sqlcgen.New(q)

	nodeRows, err := queries.ListNodeInstances(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list node instances for %q: %w", sessionName, err)
	}
	instanceRows, err := queries.ListTaskInstances(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list task instances for %q: %w", sessionName, err)
	}
	if len(nodeRows) == 0 && len(instanceRows) == 0 {
		return nil, nil
	}

	doneWhenRows, err := queries.ListTaskDoneWhenStatesForSession(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list done_when states for %q: %w", sessionName, err)
	}
	doneWhenByInstanceID := make(map[string]sqlcgen.TaskDoneWhenState, len(doneWhenRows))
	for _, r := range doneWhenRows {
		doneWhenByInstanceID[r.TaskInstanceID] = r
	}

	judgeRows, err := queries.ListTaskDoneWhenJudgesForSession(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list done_when judges for %q: %w", sessionName, err)
	}
	judgesByInstanceID := make(map[string]map[string]*contract.DoneWhenJudge)
	for _, r := range judgeRows {
		judge, err := judgeFromRow(r)
		if err != nil {
			return nil, err
		}
		m := judgesByInstanceID[r.TaskInstanceID]
		if m == nil {
			m = make(map[string]*contract.DoneWhenJudge)
			judgesByInstanceID[r.TaskInstanceID] = m
		}
		m[r.LeafID] = judge
	}

	tasks := make(map[string]*contract.TaskState, len(nodeRows)+len(instanceRows))
	for _, row := range nodeRows {
		ts, err := unmarshalNodeInstanceRecord(row.RecordJson)
		if err != nil {
			return nil, fmt.Errorf("parse node instance %q/%q record: %w", sessionName, row.NodeID, err)
		}
		ts.Scope = row.Scope
		ts.Status = row.Status
		ts.Seq = int(row.Sequence)
		ts.Dynamic = false
		finalizedAt, err := parseTimeNull(row.FinalizedAt)
		if err != nil {
			return nil, fmt.Errorf("parse node instance %q/%q finalized_at: %w", sessionName, row.NodeID, err)
		}
		ts.FinalizedAt = finalizedAt
		tasks[row.NodeID] = ts
	}
	for _, row := range instanceRows {
		ts, err := unmarshalTaskRecord(row.RecordJson)
		if err != nil {
			return nil, fmt.Errorf("parse task instance %q/%q record: %w", sessionName, row.InstanceName, err)
		}
		ts.TaskID = row.TaskID
		ts.Scope = row.Scope
		ts.Status = row.Status
		ts.Seq = int(row.Sequence)
		ts.Dynamic = true
		ts.Resource = row.Resource.String
		if row.Named {
			ts.Name = row.InstanceName
		}
		finalizedAt, err := parseTimeNull(row.FinalizedAt)
		if err != nil {
			return nil, fmt.Errorf("parse task instance %q/%q finalized_at: %w", sessionName, row.InstanceName, err)
		}
		ts.FinalizedAt = finalizedAt

		if dw, ok := doneWhenByInstanceID[row.ID]; ok {
			doneWhen, err := doneWhenFromRow(dw, judgesByInstanceID[row.ID])
			if err != nil {
				return nil, fmt.Errorf("parse done_when %q/%q: %w", sessionName, row.InstanceName, err)
			}
			ts.DoneWhen = doneWhen
		}
		tasks[row.InstanceName] = ts
	}
	return tasks, nil
}

// writeTasksTx replaces every node-instance row for sessionName (that table
// has no identity worth preserving across a write) and reconciles
// task_instances against the current Tasks map instead: each current dynamic
// instance is upserted (preserving its id across an ordinary update; see
// UpsertTaskInstance), and any instance_name no longer present is then
// explicitly deleted, which is what mints a fresh id on a later cleanup +
// setup under the same name. It never touches another session's rows.
func (db *DB) writeTasksTx(ctx context.Context, tx *sql.Tx, sessionName string, tasks map[string]*contract.TaskState) error {
	q := sqlcgen.New(tx)
	if err := q.DeleteNodeInstancesForSession(ctx, sessionName); err != nil {
		return fmt.Errorf("clear node instances for %q: %w", sessionName, err)
	}

	existing, err := q.ListTaskInstances(ctx, sessionName)
	if err != nil {
		return fmt.Errorf("list existing task instances for %q: %w", sessionName, err)
	}
	remaining := make(map[string]bool, len(existing))
	for _, row := range existing {
		remaining[row.InstanceName] = true
	}

	for key, ts := range tasks {
		if ts == nil {
			continue
		}
		if ts.Dynamic {
			delete(remaining, key)
			if err := upsertTaskInstanceTx(ctx, q, sessionName, key, ts); err != nil {
				return err
			}
			continue
		}
		if err := insertNodeInstanceTx(ctx, q, sessionName, key, ts); err != nil {
			return err
		}
	}

	for instanceName := range remaining {
		if err := q.DeleteTaskInstanceByName(ctx, sqlcgen.DeleteTaskInstanceByNameParams{
			SessionName:  sessionName,
			InstanceName: instanceName,
		}); err != nil {
			return fmt.Errorf("delete task instance %q/%q: %w", sessionName, instanceName, err)
		}
	}
	return nil
}

func insertNodeInstanceTx(ctx context.Context, q *sqlcgen.Queries, sessionName, nodeID string, ts *contract.TaskState) error {
	recordJSON, err := marshalNodeInstanceRecord(ts)
	if err != nil {
		return fmt.Errorf("marshal node instance %q/%q: %w", sessionName, nodeID, err)
	}
	if err := q.InsertNodeInstance(ctx, sqlcgen.InsertNodeInstanceParams{
		SessionName: sessionName,
		NodeID:      nodeID,
		Scope:       ts.Scope,
		Status:      ts.Status,
		Sequence:    int64(ts.Seq),
		FinalizedAt: formatTimeNull(ts.FinalizedAt),
		RecordJson:  recordJSON,
	}); err != nil {
		return fmt.Errorf("insert node instance %q/%q: %w", sessionName, nodeID, err)
	}
	return nil
}

// upsertTaskInstanceTx upserts the instance row, then replaces its
// done_when/judge rows keyed by whichever id the upsert reports (the
// preserved id on an ordinary update, or the freshly minted one on a
// genuinely new instance) — never relying on a full-table delete's cascade,
// since a surviving instance's row is no longer deleted on every write.
func upsertTaskInstanceTx(ctx context.Context, q *sqlcgen.Queries, sessionName, instanceName string, ts *contract.TaskState) error {
	recordJSON, err := marshalTaskRecord(ts)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q: %w", sessionName, instanceName, err)
	}
	named := ts.Name != ""
	id, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID:           newULID(),
		SessionName:  sessionName,
		InstanceName: instanceName,
		TaskID:       ts.TaskID,
		Scope:        ts.Scope,
		Status:       ts.Status,
		Sequence:     int64(ts.Seq),
		Resource:     nullString(ts.Resource),
		Named:        named,
		FinalizedAt:  formatTimeNull(ts.FinalizedAt),
		RecordJson:   recordJSON,
	})
	if err != nil {
		return fmt.Errorf("upsert task instance %q/%q: %w", sessionName, instanceName, err)
	}

	if err := q.DeleteTaskDoneWhenStateByInstanceID(ctx, id); err != nil {
		return fmt.Errorf("clear done_when %q/%q: %w", sessionName, instanceName, err)
	}
	if err := q.DeleteTaskDoneWhenJudgesByInstanceID(ctx, id); err != nil {
		return fmt.Errorf("clear done_when judges %q/%q: %w", sessionName, instanceName, err)
	}
	if ts.DoneWhen == nil {
		return nil
	}
	if err := insertDoneWhenTx(ctx, q, id, ts.DoneWhen); err != nil {
		return fmt.Errorf("insert done_when %q/%q: %w", sessionName, instanceName, err)
	}
	for leafID, judge := range ts.DoneWhen.Judges {
		if judge == nil {
			continue
		}
		if err := insertJudgeTx(ctx, q, id, leafID, judge); err != nil {
			return fmt.Errorf("insert done_when judge %q/%q/%q: %w", sessionName, instanceName, leafID, err)
		}
	}
	return nil
}

func insertDoneWhenTx(ctx context.Context, q *sqlcgen.Queries, taskInstanceID string, dw *contract.DoneWhenState) error {
	lastUnsatisfiedJSON, err := json.Marshal(dw.LastUnsatisfied)
	if err != nil {
		return fmt.Errorf("marshal last_unsatisfied: %w", err)
	}
	return q.InsertTaskDoneWhenState(ctx, sqlcgen.InsertTaskDoneWhenStateParams{
		TaskInstanceID:       taskInstanceID,
		HeartbeatTicks:       int64(dw.HeartbeatTicks),
		HeartbeatEscalations: int64(dw.HeartbeatEscalations),
		LastAction:           nullString(dw.LastAction),
		LastFingerprint:      nullString(dw.LastFingerprint),
		LastReason:           nullString(dw.LastReason),
		LastUnsatisfiedJson:  string(lastUnsatisfiedJSON),
		LastBody:             nullString(dw.LastBody),
		EscalatedAt:          formatTimeNull(dw.EscalatedAt),
		EscalateReason:       nullString(dw.EscalateReason),
	})
}

// insertJudgeTx never writes TargetSession/Instance: task_instance_id's own
// parent row is always the judged session/instance (verified: judge.go's
// one write site always stores a verdict on the same session/instance it
// names as TargetSession/Instance), so the persistence boundary derives
// them from the join instead of duplicating them as columns.
func insertJudgeTx(ctx context.Context, q *sqlcgen.Queries, taskInstanceID, leafID string, judge *contract.DoneWhenJudge) error {
	return q.InsertTaskDoneWhenJudge(ctx, sqlcgen.InsertTaskDoneWhenJudgeParams{
		TaskInstanceID: taskInstanceID,
		LeafID:         leafID,
		Action:         judge.Action,
		Reason:         judge.Reason,
		Revision:       judge.Revision,
		JudgeSession:   judge.ReviewerSession,
		JudgeWorkflow:  nullString(judge.ReviewerWorkflow),
		Relation:       judge.Relation,
		CreatedAt:      formatTime(judge.CreatedAt),
	})
}

func judgeFromRow(r sqlcgen.ListTaskDoneWhenJudgesForSessionRow) (*contract.DoneWhenJudge, error) {
	createdAt, err := parseTime(r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse judge %q/%q created_at: %w", r.TaskInstanceID, r.LeafID, err)
	}
	return &contract.DoneWhenJudge{
		LeafID:           r.LeafID,
		Action:           r.Action,
		Reason:           r.Reason,
		Revision:         r.Revision,
		TargetSession:    r.TargetSession,
		Instance:         r.TargetInstance,
		ReviewerSession:  r.JudgeSession,
		ReviewerWorkflow: r.JudgeWorkflow.String,
		Relation:         r.Relation,
		CreatedAt:        createdAt,
	}, nil
}

func doneWhenFromRow(dw sqlcgen.TaskDoneWhenState, judges map[string]*contract.DoneWhenJudge) (*contract.DoneWhenState, error) {
	var lastUnsatisfied []string
	if err := json.Unmarshal([]byte(dw.LastUnsatisfiedJson), &lastUnsatisfied); err != nil {
		return nil, fmt.Errorf("parse last_unsatisfied: %w", err)
	}
	escalatedAt, err := parseTimeNull(dw.EscalatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse escalated_at: %w", err)
	}
	return &contract.DoneWhenState{
		HeartbeatTicks:       int(dw.HeartbeatTicks),
		HeartbeatEscalations: int(dw.HeartbeatEscalations),
		LastAction:           dw.LastAction.String,
		LastFingerprint:      dw.LastFingerprint.String,
		LastReason:           dw.LastReason.String,
		LastUnsatisfied:      lastUnsatisfied,
		LastBody:             dw.LastBody.String,
		EscalatedAt:          escalatedAt,
		EscalateReason:       dw.EscalateReason.String,
		Judges:               judges,
	}, nil
}

// nodeInstancePayload is a node_instances row's record_json shape: a
// TaskState's fields with no relational column of their own on that table
// (scope, status, sequence, and finalized_at are columns instead; see
// marshalNodeInstanceRecord). Every field here carries omitempty/omitzero,
// so an instance with nothing beyond its columns serializes to "{}" rather
// than a page of zero-valued duplicates (e.g. "scope":"","status":"") that
// would read as a second, disagreeing authority to anyone inspecting the
// blob directly.
type nodeInstancePayload struct {
	TaskID        string                        `json:"task_id,omitempty"`
	Inputs        map[string]any                `json:"inputs,omitempty"`
	Outputs       map[string]any                `json:"outputs,omitempty"`
	Resource      string                        `json:"resource,omitempty"`
	Name          string                        `json:"name,omitempty"`
	Layers        []contract.LayerState         `json:"layers,omitempty"`
	State         map[string]any                `json:"state,omitempty"`
	Observed      *contract.ResourceObservation `json:"observed,omitempty"`
	DoneWhen      *contract.DoneWhenState       `json:"done_when,omitempty"`
	ExtraDoneWhen json.RawMessage               `json:"extra_done_when,omitempty"`
	SetupAt       time.Time                     `json:"setup_at,omitzero"`
	FailedAt      time.Time                     `json:"failed_at,omitzero"`
	CleanedAt     time.Time                     `json:"cleaned_at,omitzero"`
	Error         string                        `json:"error,omitempty"`
}

// marshalNodeInstanceRecord serializes every TaskState field not already
// carried by a relational column on node_instances (scope, status,
// sequence, finalized_at). Unlike a dynamic instance, a node instance's
// TaskID, Resource, Name, and DoneWhen (rare, and not relationally queried)
// all stay embedded here rather than split out.
func marshalNodeInstanceRecord(t *contract.TaskState) (string, error) {
	data, err := json.Marshal(nodeInstancePayload{
		TaskID:        t.TaskID,
		Inputs:        t.Inputs,
		Outputs:       t.Outputs,
		Resource:      t.Resource,
		Name:          t.Name,
		Layers:        t.Layers,
		State:         t.State,
		Observed:      t.Observed,
		DoneWhen:      t.DoneWhen,
		ExtraDoneWhen: t.ExtraDoneWhen,
		SetupAt:       t.SetupAt,
		FailedAt:      t.FailedAt,
		CleanedAt:     t.CleanedAt,
		Error:         t.Error,
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalNodeInstanceRecord(recordJSON string) (*contract.TaskState, error) {
	var p nodeInstancePayload
	if err := json.Unmarshal([]byte(recordJSON), &p); err != nil {
		return nil, err
	}
	return &contract.TaskState{
		TaskID:        p.TaskID,
		Inputs:        p.Inputs,
		Outputs:       p.Outputs,
		Resource:      p.Resource,
		Name:          p.Name,
		Layers:        p.Layers,
		State:         p.State,
		Observed:      p.Observed,
		DoneWhen:      p.DoneWhen,
		ExtraDoneWhen: p.ExtraDoneWhen,
		SetupAt:       p.SetupAt,
		FailedAt:      p.FailedAt,
		CleanedAt:     p.CleanedAt,
		Error:         p.Error,
	}, nil
}

// taskInstancePayload is a task_instances row's record_json shape: a
// TaskState's fields with no relational column (task_instances' own
// columns) and no dedicated table (task_done_when_states/judges own
// DoneWhen). See nodeInstancePayload for why every field is optional.
type taskInstancePayload struct {
	Inputs        map[string]any                `json:"inputs,omitempty"`
	Outputs       map[string]any                `json:"outputs,omitempty"`
	Layers        []contract.LayerState         `json:"layers,omitempty"`
	State         map[string]any                `json:"state,omitempty"`
	Observed      *contract.ResourceObservation `json:"observed,omitempty"`
	ExtraDoneWhen json.RawMessage               `json:"extra_done_when,omitempty"`
	SetupAt       time.Time                     `json:"setup_at,omitzero"`
	FailedAt      time.Time                     `json:"failed_at,omitzero"`
	CleanedAt     time.Time                     `json:"cleaned_at,omitzero"`
	Error         string                        `json:"error,omitempty"`
}

// marshalTaskRecord serializes every TaskState field not already carried by
// a relational column or the task_done_when_states/judges tables.
func marshalTaskRecord(t *contract.TaskState) (string, error) {
	data, err := json.Marshal(taskInstancePayload{
		Inputs:        t.Inputs,
		Outputs:       t.Outputs,
		Layers:        t.Layers,
		State:         t.State,
		Observed:      t.Observed,
		ExtraDoneWhen: t.ExtraDoneWhen,
		SetupAt:       t.SetupAt,
		FailedAt:      t.FailedAt,
		CleanedAt:     t.CleanedAt,
		Error:         t.Error,
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTaskRecord(recordJSON string) (*contract.TaskState, error) {
	var p taskInstancePayload
	if err := json.Unmarshal([]byte(recordJSON), &p); err != nil {
		return nil, err
	}
	return &contract.TaskState{
		Inputs:        p.Inputs,
		Outputs:       p.Outputs,
		Layers:        p.Layers,
		State:         p.State,
		Observed:      p.Observed,
		ExtraDoneWhen: p.ExtraDoneWhen,
		SetupAt:       p.SetupAt,
		FailedAt:      p.FailedAt,
		CleanedAt:     p.CleanedAt,
		Error:         p.Error,
	}, nil
}

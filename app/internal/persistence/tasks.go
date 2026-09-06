package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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
		ts.Resource = row.Resource
		ts.Name = row.NamedInstance

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

// writeTasksTx replaces every node-instance and task-instance row for
// sessionName: it deletes both tables' rows for the session (task_instances'
// delete cascades to task_done_when_states and task_done_when_judges via
// their id foreign key) and reinserts the current Tasks map split by
// Dynamic, all inside the caller's write transaction. It never touches
// another session's rows. Every dynamic instance gets a freshly minted id,
// so a cleanup followed by a new setup under the same instance_name is a
// distinct row with its own done_when/judge history.
func (db *DB) writeTasksTx(ctx context.Context, tx *sql.Tx, sessionName string, tasks map[string]*contract.TaskState) error {
	q := sqlcgen.New(tx)
	if err := q.DeleteNodeInstancesForSession(ctx, sessionName); err != nil {
		return fmt.Errorf("clear node instances for %q: %w", sessionName, err)
	}
	if err := q.DeleteTaskInstancesForSession(ctx, sessionName); err != nil {
		return fmt.Errorf("clear task instances for %q: %w", sessionName, err)
	}

	for key, ts := range tasks {
		if ts == nil {
			continue
		}
		if ts.Dynamic {
			if err := insertTaskInstanceTx(ctx, q, sessionName, key, ts); err != nil {
				return err
			}
			continue
		}
		if err := insertNodeInstanceTx(ctx, q, sessionName, key, ts); err != nil {
			return err
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
		RecordJson:  recordJSON,
	}); err != nil {
		return fmt.Errorf("insert node instance %q/%q: %w", sessionName, nodeID, err)
	}
	return nil
}

func insertTaskInstanceTx(ctx context.Context, q *sqlcgen.Queries, sessionName, instanceName string, ts *contract.TaskState) error {
	recordJSON, err := marshalTaskRecord(ts)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q: %w", sessionName, instanceName, err)
	}
	id := newULID()
	if err := q.InsertTaskInstance(ctx, sqlcgen.InsertTaskInstanceParams{
		ID:            id,
		SessionName:   sessionName,
		InstanceName:  instanceName,
		TaskID:        ts.TaskID,
		Scope:         ts.Scope,
		Status:        ts.Status,
		Sequence:      int64(ts.Seq),
		Resource:      ts.Resource,
		NamedInstance: ts.Name,
		RecordJson:    recordJSON,
	}); err != nil {
		return fmt.Errorf("insert task instance %q/%q: %w", sessionName, instanceName, err)
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
		LastAction:           dw.LastAction,
		LastFingerprint:      dw.LastFingerprint,
		LastReason:           dw.LastReason,
		LastUnsatisfiedJson:  string(lastUnsatisfiedJSON),
		LastBody:             dw.LastBody,
		EscalatedAt:          formatTime(dw.EscalatedAt),
		EscalateReason:       dw.EscalateReason,
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
		JudgeWorkflow:  judge.ReviewerWorkflow,
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
		ReviewerWorkflow: r.JudgeWorkflow,
		Relation:         r.Relation,
		CreatedAt:        createdAt,
	}, nil
}

func doneWhenFromRow(dw sqlcgen.TaskDoneWhenState, judges map[string]*contract.DoneWhenJudge) (*contract.DoneWhenState, error) {
	var lastUnsatisfied []string
	if err := json.Unmarshal([]byte(dw.LastUnsatisfiedJson), &lastUnsatisfied); err != nil {
		return nil, fmt.Errorf("parse last_unsatisfied: %w", err)
	}
	escalatedAt, err := parseTime(dw.EscalatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse escalated_at: %w", err)
	}
	return &contract.DoneWhenState{
		HeartbeatTicks:       int(dw.HeartbeatTicks),
		HeartbeatEscalations: int(dw.HeartbeatEscalations),
		LastAction:           dw.LastAction,
		LastFingerprint:      dw.LastFingerprint,
		LastReason:           dw.LastReason,
		LastUnsatisfied:      lastUnsatisfied,
		LastBody:             dw.LastBody,
		EscalatedAt:          escalatedAt,
		EscalateReason:       dw.EscalateReason,
		Judges:               judges,
	}, nil
}

// marshalNodeInstanceRecord serializes every TaskState field not already
// carried by a relational column on node_instances (scope, status,
// sequence). Unlike a dynamic instance, a node instance's TaskID, Resource,
// Name, and DoneWhen (rare, and not relationally queried) all stay embedded
// here rather than split out.
func marshalNodeInstanceRecord(t *contract.TaskState) (string, error) {
	clone := *t
	clone.Scope = ""
	clone.Status = ""
	clone.Seq = 0
	clone.Dynamic = false
	data, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalNodeInstanceRecord(recordJSON string) (*contract.TaskState, error) {
	var t contract.TaskState
	if err := json.Unmarshal([]byte(recordJSON), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// marshalTaskRecord serializes every TaskState field not already carried by
// a relational column or the task_done_when_states/judges tables.
func marshalTaskRecord(t *contract.TaskState) (string, error) {
	clone := *t
	clone.TaskID = ""
	clone.Scope = ""
	clone.Status = ""
	clone.Seq = 0
	clone.Dynamic = false
	clone.Resource = ""
	clone.Name = ""
	clone.DoneWhen = nil
	data, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTaskRecord(recordJSON string) (*contract.TaskState, error) {
	var t contract.TaskState
	if err := json.Unmarshal([]byte(recordJSON), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

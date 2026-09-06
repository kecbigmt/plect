package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// loadTasks assembles a session's Tasks map from task_instances joined (in
// Go, not SQL) with task_done_when and task_done_when_judges, so a judge
// verdict is read from its one authority rather than a copy folded into
// task_instances.record_json.
func loadTasks(ctx context.Context, q sqlcgen.DBTX, sessionName string) (map[string]*contract.TaskState, error) {
	queries := sqlcgen.New(q)

	rows, err := queries.ListTaskInstances(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list task instances for %q: %w", sessionName, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	doneWhenRows, err := queries.ListTaskDoneWhen(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list done_when for %q: %w", sessionName, err)
	}
	doneWhenByInstance := make(map[string]sqlcgen.TaskDoneWhen, len(doneWhenRows))
	for _, r := range doneWhenRows {
		doneWhenByInstance[r.InstanceName] = r
	}

	judgeRows, err := queries.ListTaskDoneWhenJudges(ctx, sessionName)
	if err != nil {
		return nil, fmt.Errorf("list done_when judges for %q: %w", sessionName, err)
	}
	judgesByInstance := make(map[string]map[string]*contract.DoneWhenJudge)
	for _, r := range judgeRows {
		judge, err := judgeFromRow(r)
		if err != nil {
			return nil, err
		}
		m := judgesByInstance[r.InstanceName]
		if m == nil {
			m = make(map[string]*contract.DoneWhenJudge)
			judgesByInstance[r.InstanceName] = m
		}
		m[r.LeafID] = judge
	}

	tasks := make(map[string]*contract.TaskState, len(rows))
	for _, row := range rows {
		ts, err := unmarshalTaskRecord(row.RecordJson)
		if err != nil {
			return nil, fmt.Errorf("parse task %q/%q record: %w", sessionName, row.InstanceName, err)
		}
		ts.TaskID = row.TaskID
		ts.Scope = row.Scope
		ts.Status = row.Status
		ts.Seq = int(row.Sequence)
		ts.Dynamic = row.Dynamic != 0
		ts.Resource = row.Resource
		ts.Name = row.NamedInstance

		if dw, ok := doneWhenByInstance[row.InstanceName]; ok {
			doneWhen, err := doneWhenFromRow(dw, judgesByInstance[row.InstanceName])
			if err != nil {
				return nil, fmt.Errorf("parse done_when %q/%q: %w", sessionName, row.InstanceName, err)
			}
			ts.DoneWhen = doneWhen
		}
		tasks[row.InstanceName] = ts
	}
	return tasks, nil
}

// writeTasksTx replaces every task/completion row for sessionName: it
// deletes task_instances for the session (which cascades to
// task_done_when and task_done_when_judges via their foreign keys) and
// reinserts the current Tasks map, all inside the caller's write
// transaction. It never touches another session's rows.
func (db *DB) writeTasksTx(ctx context.Context, tx *sql.Tx, sessionName string, tasks map[string]*contract.TaskState) error {
	q := sqlcgen.New(tx)
	if err := q.DeleteTaskInstancesForSession(ctx, sessionName); err != nil {
		return fmt.Errorf("clear task instances for %q: %w", sessionName, err)
	}

	for instanceName, ts := range tasks {
		if ts == nil {
			continue
		}
		if err := insertTaskInstanceTx(ctx, q, sessionName, instanceName, ts); err != nil {
			return err
		}
	}
	return nil
}

func insertTaskInstanceTx(ctx context.Context, q *sqlcgen.Queries, sessionName, instanceName string, ts *contract.TaskState) error {
	recordJSON, err := marshalTaskRecord(ts)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q: %w", sessionName, instanceName, err)
	}
	var dynamic int64
	if ts.Dynamic {
		dynamic = 1
	}
	if err := q.InsertTaskInstance(ctx, sqlcgen.InsertTaskInstanceParams{
		SessionName:   sessionName,
		InstanceName:  instanceName,
		TaskID:        ts.TaskID,
		Scope:         ts.Scope,
		Status:        ts.Status,
		Sequence:      int64(ts.Seq),
		Dynamic:       dynamic,
		Resource:      ts.Resource,
		NamedInstance: ts.Name,
		RecordJson:    recordJSON,
	}); err != nil {
		return fmt.Errorf("insert task instance %q/%q: %w", sessionName, instanceName, err)
	}

	if ts.DoneWhen == nil {
		return nil
	}
	if err := insertDoneWhenTx(ctx, q, sessionName, instanceName, ts.DoneWhen); err != nil {
		return err
	}
	for leafID, judge := range ts.DoneWhen.Judges {
		if judge == nil {
			continue
		}
		if err := insertJudgeTx(ctx, q, sessionName, instanceName, leafID, judge); err != nil {
			return err
		}
	}
	return nil
}

func insertDoneWhenTx(ctx context.Context, q *sqlcgen.Queries, sessionName, instanceName string, dw *contract.DoneWhenState) error {
	lastUnsatisfiedJSON, err := json.Marshal(dw.LastUnsatisfied)
	if err != nil {
		return fmt.Errorf("marshal done_when %q/%q last_unsatisfied: %w", sessionName, instanceName, err)
	}
	if err := q.InsertTaskDoneWhen(ctx, sqlcgen.InsertTaskDoneWhenParams{
		SessionName:          sessionName,
		InstanceName:         instanceName,
		HeartbeatTicks:       int64(dw.HeartbeatTicks),
		HeartbeatEscalations: int64(dw.HeartbeatEscalations),
		LastAction:           dw.LastAction,
		LastFingerprint:      dw.LastFingerprint,
		LastReason:           dw.LastReason,
		LastUnsatisfiedJson:  string(lastUnsatisfiedJSON),
		LastBody:             dw.LastBody,
		EscalatedAt:          formatTime(dw.EscalatedAt),
		EscalateReason:       dw.EscalateReason,
	}); err != nil {
		return fmt.Errorf("insert done_when %q/%q: %w", sessionName, instanceName, err)
	}
	return nil
}

func insertJudgeTx(ctx context.Context, q *sqlcgen.Queries, sessionName, instanceName, leafID string, judge *contract.DoneWhenJudge) error {
	if err := q.InsertTaskDoneWhenJudge(ctx, sqlcgen.InsertTaskDoneWhenJudgeParams{
		SessionName:      sessionName,
		InstanceName:     instanceName,
		LeafID:           leafID,
		Action:           judge.Action,
		Reason:           judge.Reason,
		Revision:         judge.Revision,
		TargetSession:    judge.TargetSession,
		TargetInstance:   judge.Instance,
		ReviewerSession:  judge.ReviewerSession,
		ReviewerWorkflow: judge.ReviewerWorkflow,
		Relation:         judge.Relation,
		CreatedAt:        formatTime(judge.CreatedAt),
	}); err != nil {
		return fmt.Errorf("insert done_when judge %q/%q/%q: %w", sessionName, instanceName, leafID, err)
	}
	return nil
}

func judgeFromRow(r sqlcgen.TaskDoneWhenJudge) (*contract.DoneWhenJudge, error) {
	createdAt, err := parseTime(r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse judge %q/%q/%q created_at: %w", r.SessionName, r.InstanceName, r.LeafID, err)
	}
	return &contract.DoneWhenJudge{
		LeafID:           r.LeafID,
		Action:           r.Action,
		Reason:           r.Reason,
		Revision:         r.Revision,
		TargetSession:    r.TargetSession,
		Instance:         r.TargetInstance,
		ReviewerSession:  r.ReviewerSession,
		ReviewerWorkflow: r.ReviewerWorkflow,
		Relation:         r.Relation,
		CreatedAt:        createdAt,
	}, nil
}

func doneWhenFromRow(dw sqlcgen.TaskDoneWhen, judges map[string]*contract.DoneWhenJudge) (*contract.DoneWhenState, error) {
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

// marshalTaskRecord serializes every TaskState field not already carried by
// a relational column or the task_done_when/judges tables.
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

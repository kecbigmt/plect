package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// loadTasks assembles a session's Nodes (from node_instances) and Tasks
// (from task_instances) maps, joined in Go with their layer/done_when/judge
// child tables. sessionID is the session's surrogate id, not its name.
func loadTasks(ctx context.Context, q sqlcgen.DBTX, sessionID string) (nodes, tasks map[string]*contract.TaskState, err error) {
	queries := sqlcgen.New(q)

	nodeRows, err := queries.ListNodeInstances(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list node instances for %q: %w", sessionID, err)
	}
	instanceRows, err := queries.ListTaskInstances(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list task instances for %q: %w", sessionID, err)
	}
	if len(nodeRows) == 0 && len(instanceRows) == 0 {
		return nil, nil, nil
	}

	nodeLayerRows, err := queries.ListNodeInstanceLayers(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list node instance layers for %q: %w", sessionID, err)
	}
	nodeLayersByNodeID := map[string][]sqlcgen.NodeInstanceLayer{}
	for _, r := range nodeLayerRows {
		nodeLayersByNodeID[r.NodeID] = append(nodeLayersByNodeID[r.NodeID], r)
	}

	taskLayerRows, err := queries.ListTaskInstanceLayersForSession(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list task instance layers for %q: %w", sessionID, err)
	}
	taskLayersByInstanceID := map[string][]sqlcgen.TaskInstanceLayer{}
	for _, r := range taskLayerRows {
		taskLayersByInstanceID[r.TaskInstanceID] = append(taskLayersByInstanceID[r.TaskInstanceID], r)
	}

	doneWhenRows, err := queries.ListTaskDoneWhenStatesForSession(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list done_when states for %q: %w", sessionID, err)
	}
	doneWhenByInstanceID := make(map[string]sqlcgen.TaskDoneWhenState, len(doneWhenRows))
	for _, r := range doneWhenRows {
		doneWhenByInstanceID[r.TaskInstanceID] = r
	}

	unsatisfiedRows, err := queries.ListTaskDoneWhenUnsatisfiedItemsForSession(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list done_when unsatisfied items for %q: %w", sessionID, err)
	}
	unsatisfiedByInstanceID := map[string][]string{}
	for _, r := range unsatisfiedRows {
		unsatisfiedByInstanceID[r.TaskInstanceID] = append(unsatisfiedByInstanceID[r.TaskInstanceID], r.Item)
	}

	judgeRows, err := queries.ListTaskDoneWhenJudgesForSession(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("list done_when judges for %q: %w", sessionID, err)
	}
	judgesByInstanceID := make(map[string]map[string]*contract.DoneWhenJudge)
	for _, r := range judgeRows {
		judge, err := judgeFromRow(r)
		if err != nil {
			return nil, nil, err
		}
		m := judgesByInstanceID[r.TaskInstanceID]
		if m == nil {
			m = make(map[string]*contract.DoneWhenJudge)
			judgesByInstanceID[r.TaskInstanceID] = m
		}
		m[r.LeafID] = judge
	}

	nodes = make(map[string]*contract.TaskState, len(nodeRows))
	for _, row := range nodeRows {
		ts, err := nodeInstanceFromRow(row)
		if err != nil {
			return nil, nil, fmt.Errorf("parse node instance %q/%q: %w", sessionID, row.NodeID, err)
		}
		layers, err := layersFromNodeRows(nodeLayersByNodeID[row.NodeID])
		if err != nil {
			return nil, nil, fmt.Errorf("parse node instance %q/%q layers: %w", sessionID, row.NodeID, err)
		}
		ts.Layers = layers
		nodes[row.NodeID] = ts
	}
	tasks = make(map[string]*contract.TaskState, len(instanceRows))
	for _, row := range instanceRows {
		ts, err := taskInstanceFromRow(row)
		if err != nil {
			return nil, nil, fmt.Errorf("parse task instance %q/%q: %w", sessionID, row.InstanceName, err)
		}
		layers, err := layersFromTaskRows(taskLayersByInstanceID[row.ID])
		if err != nil {
			return nil, nil, fmt.Errorf("parse task instance %q/%q layers: %w", sessionID, row.InstanceName, err)
		}
		ts.Layers = layers

		if dw, ok := doneWhenByInstanceID[row.ID]; ok {
			doneWhen, err := doneWhenFromRow(dw, unsatisfiedByInstanceID[row.ID], judgesByInstanceID[row.ID])
			if err != nil {
				return nil, nil, fmt.Errorf("parse done_when %q/%q: %w", sessionID, row.InstanceName, err)
			}
			ts.DoneWhen = doneWhen
		}
		tasks[row.InstanceName] = ts
	}
	return nodes, tasks, nil
}

// loadTasksBatch is loadTasks generalized over many sessions at once,
// batched by session_id via one IN-clause query per original query rather
// than one query per session. A session with a row in either input table
// gets both maps pre-seeded (possibly empty), matching loadTasks' own
// per-session guard: reflect.DeepEqual treats nil and an empty map as
// different, which the batched-vs-GetSession equivalence test caught.
// Layer rows key on (session_id, node_id), since node_id alone is not
// globally unique the way a task_instances.id is.
func loadTasksBatch(ctx context.Context, q sqlcgen.DBTX, sessionIDs []string) (nodesBySession, tasksBySession map[string]map[string]*contract.TaskState, err error) {
	if len(sessionIDs) == 0 {
		return nil, nil, nil
	}
	queries := sqlcgen.New(q)

	nodeRows, err := queries.ListNodeInstancesForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list node instances: %w", err)
	}
	instanceRows, err := queries.ListTaskInstancesForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list task instances: %w", err)
	}
	if len(nodeRows) == 0 && len(instanceRows) == 0 {
		return nil, nil, nil
	}

	type nodeKey struct{ sessionID, nodeID string }
	nodeLayerRows, err := queries.ListNodeInstanceLayersForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list node instance layers: %w", err)
	}
	nodeLayersByKey := map[nodeKey][]sqlcgen.NodeInstanceLayer{}
	for _, r := range nodeLayerRows {
		key := nodeKey{r.SessionID, r.NodeID}
		nodeLayersByKey[key] = append(nodeLayersByKey[key], r)
	}

	taskLayerRows, err := queries.ListTaskInstanceLayersForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list task instance layers: %w", err)
	}
	taskLayersByInstanceID := map[string][]sqlcgen.TaskInstanceLayer{}
	for _, r := range taskLayerRows {
		taskLayersByInstanceID[r.TaskInstanceID] = append(taskLayersByInstanceID[r.TaskInstanceID], r)
	}

	doneWhenRows, err := queries.ListTaskDoneWhenStatesForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list done_when states: %w", err)
	}
	doneWhenByInstanceID := make(map[string]sqlcgen.TaskDoneWhenState, len(doneWhenRows))
	for _, r := range doneWhenRows {
		doneWhenByInstanceID[r.TaskInstanceID] = r
	}

	unsatisfiedRows, err := queries.ListTaskDoneWhenUnsatisfiedItemsForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list done_when unsatisfied items: %w", err)
	}
	unsatisfiedByInstanceID := map[string][]string{}
	for _, r := range unsatisfiedRows {
		unsatisfiedByInstanceID[r.TaskInstanceID] = append(unsatisfiedByInstanceID[r.TaskInstanceID], r.Item)
	}

	judgeRows, err := queries.ListTaskDoneWhenJudgesForSessions(ctx, sessionIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("list done_when judges: %w", err)
	}
	judgesByInstanceID := make(map[string]map[string]*contract.DoneWhenJudge)
	for _, r := range judgeRows {
		judge, err := judgeFromRow(r)
		if err != nil {
			return nil, nil, err
		}
		m := judgesByInstanceID[r.TaskInstanceID]
		if m == nil {
			m = make(map[string]*contract.DoneWhenJudge)
			judgesByInstanceID[r.TaskInstanceID] = m
		}
		m[r.LeafID] = judge
	}

	// A session with a row in either table gets both maps pre-seeded
	// (possibly empty), matching loadTasks' own per-session guard: it makes
	// both maps unconditionally as soon as either input table is non-empty
	// for that session, rather than making only the one with rows.
	known := make(map[string]bool)
	for _, row := range nodeRows {
		known[row.SessionID] = true
	}
	for _, row := range instanceRows {
		known[row.SessionID] = true
	}
	nodesBySession = make(map[string]map[string]*contract.TaskState, len(known))
	tasksBySession = make(map[string]map[string]*contract.TaskState, len(known))
	for sid := range known {
		nodesBySession[sid] = make(map[string]*contract.TaskState)
		tasksBySession[sid] = make(map[string]*contract.TaskState)
	}

	for _, row := range nodeRows {
		ts, err := nodeInstanceFromRow(row)
		if err != nil {
			return nil, nil, fmt.Errorf("parse node instance %q/%q: %w", row.SessionID, row.NodeID, err)
		}
		layers, err := layersFromNodeRows(nodeLayersByKey[nodeKey{row.SessionID, row.NodeID}])
		if err != nil {
			return nil, nil, fmt.Errorf("parse node instance %q/%q layers: %w", row.SessionID, row.NodeID, err)
		}
		ts.Layers = layers
		nodesBySession[row.SessionID][row.NodeID] = ts
	}

	for _, row := range instanceRows {
		ts, err := taskInstanceFromRow(row)
		if err != nil {
			return nil, nil, fmt.Errorf("parse task instance %q/%q: %w", row.SessionID, row.InstanceName, err)
		}
		layers, err := layersFromTaskRows(taskLayersByInstanceID[row.ID])
		if err != nil {
			return nil, nil, fmt.Errorf("parse task instance %q/%q layers: %w", row.SessionID, row.InstanceName, err)
		}
		ts.Layers = layers

		if dw, ok := doneWhenByInstanceID[row.ID]; ok {
			doneWhen, err := doneWhenFromRow(dw, unsatisfiedByInstanceID[row.ID], judgesByInstanceID[row.ID])
			if err != nil {
				return nil, nil, fmt.Errorf("parse done_when %q/%q: %w", row.SessionID, row.InstanceName, err)
			}
			ts.DoneWhen = doneWhen
		}
		tasks := tasksBySession[row.SessionID]
		tasks[row.InstanceName] = ts
	}
	return nodesBySession, tasksBySession, nil
}

// writeTasksTx replaces every node-instance row for sessionID (that table
// has no identity worth preserving across a write) from nodes, and
// reconciles task_instances against tasks instead: each current dynamic
// instance is upserted (preserving its id across an ordinary update; see
// UpsertTaskInstance), and any instance_name no longer present is then
// explicitly deleted, which is what mints a fresh id on a later cleanup +
// setup under the same name. It never touches another session's rows.
func (db *DB) writeTasksTx(ctx context.Context, tx *sql.Tx, sessionID string, nodes, tasks map[string]*contract.TaskState) error {
	q := sqlcgen.New(tx)
	if err := q.DeleteNodeInstancesForSession(ctx, sessionID); err != nil {
		return fmt.Errorf("clear node instances for %q: %w", sessionID, err)
	}
	for key, ts := range nodes {
		if ts == nil {
			continue
		}
		if err := insertNodeInstanceTx(ctx, q, sessionID, key, ts); err != nil {
			return err
		}
	}

	existing, err := q.ListTaskInstances(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list existing task instances for %q: %w", sessionID, err)
	}
	remaining := make(map[string]bool, len(existing))
	for _, row := range existing {
		remaining[row.InstanceName] = true
	}

	for key, ts := range tasks {
		if ts == nil {
			continue
		}
		delete(remaining, key)
		if err := upsertTaskInstanceTx(ctx, q, sessionID, key, ts); err != nil {
			return err
		}
	}

	for instanceName := range remaining {
		if err := q.DeleteTaskInstanceByName(ctx, sqlcgen.DeleteTaskInstanceByNameParams{
			SessionID:    sessionID,
			InstanceName: instanceName,
		}); err != nil {
			return fmt.Errorf("delete task instance %q/%q: %w", sessionID, instanceName, err)
		}
	}
	return nil
}

// resourceObservationColumns/observationFromColumns split a
// *ResourceObservation into its two columns (see schema.sql's
// node_instances comment for why At gets a real timestamp column instead
// of living inside the opaque state blob).
func resourceObservationColumns(o *contract.ResourceObservation) (stateJSON, at sql.NullString, err error) {
	if o == nil {
		return sql.NullString{}, sql.NullString{}, nil
	}
	stateJSON, err = marshalJSONMap(o.State)
	if err != nil {
		return sql.NullString{}, sql.NullString{}, err
	}
	return stateJSON, formatTimeNull(o.At), nil
}

func observationFromColumns(stateJSON, at sql.NullString) (*contract.ResourceObservation, error) {
	if !stateJSON.Valid && !at.Valid {
		return nil, nil
	}
	state, err := unmarshalJSONMap(stateJSON)
	if err != nil {
		return nil, err
	}
	atTime, err := parseTimeNull(at)
	if err != nil {
		return nil, err
	}
	return &contract.ResourceObservation{State: state, At: atTime}, nil
}

// nullRawJSON/rawJSONFromColumn convert TaskState.ExtraDoneWhen
// (json.RawMessage, already-encoded JSON text) to and from a nullable
// column, without re-marshaling its contents.
func nullRawJSON(raw json.RawMessage) sql.NullString {
	if len(raw) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(raw), Valid: true}
}

func rawJSONFromColumn(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

func insertNodeInstanceTx(ctx context.Context, q *sqlcgen.Queries, sessionID, nodeID string, ts *contract.TaskState) error {
	inputsJSON, err := marshalJSONMap(ts.Inputs)
	if err != nil {
		return fmt.Errorf("marshal node instance %q/%q inputs: %w", sessionID, nodeID, err)
	}
	outputsJSON, err := marshalJSONMap(ts.Outputs)
	if err != nil {
		return fmt.Errorf("marshal node instance %q/%q outputs: %w", sessionID, nodeID, err)
	}
	stateJSON, err := marshalJSONMap(ts.State)
	if err != nil {
		return fmt.Errorf("marshal node instance %q/%q state: %w", sessionID, nodeID, err)
	}
	observationJSON, observedAt, err := resourceObservationColumns(ts.Observed)
	if err != nil {
		return fmt.Errorf("marshal node instance %q/%q observed: %w", sessionID, nodeID, err)
	}
	doneWhenJSON, err := marshalJSONValue(ts.DoneWhen)
	if err != nil {
		return fmt.Errorf("marshal node instance %q/%q done_when: %w", sessionID, nodeID, err)
	}
	if err := q.InsertNodeInstance(ctx, sqlcgen.InsertNodeInstanceParams{
		SessionID:               sessionID,
		NodeID:                  nodeID,
		TaskID:                  nullString(ts.TaskID),
		Name:                    nullString(ts.Name),
		Scope:                   ts.Scope,
		Status:                  ts.Status,
		Sequence:                int64(ts.Seq),
		Resource:                nullString(ts.Resource),
		InputsJson:              inputsJSON,
		OutputsJson:             outputsJSON,
		StateJson:               stateJSON,
		ResourceObservationJson: observationJSON,
		ResourceObservedAt:      observedAt,
		DoneWhenJson:            doneWhenJSON,
		ExtraDoneWhenJson:       nullRawJSON(ts.ExtraDoneWhen),
		Error:                   nullString(ts.Error),
		SetupAt:                 formatTimeNull(ts.SetupAt),
		FailedAt:                formatTimeNull(ts.FailedAt),
		CleanedAt:               formatTimeNull(ts.CleanedAt),
		FinalizedAt:             formatTimeNull(ts.FinalizedAt),
	}); err != nil {
		return fmt.Errorf("insert node instance %q/%q: %w", sessionID, nodeID, err)
	}
	return insertNodeInstanceLayersTx(ctx, q, sessionID, nodeID, ts.Layers)
}

func nodeInstanceFromRow(row sqlcgen.NodeInstance) (*contract.TaskState, error) {
	inputs, err := unmarshalJSONMap(row.InputsJson)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	outputs, err := unmarshalJSONMap(row.OutputsJson)
	if err != nil {
		return nil, fmt.Errorf("outputs: %w", err)
	}
	state, err := unmarshalJSONMap(row.StateJson)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	observed, err := observationFromColumns(row.ResourceObservationJson, row.ResourceObservedAt)
	if err != nil {
		return nil, fmt.Errorf("observed: %w", err)
	}
	var doneWhen *contract.DoneWhenState
	if err := unmarshalJSONValue(row.DoneWhenJson, &doneWhen); err != nil {
		return nil, fmt.Errorf("done_when: %w", err)
	}
	setupAt, err := parseTimeNull(row.SetupAt)
	if err != nil {
		return nil, fmt.Errorf("setup_at: %w", err)
	}
	failedAt, err := parseTimeNull(row.FailedAt)
	if err != nil {
		return nil, fmt.Errorf("failed_at: %w", err)
	}
	cleanedAt, err := parseTimeNull(row.CleanedAt)
	if err != nil {
		return nil, fmt.Errorf("cleaned_at: %w", err)
	}
	finalizedAt, err := parseTimeNull(row.FinalizedAt)
	if err != nil {
		return nil, fmt.Errorf("finalized_at: %w", err)
	}
	return &contract.TaskState{
		TaskID:        row.TaskID.String,
		Name:          row.Name.String,
		Scope:         row.Scope,
		Status:        row.Status,
		Seq:           int(row.Sequence),
		Resource:      row.Resource.String,
		Inputs:        inputs,
		Outputs:       outputs,
		State:         state,
		Observed:      observed,
		DoneWhen:      doneWhen,
		ExtraDoneWhen: rawJSONFromColumn(row.ExtraDoneWhenJson),
		Error:         row.Error.String,
		SetupAt:       setupAt,
		FailedAt:      failedAt,
		CleanedAt:     cleanedAt,
		FinalizedAt:   finalizedAt,
	}, nil
}

// upsertTaskInstanceTx upserts the instance row, then replaces its
// layer/done_when/judge/unsatisfied-item rows keyed by whichever id the
// upsert reports (the preserved id on an ordinary update, or the freshly
// minted one on a genuinely new instance) — never relying on a full-table
// delete's cascade, since a surviving instance's row is no longer deleted
// on every write.
func upsertTaskInstanceTx(ctx context.Context, q *sqlcgen.Queries, sessionID, instanceName string, ts *contract.TaskState) error {
	inputsJSON, err := marshalJSONMap(ts.Inputs)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q inputs: %w", sessionID, instanceName, err)
	}
	outputsJSON, err := marshalJSONMap(ts.Outputs)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q outputs: %w", sessionID, instanceName, err)
	}
	stateJSON, err := marshalJSONMap(ts.State)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q state: %w", sessionID, instanceName, err)
	}
	observationJSON, observedAt, err := resourceObservationColumns(ts.Observed)
	if err != nil {
		return fmt.Errorf("marshal task %q/%q observed: %w", sessionID, instanceName, err)
	}
	named := ts.Name != ""
	id, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID:                      newULID(),
		SessionID:               sessionID,
		InstanceName:            instanceName,
		TaskID:                  ts.TaskID,
		Scope:                   ts.Scope,
		Status:                  ts.Status,
		Sequence:                int64(ts.Seq),
		Resource:                nullString(ts.Resource),
		Named:                   named,
		InputsJson:              inputsJSON,
		OutputsJson:             outputsJSON,
		StateJson:               stateJSON,
		ResourceObservationJson: observationJSON,
		ResourceObservedAt:      observedAt,
		ExtraDoneWhenJson:       nullRawJSON(ts.ExtraDoneWhen),
		Error:                   nullString(ts.Error),
		SetupAt:                 formatTimeNull(ts.SetupAt),
		FailedAt:                formatTimeNull(ts.FailedAt),
		CleanedAt:               formatTimeNull(ts.CleanedAt),
		FinalizedAt:             formatTimeNull(ts.FinalizedAt),
	})
	if err != nil {
		return fmt.Errorf("upsert task instance %q/%q: %w", sessionID, instanceName, err)
	}

	// Unlike node_instances (whose row is deleted outright and its layers
	// cascade with it), a surviving task instance's row is preserved across
	// an ordinary update, so its layer rows need an explicit replace rather
	// than relying on a delete this write never does.
	if err := q.DeleteTaskInstanceLayersByInstanceID(ctx, id); err != nil {
		return fmt.Errorf("clear layers %q/%q: %w", sessionID, instanceName, err)
	}
	if err := insertTaskInstanceLayersTx(ctx, q, id, ts.Layers); err != nil {
		return err
	}

	if err := q.DeleteTaskDoneWhenStateByInstanceID(ctx, id); err != nil {
		return fmt.Errorf("clear done_when %q/%q: %w", sessionID, instanceName, err)
	}
	if err := q.DeleteTaskDoneWhenUnsatisfiedItemsByInstanceID(ctx, id); err != nil {
		return fmt.Errorf("clear done_when unsatisfied items %q/%q: %w", sessionID, instanceName, err)
	}
	if err := q.DeleteTaskDoneWhenJudgesByInstanceID(ctx, id); err != nil {
		return fmt.Errorf("clear done_when judges %q/%q: %w", sessionID, instanceName, err)
	}
	if ts.DoneWhen == nil {
		return nil
	}
	if err := insertDoneWhenTx(ctx, q, id, ts.DoneWhen); err != nil {
		return fmt.Errorf("insert done_when %q/%q: %w", sessionID, instanceName, err)
	}
	for leafID, judge := range ts.DoneWhen.Judges {
		if judge == nil {
			continue
		}
		if err := insertJudgeTx(ctx, q, id, leafID, judge); err != nil {
			return fmt.Errorf("insert done_when judge %q/%q/%q: %w", sessionID, instanceName, leafID, err)
		}
	}
	return nil
}

func taskInstanceFromRow(row sqlcgen.TaskInstance) (*contract.TaskState, error) {
	inputs, err := unmarshalJSONMap(row.InputsJson)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	outputs, err := unmarshalJSONMap(row.OutputsJson)
	if err != nil {
		return nil, fmt.Errorf("outputs: %w", err)
	}
	state, err := unmarshalJSONMap(row.StateJson)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	observed, err := observationFromColumns(row.ResourceObservationJson, row.ResourceObservedAt)
	if err != nil {
		return nil, fmt.Errorf("observed: %w", err)
	}
	setupAt, err := parseTimeNull(row.SetupAt)
	if err != nil {
		return nil, fmt.Errorf("setup_at: %w", err)
	}
	failedAt, err := parseTimeNull(row.FailedAt)
	if err != nil {
		return nil, fmt.Errorf("failed_at: %w", err)
	}
	cleanedAt, err := parseTimeNull(row.CleanedAt)
	if err != nil {
		return nil, fmt.Errorf("cleaned_at: %w", err)
	}
	finalizedAt, err := parseTimeNull(row.FinalizedAt)
	if err != nil {
		return nil, fmt.Errorf("finalized_at: %w", err)
	}
	ts := &contract.TaskState{
		TaskID:        row.TaskID,
		Scope:         row.Scope,
		Status:        row.Status,
		Seq:           int(row.Sequence),
		Resource:      row.Resource.String,
		Inputs:        inputs,
		Outputs:       outputs,
		State:         state,
		Observed:      observed,
		ExtraDoneWhen: rawJSONFromColumn(row.ExtraDoneWhenJson),
		Error:         row.Error.String,
		SetupAt:       setupAt,
		FailedAt:      failedAt,
		CleanedAt:     cleanedAt,
		FinalizedAt:   finalizedAt,
	}
	if row.Named {
		ts.Name = row.InstanceName
	}
	return ts, nil
}

func insertDoneWhenTx(ctx context.Context, q *sqlcgen.Queries, taskInstanceID string, dw *contract.DoneWhenState) error {
	if err := q.InsertTaskDoneWhenState(ctx, sqlcgen.InsertTaskDoneWhenStateParams{
		TaskInstanceID:       taskInstanceID,
		HeartbeatTicks:       int64(dw.HeartbeatTicks),
		HeartbeatEscalations: int64(dw.HeartbeatEscalations),
		LastAction:           nullString(dw.LastAction),
		LastFingerprint:      nullString(dw.LastFingerprint),
		LastReason:           nullString(dw.LastReason),
		LastBody:             nullString(dw.LastBody),
		EscalatedAt:          formatTimeNull(dw.EscalatedAt),
		EscalateReason:       nullString(dw.EscalateReason),
	}); err != nil {
		return err
	}
	for i, item := range dw.LastUnsatisfied {
		if err := q.InsertTaskDoneWhenUnsatisfiedItem(ctx, sqlcgen.InsertTaskDoneWhenUnsatisfiedItemParams{
			TaskInstanceID: taskInstanceID,
			Position:       int64(i),
			Item:           item,
		}); err != nil {
			return fmt.Errorf("insert unsatisfied item %d: %w", i, err)
		}
	}
	return nil
}

func insertJudgeTx(ctx context.Context, q *sqlcgen.Queries, taskInstanceID, leafID string, judge *contract.DoneWhenJudge) error {
	return q.InsertTaskDoneWhenJudge(ctx, sqlcgen.InsertTaskDoneWhenJudgeParams{
		TaskInstanceID: taskInstanceID,
		LeafID:         leafID,
		Action:         judge.Action,
		Reason:         judge.Reason,
		Revision:       judge.Revision,
		JudgeSession:   judge.JudgeSession,
		JudgeWorkflow:  nullString(judge.JudgeWorkflow),
		Relation:       judge.Relation,
		CreatedAt:      formatTime(judge.CreatedAt),
	})
}

func judgeFromRow(r sqlcgen.TaskDoneWhenJudge) (*contract.DoneWhenJudge, error) {
	createdAt, err := parseTime(r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse judge %q/%q created_at: %w", r.TaskInstanceID, r.LeafID, err)
	}
	return &contract.DoneWhenJudge{
		LeafID:        r.LeafID,
		Action:        r.Action,
		Reason:        r.Reason,
		Revision:      r.Revision,
		JudgeSession:  r.JudgeSession,
		JudgeWorkflow: r.JudgeWorkflow.String,
		Relation:      r.Relation,
		CreatedAt:     createdAt,
	}, nil
}

func doneWhenFromRow(dw sqlcgen.TaskDoneWhenState, unsatisfied []string, judges map[string]*contract.DoneWhenJudge) (*contract.DoneWhenState, error) {
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
		LastUnsatisfied:      unsatisfied,
		LastBody:             dw.LastBody.String,
		EscalatedAt:          escalatedAt,
		EscalateReason:       dw.EscalateReason.String,
		Judges:               judges,
	}, nil
}

// Layers

func insertNodeInstanceLayersTx(ctx context.Context, q *sqlcgen.Queries, sessionID, nodeID string, layers []contract.LayerState) error {
	for i, l := range layers {
		inputsJSON, err := marshalJSONMap(l.Inputs)
		if err != nil {
			return fmt.Errorf("marshal layer %d inputs: %w", i, err)
		}
		localsJSON, err := marshalJSONMap(l.Locals)
		if err != nil {
			return fmt.Errorf("marshal layer %d locals: %w", i, err)
		}
		outputsJSON, err := marshalJSONMap(l.Outputs)
		if err != nil {
			return fmt.Errorf("marshal layer %d outputs: %w", i, err)
		}
		envJSON, err := marshalEnv(l.Env)
		if err != nil {
			return fmt.Errorf("marshal layer %d env: %w", i, err)
		}
		if err := q.InsertNodeInstanceLayer(ctx, sqlcgen.InsertNodeInstanceLayerParams{
			SessionID:            sessionID,
			NodeID:               nodeID,
			Position:             int64(i),
			EffectID:             l.EffectID,
			Status:               l.Status,
			InputsJson:           inputsJSON,
			LocalsJson:           localsJSON,
			OutputsJson:          outputsJSON,
			EnvJson:              envJSON,
			HeartbeatTicks:       nullInt(l.HeartbeatTicks),
			HeartbeatEscalations: nullInt(l.HeartbeatEscalations),
			SetupAt:              formatTimeNull(l.SetupAt),
			FailedAt:             formatTimeNull(l.FailedAt),
			CleanedAt:            formatTimeNull(l.CleanedAt),
			Error:                nullString(l.Error),
		}); err != nil {
			return fmt.Errorf("insert node instance layer %d: %w", i, err)
		}
	}
	return nil
}

func insertTaskInstanceLayersTx(ctx context.Context, q *sqlcgen.Queries, taskInstanceID string, layers []contract.LayerState) error {
	for i, l := range layers {
		inputsJSON, err := marshalJSONMap(l.Inputs)
		if err != nil {
			return fmt.Errorf("marshal layer %d inputs: %w", i, err)
		}
		localsJSON, err := marshalJSONMap(l.Locals)
		if err != nil {
			return fmt.Errorf("marshal layer %d locals: %w", i, err)
		}
		outputsJSON, err := marshalJSONMap(l.Outputs)
		if err != nil {
			return fmt.Errorf("marshal layer %d outputs: %w", i, err)
		}
		envJSON, err := marshalEnv(l.Env)
		if err != nil {
			return fmt.Errorf("marshal layer %d env: %w", i, err)
		}
		if err := q.InsertTaskInstanceLayer(ctx, sqlcgen.InsertTaskInstanceLayerParams{
			TaskInstanceID:       taskInstanceID,
			Position:             int64(i),
			EffectID:             l.EffectID,
			Status:               l.Status,
			InputsJson:           inputsJSON,
			LocalsJson:           localsJSON,
			OutputsJson:          outputsJSON,
			EnvJson:              envJSON,
			HeartbeatTicks:       nullInt(l.HeartbeatTicks),
			HeartbeatEscalations: nullInt(l.HeartbeatEscalations),
			SetupAt:              formatTimeNull(l.SetupAt),
			FailedAt:             formatTimeNull(l.FailedAt),
			CleanedAt:            formatTimeNull(l.CleanedAt),
			Error:                nullString(l.Error),
		}); err != nil {
			return fmt.Errorf("insert task instance layer %d: %w", i, err)
		}
	}
	return nil
}

func marshalEnv(env map[string]string) (sql.NullString, error) {
	if len(env) == 0 {
		return sql.NullString{}, nil
	}
	return marshalJSONValue(env)
}

func layersFromNodeRows(rows []sqlcgen.NodeInstanceLayer) ([]contract.LayerState, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]contract.LayerState, len(rows))
	for i, r := range rows {
		l, err := layerFromColumns(r.EffectID, r.Status, r.InputsJson, r.LocalsJson, r.OutputsJson, r.EnvJson,
			r.HeartbeatTicks, r.HeartbeatEscalations, r.SetupAt, r.FailedAt, r.CleanedAt, r.Error)
		if err != nil {
			return nil, fmt.Errorf("layer at position %d: %w", r.Position, err)
		}
		out[i] = l
	}
	return out, nil
}

func layersFromTaskRows(rows []sqlcgen.TaskInstanceLayer) ([]contract.LayerState, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]contract.LayerState, len(rows))
	for i, r := range rows {
		l, err := layerFromColumns(r.EffectID, r.Status, r.InputsJson, r.LocalsJson, r.OutputsJson, r.EnvJson,
			r.HeartbeatTicks, r.HeartbeatEscalations, r.SetupAt, r.FailedAt, r.CleanedAt, r.Error)
		if err != nil {
			return nil, fmt.Errorf("layer at position %d: %w", r.Position, err)
		}
		out[i] = l
	}
	return out, nil
}

// layerFromColumns is shared by both layer tables' rows, which carry the
// identical set of non-key columns.
func layerFromColumns(effectID, status string, inputsJSON, localsJSON, outputsJSON, envJSON sql.NullString,
	heartbeatTicks, heartbeatEscalations sql.NullInt64, setupAt, failedAt, cleanedAt sql.NullString, errCol sql.NullString) (contract.LayerState, error) {
	inputs, err := unmarshalJSONMap(inputsJSON)
	if err != nil {
		return contract.LayerState{}, fmt.Errorf("inputs: %w", err)
	}
	locals, err := unmarshalJSONMap(localsJSON)
	if err != nil {
		return contract.LayerState{}, fmt.Errorf("locals: %w", err)
	}
	outputs, err := unmarshalJSONMap(outputsJSON)
	if err != nil {
		return contract.LayerState{}, fmt.Errorf("outputs: %w", err)
	}
	var env map[string]string
	if err := unmarshalJSONValue(envJSON, &env); err != nil {
		return contract.LayerState{}, fmt.Errorf("env: %w", err)
	}
	setup, err := parseTimeNull(setupAt)
	if err != nil {
		return contract.LayerState{}, fmt.Errorf("setup_at: %w", err)
	}
	failed, err := parseTimeNull(failedAt)
	if err != nil {
		return contract.LayerState{}, fmt.Errorf("failed_at: %w", err)
	}
	cleaned, err := parseTimeNull(cleanedAt)
	if err != nil {
		return contract.LayerState{}, fmt.Errorf("cleaned_at: %w", err)
	}
	return contract.LayerState{
		EffectID:             effectID,
		Status:               status,
		Inputs:               inputs,
		Locals:               locals,
		Outputs:              outputs,
		Env:                  env,
		HeartbeatTicks:       int(heartbeatTicks.Int64),
		HeartbeatEscalations: int(heartbeatEscalations.Int64),
		SetupAt:              setup,
		FailedAt:             failed,
		CleanedAt:            cleaned,
		Error:                errCol.String,
	}, nil
}

package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// PutSession upserts one session and replaces its task, layer, and channel
// health rows in one write transaction.
func (db *DB) PutSession(ctx context.Context, s *domain.Session) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		return db.writeSessionTx(ctx, tx, s)
	})
}

// GetSession returns a session's live row by name, or (nil, nil) if none
// exists. The row and its children/tasks are read inside one transaction,
// so a concurrent Put/Update can never be interleaved into a value that
// never existed as such.
func (db *DB) GetSession(ctx context.Context, name string) (*domain.Session, error) {
	var s *domain.Session
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetLiveSession(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get session %q: %w", name, err)
		}
		parsed, err := sessionFromRow(row)
		if err != nil {
			return err
		}
		if err := db.loadSessionExtras(ctx, tx, parsed); err != nil {
			return err
		}
		s = parsed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// AllSessions returns every live session, keyed by name (see GetSession).
func (db *DB) AllSessions(ctx context.Context) (map[string]*domain.Session, error) {
	result := make(map[string]*domain.Session)
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListLiveSessions(ctx)
		if err != nil {
			return fmt.Errorf("list sessions: %w", err)
		}
		for _, row := range rows {
			s, err := sessionFromRow(row)
			if err != nil {
				return err
			}
			if err := db.loadSessionExtras(ctx, tx, s); err != nil {
				return err
			}
			result[s.Name] = s
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// FindSessionsByAlias returns every live session whose alias equals alias.
// An empty alias is rejected before querying, rather than incidentally
// matching no row: a caller passing "" almost certainly means "unset".
func (db *DB) FindSessionsByAlias(ctx context.Context, alias string) ([]*domain.Session, error) {
	if alias == "" {
		return nil, nil
	}
	var result []*domain.Session
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListLiveSessionsByAlias(ctx, sql.NullString{String: alias, Valid: true})
		if err != nil {
			return fmt.Errorf("find sessions by alias %q: %w", alias, err)
		}
		for _, row := range rows {
			s, err := sessionFromRow(row)
			if err != nil {
				return err
			}
			if err := db.loadSessionExtras(ctx, tx, s); err != nil {
				return err
			}
			result = append(result, s)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateSession reads the named session's live row, runs fn, and writes
// the result back in one transaction. A session is destroyed the same
// way: fn sets Status to SessionStatusDestroyed on the same row.
func (db *DB) UpdateSession(ctx context.Context, name string, fn func(*domain.Session) error) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetLiveSession(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no state entry for session %q", name)
			}
			return fmt.Errorf("get session %q: %w", name, err)
		}
		s, err := sessionFromRow(row)
		if err != nil {
			return err
		}
		if err := db.loadSessionExtras(ctx, tx, s); err != nil {
			return err
		}

		if err := fn(s); err != nil {
			return err
		}

		return db.writeSessionTx(ctx, tx, s)
	})
}

// EnsureLiveSession returns name's live session id, minting a minimal
// placeholder row first if none exists yet -- the append path's one
// lazy-start entry point. It applies only to a name with no row at all,
// ever: a destroyed name must not be resurrected just because an
// unrelated event (e.g. a node-result recording) races its teardown.
func (db *DB) EnsureLiveSession(ctx context.Context, name string) (string, error) {
	id, err := db.EventStreamID(ctx, name)
	if err != nil {
		return "", fmt.Errorf("ensure live session %q: %w", name, err)
	}
	if id != "" {
		return id, nil
	}
	existed, err := db.sessionEverExisted(ctx, name)
	if err != nil {
		return "", fmt.Errorf("ensure live session %q: %w", name, err)
	}
	if existed {
		return "", fmt.Errorf("no live session named %q", name)
	}
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: name, Status: contract.SessionStatusDown, CreatedAt: now, UpdatedAt: now}); err != nil {
		return "", fmt.Errorf("ensure live session %q: %w", name, err)
	}
	id, err = db.EventStreamID(ctx, name)
	if err != nil {
		return "", fmt.Errorf("ensure live session %q: %w", name, err)
	}
	return id, nil
}

// sessionEverExisted reports whether name has ever had a sessions row, live or destroyed.
func (db *DB) sessionEverExisted(ctx context.Context, name string) (bool, error) {
	var existed bool
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		got, err := sqlcgen.New(tx).SessionEverExistedByName(ctx, name)
		if err != nil {
			return err
		}
		existed = got != 0
		return nil
	})
	return existed, err
}

// DestroySession transitions name's live row to SessionStatusDestroyed
// (retaining the row and its history) and releases its up-slot
// reservation, if any.
func (db *DB) DestroySession(ctx context.Context, name string, destroyedAt time.Time) error {
	if err := db.UpdateSession(ctx, name, func(s *domain.Session) error {
		s.Status = contract.SessionStatusDestroyed
		s.DestroyedAt = destroyedAt
		return nil
	}); err != nil {
		return fmt.Errorf("destroy session %q: %w", name, err)
	}
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		if err := sqlcgen.New(tx).DeleteUpReservation(ctx, name); err != nil {
			return fmt.Errorf("delete up-slot reservation for %q: %w", name, err)
		}
		return nil
	})
}

func (db *DB) writeSessionTx(ctx context.Context, tx *sql.Tx, s *domain.Session) error {
	q := sqlcgen.New(tx)

	existingID, err := q.SessionIDByLiveName(ctx, s.Name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		existingID = ""
	case err != nil:
		return fmt.Errorf("resolve existing id for session %q: %w", s.Name, err)
	}

	candidateID := existingID
	if candidateID == "" {
		candidateID = newULID()
	}

	// A parent/root link is resolved against the referenced session's live
	// row exactly once -- at creation, or on a later write if none was set
	// yet -- and passes through unchanged once s.ParentSessionID/
	// RootSessionID already carry a value: a parent later destroyed and
	// recreated under the same name must not retarget this session.
	var parentCol, rootCol sql.NullString
	if s.ParentSessionID != "" || s.RootSessionID != "" {
		parentCol = nullString(s.ParentSessionID)
		rootCol = nullString(s.RootSessionID)
	} else {
		parentCol, rootCol, err = resolveParentColumns(ctx, q, existingID, s.ParentSession)
		if err != nil {
			return fmt.Errorf("resolve parent for session %q: %w", s.Name, err)
		}
	}

	var populationWorkflow, populationName sql.NullString
	if s.Population != nil {
		populationWorkflow = sql.NullString{String: s.Population.Workflow, Valid: true}
		populationName = sql.NullString{String: s.Population.Name, Valid: true}
	}

	status := s.Status
	if status == "" {
		status = contract.SessionStatusDown
	}

	health := sessionHealthColumns(s.Health)
	tickConsecutiveUnchanged, tickLastFingerprint := sessionTickColumns(s.TickBackoff)

	inputsJSON, err := marshalJSONMap(s.Inputs)
	if err != nil {
		return fmt.Errorf("marshal session %q inputs: %w", s.Name, err)
	}

	if existingID == "" {
		params, err := insertSessionParams(candidateID, status, parentCol, rootCol, populationWorkflow, populationName, inputsJSON, health, tickConsecutiveUnchanged, tickLastFingerprint, s)
		if err != nil {
			return err
		}
		if err := q.InsertSession(ctx, params); err != nil {
			return fmt.Errorf("insert session %q: %w", s.Name, err)
		}
	} else if err := q.UpdateSessionByID(ctx, sqlcgen.UpdateSessionByIDParams{
		ID:                       candidateID,
		Name:                     s.Name,
		Status:                   status,
		DestroyedAt:              formatTimeNull(s.DestroyedAt),
		ParentSessionID:          parentCol,
		RootSessionID:            rootCol,
		ResourceID:               nullString(s.ResourceID),
		Alias:                    nullString(s.Alias),
		Workflow:                 s.Workflow,
		WorkspaceDir:             nullString(s.WorkspaceDirPath),
		PopulationWorkflow:       populationWorkflow,
		PopulationName:           populationName,
		InputsJson:               inputsJSON,
		HealthLastCheckedAt:      health.lastCheckedAt,
		HealthLastActivityAt:     health.lastActivityAt,
		HealthLastFingerprint:    health.lastFingerprint,
		HealthLastState:          health.lastState,
		HealthLastReason:         health.lastReason,
		HealthLastNotifiedAt:     health.lastNotifiedAt,
		HealthNotifyCount:        health.notifyCount,
		TickConsecutiveUnchanged: tickConsecutiveUnchanged,
		TickLastFingerprint:      tickLastFingerprint,
		LastTickAt:               formatTimeNull(s.LastTickAt),
		UpdatedAt:                formatTime(s.UpdatedAt),
	}); err != nil {
		return fmt.Errorf("update session %q: %w", s.Name, err)
	}

	if err := db.writeTasksTx(ctx, tx, candidateID, s.Tasks); err != nil {
		return err
	}
	return writeChannelHealthTx(ctx, q, candidateID, s.ChannelValidationHealth, s.ChannelDeliveryHealth)
}

// insertSessionParams builds InsertSession's parameters from already-resolved
// column values, shared by writeSessionTx's own insert branch (id = a freshly
// minted ULID, parent/root/population resolved against live rows) and
// ImportSession (id = a preserved legacy identity, parent/root left NULL for
// a later resolution pass — see ImportSession).
func insertSessionParams(id, status string, parentCol, rootCol, populationWorkflow, populationName, inputsJSON sql.NullString, health healthColumns, tickConsecutiveUnchanged sql.NullInt64, tickLastFingerprint sql.NullString, s *domain.Session) (sqlcgen.InsertSessionParams, error) {
	return sqlcgen.InsertSessionParams{
		ID:                       id,
		Name:                     s.Name,
		Status:                   status,
		DestroyedAt:              formatTimeNull(s.DestroyedAt),
		ParentSessionID:          parentCol,
		RootSessionID:            rootCol,
		ResourceID:               nullString(s.ResourceID),
		Alias:                    nullString(s.Alias),
		Workflow:                 s.Workflow,
		WorkspaceDir:             nullString(s.WorkspaceDirPath),
		PopulationWorkflow:       populationWorkflow,
		PopulationName:           populationName,
		InputsJson:               inputsJSON,
		HealthLastCheckedAt:      health.lastCheckedAt,
		HealthLastActivityAt:     health.lastActivityAt,
		HealthLastFingerprint:    health.lastFingerprint,
		HealthLastState:          health.lastState,
		HealthLastReason:         health.lastReason,
		HealthLastNotifiedAt:     health.lastNotifiedAt,
		HealthNotifyCount:        health.notifyCount,
		TickConsecutiveUnchanged: tickConsecutiveUnchanged,
		TickLastFingerprint:      tickLastFingerprint,
		LastTickAt:               formatTimeNull(s.LastTickAt),
		CreatedAt:                formatTime(s.CreatedAt),
		UpdatedAt:                formatTime(s.UpdatedAt),
	}, nil
}

// ImportSession inserts s as a brand-new session row using id as its durable
// identity (a legacy events/<session>/.gen id, or a freshly minted one where
// none survived) instead of minting a fresh ULID — the one-time legacy
// importer's own entry point. It always inserts (import only ever targets a
// freshly created database) and leaves parent_session_id/root_session_id
// NULL: id resolution for a parent link depends on that parent's own row
// existing, which is not guaranteed yet during a single import pass over an
// unordered session map. A second PutSession(ctx, s) call once every
// session row exists resolves ParentSession as any other write does, and
// also writes s.Tasks/channel health, so this method's own effect is scoped
// to the bare row alone.
func (db *DB) ImportSession(ctx context.Context, id string, s *domain.Session) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)

		var populationWorkflow, populationName sql.NullString
		if s.Population != nil {
			populationWorkflow = sql.NullString{String: s.Population.Workflow, Valid: true}
			populationName = sql.NullString{String: s.Population.Name, Valid: true}
		}
		status := s.Status
		if status == "" {
			status = contract.SessionStatusDown
		}
		health := sessionHealthColumns(s.Health)
		tickConsecutiveUnchanged, tickLastFingerprint := sessionTickColumns(s.TickBackoff)
		inputsJSON, err := marshalJSONMap(s.Inputs)
		if err != nil {
			return fmt.Errorf("marshal session %q inputs: %w", s.Name, err)
		}
		params, err := insertSessionParams(id, status, sql.NullString{}, sql.NullString{}, populationWorkflow, populationName, inputsJSON, health, tickConsecutiveUnchanged, tickLastFingerprint, s)
		if err != nil {
			return err
		}
		if err := q.InsertSession(ctx, params); err != nil {
			return fmt.Errorf("import session %q: %w", s.Name, err)
		}
		return nil
	})
}

// healthColumns is sessionHealthColumns' result: one *HealthState's health_* column values.
type healthColumns struct {
	lastCheckedAt   sql.NullString
	lastActivityAt  sql.NullString
	lastFingerprint sql.NullString
	lastState       sql.NullString
	lastReason      sql.NullString
	lastNotifiedAt  sql.NullString
	notifyCount     sql.NullInt64
}

// sessionHealthColumns flattens a possibly-nil *HealthState. NotifyCount
// stays NULL only when h itself is nil -- its own zero value, 0, is a
// real count once a session has been evaluated at all.
func sessionHealthColumns(h *contract.HealthState) healthColumns {
	if h == nil {
		return healthColumns{}
	}
	return healthColumns{
		lastCheckedAt:   formatTimeNull(h.LastCheckedAt),
		lastActivityAt:  formatTimeNull(h.LastActivityAt),
		lastFingerprint: nullString(h.LastFingerprint),
		lastState:       nullString(h.LastState),
		lastReason:      nullString(h.LastReason),
		lastNotifiedAt:  formatTimeNull(h.LastNotifiedAt),
		notifyCount:     sql.NullInt64{Int64: int64(h.NotifyCount), Valid: true},
	}
}

// sessionTickColumns flattens a possibly-nil *TickBackoff into its column
// values; see sessionHealthColumns for the same nil-safety rationale.
func sessionTickColumns(tb *contract.TickBackoff) (consecutiveUnchanged sql.NullInt64, lastFingerprint sql.NullString) {
	if tb == nil {
		return sql.NullInt64{}, sql.NullString{}
	}
	return sql.NullInt64{Int64: int64(tb.ConsecutiveUnchanged), Valid: true}, nullString(tb.LastFingerprint)
}

// resolveParentColumns translates Session.ParentSession into
// parent_session_id/root_session_id, silently clearing a self-reference,
// a cycle, or a reference with no live row. selfID is the writing
// session's own existing live id, or "" for a brand new session.
func resolveParentColumns(ctx context.Context, q *sqlcgen.Queries, selfID, parentSession string) (parent, root sql.NullString, err error) {
	if parentSession == "" {
		return sql.NullString{}, sql.NullString{}, nil
	}

	if target, ok := strings.CutPrefix(parentSession, "root:"); ok {
		if target == "" {
			return sql.NullString{}, sql.NullString{}, nil
		}
		id, err := q.SessionIDByLiveName(ctx, target)
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullString{}, sql.NullString{}, nil
		}
		if err != nil {
			return sql.NullString{}, sql.NullString{}, err
		}
		if id == selfID {
			return sql.NullString{}, sql.NullString{}, nil
		}
		return sql.NullString{}, sql.NullString{String: id, Valid: true}, nil
	}

	id, err := q.SessionIDByLiveName(ctx, parentSession)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, sql.NullString{}, nil
	}
	if err != nil {
		return sql.NullString{}, sql.NullString{}, err
	}
	if id == selfID {
		return sql.NullString{}, sql.NullString{}, nil
	}
	if selfID != "" {
		cyclic, err := wouldCreateCycle(ctx, q, selfID, id)
		if err != nil {
			return sql.NullString{}, sql.NullString{}, err
		}
		if cyclic {
			return sql.NullString{}, sql.NullString{}, nil
		}
	}
	return sql.NullString{String: id, Valid: true}, sql.NullString{}, nil
}

// wouldCreateCycle walks parentID's chain looking for childID or a repeat.
func wouldCreateCycle(ctx context.Context, q *sqlcgen.Queries, childID, parentID string) (bool, error) {
	seen := map[string]bool{}
	cur := parentID
	for cur != "" {
		if cur == childID {
			return true, nil
		}
		if seen[cur] {
			return true, nil
		}
		seen[cur] = true
		next, err := q.LiveSessionParentID(ctx, cur)
		if err != nil {
			return false, fmt.Errorf("walk parent chain from %q: %w", cur, err)
		}
		if !next.Valid {
			return false, nil
		}
		cur = next.String
	}
	return false, nil
}

// deriveParentSession is resolveParentColumns' inverse for reads: it names
// parentID/rootID by session name, since ParentSession is name-shaped
// elsewhere in core. A dangling reference reads as no parent, not an error.
func deriveParentSession(ctx context.Context, q *sqlcgen.Queries, parent, root sql.NullString) (string, error) {
	switch {
	case parent.Valid:
		name, err := q.SessionNameByID(ctx, parent.String)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("resolve parent session name: %w", err)
		}
		return name, nil
	case root.Valid:
		name, err := q.SessionNameByID(ctx, root.String)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("resolve root session name: %w", err)
		}
		return "root:" + name, nil
	default:
		return "", nil
	}
}

// sessionFromRow decodes a sessions row into a domain.Session; it does not
// populate Children or Tasks — callers needing those call loadSessionExtras
// (which also resolves ParentSession, since that requires another query).
func sessionFromRow(row sqlcgen.Session) (*domain.Session, error) {
	s := &domain.Session{
		ID:               row.ID,
		Name:             row.Name,
		Status:           row.Status,
		ResourceID:       row.ResourceID.String,
		ParentSessionID:  row.ParentSessionID.String,
		RootSessionID:    row.RootSessionID.String,
		Alias:            row.Alias.String,
		Workflow:         row.Workflow,
		WorkspaceDirPath: row.WorkspaceDir.String,
	}
	if row.PopulationWorkflow.Valid && row.PopulationName.Valid {
		s.Population = &contract.PopulationProvenance{Workflow: row.PopulationWorkflow.String, Name: row.PopulationName.String}
	}

	inputs, err := unmarshalJSONMap(row.InputsJson)
	if err != nil {
		return nil, fmt.Errorf("parse session %q inputs_json: %w", row.Name, err)
	}
	s.Inputs = inputs

	destroyedAt, err := parseTimeNull(row.DestroyedAt)
	if err != nil {
		return nil, fmt.Errorf("parse session %q destroyed_at: %w", row.Name, err)
	}
	s.DestroyedAt = destroyedAt

	health, err := healthFromRow(row)
	if err != nil {
		return nil, fmt.Errorf("parse session %q health: %w", row.Name, err)
	}
	s.Health = health

	if row.TickConsecutiveUnchanged.Valid || row.TickLastFingerprint.Valid {
		s.TickBackoff = &contract.TickBackoff{
			LastFingerprint:      row.TickLastFingerprint.String,
			ConsecutiveUnchanged: int(row.TickConsecutiveUnchanged.Int64),
		}
	}
	lastTickAt, err := parseTimeNull(row.LastTickAt)
	if err != nil {
		return nil, fmt.Errorf("parse session %q last_tick_at: %w", row.Name, err)
	}
	s.LastTickAt = lastTickAt

	createdAt, err := parseTime(row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse session %q created_at: %w", row.Name, err)
	}
	updatedAt, err := parseTime(row.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse session %q updated_at: %w", row.Name, err)
	}
	s.CreatedAt = createdAt
	s.UpdatedAt = updatedAt
	return s, nil
}

// healthFromRow reconstructs *HealthState from its columns, or nil if none
// of them were ever populated (no healthcheck sweep has run yet).
func healthFromRow(row sqlcgen.Session) (*contract.HealthState, error) {
	if !row.HealthLastCheckedAt.Valid && !row.HealthLastActivityAt.Valid && !row.HealthLastFingerprint.Valid &&
		!row.HealthLastState.Valid && !row.HealthLastReason.Valid && !row.HealthLastNotifiedAt.Valid && !row.HealthNotifyCount.Valid {
		return nil, nil
	}
	lastCheckedAt, err := parseTimeNull(row.HealthLastCheckedAt)
	if err != nil {
		return nil, fmt.Errorf("last_checked_at: %w", err)
	}
	lastActivityAt, err := parseTimeNull(row.HealthLastActivityAt)
	if err != nil {
		return nil, fmt.Errorf("last_activity_at: %w", err)
	}
	lastNotifiedAt, err := parseTimeNull(row.HealthLastNotifiedAt)
	if err != nil {
		return nil, fmt.Errorf("last_notified_at: %w", err)
	}
	return &contract.HealthState{
		LastCheckedAt:   lastCheckedAt,
		LastActivityAt:  lastActivityAt,
		LastFingerprint: row.HealthLastFingerprint.String,
		LastState:       row.HealthLastState.String,
		LastReason:      row.HealthLastReason.String,
		LastNotifiedAt:  lastNotifiedAt,
		NotifyCount:     int(row.HealthNotifyCount.Int64),
	}, nil
}

// loadSessionExtras populates the fields sessionFromRow leaves unset:
// ParentSession (a name, projected from the already-loaded
// ParentSessionID/RootSessionID), Children (derived from other rows'
// parent_session_id, never stored), channel health, and Tasks (assembled
// from node/task instance rows).
func (db *DB) loadSessionExtras(ctx context.Context, q sqlcgen.DBTX, s *domain.Session) error {
	queries := sqlcgen.New(q)

	parentSession, err := deriveParentSession(ctx, queries, nullString(s.ParentSessionID), nullString(s.RootSessionID))
	if err != nil {
		return err
	}
	s.ParentSession = parentSession

	children, err := queries.ListLiveChildSessionNames(ctx, sql.NullString{String: s.ID, Valid: true})
	if err != nil {
		return fmt.Errorf("list children of %q: %w", s.Name, err)
	}
	if len(children) > 0 {
		s.Children = children
	} else {
		s.Children = nil
	}

	validationHealth, deliveryHealth, err := loadChannelHealth(ctx, queries, s.ID)
	if err != nil {
		return err
	}
	s.ChannelValidationHealth = validationHealth
	s.ChannelDeliveryHealth = deliveryHealth

	tasks, err := loadTasks(ctx, q, s.ID)
	if err != nil {
		return err
	}
	s.Tasks = tasks
	return nil
}

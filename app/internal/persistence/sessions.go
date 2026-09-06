package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// PutSession upserts one session and replaces its task and completion rows,
// in one write transaction. It never touches an unrelated session's rows.
func (db *DB) PutSession(ctx context.Context, s *domain.Session) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		return db.writeSessionTx(ctx, tx, s)
	})
}

// GetSession returns a session by name, or (nil, nil) if it does not exist.
// The base row and its children/tasks are read inside one transaction, so a
// concurrent Put or Update can never be interleaved into a single logical
// session value that never existed as such.
func (db *DB) GetSession(ctx context.Context, name string) (*domain.Session, error) {
	var s *domain.Session
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetSession(ctx, name)
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

// AllSessions returns every session, keyed by name, as of one consistent
// snapshot (see GetSession).
func (db *DB) AllSessions(ctx context.Context) (map[string]*domain.Session, error) {
	result := make(map[string]*domain.Session)
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListSessions(ctx)
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

// FindSessionsByAlias returns every session whose create-time alias equals
// alias, as of one consistent snapshot (see GetSession). An empty alias is
// rejected before querying: an alias-less session stores NULL, not "", so
// alias = "" would simply match no row, but a caller passing "" almost
// certainly means "unset" and this makes that a guaranteed empty result
// rather than an incidental one.
func (db *DB) FindSessionsByAlias(ctx context.Context, alias string) ([]*domain.Session, error) {
	if alias == "" {
		return nil, nil
	}
	var result []*domain.Session
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := sqlcgen.New(tx).ListSessionsByAlias(ctx, sql.NullString{String: alias, Valid: true})
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

// UpdateSession reads the named session (with its task and completion
// rows), runs fn against it, and writes the result back — all inside one
// write transaction. fn returning an error aborts without writing.
func (db *DB) UpdateSession(ctx context.Context, name string, fn func(*domain.Session) error) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetSession(ctx, name)
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

// DeleteSession removes a session, its task/completion rows (cascaded by
// the schema's foreign keys), and its up-slot reservation if any. Sessions
// that named it as their parent are detached (parent_session_name set to
// NULL), also by the schema's ON DELETE SET NULL — event history for the
// deleted session is untouched.
func (db *DB) DeleteSession(ctx context.Context, name string) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.DeleteSession(ctx, name); err != nil {
			return fmt.Errorf("delete session %q: %w", name, err)
		}
		if err := q.DeleteUpReservation(ctx, name); err != nil {
			return fmt.Errorf("delete up-slot reservation for %q: %w", name, err)
		}
		return nil
	})
}

func (db *DB) writeSessionTx(ctx context.Context, tx *sql.Tx, s *domain.Session) error {
	parentCol, rootCol, err := resolveParentColumns(ctx, tx, s.Name, s.ParentSession)
	if err != nil {
		return fmt.Errorf("resolve parent for session %q: %w", s.Name, err)
	}

	recordJSON, err := marshalSessionRecord(s)
	if err != nil {
		return fmt.Errorf("marshal session %q: %w", s.Name, err)
	}

	var populationWorkflow, populationName sql.NullString
	if s.Population != nil {
		populationWorkflow = sql.NullString{String: s.Population.Workflow, Valid: true}
		populationName = sql.NullString{String: s.Population.Name, Valid: true}
	}

	if err := sqlcgen.New(tx).UpsertSession(ctx, sqlcgen.UpsertSessionParams{
		Name:               s.Name,
		ParentSessionName:  parentCol,
		RootSessionName:    rootCol,
		ResourceID:         nullString(s.ResourceID),
		Alias:              nullString(s.Alias),
		Workflow:           s.Workflow,
		WorkspaceDir:       nullString(s.WorkspaceDirPath),
		PopulationWorkflow: populationWorkflow,
		PopulationName:     populationName,
		CreatedAt:          formatTime(s.CreatedAt),
		UpdatedAt:          formatTime(s.UpdatedAt),
		RecordJson:         recordJSON,
	}); err != nil {
		return fmt.Errorf("upsert session %q: %w", s.Name, err)
	}

	return db.writeTasksTx(ctx, tx, s.Name, s.Tasks)
}

// resolveParentColumns translates a Session.ParentSession value into the
// sessions table's parent_session_name / root_session_name columns,
// silently clearing (rather than rejecting) a self-reference, a cycle, or a
// reference to a session that does not exist — mirroring the JSON store's
// normalizeSessionTree, which dropped exactly these cases instead of
// failing the write they arrived in.
func resolveParentColumns(ctx context.Context, tx *sql.Tx, name, parentSession string) (parent, root sql.NullString, err error) {
	if parentSession == "" {
		return sql.NullString{}, sql.NullString{}, nil
	}
	q := sqlcgen.New(tx)

	if target, ok := strings.CutPrefix(parentSession, "root:"); ok {
		if target == "" || target == name {
			return sql.NullString{}, sql.NullString{}, nil
		}
		count, err := q.CountSessionsNamed(ctx, target)
		if err != nil {
			return sql.NullString{}, sql.NullString{}, err
		}
		if count == 0 {
			return sql.NullString{}, sql.NullString{}, nil
		}
		return sql.NullString{}, sql.NullString{String: target, Valid: true}, nil
	}

	if parentSession == name {
		return sql.NullString{}, sql.NullString{}, nil
	}
	count, err := q.CountSessionsNamed(ctx, parentSession)
	if err != nil {
		return sql.NullString{}, sql.NullString{}, err
	}
	if count == 0 {
		return sql.NullString{}, sql.NullString{}, nil
	}
	cyclic, err := wouldCreateCycle(ctx, q, name, parentSession)
	if err != nil {
		return sql.NullString{}, sql.NullString{}, err
	}
	if cyclic {
		return sql.NullString{}, sql.NullString{}, nil
	}
	return sql.NullString{String: parentSession, Valid: true}, sql.NullString{}, nil
}

// wouldCreateCycle walks parentName's real-parent chain looking for
// childName; a chain that reaches it (or repeats a name, defensively)
// means assigning parentName as childName's parent would create a cycle.
func wouldCreateCycle(ctx context.Context, q *sqlcgen.Queries, childName, parentName string) (bool, error) {
	seen := map[string]bool{}
	cur := parentName
	for cur != "" {
		if cur == childName {
			return true, nil
		}
		if seen[cur] {
			return true, nil
		}
		seen[cur] = true
		next, err := q.SessionParent(ctx, cur)
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

func deriveParentSession(parent, root sql.NullString) string {
	if parent.Valid {
		return parent.String
	}
	if root.Valid {
		return "root:" + root.String
	}
	return ""
}

// sessionFromRow decodes record_json and overlays it with the row's
// relational columns; it does not populate Children or Tasks — callers
// needing those call loadSessionExtras.
func sessionFromRow(row sqlcgen.Session) (*domain.Session, error) {
	s, err := unmarshalSessionRecord(row.RecordJson)
	if err != nil {
		return nil, fmt.Errorf("parse session %q record: %w", row.Name, err)
	}
	s.Name = row.Name
	s.ResourceID = row.ResourceID.String
	s.Alias = row.Alias.String
	s.Workflow = row.Workflow
	s.WorkspaceDirPath = row.WorkspaceDir.String
	s.ParentSession = deriveParentSession(row.ParentSessionName, row.RootSessionName)
	s.Population = nil
	if row.PopulationWorkflow.Valid && row.PopulationName.Valid {
		s.Population = &contract.PopulationProvenance{Workflow: row.PopulationWorkflow.String, Name: row.PopulationName.String}
	}

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

// loadSessionExtras populates the fields sessionFromRow leaves unset:
// Children (derived from other rows' parent_session_name, never stored)
// and Tasks (assembled from task_instances/task_done_when/judges).
func (db *DB) loadSessionExtras(ctx context.Context, q sqlcgen.DBTX, s *domain.Session) error {
	children, err := sqlcgen.New(q).ListChildSessionNames(ctx, sql.NullString{String: s.Name, Valid: true})
	if err != nil {
		return fmt.Errorf("list children of %q: %w", s.Name, err)
	}
	if len(children) > 0 {
		s.Children = children
	} else {
		s.Children = nil
	}

	tasks, err := loadTasks(ctx, q, s.Name)
	if err != nil {
		return err
	}
	s.Tasks = tasks
	return nil
}

// sessionPayload is record_json's actual on-disk shape: every Session field
// with no relational column of its own. Unlike contract.Session, every field
// here can genuinely be absent, so each carries omitempty/omitzero — a fresh
// session's blob is "{}" rather than a page of zero-valued columns
// (session_name, branch, workspace_dir_path, year-1 timestamps) that would
// read as a second, disagreeing copy of the relational row to anyone
// inspecting the blob directly (the importer included). contract.Session
// itself is unchanged; this type exists only at the persistence boundary.
type sessionPayload struct {
	Branch                  string                  `json:"branch,omitempty"`
	Message                 *contract.Message       `json:"message,omitempty"`
	Inputs                  map[string]any          `json:"inputs,omitempty"`
	Health                  *contract.HealthState   `json:"health,omitempty"`
	ChannelValidationHealth *contract.ChannelHealth `json:"channel_validation_health,omitempty"`
	ChannelDeliveryHealth   *contract.ChannelHealth `json:"channel_delivery_health,omitempty"`
	LastTickAt              time.Time               `json:"last_tick_at,omitzero"`
	TickBackoff             *contract.TickBackoff   `json:"tick_backoff,omitempty"`
}

// marshalSessionRecord serializes every Session field not already carried by
// a relational column (name, resource_id, alias, workflow, workspace_dir,
// population_workflow/name, parent/root_session_name, created_at,
// updated_at) or derived at read time (children, tasks).
func marshalSessionRecord(s *domain.Session) (string, error) {
	data, err := json.Marshal(sessionPayload{
		Branch:                  s.Branch,
		Message:                 s.Message,
		Inputs:                  s.Inputs,
		Health:                  s.Health,
		ChannelValidationHealth: s.ChannelValidationHealth,
		ChannelDeliveryHealth:   s.ChannelDeliveryHealth,
		LastTickAt:              s.LastTickAt,
		TickBackoff:             s.TickBackoff,
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalSessionRecord(recordJSON string) (*domain.Session, error) {
	var p sessionPayload
	if err := json.Unmarshal([]byte(recordJSON), &p); err != nil {
		return nil, err
	}
	return &contract.Session{
		Branch:                  p.Branch,
		Message:                 p.Message,
		Inputs:                  p.Inputs,
		Health:                  p.Health,
		ChannelValidationHealth: p.ChannelValidationHealth,
		ChannelDeliveryHealth:   p.ChannelDeliveryHealth,
		LastTickAt:              p.LastTickAt,
		TickBackoff:             p.TickBackoff,
	}, nil
}

package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

// splitPopulationKey recovers a population's (workflow, name) domain
// identity from the "workflow/name" string callers use as a JSON map key
// and in-memory lookup key. It splits on the last slash — safe because a
// workflow address is dot-separated (plecture.schema.json's staticReference
// grammar) and a population name is a plain identifier, so neither half can
// itself contain a slash.
func splitPopulationKey(key string) (workflow, name string) {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

// Population returns one population's durable state, or (nil, nil) if key
// has never been recorded. The population row and its members are read
// inside one transaction, so a concurrent UpdatePopulation can never be
// interleaved into a single logical value that never existed as such.
func (db *DB) Population(ctx context.Context, key string) (*domain.PopulationState, error) {
	workflow, name := splitPopulationKey(key)
	var population *domain.PopulationState
	err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		row, err := sqlcgen.New(tx).GetPopulation(ctx, sqlcgen.GetPopulationParams{Workflow: workflow, Name: name})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("get population %q: %w", key, err)
		}
		members, err := loadPopulationMembers(ctx, tx, workflow, name)
		if err != nil {
			return err
		}
		population = &domain.PopulationState{Workflow: row.Workflow, Name: row.Name, Members: members}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return population, nil
}

// UpdatePopulation reads or creates one population and its members, runs fn
// against it, and replaces that population's row and member rows — all in
// one write transaction scoped to key alone.
func (db *DB) UpdatePopulation(ctx context.Context, key string, fn func(*domain.PopulationState) error) error {
	workflow, name := splitPopulationKey(key)
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetPopulation(ctx, sqlcgen.GetPopulationParams{Workflow: workflow, Name: name})
		var population *domain.PopulationState
		switch {
		case errors.Is(err, sql.ErrNoRows):
			population = &domain.PopulationState{Workflow: workflow, Name: name}
		case err != nil:
			return fmt.Errorf("get population %q: %w", key, err)
		default:
			members, err := loadPopulationMembers(ctx, tx, workflow, name)
			if err != nil {
				return err
			}
			population = &domain.PopulationState{Workflow: row.Workflow, Name: row.Name, Members: members}
		}
		if population.Members == nil {
			population.Members = make(map[string]*domain.PopulationMember)
		}

		if err := fn(population); err != nil {
			return err
		}

		// workflow/name (split from key) are the write's own FK target for
		// population_members below; used here too rather than whatever fn
		// left in population.Workflow/Name, so the two can never disagree.
		if err := q.UpsertPopulation(ctx, sqlcgen.UpsertPopulationParams{
			Workflow: workflow,
			Name:     name,
		}); err != nil {
			return fmt.Errorf("upsert population %q: %w", key, err)
		}
		if err := q.DeletePopulationMembersForPopulation(ctx, sqlcgen.DeletePopulationMembersForPopulationParams{
			Workflow: workflow,
			Name:     name,
		}); err != nil {
			return fmt.Errorf("clear population members for %q: %w", key, err)
		}
		for resource, member := range population.Members {
			if member == nil {
				continue
			}
			if err := insertPopulationMemberTx(ctx, q, workflow, name, resource, member); err != nil {
				return err
			}
		}
		return nil
	})
}

func loadPopulationMembers(ctx context.Context, q sqlcgen.DBTX, workflow, name string) (map[string]*domain.PopulationMember, error) {
	rows, err := sqlcgen.New(q).ListPopulationMembers(ctx, sqlcgen.ListPopulationMembersParams{Workflow: workflow, Name: name})
	if err != nil {
		return nil, fmt.Errorf("list population members for %q/%q: %w", workflow, name, err)
	}
	members := make(map[string]*domain.PopulationMember, len(rows))
	for _, row := range rows {
		member, err := populationMemberFromRow(row)
		if err != nil {
			return nil, err
		}
		members[row.ResourceID] = member
	}
	return members, nil
}

// lastDecisionFromColumns rejoins decision_kind/decision_reason into the
// domain's single "kind:reason" (or bare "kind") LastDecision string.
func lastDecisionFromColumns(kind, reason sql.NullString) string {
	if !kind.Valid {
		return ""
	}
	if !reason.Valid {
		return kind.String
	}
	return kind.String + ":" + reason.String
}

// splitLastDecision is lastDecisionFromColumns's inverse.
func splitLastDecision(lastDecision string) (kind, reason sql.NullString) {
	if lastDecision == "" {
		return sql.NullString{}, sql.NullString{}
	}
	if i := strings.Index(lastDecision, ":"); i >= 0 {
		return sql.NullString{String: lastDecision[:i], Valid: true}, sql.NullString{String: lastDecision[i+1:], Valid: true}
	}
	return sql.NullString{String: lastDecision, Valid: true}, sql.NullString{}
}

func populationMemberFromRow(row sqlcgen.PopulationMember) (*domain.PopulationMember, error) {
	var item map[string]any
	if err := json.Unmarshal([]byte(row.ItemJson), &item); err != nil {
		return nil, fmt.Errorf("parse population member %q item: %w", row.ResourceID, err)
	}
	var blockers []string
	if err := json.Unmarshal([]byte(row.LastBlockersJson), &blockers); err != nil {
		return nil, fmt.Errorf("parse population member %q blockers: %w", row.ResourceID, err)
	}
	acceptedAt, err := parseTimeNull(row.AcceptedAt)
	if err != nil {
		return nil, fmt.Errorf("parse population member %q accepted_at: %w", row.ResourceID, err)
	}
	lastAppearance, err := parseTimeNull(row.LastAppearance)
	if err != nil {
		return nil, fmt.Errorf("parse population member %q last_appearance: %w", row.ResourceID, err)
	}
	lastInbound, err := parseTimeNull(row.LastInbound)
	if err != nil {
		return nil, fmt.Errorf("parse population member %q last_inbound: %w", row.ResourceID, err)
	}
	return &domain.PopulationMember{
		ResourceID:     row.ResourceID,
		Item:           item,
		SessionName:    row.SessionName.String,
		Generation:     uint64(row.Generation),
		AcceptedAt:     acceptedAt,
		LastAppearance: lastAppearance,
		LastInbound:    lastInbound,
		Tombstoned:     row.Tombstoned,
		PendingUp:      row.PendingUp,
		LastDecision:   lastDecisionFromColumns(row.DecisionKind, row.DecisionReason),
		LastBlockers:   blockers,
	}, nil
}

func insertPopulationMemberTx(ctx context.Context, q *sqlcgen.Queries, workflow, name, resource string, member *domain.PopulationMember) error {
	itemJSON, err := json.Marshal(member.Item)
	if err != nil {
		return fmt.Errorf("marshal population member %q item: %w", resource, err)
	}
	blockersJSON, err := json.Marshal(member.LastBlockers)
	if err != nil {
		return fmt.Errorf("marshal population member %q blockers: %w", resource, err)
	}
	decisionKind, decisionReason := splitLastDecision(member.LastDecision)
	if err := q.InsertPopulationMember(ctx, sqlcgen.InsertPopulationMemberParams{
		Workflow:         workflow,
		Name:             name,
		ResourceID:       resource,
		SessionName:      nullString(member.SessionName),
		Generation:       int64(member.Generation),
		AcceptedAt:       formatTimeNull(member.AcceptedAt),
		LastAppearance:   formatTimeNull(member.LastAppearance),
		LastInbound:      formatTimeNull(member.LastInbound),
		Tombstoned:       member.Tombstoned,
		PendingUp:        member.PendingUp,
		DecisionKind:     decisionKind,
		DecisionReason:   decisionReason,
		ItemJson:         string(itemJSON),
		LastBlockersJson: string(blockersJSON),
	}); err != nil {
		return fmt.Errorf("insert population member %q: %w", resource, err)
	}
	return nil
}

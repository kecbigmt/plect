package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

// Population returns one population's durable state, or (nil, nil) if key
// has never been recorded.
func (db *DB) Population(ctx context.Context, key string) (*domain.PopulationState, error) {
	row, err := sqlcgen.New(db.read).GetPopulation(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get population %q: %w", key, err)
	}
	members, err := loadPopulationMembers(ctx, db.read, key)
	if err != nil {
		return nil, err
	}
	return &domain.PopulationState{Workflow: row.Workflow, Name: row.Name, Members: members}, nil
}

// UpdatePopulation reads or creates one population and its members, runs fn
// against it, and replaces that population's row and member rows — all in
// one write transaction scoped to key alone.
func (db *DB) UpdatePopulation(ctx context.Context, key string, fn func(*domain.PopulationState) error) error {
	return db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetPopulation(ctx, key)
		var population *domain.PopulationState
		switch {
		case errors.Is(err, sql.ErrNoRows):
			population = &domain.PopulationState{}
		case err != nil:
			return fmt.Errorf("get population %q: %w", key, err)
		default:
			members, err := loadPopulationMembers(ctx, tx, key)
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

		if err := q.UpsertPopulation(ctx, sqlcgen.UpsertPopulationParams{
			PopulationKey: key,
			Workflow:      population.Workflow,
			Name:          population.Name,
		}); err != nil {
			return fmt.Errorf("upsert population %q: %w", key, err)
		}
		if err := q.DeletePopulationMembersForPopulation(ctx, key); err != nil {
			return fmt.Errorf("clear population members for %q: %w", key, err)
		}
		for resource, member := range population.Members {
			if member == nil {
				continue
			}
			if err := insertPopulationMemberTx(ctx, q, key, resource, member); err != nil {
				return err
			}
		}
		return nil
	})
}

func loadPopulationMembers(ctx context.Context, q sqlcgen.DBTX, key string) (map[string]*domain.PopulationMember, error) {
	rows, err := sqlcgen.New(q).ListPopulationMembers(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("list population members for %q: %w", key, err)
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

func populationMemberFromRow(row sqlcgen.PopulationMember) (*domain.PopulationMember, error) {
	var item map[string]any
	if err := json.Unmarshal([]byte(row.ItemJson), &item); err != nil {
		return nil, fmt.Errorf("parse population member %q item: %w", row.ResourceID, err)
	}
	var blockers []string
	if err := json.Unmarshal([]byte(row.LastBlockersJson), &blockers); err != nil {
		return nil, fmt.Errorf("parse population member %q blockers: %w", row.ResourceID, err)
	}
	acceptedAt, err := parseTime(row.AcceptedAt)
	if err != nil {
		return nil, fmt.Errorf("parse population member %q accepted_at: %w", row.ResourceID, err)
	}
	lastAppearance, err := parseTime(row.LastAppearance)
	if err != nil {
		return nil, fmt.Errorf("parse population member %q last_appearance: %w", row.ResourceID, err)
	}
	lastInbound, err := parseTime(row.LastInbound)
	if err != nil {
		return nil, fmt.Errorf("parse population member %q last_inbound: %w", row.ResourceID, err)
	}
	return &domain.PopulationMember{
		ResourceID:     row.ResourceID,
		Item:           item,
		SessionName:    row.SessionName,
		Generation:     uint64(row.Generation),
		AcceptedAt:     acceptedAt,
		LastAppearance: lastAppearance,
		LastInbound:    lastInbound,
		Tombstoned:     row.Tombstoned != 0,
		PendingUp:      row.PendingUp != 0,
		LastDecision:   row.LastDecision,
		LastBlockers:   blockers,
	}, nil
}

func insertPopulationMemberTx(ctx context.Context, q *sqlcgen.Queries, key, resource string, member *domain.PopulationMember) error {
	itemJSON, err := json.Marshal(member.Item)
	if err != nil {
		return fmt.Errorf("marshal population member %q item: %w", resource, err)
	}
	blockersJSON, err := json.Marshal(member.LastBlockers)
	if err != nil {
		return fmt.Errorf("marshal population member %q blockers: %w", resource, err)
	}
	var tombstoned, pendingUp int64
	if member.Tombstoned {
		tombstoned = 1
	}
	if member.PendingUp {
		pendingUp = 1
	}
	if err := q.InsertPopulationMember(ctx, sqlcgen.InsertPopulationMemberParams{
		PopulationKey:    key,
		ResourceID:       resource,
		SessionName:      member.SessionName,
		Generation:       int64(member.Generation),
		AcceptedAt:       formatTime(member.AcceptedAt),
		LastAppearance:   formatTime(member.LastAppearance),
		LastInbound:      formatTime(member.LastInbound),
		Tombstoned:       tombstoned,
		PendingUp:        pendingUp,
		LastDecision:     member.LastDecision,
		ItemJson:         string(itemJSON),
		LastBlockersJson: string(blockersJSON),
	}); err != nil {
		return fmt.Errorf("insert population member %q: %w", resource, err)
	}
	return nil
}

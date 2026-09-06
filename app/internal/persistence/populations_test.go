package persistence

import (
	"context"
	"errors"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
)

var errBoom = errors.New("boom")

func TestPopulation_MissingReturnsNilNil(t *testing.T) {
	db := migratedTestDB(t)
	got, err := db.Population(context.Background(), "nonexistent")
	if err != nil {
		t.Fatalf("Population: %v", err)
	}
	if got != nil {
		t.Errorf("Population = %v, want nil", got)
	}
}

func TestUpdatePopulation_CreatesAndPersistsMembers(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	err := db.UpdatePopulation(ctx, "wf/pop", func(p *domain.PopulationState) error {
		p.Workflow = "wf"
		p.Name = "pop"
		p.Members["res-1"] = &domain.PopulationMember{ResourceID: "res-1", Generation: 1, PendingUp: true, Item: map[string]any{"k": "v"}}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdatePopulation: %v", err)
	}

	got, err := db.Population(ctx, "wf/pop")
	if err != nil {
		t.Fatalf("Population: %v", err)
	}
	if got == nil {
		t.Fatal("Population returned nil after UpdatePopulation")
	}
	if got.Workflow != "wf" || got.Name != "pop" {
		t.Errorf("population = %+v", got)
	}
	member := got.Members["res-1"]
	if member == nil {
		t.Fatal("member res-1 missing")
	}
	if member.Generation != 1 || !member.PendingUp {
		t.Errorf("member = %+v", member)
	}
	if member.Item["k"] != "v" {
		t.Errorf("member.Item = %v", member.Item)
	}
}

func TestUpdatePopulation_ReplacesMembersRatherThanAccumulating(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	mutate := func(fn func(*domain.PopulationState)) {
		if err := db.UpdatePopulation(ctx, "wf/pop", func(p *domain.PopulationState) error {
			fn(p)
			return nil
		}); err != nil {
			t.Fatalf("UpdatePopulation: %v", err)
		}
	}

	mutate(func(p *domain.PopulationState) {
		p.Members["a"] = &domain.PopulationMember{ResourceID: "a", Generation: 1}
	})
	mutate(func(p *domain.PopulationState) {
		delete(p.Members, "a")
		p.Members["b"] = &domain.PopulationMember{ResourceID: "b", Generation: 1}
	})

	got, err := db.Population(ctx, "wf/pop")
	if err != nil {
		t.Fatalf("Population: %v", err)
	}
	if _, ok := got.Members["a"]; ok {
		t.Error("member a survived after being removed from the callback's map")
	}
	if _, ok := got.Members["b"]; !ok {
		t.Error("member b missing")
	}
}

func TestUpdatePopulation_FnErrorAbortsWithoutWriting(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	wantErr := errBoom
	err := db.UpdatePopulation(ctx, "wf/pop", func(p *domain.PopulationState) error {
		p.Workflow = "should-not-persist"
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("UpdatePopulation error = %v, want %v", err, wantErr)
	}

	got, err := db.Population(ctx, "wf/pop")
	if err != nil {
		t.Fatalf("Population: %v", err)
	}
	if got != nil {
		t.Errorf("Population = %+v, want nil (fn's error must have prevented any write)", got)
	}
}

func TestUpdatePopulation_MembersMapIsNeverNilInsideFn(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	var sawNilMembers bool
	if err := db.UpdatePopulation(ctx, "wf/pop", func(p *domain.PopulationState) error {
		if p.Members == nil {
			sawNilMembers = true
		}
		return nil
	}); err != nil {
		t.Fatalf("UpdatePopulation: %v", err)
	}
	if sawNilMembers {
		t.Error("fn saw a nil Members map; callers index it directly and would panic")
	}
}

// Package state is the persistence-backed facade over core's durable
// runtime state: sessions, task instances, done_when/judge state,
// population state, and up-slot reservations. It is a thin translation
// seam over app/internal/persistence's SQLite-backed store — the type,
// constructor, and method set below are what the rest of core depends on,
// so they stay stable even though the storage underneath is not state.json.
package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/datahome"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
)

// UpReservation is an alias for the shared domain type: it must live
// outside this package so app/internal/persistence (a lower layer this
// package depends on) can also use it without importing state back.
type UpReservation = domain.UpReservation

// PopulationState is an alias for the shared domain type; see UpReservation.
type PopulationState = domain.PopulationState

// PopulationMember is an alias for the shared domain type; see UpReservation.
type PopulationMember = domain.PopulationMember

// ErrUpAlreadyReserved: childName's reservation is held by another live
// process — a second concurrent `plect up` for that child, not a sibling.
var ErrUpAlreadyReserved = domain.ErrUpAlreadyReserved

// Store manages session, task, population, and reservation persistence
// against a SQLite database in its data directory.
type Store struct {
	dir string
}

// NewStore creates a Store using the given directory to hold the database.
// If dir is empty, defaults to datahome.Resolve(). The database is not
// opened until the first call that needs it.
func NewStore(dir string) *Store {
	if dir == "" {
		dir = datahome.Resolve()
	}
	return &Store{dir: dir}
}

// Dir returns the directory holding the database. Co-located stores (e.g.
// the event log) derive their root from it so they share the same data
// home.
func (s *Store) Dir() string {
	return s.dir
}

// dbHandle opens and migrates the database on first use (via
// persistence.EnsureCurrentShared, the same gated entry point every plect
// command goes through), sharing the connection pool with any eventlog.Store
// over the same directory rather than each holding its own.
func (s *Store) dbHandle() (*persistence.DB, error) {
	db, err := persistence.EnsureCurrentShared(context.Background(), persistence.PathIn(s.dir))
	if err != nil {
		return nil, fmt.Errorf("state: open database: %w", err)
	}
	return db, nil
}

// CheckReadable verifies that the database can be opened, migrated, and
// read by this binary.
func (s *Store) CheckReadable() error {
	db, err := s.dbHandle()
	if err != nil {
		return err
	}
	_, err = db.AllSessions(context.Background())
	return err
}

// Get returns a session by name, or nil if not found or unreadable.
// Compatibility read paths degrade to nil when callers have no error
// return to propagate; new read paths should use GetE.
func (s *Store) Get(name string) *domain.Session {
	session, err := s.GetE(name)
	if err != nil {
		return nil
	}
	return session
}

// GetE returns a session by name while preserving read errors.
func (s *Store) GetE(name string) (*domain.Session, error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, err
	}
	return db.GetSession(context.Background(), name)
}

// Put saves or updates a session.
func (s *Store) Put(session *domain.Session) error {
	db, err := s.dbHandle()
	if err != nil {
		return fmt.Errorf("state: put %q: %w", session.Name, err)
	}
	if err := db.PutSession(context.Background(), session); err != nil {
		return fmt.Errorf("state: put %q: %w", session.Name, err)
	}
	return nil
}

// Update atomically applies fn to the named session and persists the
// result. This is the read-modify-write primitive for callers that may
// race with other plect processes (e.g. a watcher daemon merging task
// outputs while a lifecycle command runs). fn returning an error aborts
// without writing.
func (s *Store) Update(name string, fn func(*domain.Session) error) error {
	db, err := s.dbHandle()
	if err != nil {
		return fmt.Errorf("state: update %q: %w", name, err)
	}
	// fn's own error is returned verbatim, not wrapped: callers type-assert
	// or errors.Is against sentinel/typed errors fn returns (e.g. a service
	// *Error), and this is the read-modify-write primitive they call
	// through, so this seam must not obscure that type.
	return db.UpdateSession(context.Background(), name, fn)
}

// Population returns one population's durable state, or nil if key has
// never been recorded.
func (s *Store) Population(key string) (*PopulationState, error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, err
	}
	return db.Population(context.Background(), key)
}

// UpdatePopulation atomically applies fn to the named population (creating
// it, and its Members map, if absent) and persists the result.
func (s *Store) UpdatePopulation(key string, fn func(*PopulationState) error) error {
	db, err := s.dbHandle()
	if err != nil {
		return fmt.Errorf("state: update population %q: %w", key, err)
	}
	// fn's own error is returned verbatim; see Update's identical rationale.
	return db.UpdatePopulation(context.Background(), key, fn)
}

// ReserveUpSlot lets fn weigh every session and reservation together before
// admitting childName; it never overwrites a still-live reservation for
// childName itself (ErrUpAlreadyReserved).
func (s *Store) ReserveUpSlot(childName, parentName string, fn func(sessions map[string]*domain.Session, reservations map[string]UpReservation) (approved bool)) (bool, error) {
	db, err := s.dbHandle()
	if err != nil {
		return false, fmt.Errorf("state: reserve up-slot for %q: %w", childName, err)
	}
	approved, err := db.ReserveUpSlot(context.Background(), childName, parentName, fn)
	if err != nil {
		if errors.Is(err, ErrUpAlreadyReserved) {
			return false, err
		}
		return false, fmt.Errorf("state: reserve up-slot for %q: %w", childName, err)
	}
	return approved, nil
}

func (s *Store) ReleaseUpSlot(childName string) error {
	db, err := s.dbHandle()
	if err != nil {
		return fmt.Errorf("state: release up-slot for %q: %w", childName, err)
	}
	if err := db.ReleaseUpSlot(context.Background(), childName); err != nil {
		return fmt.Errorf("state: release up-slot for %q: %w", childName, err)
	}
	return nil
}

// Destroy transitions a session's live row to SessionStatusDestroyed
// (retaining the row and its task/event history) and releases its up-slot
// reservation. This is the persistence-backed session lifecycle's terminal
// transition, not a physical row delete: sessions are retained across
// destroy so a later create under the same name starts a new row with a
// new id rather than reviving this one.
func (s *Store) Destroy(name string) error {
	db, err := s.dbHandle()
	if err != nil {
		return fmt.Errorf("state: destroy %q: %w", name, err)
	}
	if err := db.DestroySession(context.Background(), name, time.Now()); err != nil {
		return fmt.Errorf("state: destroy %q: %w", name, err)
	}
	return nil
}

// FindByAlias returns all sessions whose create-time alias equals the given
// string. Multiple hits are possible (tag variants share the alias), so the
// caller decides how to disambiguate.
func (s *Store) FindByAlias(alias string) []*domain.Session {
	sessions, err := s.FindByAliasE(alias)
	if err != nil {
		return nil
	}
	return sessions
}

// FindByAliasE returns alias matches while preserving read errors.
func (s *Store) FindByAliasE(alias string) ([]*domain.Session, error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, err
	}
	return db.FindSessionsByAlias(context.Background(), alias)
}

// All returns all sessions.
func (s *Store) All() map[string]*domain.Session {
	sessions, err := s.AllE()
	if err != nil {
		return make(map[string]*domain.Session)
	}
	return sessions
}

// AllE returns all sessions while preserving read errors.
func (s *Store) AllE() (map[string]*domain.Session, error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, err
	}
	return db.AllSessions(context.Background())
}

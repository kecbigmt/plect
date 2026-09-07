// Package eventlogtest is a test-only helper for minting a session's next
// event-log incarnation directly, for packages whose tests exercise
// rotation (a superseded incarnation's own tail, a live reader crossing
// onto the new one) without going through the full state.Store session
// lifecycle. A session row now is one incarnation (see
// docs/design/sqlite-persistence.md), so unlike the retired
// event_streams-backed NewStream, minting a genuinely new one requires
// first retiring whatever row is currently live under that name.
package eventlogtest

import (
	"context"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// NewIncarnation mints session's next incarnation in the database rooted at
// dir (the same directory an eventlog.Store or state.Store over dir would
// use): a live row under that name is first destroyed, then a fresh one is
// created, mirroring `plect destroy` followed by `plect up`/`plect create`
// — the only way a session name gets a second, distinct incarnation. A
// session with no live row yet is simply created (matching NewStream's own
// "first mint" behavior).
func NewIncarnation(dir, session string) error {
	ctx := context.Background()
	db, err := persistence.EnsureCurrentShared(ctx, persistence.PathIn(dir))
	if err != nil {
		return fmt.Errorf("eventlogtest: open database: %w", err)
	}
	existing, err := db.GetSession(ctx, session)
	if err != nil {
		return fmt.Errorf("eventlogtest: check existing session %q: %w", session, err)
	}
	if existing != nil {
		if err := db.DestroySession(ctx, session, time.Now().UTC()); err != nil {
			return fmt.Errorf("eventlogtest: destroy existing incarnation of %q: %w", session, err)
		}
	}
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: session, Status: contract.SessionStatusDown, CreatedAt: now, UpdatedAt: now}); err != nil {
		return fmt.Errorf("eventlogtest: create incarnation of %q: %w", session, err)
	}
	return nil
}

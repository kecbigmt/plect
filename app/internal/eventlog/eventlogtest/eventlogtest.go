// Package eventlogtest mints a session's next event-log incarnation
// directly, for tests exercising rotation without the full state.Store
// lifecycle.
package eventlogtest

import (
	"context"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// NewIncarnation mints session's next incarnation in the database rooted
// at dir, destroying any live row under that name first.
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

package eventlog_test

import (
	"context"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

func TestEventlogAndStateShareOneConnectionPoolClosedByOwner(t *testing.T) {
	dir := t.TempDir()
	dbPath := persistence.PathIn(dir)

	if err := state.NewStore(dir).CheckReadable(); err != nil {
		t.Fatalf("state store touch: %v", err)
	}
	if _, _, _, err := eventlog.NewStore(dir).Append(event.Event{SessionName: "session-1", Type: "user.note", Direction: event.Internal}); err != nil {
		t.Fatalf("eventlog store touch: %v", err)
	}

	db, err := persistence.EnsureCurrentShared(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("resolve shared handle: %v", err)
	}
	if got := db.OpenConnections(); got == 0 {
		t.Fatalf("open connections while state and eventlog are both in use = %d, want > 0", got)
	}

	if err := persistence.CloseShared(dbPath); err != nil {
		t.Fatalf("close shared: %v", err)
	}
	if got := db.OpenConnections(); got != 0 {
		t.Fatalf("open connections after CloseShared = %d, want 0 (the owner's close must actually release the handle both Store types were using)", got)
	}

	if _, err := eventlog.NewStore(dir).StreamID("session-1"); err != nil {
		t.Fatalf("access after close: %v", err)
	}
}

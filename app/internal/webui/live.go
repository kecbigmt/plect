package webui

import (
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// LiveService is the production SessionService: it calls service.* against the
// real config and state store (the same code path as the CLI and MCP server).
type LiveService struct {
	cfg   *config.Config
	store *state.Store
}

// NewLiveService wires config.Load() and the default state store.
// store.CheckReadable() is plect-web's own entry point for the same
// storage.db currency check every other entry point performs (persistence.
// EnsureCurrent, via state.Store): plect-web has no cobra parent chain of
// its own (unlike the `plect` CLI's root.go) to run it ahead of time.
func NewLiveService() (*LiveService, error) {
	store := state.NewStore("")
	if err := store.CheckReadable(); err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return newLiveService(cfg, store), nil
}

// newLiveService injects the cfg and store so tests can supply a temp store
// without touching unexported fields.
func newLiveService(cfg *config.Config, store *state.Store) *LiveService {
	return &LiveService{cfg: cfg, store: store}
}

func (l *LiveService) List() ([]service.ListEntry, error) {
	return service.List(l.cfg, l.store)
}

func (l *LiveService) Status(name string) (*service.StatusResult, error) {
	return service.Status(l.cfg, l.store, name)
}

// eventTimelineLimit caps the detail-page timeline. The log is durable and
// unbounded (it survives destroy and stores bodies), and the page re-fetches
// every 5s, so reading/rendering the whole history would scale poorly; the
// newest N is what the timeline shows.
const eventTimelineLimit = 100

func (l *LiveService) Events(name string) ([]event.Event, error) {
	return service.EventRecent(l.cfg, l.store, name, eventTimelineLimit)
}

// EventsSubtree returns the most recent events of the session tree rooted at
// root (newest first), via the same merged read (EventPageSubtree) the CLI and
// MCP subtree views use — the web never re-implements the merge or ordering.
func (l *LiveService) EventsSubtree(root string) ([]event.Event, error) {
	page, err := service.EventPageSubtree(l.cfg, l.store, root, service.EventPageParams{
		Order:  event.OrderDesc,
		Filter: event.Filter{Limit: eventTimelineLimit},
	})
	if err != nil {
		return nil, err
	}
	return page.Events, nil
}

// EventPage returns one page of a single session's event history via opaque
// cursors — the webapi.SessionReader seam's history read. Unlike Events/
// EventsSubtree above (both fixed-size tails for the htmx timeline), this
// supports the JSON API's cursor-based paging unchanged from service.EventPage.
func (l *LiveService) EventPage(name string, p service.EventPageParams) (service.EventPageResult, error) {
	return service.EventPage(l.cfg, l.store, name, p)
}

func (l *LiveService) EventStreamResume(name, cursor string) (string, int64, error) {
	return service.EventStreamResume(l.cfg, l.store, name, cursor)
}

// PublishEvent appends an event to the session's log. The bus tailer fans the
// appended event to the live SSE stream, so an open timeline updates without a
// round-trip from this handler.
func (l *LiveService) PublishEvent(name string, p service.EventPublishParams) (event.Event, error) {
	return service.EventPublish(l.cfg, l.store, name, p)
}

func (l *LiveService) Create(p service.CreateParams) (*service.CreateResult, error) {
	return service.Create(l.cfg, l.store, p)
}

func (l *LiveService) Up(p service.UpParams) (*service.UpResult, error) {
	return service.Up(l.cfg, l.store, p)
}

func (l *LiveService) Down(p service.DownParams) (*service.DownResult, error) {
	return service.Down(l.cfg, l.store, p)
}

func (l *LiveService) Destroy(p service.DestroyParams) (*service.DestroyResult, error) {
	return service.Destroy(l.cfg, l.store, p)
}

package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// isolateMachineConfig strips the layers this machine owns, so a case that
// loads a real config still answers only for what it declares itself.
//
// Shared by both the integration- and browser-tagged acceptance suites (they
// never compile together), so it lives in a plain, untagged file rather than
// being duplicated per tag.
func isolateMachineConfig(cfg *config.Config) {
	cfg.BaseDir = ""
	cfg.PluginDirs = nil
	cfg.Plugins = nil
}

// sinceFromQuery parses a "since" offset query parameter, defaulting to the
// start of the log. Shared with startEventBusRelay below.
func sinceFromQuery(r *http.Request) int64 {
	v := r.URL.Query().Get("since")
	if v == "" {
		return 0
	}
	var n int64
	_, _ = fmt.Sscanf(v, "%d", &n)
	return n
}

// startEventBusRelay is a minimal bus that replays a session's events from
// the real store over SSE, from an offset given by a "since" query parameter
// — a stand-in for the real event bus daemon, exercising the JSON relay
// layer (app/internal/webui/events_stream_json.go's client side) and cursor
// decoding against a real service+eventlog stack, not the bus's own fan-out.
func startEventBusRelay(t *testing.T, cfg *config.Config, store *state.Store) *httptest.Server {
	t.Helper()
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		evs, offs, _, err := service.EventList(cfg, store, session, sinceFromQuery(r), event.Filter{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i, ev := range evs {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", offs[i], b)
		}
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(bus.Close)
	return bus
}

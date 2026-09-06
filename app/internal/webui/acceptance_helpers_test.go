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

func startEventBusRelay(t *testing.T, cfg *config.Config, store *state.Store) *httptest.Server {
	t.Helper()
	bus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		_, since, _ := event.ParseResumeToken(r.URL.Query().Get("since"))
		evs, offs, _, err := service.EventList(cfg, store, session, since, event.Filter{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		streamID, _, err := service.EventStreamResume(cfg, store, session, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i, ev := range evs {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %s\ndata: %s\n\n", event.EncodeResumeToken(streamID, offs[i]), b)
		}
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(bus.Close)
	return bus
}

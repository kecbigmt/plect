package service

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// ForwardDownSessionEvent relays ev, an inbound event that landed on
// origin's own log while origin is down, to origin's nearest live ancestor
// (resolveLiveAncestor) as a plect.resource.forwarded push. Reports false,
// nil when origin has no live ancestor (a root session, or one whose whole
// remaining chain is down/unhealthy): a structural fact, not a transient
// condition worth retrying.
func ForwardDownSessionEvent(cfg *config.Config, store *state.Store, origin string, ev event.Event) (bool, error) {
	s, err := store.GetE(origin)
	if err != nil {
		return false, err
	}
	if s == nil {
		return false, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q not found", origin)}
	}
	target, err := resolveLiveAncestor(cfg, store, origin)
	if err != nil {
		return false, err
	}
	if target == "" {
		return false, nil
	}
	meta := map[string]string{
		"forward_kind":   "resource",
		"resource":       s.ResourceID,
		"forwarded_type": ev.Type,
	}
	if url := ev.Metadata["url"]; url != "" {
		meta["forwarded_url"] = url
	}
	id, wakeErr, err := publishTerminalTo(cfg, store, origin, target, true, TerminalParams{
		Type:     event.TypeResourceForwarded,
		Summary:  ev.Summary,
		Body:     resourceForwardBody(origin, s.ResourceID, ev),
		Metadata: meta,
		DedupKey: origin + "|resource_forward|" + ev.ID,
	})
	if err != nil {
		return false, err
	}
	if wakeErr != nil {
		slog.Warn("resource-event forward delivered but ancestor wake failed", "session", origin, "target", target, "error", wakeErr)
	}
	return id != "", nil
}

func resourceForwardBody(origin, resource string, ev event.Event) string {
	lines := []string{
		fmt.Sprintf("%s's registered resource received an event while the session was down (not destroyed).", origin),
		"",
	}
	if resource != "" {
		lines = append(lines, "resource: "+resource)
	}
	lines = append(lines, "type: "+ev.Type, "summary: "+ev.Summary)
	if url := ev.Metadata["url"]; url != "" {
		lines = append(lines, "url: "+url)
	}
	return strings.Join(lines, "\n")
}

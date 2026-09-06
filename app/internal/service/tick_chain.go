package service

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

func publishAlreadyActiveChainKick(cfg *config.Config, store *state.Store, workSession string, sp ChainSpawn) (bool, error) {
	if sp.TargetSession == "" {
		return false, nil
	}
	dedupKey := chainKickDedupKey(workSession, sp)
	log := eventlog.NewStore(store.Dir())
	evs, _, _, err := log.List(sp.TargetSession, 0, event.Filter{
		Types:   []string{event.TypeUserEmit},
		Sources: []string{event.SourceTick},
	})
	if err != nil {
		return false, err
	}
	for _, ev := range evs {
		if ev.Metadata["chain_dedup_key"] == dedupKey {
			return false, nil
		}
	}

	meta := map[string]string{
		"work_session":    workSession,
		"chain_id":        sp.ChainID,
		"instance":        sp.Instance,
		"chain_dedup_key": dedupKey,
	}
	for _, key := range []string{"revision", "judge_ids", "pr_url"} {
		if v := chainInputString(sp.Inputs, key); v != "" {
			meta[key] = v
		}
	}
	_, err = EventPublish(cfg, store, sp.TargetSession, EventPublishParams{
		Type:      event.TypeUserEmit,
		Source:    event.SourceTick,
		Direction: event.Outbound,
		Summary:   fmt.Sprintf("Re-evaluate %s at revision %s", workSession, metaValue(meta, "revision", "current")),
		Body:      chainKickBody(workSession, sp, meta),
		Metadata:  meta,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

const chainAttemptReasonCap = "cap"

// publishChainCapAttempt appends one plect.chain.attempt event to the ticking
// session's own log (workSession) when its chain's spawn was refused by the
// parent's max_up_children cap. It dedupes against that session's own recent
// chain-attempt events on (chain_id, instance, target, reason), but only
// within the current refusal streak: a later recurrence of the identical
// tuple, once targetSpawnedSince proves the derived target actually spawned
// and was torn down in between, is a new streak and gets its own event —
// matching evalDocumentChain's own reasoning that a cap-refused entry never
// keeps a target's identity from resolving the same way twice.
func publishChainCapAttempt(cfg *config.Config, store *state.Store, workSession string, sp ChainSpawn) error {
	if sp.TargetSession == "" {
		return nil
	}
	log := eventlog.NewStore(store.Dir())
	evs, _, _, err := log.List(workSession, 0, event.Filter{
		Types:   []string{event.TypeChainAttempt},
		Sources: []string{event.SourceTick},
	})
	if err != nil {
		return err
	}
	var lastMatch *event.Event
	for i := range evs {
		ev := &evs[i]
		if ev.Metadata["chain_id"] != sp.ChainID ||
			ev.Metadata["instance"] != sp.Instance ||
			ev.Metadata["target"] != sp.TargetSession ||
			ev.Metadata["reason"] != chainAttemptReasonCap {
			continue
		}
		if lastMatch == nil || ev.Time.After(lastMatch.Time) {
			lastMatch = ev
		}
	}
	if lastMatch != nil {
		spawned, err := targetSpawnedSince(cfg, store, sp.TargetSession, lastMatch.Time)
		if err != nil {
			return err
		}
		if !spawned {
			return nil
		}
	}
	_, err = EventPublish(cfg, store, workSession, EventPublishParams{
		Type:      event.TypeChainAttempt,
		Source:    event.SourceTick,
		Direction: event.Internal,
		Summary:   fmt.Sprintf("chain %s refused: parent at its max_up_children cap (target %s)", sp.ChainID, sp.TargetSession),
		Body:      strings.Join(sp.Warnings, "\n"),
		Metadata: map[string]string{
			"chain_id": sp.ChainID,
			"instance": sp.Instance,
			"target":   sp.TargetSession,
			"reason":   chainAttemptReasonCap,
		},
	})
	return err
}

// targetSpawnedSince reports whether target was actually created after t —
// evidence that an earlier refusal's streak ended (capacity freed and the
// chain fired) before a later refusal for the identical tuple began a new
// one. A currently-live session newer than t is that evidence directly; a
// torn-down one leaves it in its tombstone's CreatedAt, since a cap refusal
// is only ever reported for a target that does not presently exist —
// evalDocumentChain reports an existing one as already-active instead of
// retrying the fire.
func targetSpawnedSince(cfg *config.Config, store *state.Store, target string, t time.Time) (bool, error) {
	if s := store.Get(target); s != nil && s.CreatedAt.After(t) {
		return true, nil
	}
	tomb, err := lookupTombstone(cfg, store, target)
	if err != nil {
		return false, err
	}
	return tomb != nil && tomb.CreatedAt.After(t), nil
}

func chainKickDedupKey(workSession string, sp ChainSpawn) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		workSession,
		sp.TargetSession,
		sp.ChainID,
		sp.Instance,
		chainInputString(sp.Inputs, "revision"),
		chainInputString(sp.Inputs, "judge_ids"),
	}, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

func chainInputString(inputs map[string]any, key string) string {
	if len(inputs) == 0 {
		return ""
	}
	v, ok := inputs[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func metaValue(meta map[string]string, key, fallback string) string {
	if v := strings.TrimSpace(meta[key]); v != "" {
		return v
	}
	return fallback
}

func chainKickBody(workSession string, sp ChainSpawn, meta map[string]string) string {
	lines := []string{
		fmt.Sprintf("Re-evaluate the work session `%s`.", workSession),
		"",
		fmt.Sprintf("- PR: %s", metaValue(meta, "pr_url", sp.Resource)),
		fmt.Sprintf("- Revision: %s", metaValue(meta, "revision", "current")),
		fmt.Sprintf("- Pending judge ids: %s", metaValue(meta, "judge_ids", "(unspecified)")),
		"",
		"Record one `plect judge` action per pending judge id against the work session.",
	}
	return strings.Join(lines, "\n")
}

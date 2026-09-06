package service

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"

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

func chainAttemptFingerprint(capRefused bool, target string) string {
	if !capRefused || target == "" {
		return ""
	}
	return chainAttemptReasonCap + "|" + target
}

// chainAttemptStreamID scopes a session's chain-attempt markers to this
// particular incarnation of its name via its event stream's own id: a
// session create mints a fresh stream, so a destroy-then-recreate under the
// same name gets a new one and never shares a key with the incarnation it
// replaced. A lookup failure logs and falls back to "" rather than failing
// the tick.
func chainAttemptStreamID(store *state.Store, sessionName string) string {
	id, err := eventlog.NewStore(store.Dir()).StreamID(sessionName)
	if err != nil {
		slog.Default().Warn("tick: read event stream id for chain-attempt scoping failed", "session", sessionName, "error", err)
		return ""
	}
	return id
}

// syncChainAttemptStreak atomically compares-and-sets the persisted
// chain-attempt streak marker for one instance's chain, reporting the prior
// value (for revertChainAttemptStreak) and whether this call is the one that
// changed it. The event log alone cannot tell an interrupted refusal streak
// from an uninterrupted one — it only ever records a refusal, so a
// resolved-then-refused-again recurrence looks identical to a continuing one
// — so TickSession keeps this boundary marker instead, synced for every
// chain on every tick regardless of outcome.
func syncChainAttemptStreak(store *state.Store, sessionName, instance, chainID, scope, newFingerprint string) (previous string, won bool, err error) {
	return eventlog.NewStore(store.Dir()).SwapChainAttempt(sessionName, instance, chainID, scope, newFingerprint)
}

// revertChainAttemptStreak compensates a syncChainAttemptStreak win (claimed)
// whose event never actually got published, restoring previous — but only if
// the marker still holds claimed. See eventlog.Store.RevertChainAttempt for
// why the restore must stay conditional.
func revertChainAttemptStreak(store *state.Store, sessionName, instance, chainID, scope, claimed, previous string) error {
	_, err := eventlog.NewStore(store.Dir()).RevertChainAttempt(sessionName, instance, chainID, scope, claimed, previous)
	return err
}

// publishChainCapAttempt appends one plect.chain.attempt event to the ticking
// session's own log (workSession) when its chain's spawn was refused by the
// parent's max_up_children cap. The caller (TickSession) only calls this once
// syncChainAttemptStreak has confirmed the refusal starts a new streak.
func publishChainCapAttempt(cfg *config.Config, store *state.Store, workSession string, sp ChainSpawn) error {
	if sp.TargetSession == "" {
		return nil
	}
	_, err := EventPublish(cfg, store, workSession, EventPublishParams{
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

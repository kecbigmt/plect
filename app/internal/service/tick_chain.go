package service

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
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

// chainAttemptFingerprint is the contract.TaskState.ChainAttempts[chainID]
// value that marks an ongoing cap-refusal streak; any other outcome —
// spawned, already-active, or the predicate not holding at all — fingerprints
// as "", which is what ends a streak.
func chainAttemptFingerprint(capRefused bool, target string) string {
	if !capRefused || target == "" {
		return ""
	}
	return chainAttemptReasonCap + "|" + target
}

// syncChainAttemptStreak persists newFingerprint for one instance's chain and
// reports whether it actually changed. The event log alone cannot tell an
// interrupted refusal streak from an uninterrupted one — it only ever
// records a refusal, so a resolved-then-refused-again recurrence looks
// identical to a continuing one — so TickSession keeps this boundary marker
// instead, updated for every chain on every tick regardless of outcome.
func syncChainAttemptStreak(store *state.Store, sessionName, instance, chainID, newFingerprint string) (changed bool, err error) {
	session := store.Get(sessionName)
	if session == nil {
		return false, nil
	}
	if st := session.Tasks[instance]; st == nil || st.ChainAttempts[chainID] == newFingerprint {
		return false, nil
	}
	if err := store.Update(sessionName, func(s *domain.Session) error {
		st := s.Tasks[instance]
		if st == nil {
			return nil
		}
		if newFingerprint == "" {
			delete(st.ChainAttempts, chainID)
			return nil
		}
		if st.ChainAttempts == nil {
			st.ChainAttempts = map[string]string{}
		}
		st.ChainAttempts[chainID] = newFingerprint
		return nil
	}); err != nil {
		return false, err
	}
	return true, nil
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

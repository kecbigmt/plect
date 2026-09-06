package dispatch

import (
	"log/slog"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// nextChannelFailure computes the next streak state given the previous one
// (nil for no open streak) and one more observed failure. It never mutates
// prev, so a caller can safely read prev again if the state.Store.Update it's
// inside retries. The escalation decision itself is not made here — it lives
// in internal/service (service.CheckChannelHealth, driven by the reactor)
// because it needs service.Up's wake-if-down behavior, and internal/service
// already imports internal/dispatch (see internal/reactor/supervisor.go's
// package doc for the identical constraint on tick), so this package cannot
// call back into it.
func nextChannelFailure(prev *contract.ChannelHealth, channelName string, cause error, at time.Time) *contract.ChannelHealth {
	ch := &contract.ChannelHealth{}
	if prev != nil {
		*ch = *prev
	}
	if ch.ConsecutiveFailures == 0 {
		ch.FirstFailureAt = at
	}
	ch.ConsecutiveFailures++
	ch.LastFailureAt = at
	ch.LastChannel = channelName
	if cause != nil {
		ch.LastError = cause.Error()
	}
	return ch
}

// recordValidationFailure appends one failure to the session's channel
// validation streak (Session.ChannelValidationHealth) — independent of
// Session.ChannelDeliveryHealth, since validation (checked once, at a
// dispatcher's build) and delivery (checked per event) run on unrelated
// schedules.
func recordValidationFailure(st *state.Store, session string, cause error, at time.Time) {
	err := st.Update(session, func(s *domain.Session) error {
		s.ChannelValidationHealth = nextChannelFailure(s.ChannelValidationHealth, "", cause, at)
		s.UpdatedAt = at
		return nil
	})
	if err != nil {
		slog.Default().Warn("dispatcher: persist channel validation failure failed", "session", session, "error", err)
	}
}

// recordDeliveryFailure appends one failure to the session's channel
// delivery streak (Session.ChannelDeliveryHealth); see recordValidationFailure.
func recordDeliveryFailure(st *state.Store, session, channelName string, cause error, at time.Time) {
	err := st.Update(session, func(s *domain.Session) error {
		s.ChannelDeliveryHealth = nextChannelFailure(s.ChannelDeliveryHealth, channelName, cause, at)
		s.UpdatedAt = at
		return nil
	})
	if err != nil {
		slog.Default().Warn("dispatcher: persist channel delivery failure failed", "session", session, "error", err)
	}
}

// recordValidationSuccess clears an open validation streak, ending that
// episode; a later validation failure starts a new one, free to escalate
// again. It never touches ChannelDeliveryHealth — a validation success says
// nothing about whether a separate, still-open delivery failure has been
// fixed. The cheap Get check avoids a session persist on the path where
// nothing needs clearing, the overwhelming majority of validations.
func recordValidationSuccess(st *state.Store, session string) {
	if s := st.Get(session); s == nil || s.ChannelValidationHealth == nil || s.ChannelValidationHealth.ConsecutiveFailures == 0 {
		return
	}
	err := st.Update(session, func(s *domain.Session) error {
		if s.ChannelValidationHealth == nil || s.ChannelValidationHealth.ConsecutiveFailures == 0 {
			return nil
		}
		s.ChannelValidationHealth = nil
		s.UpdatedAt = time.Now()
		return nil
	})
	if err != nil {
		slog.Default().Warn("dispatcher: clear channel validation failure failed", "session", session, "error", err)
	}
}

// recordDeliverySuccess clears an open delivery streak; see
// recordValidationSuccess (mirrored for Session.ChannelDeliveryHealth).
func recordDeliverySuccess(st *state.Store, session string) {
	if s := st.Get(session); s == nil || s.ChannelDeliveryHealth == nil || s.ChannelDeliveryHealth.ConsecutiveFailures == 0 {
		return
	}
	err := st.Update(session, func(s *domain.Session) error {
		if s.ChannelDeliveryHealth == nil || s.ChannelDeliveryHealth.ConsecutiveFailures == 0 {
			return nil
		}
		s.ChannelDeliveryHealth = nil
		s.UpdatedAt = time.Now()
		return nil
	})
	if err != nil {
		slog.Default().Warn("dispatcher: clear channel delivery failure failed", "session", session, "error", err)
	}
}

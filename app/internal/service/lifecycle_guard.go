package service

import (
	"fmt"
	"os"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// checkLifecycleRelationGuard authorizes a destructive lifecycle operation
// (down/destroy) by tree relation: only the target session itself or one of
// its descendants (its own dispatched subtree) may act on it. The caller
// identity is the ambient PLECT_SESSION_NAME; its absence means a human is
// running the raw CLI outside any session pane, which stays exempt so manual
// recovery is never blocked by this guard.
func checkLifecycleRelationGuard(store *state.Store, targetName, op string) *Error {
	caller := os.Getenv("PLECT_SESSION_NAME")
	if caller == "" {
		return nil
	}
	sessions, err := store.AllE()
	if err != nil {
		return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("read session state: %v", err)}
	}
	switch rel := domain.RelationFromTarget(sessions, caller, targetName); rel {
	case domain.RelationSelf, domain.RelationChild, domain.RelationDescendant:
		return nil
	default:
		return &Error{
			Code: ErrRelationNotAllowed,
			Message: fmt.Sprintf(
				"session %q cannot %s session %q: relation is %q, not self or a descendant",
				caller, op, targetName, rel,
			),
		}
	}
}

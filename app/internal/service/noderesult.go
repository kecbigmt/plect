package service

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
)

// noopTaskObserver: several lifecycle callers pass no Observer at all.
type noopTaskObserver struct{}

func (noopTaskObserver) OnStart(string, string)                                 {}
func (noopTaskObserver) OnSkip(string, string, string)                          {}
func (noopTaskObserver) OnSuccess(string, string, time.Duration, []byte)        {}
func (noopTaskObserver) OnFailure(string, string, time.Duration, error, []byte) {}

type nodeResultObserver struct {
	inner       task.Observer
	log         *eventlog.Store
	sessionName string
}

// withNodeResultRecording wraps inner (nil becomes noopTaskObserver) so
// task.RunSetup/RunCleanup's plect.node.result reports are appended to
// sessionName's log, in addition to inner's own CLI/UI rendering.
func withNodeResultRecording(store *state.Store, sessionName string, inner task.Observer) task.Observer {
	if inner == nil {
		inner = noopTaskObserver{}
	}
	return &nodeResultObserver{inner: inner, log: eventlog.NewStore(store.Dir()), sessionName: sessionName}
}

func (o *nodeResultObserver) OnStart(scope, id string)        { o.inner.OnStart(scope, id) }
func (o *nodeResultObserver) OnSkip(scope, id, reason string) { o.inner.OnSkip(scope, id, reason) }
func (o *nodeResultObserver) OnSuccess(scope, id string, elapsed time.Duration, stderr []byte) {
	o.inner.OnSuccess(scope, id, elapsed, stderr)
}
func (o *nodeResultObserver) OnFailure(scope, id string, elapsed time.Duration, err error, stderr []byte) {
	o.inner.OnFailure(scope, id, elapsed, err, stderr)
}

// A failed append is swallowed, like the population engine's own
// event.Event writes: it must not abort the lifecycle operation.
func (o *nodeResultObserver) OnResult(scope, node, effectID, action, result string, elapsed time.Duration, body string) {
	_, _, _, _ = o.log.Append(event.Event{
		SessionName: o.sessionName,
		Type:        event.TypeNodeResult,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     fmt.Sprintf("%s %s %s", node, action, result),
		Body:        body,
		Metadata: map[string]string{
			"node":        node,
			"effect":      effectID,
			"scope":       scope,
			"action":      action,
			"result":      result,
			"duration_ms": strconv.FormatInt(elapsed.Milliseconds(), 10),
		},
	})
}

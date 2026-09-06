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

// noopTaskObserver lets nodeResultObserver always hold a callable inner
// Observer. Many lifecycle callers (population repair, tick's own re-up,
// mcpserver.Up) pass no Observer at all — exactly the unattended paths
// decision 6 of the runtime-failure-model ADR most needs a durable record
// from — so nodeResultObserver must not assume a real one underneath it.
type noopTaskObserver struct{}

func (noopTaskObserver) OnStart(string, string)                                 {}
func (noopTaskObserver) OnSkip(string, string, string)                          {}
func (noopTaskObserver) OnSuccess(string, string, time.Duration, []byte)        {}
func (noopTaskObserver) OnFailure(string, string, time.Duration, error, []byte) {}

// nodeResultObserver wraps a task.Observer with plect.node.result recording:
// every terminal node outcome task.RunSetup/RunCleanup reports (via the
// optional task.ResultObserver extension) is appended to sessionName's own
// event log, in addition to being forwarded to inner for CLI/UI rendering.
//
// Delivering the event to a workflow channel is the ordinary session
// dispatcher's job — it already follows this same log — so this type's only
// responsibility is getting the fact durably recorded, for a manual
// session, a child session, and a population member alike: appending to
// sessionName's own log rather than depending on any particular caller
// makes that "alike" hold, since every lifecycle entry point wraps its
// Observer with this before running setup or cleanup (the runtime-failure-model
// ADR's decision on population-produced sessions).
type nodeResultObserver struct {
	inner       task.Observer
	log         *eventlog.Store
	sessionName string
}

// withNodeResultRecording returns an Observer that also appends
// plect.node.result to sessionName's log, wrapping inner (nil is fine — see
// noopTaskObserver) for the CLI/UI callbacks. Call sites reassign their own
// Observer field with the result before it reaches task.RunSetup/RunCleanup;
// wrapping an already-wrapped Observer is harmless; reportResult (task.go)
// asserts only the outermost value RunSetup/RunCleanup was actually given,
// so exactly one event is still appended, to the same sessionName either way.
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

// OnResult implements task.ResultObserver. A best-effort append (like the
// population engine's own event.Event writes) — a node result missing from
// the log is a lesser failure than aborting the lifecycle operation over a
// log-append error the caller has no way to act on.
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

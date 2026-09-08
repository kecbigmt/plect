package channel

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/contracts/event"
)

// RetryPolicy bounds a channel worker's delivery attempts. DefaultRetryPolicy is
// the baseline; a channel can shorten an attempt via its own timeout.
type RetryPolicy struct {
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Timeout     time.Duration // per-attempt timeout when the channel sets none
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseBackoff: 200 * time.Millisecond,
		MaxBackoff:  2 * time.Second,
		Timeout:     5 * time.Second,
	}
}

// DeliverWithRetry attempts delivery up to policy.MaxAttempts with exponential
// backoff, returning the attempts made and the final error (nil on success).
// Each attempt is bounded by the channel's own timeout, falling back to
// policy.Timeout. A cancelled ctx ends the loop and is returned.
func DeliverWithRetry(ctx context.Context, def config.ChannelDefinition, inputs map[string]any, ev event.Event, policy RetryPolicy) (int, error) {
	timeout, err := ResolveTimeout(def, inputs)
	if err != nil {
		return 0, err
	}
	return retry(ctx, policy, timeout, func(attemptCtx context.Context) error {
		return Deliver(attemptCtx, def, inputs, ev)
	})
}

// DeliverWithRetryAndOptions is DeliverWithRetry with the full set of
// per-delivery overrides — see DeliverWithOptions.
func DeliverWithRetryAndOptions(ctx context.Context, def config.ChannelDefinition, inputs map[string]any, ev event.Event, policy RetryPolicy, opts DeliverOptions) (int, error) {
	timeout, err := ResolveTimeout(def, inputs)
	if err != nil {
		return 0, err
	}
	return retry(ctx, policy, timeout, func(attemptCtx context.Context) error {
		return DeliverWithOptions(attemptCtx, def, inputs, ev, opts)
	})
}

// ResolveTimeout resolves a definition's `timeout` against the resolved
// channel inputs. The evaluation carries inputs alone: the per-attempt
// deadline is a property of the channel wiring, and letting one event's
// payload shorten or lengthen it would make delivery timing an
// attacker-influenced value.
func ResolveTimeout(def config.ChannelDefinition, inputs map[string]any) (time.Duration, error) {
	if def.Timeout == nil {
		return 0, nil
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	eval := lang.Eval{Roots: lang.Roots{"inputs": inputs}}
	resolved, _, err := eval.Argument(def.Timeout)
	if err != nil {
		return 0, fmt.Errorf("channel timeout: %w", err)
	}
	d, err := config.ParseDuration(strings.TrimSpace(resolved))
	if err != nil {
		return 0, fmt.Errorf("channel timeout %q: %w", resolved, err)
	}
	return d, nil
}

// retry runs attempt up to policy.MaxAttempts with exponential backoff, bounding
// each by perAttemptTimeout (or policy.Timeout when unset). Split from
// DeliverWithRetry so the backoff/attempt-count logic is testable without a real
// socket or process.
func retry(ctx context.Context, policy RetryPolicy, perAttemptTimeout time.Duration, attempt func(context.Context) error) (int, error) {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	timeout := perAttemptTimeout
	if timeout <= 0 {
		timeout = policy.Timeout
	}
	backoff := policy.BaseBackoff
	var lastErr error
	for n := 1; n <= policy.MaxAttempts; n++ {
		attemptCtx, cancel := ctx, func() {}
		if timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		err := attempt(attemptCtx)
		cancel()
		if err == nil {
			return n, nil
		}
		lastErr = err
		if n == policy.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if policy.MaxBackoff > 0 && backoff > policy.MaxBackoff {
			backoff = policy.MaxBackoff
		}
	}
	return policy.MaxAttempts, lastErr
}

// ChannelValidationErrorEvent builds the plect.channel.error event the
// supervisor appends when a session's declared event channels fail to
// validate against the loaded channel definitions — the WARN log alone is
// visible only in journalctl, which nobody watches during an outage. It has
// no originating event or per-channel name (validation runs once over the
// whole workflow declaration, before any specific event is dispatched), so
// unlike ChannelErrorEvent it carries the workflow id instead.
func ChannelValidationErrorEvent(session, workflowID string, cause error) event.Event {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return event.Event{
		SessionName: session,
		Type:        event.TypeChannelError,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     fmt.Sprintf("event channels did not validate for workflow %s", workflowID),
		Body:        reason,
		Metadata: map[string]string{
			"workflow": workflowID,
		},
	}
}

// ChannelErrorEvent builds the plect.channel.error event a worker appends after
// exhausting retries. Metadata (channel/event_id/attempts) lets a reader trace
// the failure and lets the dispatcher scope relay: `channel` names the
// channel whose own delivery this records, so a relay can exclude it without
// excluding every other channel; `event_type` carries orig's type so a
// relay can tell a channel.error about an ordinary event apart from one
// about another channel.error (the mutual-failure case).
func ChannelErrorEvent(orig event.Event, channelName string, attempts int, cause error) event.Event {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	return event.Event{
		SessionName: orig.SessionName,
		Type:        event.TypeChannelError,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     fmt.Sprintf("event channel %s failed after %d attempts", channelName, attempts),
		Body:        fmt.Sprintf("failed to deliver %s to channel %s: %s", orig.Type, channelName, reason),
		Metadata: map[string]string{
			"channel":    channelName,
			"event_id":   orig.ID,
			"attempts":   strconv.Itoa(attempts),
			"event_type": orig.Type,
		},
	}
}

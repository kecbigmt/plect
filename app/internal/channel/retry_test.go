package channel

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/contracts/event"
)

func fastPolicy(maxAttempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: maxAttempts, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Timeout: time.Second}
}

func TestRetry_SucceedsFirstAttempt(t *testing.T) {
	calls := 0
	attempts, err := retry(context.Background(), fastPolicy(3), 0, func(context.Context) error {
		calls++
		return nil
	})
	if err != nil || attempts != 1 || calls != 1 {
		t.Fatalf("attempts=%d calls=%d err=%v, want 1/1/nil", attempts, calls, err)
	}
}

func TestRetry_RecoversAfterFailures(t *testing.T) {
	calls := 0
	attempts, err := retry(context.Background(), fastPolicy(5), 0, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("flaky")
		}
		return nil
	})
	if err != nil || attempts != 3 || calls != 3 {
		t.Fatalf("attempts=%d calls=%d err=%v, want 3/3/nil", attempts, calls, err)
	}
}

func TestRetry_Exhausts(t *testing.T) {
	calls := 0
	want := errors.New("always")
	attempts, err := retry(context.Background(), fastPolicy(3), 0, func(context.Context) error {
		calls++
		return want
	})
	if attempts != 3 || calls != 3 || !errors.Is(err, want) {
		t.Fatalf("attempts=%d calls=%d err=%v, want 3/3/%v", attempts, calls, err, want)
	}
}

func TestRetry_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	attempts, err := retry(ctx, RetryPolicy{MaxAttempts: 5, BaseBackoff: time.Hour, Timeout: time.Second}, 0, func(context.Context) error {
		calls++
		cancel() // cancel during the first attempt; the backoff wait should abort
		return errors.New("fail")
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 || calls != 1 {
		t.Fatalf("attempts=%d calls=%d err=%v, want 1/1/context.Canceled", attempts, calls, err)
	}
}

func TestRetry_PerAttemptTimeoutCutsAttempt(t *testing.T) {
	calls, deadlines := 0, 0
	// perAttemptTimeout (5ms) must bound each attempt even though policy.Timeout
	// is an hour, and the loop must keep retrying after a timed-out attempt.
	policy := RetryPolicy{MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, Timeout: time.Hour}
	attempts, err := retry(context.Background(), policy, 5*time.Millisecond, func(ctx context.Context) error {
		calls++
		if _, ok := ctx.Deadline(); ok {
			deadlines++
		}
		<-ctx.Done() // block until the per-attempt timeout fires
		return ctx.Err()
	})
	if attempts != 2 || calls != 2 || deadlines != 2 || err == nil {
		t.Fatalf("attempts=%d calls=%d deadlines=%d err=%v, want 2/2/2/non-nil", attempts, calls, deadlines, err)
	}
}

func TestDeliverWithRetry_ExhaustsOnBadSocket(t *testing.T) {
	def := config.ChannelDefinition{
		Type: config.ChannelTypeUnixSocket,
		Path: &lang.Value{Form: lang.FormLiteral, Literal: filepath.Join(t.TempDir(), "absent.sock")},
		Body: &lang.Value{Form: lang.FormLiteral, Literal: "{}"},
	}
	attempts, err := DeliverWithRetry(context.Background(), def, nil, event.Event{Type: event.TypeInstruction}, fastPolicy(3))
	if err == nil {
		t.Fatal("expected error delivering to a non-existent socket")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestChannelErrorEvent(t *testing.T) {
	orig := event.Event{ID: "01ABC", SessionName: "widget-1", Type: event.TypeInstruction}
	ce := ChannelErrorEvent(orig, "runtime", 3, errors.New("delivery exited 1"))
	if ce.Type != event.TypeChannelError {
		t.Errorf("Type = %q, want %q", ce.Type, event.TypeChannelError)
	}
	if ce.Source != event.SourcePlect || ce.Direction != event.Internal {
		t.Errorf("source/direction = %q/%q", ce.Source, ce.Direction)
	}
	if ce.SessionName != "widget-1" {
		t.Errorf("session not carried: %+v", ce)
	}
	if ce.Metadata["channel"] != "runtime" || ce.Metadata["event_id"] != "01ABC" || ce.Metadata["attempts"] != "3" || ce.Metadata["event_type"] != event.TypeInstruction {
		t.Errorf("metadata = %+v", ce.Metadata)
	}
}

// TestChannelErrorEvent_OrigChannelErrorCarriesEventType pins the data the
// dispatcher's mutual-failure guard reads: a channel.error about a failed
// relay of another channel.error must itself be marked as such, so it can be
// told apart from a channel.error about an ordinary event.
func TestChannelErrorEvent_OrigChannelErrorCarriesEventType(t *testing.T) {
	orig := event.Event{ID: "01DEF", SessionName: "widget-1", Type: event.TypeChannelError}
	ce := ChannelErrorEvent(orig, "b", 1, errors.New("delivery exited 1"))
	if ce.Metadata["event_type"] != event.TypeChannelError {
		t.Errorf("event_type = %q, want %q", ce.Metadata["event_type"], event.TypeChannelError)
	}
}

func TestChannelErrorEvent_NilCause(t *testing.T) {
	ce := ChannelErrorEvent(event.Event{ID: "x", Type: event.TypeInstruction}, "runtime", 1, nil)
	if ce.Type != event.TypeChannelError || ce.Metadata["attempts"] != "1" {
		t.Errorf("nil-cause error event malformed: %+v", ce)
	}
}

func TestResolveTimeout_LiteralAndProjection(t *testing.T) {
	literal := func(v string) *lang.Value { return &lang.Value{Form: lang.FormLiteral, Literal: v} }
	projection := func(path string) *lang.Value { return &lang.Value{Form: lang.FormFrom, From: path} }
	tests := []struct {
		name    string
		def     config.ChannelDefinition
		inputs  map[string]any
		want    time.Duration
		wantErr bool
	}{
		{name: "absent is zero", def: config.ChannelDefinition{}},
		{name: "literal", def: config.ChannelDefinition{Timeout: literal("5s")}, want: 5 * time.Second},
		{
			name:   "a projection reads the resolved input",
			def:    config.ChannelDefinition{Timeout: projection("inputs.enqueue_timeout")},
			inputs: map[string]any{"enqueue_timeout": "30s"},
			want:   30 * time.Second,
		},
		{
			name:    "a projection resolving to a non-duration fails delivery",
			def:     config.ChannelDefinition{Timeout: projection("inputs.enqueue_timeout")},
			inputs:  map[string]any{"enqueue_timeout": "soon"},
			wantErr: true,
		},
		{
			name:    "a projection of an unset input fails delivery",
			def:     config.ChannelDefinition{Timeout: projection("inputs.enqueue_timeout")},
			inputs:  map[string]any{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveTimeout(tt.def, tt.inputs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveTimeout = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTimeout: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveTimeout = %v, want %v", got, tt.want)
			}
		})
	}
}

// The deadline is a property of the channel wiring, so an event's payload
// must not be able to reach it even if a definition tried.
func TestResolveTimeout_EventDataIsOutOfScope(t *testing.T) {
	def := config.ChannelDefinition{Timeout: &lang.Value{Form: lang.FormFrom, From: "event.body"}}
	if _, err := ResolveTimeout(def, map[string]any{}); err == nil {
		t.Fatal("expected a timeout reading event data to fail")
	}
}

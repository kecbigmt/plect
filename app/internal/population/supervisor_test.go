package population

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/admitstatus"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
)

type restartingQueryRunner struct {
	calls  atomic.Int32
	cancel context.CancelFunc
}

func (r *restartingQueryRunner) Poll(context.Context, Definition) ([]map[string]any, error) {
	return nil, nil
}

func (r *restartingQueryRunner) Subscribe(context.Context, Definition, func(map[string]any) error) error {
	if r.calls.Add(1) == 2 {
		r.cancel()
	}
	return errors.New("source exited")
}

func TestSubscriptionRestartsAfterSourceFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &restartingQueryRunner{cancel: cancel}
	done := make(chan struct{})
	go func() {
		(&Supervisor{}).runSubscription(ctx, runner, Definition{}, make(chan sourceResult, 2))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription did not restart under bounded backoff")
	}
	if runner.calls.Load() != 2 {
		t.Fatalf("subscribe calls = %d, want restart after first failure", runner.calls.Load())
	}
}

type recordingRunner struct {
	polls      atomic.Int32
	subscribes atomic.Int32
}

func (r *recordingRunner) Poll(context.Context, Definition) ([]map[string]any, error) {
	r.polls.Add(1)
	return nil, nil
}

func (r *recordingRunner) Subscribe(ctx context.Context, _ Definition, _ func(map[string]any) error) error {
	r.subscribes.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestRunEvaluatorOnlyStartsSelectedMeans(t *testing.T) {
	store := state.NewStore(t.TempDir())
	def := Definition{
		Workflow: config.WorkflowFile{Address: "agent"},
		Population: config.WorkflowPopulation{
			Name:      "dispatch",
			Uses:      []string{"poll"},
			PollEvery: config.Duration{Duration: 10 * time.Millisecond},
		},
		Observer: config.ResourceDef{Query: &config.ResourceQuery{Poll: &lang.Action{}, Subscribe: &lang.Action{}}},
	}
	logStore := eventlog.NewStore(store.Dir())
	engine := NewEngine(def, store, logStore, admitstatus.NewCache(logStore), Hooks{})
	runner := &recordingRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	(&Supervisor{}).runEvaluator(ctx, engine, runner)
	if runner.polls.Load() == 0 {
		t.Fatal("poll never ran despite being selected")
	}
	if runner.subscribes.Load() != 0 {
		t.Fatalf("subscribe ran %d times though `uses` did not select it", runner.subscribes.Load())
	}
}

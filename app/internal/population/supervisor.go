package population

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/kecbigmt/plecture/app/internal/admitstatus"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
)

type Supervisor struct {
	cfg      func() *config.Config
	state    *state.Store
	log      *eventlog.Store
	logger   *slog.Logger
	poll     time.Duration
	capacity *capacityCoordinator
	// cache is shared by capacity (reader) and every population's Engine
	// (writer), so an admit outcome recorded by one engine is immediately
	// visible to the capacity gate for every population, not just its own.
	cache *admitstatus.Cache
}

func NewSupervisor(cfg func() *config.Config, stateStore *state.Store, logStore *eventlog.Store) *Supervisor {
	cache := admitstatus.NewCache(logStore)
	return &Supervisor{
		cfg: cfg, state: stateStore, log: logStore, logger: slog.Default(), poll: time.Second,
		capacity: newCapacityCoordinator(cfg, stateStore, logStore, cache), cache: cache,
	}
}

type activeEvaluator struct {
	cancel      context.CancelFunc
	done        <-chan struct{}
	fingerprint [32]byte
}

func (s *Supervisor) Run(ctx context.Context) {
	active := make(map[string]activeEvaluator)
	var wg sync.WaitGroup
	defer func() {
		for _, evaluator := range active {
			evaluator.cancel()
		}
		wg.Wait()
	}()
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		s.reconcile(ctx, active, &wg)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) reconcile(ctx context.Context, active map[string]activeEvaluator, wg *sync.WaitGroup) {
	cfg := s.cfg()
	definitions, err := Load(cfg)
	if err != nil {
		s.logger.Warn("workflow population config reload failed; keeping previous evaluators", "error", err)
		return
	}
	s.capacity.setDefinitions(definitions)
	desired := make(map[string]Definition, len(definitions))
	fingerprints := make(map[string][32]byte, len(definitions))
	for _, definition := range definitions {
		key := populationKey(definition)
		desired[key] = definition
		fingerprints[key] = definitionFingerprint(definition)
	}
	for key, running := range active {
		if _, ok := desired[key]; !ok || running.fingerprint != fingerprints[key] {
			running.cancel()
			<-running.done
			delete(active, key)
		}
	}
	for key, definition := range desired {
		if _, ok := active[key]; ok {
			continue
		}
		evalCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		active[key] = activeEvaluator{cancel: cancel, done: done, fingerprint: fingerprints[key]}
		engine := NewEngine(definition, s.state, s.log, s.cache, serviceHooks(s.cfg, s.state, definition, s.capacity))
		runner := actionRunner{cfg: cfg}
		wg.Go(func() {
			defer close(done)
			s.runEvaluator(evalCtx, engine, runner)
		})
	}
}

func definitionFingerprint(def Definition) [32]byte {
	raw, _ := json.Marshal(struct {
		Workflow   *lang.Definition
		Population config.WorkflowPopulation
		Observer   *lang.Definition
		Provider   config.WorkspaceProviderConfig
		Task       *lang.Definition
	}{def.Workflow.Definition, def.Population, def.Observer.Definition, def.Provider, taskDefinition(def.InitialTask)})
	return sha256.Sum256(raw)
}

func taskDefinition(task *config.TaskDocument) *lang.Definition {
	if task == nil {
		return nil
	}
	return task.Definition
}

type sourceResult struct {
	item map[string]any
	err  error
}

func (s *Supervisor) runEvaluator(ctx context.Context, engine *Engine, runner queryRunner) {
	def := engine.definition
	source := make(chan sourceResult)
	if def.Observer.Query.Subscribe != nil && def.Population.UsesSubscribe() {
		go s.runSubscription(ctx, runner, def, source)
	}
	var poll <-chan time.Time
	var pollTicker *time.Ticker
	if def.Observer.Query.Poll != nil && def.Population.UsesPoll() {
		pollTicker = time.NewTicker(def.Population.PollEvery.Duration)
		defer pollTicker.Stop()
		immediate := make(chan time.Time, 1)
		immediate <- time.Now()
		poll = immediate
	}
	sweep := time.NewTicker(time.Second)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll:
			items, err := runner.Poll(ctx, def)
			if err != nil {
				engine.RecordFailure("poll", err)
				s.logger.Warn("workflow population poll failed", "population", engine.key, "error", err)
			} else if err := engine.ApplyPoll(ctx, items); err != nil {
				engine.RecordFailure("poll_validation", err)
				s.logger.Warn("workflow population poll application failed", "population", engine.key, "error", err)
			}
			poll = pollTicker.C
		case result := <-source:
			if result.err != nil {
				engine.RecordFailure("subscribe", result.err)
				s.logger.Warn("workflow population subscription failed", "population", engine.key, "error", result.err)
				continue
			}
			if err := engine.ApplyAppearance(ctx, result.item); err != nil {
				engine.RecordFailure("subscribe_item", err)
				s.logger.Warn("workflow population appearance rejected", "population", engine.key, "error", err)
			}
		case <-sweep.C:
			if err := engine.SweepExpiry(ctx); err != nil {
				s.logger.Warn("workflow population reconciliation failed", "population", engine.key, "error", err)
			}
		}
	}
}

func (s *Supervisor) runSubscription(ctx context.Context, runner queryRunner, def Definition, output chan<- sourceResult) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := runner.Subscribe(ctx, def, func(item map[string]any) error {
			select {
			case output <- sourceResult{item: item}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if ctx.Err() != nil {
			return
		}
		select {
		case output <- sourceResult{err: err}:
		case <-ctx.Done():
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

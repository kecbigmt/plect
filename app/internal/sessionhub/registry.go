// Package sessionhub provides one per-session live-tail reader over the event
// log, shared by every consumer of a session — the SSE subscribers and the
// channel dispatcher — so a session has a single log-follow loop no matter how
// many consumers it has. The reader is a pure live tail: it starts at the log's
// current end and broadcasts only newer records. Frame consumers (SSE) receive
// each new record and replay older history themselves up to an atomically-
// captured join boundary; wake consumers (the channel dispatcher) get a
// coalescing signal and re-read from their own durable cursor.
package sessionhub

import (
	"context"
	"sync"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
)

const pollInterval = 500 * time.Millisecond

// frameBuffer bounds a subscriber's pending frames; a subscriber that can't keep
// up is dropped rather than stalling the shared reader (see FrameSub.deliver).
const frameBuffer = 256

// Frame is one delivered event with its SSE resume position (the sequence
// number past the record — the same id-frame cursor the bus has always
// emitted).
type Frame struct {
	Event  event.Event
	Start  int64 // sequence of the record itself
	Resume int64 // sequence past the record
}

// FrameSub is a live frame subscriber (SSE). Start is the reader's broadcast
// watermark when the sub enrolled: the subscriber owns history strictly before
// Start (it reads that itself), and the reader delivers records at/after Start.
type FrameSub struct {
	ch        chan Frame
	start     int64
	release   func()
	closeOnce sync.Once
	dead      bool // set under reader.mu after an overflow close
}

func (s *FrameSub) Frames() <-chan Frame { return s.ch }
func (s *FrameSub) Start() int64         { return s.start }

// Close is idempotent so a double Close can't over-decrement the reader refcount.
func (s *FrameSub) Close() { s.closeOnce.Do(s.release) }

// deliver is called only under reader.mu. It never blocks the reader: a full
// buffer means the consumer is too slow, so close its channel — its handler
// returns and the client reconnects with Last-Event-ID, resuming exactly.
func (s *FrameSub) deliver(f Frame) {
	if s.dead {
		return
	}
	select {
	case s.ch <- f:
	default:
		s.dead = true
		close(s.ch)
	}
}

// WakeSub is a coalescing wake. The reader signals it (non-blocking, cap 1) when
// the session log advances, so a consumer re-reads on demand instead of polling.
// A pending signal absorbs duplicates; because the consumer reads from a durable
// cursor, a coalesced or missed wake only delays a re-read — it never loses an
// event (a fallback re-read bounds the worst case).
type WakeSub struct {
	ch        chan struct{}
	release   func()
	closeOnce sync.Once
}

func (w *WakeSub) Wake() <-chan struct{} { return w.ch }
func (w *WakeSub) Close()                { w.closeOnce.Do(w.release) }

// signal is called only under reader.mu; the cap-1 non-blocking send coalesces.
func (w *WakeSub) signal() {
	select {
	case w.ch <- struct{}{}:
	default:
	}
}

// reader is the single goroutine polling one session's log and broadcasting new
// records to its frame subscribers and signalling its wake subscribers.
type reader struct {
	session string
	store   *eventlog.Store
	poll    time.Duration
	cancel  context.CancelFunc
	// done closes when run returns, giving releaseRef and Close a point to
	// wait on so neither can return while the reader might still touch the log.
	done chan struct{}

	mu     sync.Mutex
	cursor int64 // broadcast watermark: everything < cursor has been broadcast
	frames map[*FrameSub]struct{}
	wakes  map[*WakeSub]struct{}
}

func (r *reader) run(ctx context.Context) {
	defer close(r.done)
	cur := r.cursor
	for {
		evs, offs, next, err := r.store.List(r.session, cur, event.Filter{})
		if err == nil && len(evs) > 0 {
			r.mu.Lock()
			for i := range evs {
				resume := next
				if i+1 < len(offs) {
					resume = offs[i+1]
				}
				f := Frame{Event: evs[i], Start: offs[i], Resume: resume}
				for s := range r.frames {
					s.deliver(f)
				}
			}
			for wk := range r.wakes {
				wk.signal()
			}
			r.cursor = next
			r.mu.Unlock()
			cur = next
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.poll):
		}
	}
}

// Registry owns at most one reader per session, ref-counted across all consumers.
type Registry struct {
	store *eventlog.Store
	poll  time.Duration

	mu      sync.Mutex
	readers map[string]*entry
}

type entry struct {
	reader *reader
	refs   int
}

// Option configures a Registry.
type Option func(*Registry)

// WithPollInterval overrides the reader's live-tail poll cadence (mainly to
// tighten tests; production uses the default).
func WithPollInterval(d time.Duration) Option {
	return func(r *Registry) { r.poll = d }
}

func NewRegistry(store *eventlog.Store, opts ...Option) *Registry {
	reg := &Registry{store: store, poll: pollInterval, readers: map[string]*entry{}}
	for _, o := range opts {
		o(reg)
	}
	return reg
}

// SubscribeFrames enrolls an SSE subscriber. The returned sub's Start is the join
// boundary: replay the log up to Start yourself, then consume Frames().
func (reg *Registry) SubscribeFrames(session string) *FrameSub {
	r := reg.acquire(session)
	r.mu.Lock()
	defer r.mu.Unlock()
	sub := &FrameSub{ch: make(chan Frame, frameBuffer), start: r.cursor}
	sub.release = func() {
		r.mu.Lock()
		delete(r.frames, sub)
		r.mu.Unlock()
		reg.releaseRef(session, r)
	}
	r.frames[sub] = struct{}{}
	return sub
}

// Watch enrolls a coalescing wake consumer (the channel dispatcher).
func (reg *Registry) Watch(session string) *WakeSub {
	r := reg.acquire(session)
	r.mu.Lock()
	defer r.mu.Unlock()
	wk := &WakeSub{ch: make(chan struct{}, 1)}
	wk.release = func() {
		r.mu.Lock()
		delete(r.wakes, wk)
		r.mu.Unlock()
		reg.releaseRef(session, r)
	}
	r.wakes[wk] = struct{}{}
	return wk
}

// acquire returns the session's reader, lazily starting it and bumping its
// refcount. The reader is seeded to the log tail so it broadcasts only events
// appended afterward; consumers replay earlier history themselves.
func (reg *Registry) acquire(session string) *reader {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	e, ok := reg.readers[session]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		r := &reader{
			session: session,
			store:   reg.store,
			poll:    reg.poll,
			cancel:  cancel,
			done:    make(chan struct{}),
			frames:  map[*FrameSub]struct{}{},
			wakes:   map[*WakeSub]struct{}{},
		}
		if _, _, end, err := reg.store.List(session, 0, event.Filter{}); err == nil {
			r.cursor = end
		}
		go r.run(ctx)
		e = &entry{reader: r}
		reg.readers[session] = e
	}
	e.refs++
	return e.reader
}

// releaseRef drops r's refcount and, once the last consumer has left, cancels
// and joins it before returning. Joining (not just cancelling) matters because
// a caller that immediately tears down the session's on-disk directory (as a
// test's t.TempDir() cleanup does) would otherwise race a reader goroutine
// still mid-poll: its next store.List call reopens the lock file with
// O_CREATE, resurrecting it inside a directory already being removed. The
// wait itself happens outside reg.mu so it doesn't stall unrelated sessions.
func (reg *Registry) releaseRef(session string, r *reader) {
	reg.mu.Lock()
	e, ok := reg.readers[session]
	if !ok || e.reader != r {
		reg.mu.Unlock()
		return
	}
	e.refs--
	last := e.refs <= 0
	if last {
		delete(reg.readers, session)
	}
	reg.mu.Unlock()
	if last {
		r.cancel()
		<-r.done
	}
}

// Close cancels and joins every live reader during resident shutdown. This is
// the belt-and-suspenders teardown for any reader still alive after subscribers,
// dispatchers, and reactors have left; joining (see releaseRef) guarantees no
// reader touches the log again once Close has returned.
func (reg *Registry) Close() {
	reg.mu.Lock()
	readers := make([]*reader, 0, len(reg.readers))
	for _, e := range reg.readers {
		e.reader.cancel()
		readers = append(readers, e.reader)
	}
	reg.readers = map[string]*entry{}
	reg.mu.Unlock()
	for _, r := range readers {
		<-r.done
	}
}

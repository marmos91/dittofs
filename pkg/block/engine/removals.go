package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// answer is what one run of a loop item answers.
type answer uint8

const (
	itemDone           answer = iota + 1 // the item is finished
	itemRetry                            // it failed: retried after the backoff, counted toward a stall
	itemRetryUncounted                   // it is held by something outside it: retried, never counted
)

// stallAfter is the count of consecutive failures after which an item is
// removal_stalled.
const stallAfter = 5

// loopBackoff is the wait before an item that did not finish runs again.
const loopBackoff = time.Second

// loopItem is one piece of removal work: a removal's phase 2 and prune, or a
// phase 1 the call that began it could not finish.
type loopItem struct {
	b    *binding
	f    entity.FileID
	kind entity.RemovalKind
	run  func(ctx context.Context) answer
	// end is called once if the loop ends, or its share is dropped, before
	// the item finished; nil when there is nothing to end.
	end func()

	queued    time.Time // when it was first enqueued
	notBefore time.Time
	fails     int
	stalled   bool
}

// removalLoop runs one journal's removal work.
//
// ponytail: removal phase 2 and its prune run from one loop per journal, a
// FIFO with a fixed backoff, not the shared work scheduler. Replace it with
// the scheduler's removal source when offload adds the first work that needs
// fairness between shares.
type removalLoop struct {
	e       *Engine
	mu      sync.Mutex
	items   []*loopItem
	closed  bool
	dropped map[entity.ShareID]bool // shares dropped: an item of one, running or enqueued, is ended
	steps   atomic.Int64            // items run, for a test to wait on a run
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

// removalSeries is engine_removal_batches_total, by result.
type removalSeries struct{ ok, conflict, failed atomic.Int64 }

func newRemovalLoop(e *Engine) *removalLoop {
	l := &removalLoop{e: e, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	return l
}

// enqueue adds it at the loop's back. On a closed loop it is ended at once.
func (l *removalLoop) enqueue(it *loopItem) {
	now := l.e.d.Clock.Now()
	l.mu.Lock()
	if l.closed || l.dropped[it.b.share.ID] {
		l.mu.Unlock()
		if it.end != nil {
			it.end()
		}
		return
	}
	it.queued = now
	l.items = append(l.items, it)
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *removalLoop) run() {
	defer close(l.done)
	for {
		it, wait := l.next()
		if it != nil {
			l.step(it)
			continue
		}
		var timer <-chan struct{}
		if wait >= 0 {
			timer = l.e.d.Clock.After(wait)
		}
		select {
		case <-l.wake:
		case <-timer:
		case <-l.stop:
			return
		}
	}
}

// next is the first item whose backoff has passed, taken off the loop, or
// how long until the first one's has, -1 when the loop is empty.
func (l *removalLoop) next() (*loopItem, time.Duration) {
	now := l.e.d.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	wait := time.Duration(-1)
	for i, it := range l.items {
		if !it.notBefore.After(now) {
			l.items = slices.Delete(l.items, i, i+1)
			return it, 0
		}
		if w := it.notBefore.Sub(now); wait < 0 || w < wait {
			wait = w
		}
	}
	return nil, wait
}

// step runs it once, under a root span, and puts it back at the loop's end
// unless it finished, raising removal_stalled after five failures in a row
// and clearing it once it finishes.
func (l *removalLoop) step(it *loopItem) {
	ctx, end := l.e.d.Tracer(l.ctx, "engine.removal_batch", slog.String("kind", it.kind.String()))
	a := it.run(ctx)
	end(nil)
	l.steps.Add(1)
	log := l.e.d.Logger
	switch a {
	case itemDone:
		if it.stalled {
			log.Warn("removal_stalled cleared", "file", it.f, "kind", it.kind)
		}
		return
	case itemRetry:
		it.fails++
		if it.fails >= stallAfter && !it.stalled {
			it.stalled = true
			log.Warn("removal_stalled", "file", it.f, "kind", it.kind)
		}
	}
	it.notBefore = l.e.d.Clock.Now().Add(loopBackoff)
	l.mu.Lock()
	ended := l.closed || l.dropped[it.b.share.ID]
	if !ended {
		l.items = append(l.items, it)
	}
	l.mu.Unlock()
	if ended && it.end != nil {
		it.end()
	}
}

// close stops the loop, joins its goroutine and ends every item left. A
// second close does nothing.
func (l *removalLoop) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	items := l.items
	l.items = nil
	l.mu.Unlock()
	l.cancel()
	close(l.stop)
	<-l.done
	for _, it := range items {
		if it.end != nil {
			it.end()
		}
	}
}

// dropShare takes share s's items off the loop, ending each; one running
// now is ended when its run returns, and none of the share's is enqueued
// after.
func (l *removalLoop) dropShare(s entity.ShareID) {
	l.mu.Lock()
	if l.dropped == nil {
		l.dropped = map[entity.ShareID]bool{}
	}
	l.dropped[s] = true
	var dropped []*loopItem
	l.items = slices.DeleteFunc(l.items, func(it *loopItem) bool {
		if it.b.share.ID == s {
			dropped = append(dropped, it)
			return true
		}
		return false
	})
	l.mu.Unlock()
	for _, it := range dropped {
		if it.end != nil {
			it.end()
		}
	}
}

// isDropped reports whether share s was dropped: nothing of it is noted for
// a commit again.
func (l *removalLoop) isDropped(s entity.ShareID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped[s]
}

// pending is the count of items on the loop and the oldest one's age:
// engine_removals_pending and engine_removals_oldest_seconds.
func (l *removalLoop) pending() (n int, oldest time.Duration) {
	now := l.e.d.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, it := range l.items {
		oldest = max(oldest, now.Sub(it.queued))
	}
	return len(l.items), oldest
}

// stalledOn reports whether f has an item removal_stalled.
func (l *removalLoop) stalledOn(f entity.FileID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, it := range l.items {
		if it.f == f && it.stalled {
			return true
		}
	}
	return false
}

// count records one batch's result in engine_removal_batches_total.
func (e *Engine) count(err error) {
	switch {
	case err == nil:
		e.removalBatches.ok.Add(1)
	case errors.Is(err, kvstore.ErrConflict):
		e.removalBatches.conflict.Add(1)
	default:
		e.removalBatches.failed.Add(1)
	}
}

// phase2 is the item that drops r's masked records and prunes its removal
// record: the store's Resume runs every batch of the file's removals not
// done, and the prune, with no pass in flight, is at r's own version.
func (e *Engine) phase2(r *removal) *loopItem {
	share, v := r.b.share.ID, entity.JournalVersion(r.v.Bytes())
	return &loopItem{b: r.b, f: r.f, kind: r.kind, run: func(ctx context.Context) answer {
		ctx, cancel := context.WithTimeout(ctx, conflictBound.Load().(time.Duration))
		defer cancel()
		err := e.d.Content.Resume(ctx, share, r.f)
		if err == nil {
			err = e.d.Content.PruneRemovals(ctx, share, r.f, v)
		}
		e.count(err)
		if err != nil {
			return itemRetry
		}
		return itemDone
	}}
}

// finish ends r after its phase 1 committed: the guard released, the
// journal settled through it, and its phase 2 posted to its journal's loop.
func (e *Engine) finish(r *removal) {
	e.release(r, true)
	r.b.gc.removals.enqueue(e.phase2(r))
}

// retryLater hands r's phase 1, which its call could not commit, to its
// journal's loop, which retries it keeping the guard and the in-flight mark
// until it commits, or until the file is found gone. outcome, when not nil,
// is told once how it ended: nil once it commits.
//
// decision: a phase 1 its loop's end cuts short, as Close's stop of the work
// queue does, leaves its file as it is: every pending operation, the
// removal's own included, stays in the entry and the guard stays held, so no
// group commit after it, Close's last included, commits any of them, and the
// journal's unsettled marker leaves the removal to the next start's recovery
// (RFC 8 §2.5 step 2). A caller waiting on the guard waits to its own
// deadline. Withdraw it if the loop can ever end while the engine still
// serves the file.
func (e *Engine) retryLater(r *removal, op kvstore.RemoveOp, outcome func(error)) {
	tell := func(err error) {
		if outcome != nil {
			outcome(err)
		}
	}
	r.b.gc.removals.enqueue(&loopItem{b: r.b, f: r.f, kind: r.kind,
		run: func(ctx context.Context) answer {
			ctx, cancel := context.WithTimeout(ctx, conflictBound.Load().(time.Duration))
			defer cancel()
			err := e.tryPhase1(ctx, r, op)
			e.count(err)
			switch {
			case err == nil:
				tell(nil)
				return itemDone
			case errors.Is(err, kvstore.ErrHeld):
				tell(e.abort(r))
				return itemDone
			case terminal(err):
				e.unstage(r, err)
				tell(e.storeErr(err))
				return itemDone
			}
			return itemRetry
		},
		end: func() { tell(errLoopEnded) },
	})
}

// errLoopEnded is a retried phase 1's outcome when its loop ended first:
// vfserr.ErrUnavailable.
var errLoopEnded = errClosed

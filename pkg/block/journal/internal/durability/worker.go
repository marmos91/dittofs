package durability

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// run is one stream's worker: on each wake it completes the entries a sync
// covered, stamps the ones it has not seen, issues the sync a waiter asked for
// or the sync bound's timer calls for, and arms the timer, until Stop closes
// done.
func (d *Durability) run(st *stream) {
	defer d.wg.Done()
	var fire <-chan struct{} // the one outstanding timer, nil when none is
	for {
		fired := false
		select {
		case <-d.done:
			return
		case <-st.kick:
		case <-fire:
			fire, fired = nil, true
		}
		d.complete(st)
		d.stamp(st)
		d.syncStep(st, fired)
		if fire == nil {
			fire = d.arm(st)
		}
	}
}

// syncStep issues the sync a waiter asked for, or, when the timer fired and the
// oldest uncovered record is past the bound, the bound's sync, with no lock
// held.
// A failure is recorded against the segment that was active and is never
// retried (R10): the segment's tracker froze at the failure and will not
// report it again.
//
// ponytail: a sync covers the whole active segment, so a file that syncs
// often waits for what another writer appended. The cost is bounded by what
// one stream accepts in one sync's latency. Upgrade to a stream of its own for
// a frequently flushing file when a check of mixed durability shows the
// latency rising.
func (d *Durability) syncStep(st *stream, fired bool) {
	var now time.Duration
	if fired {
		now = d.clock.Elapsed()
	}
	st.mu.Lock()
	if !st.want {
		seen, ok := st.oldestLocked()
		if !fired || !ok || now-seen < core.SyncBound {
			st.mu.Unlock()
			return
		}
	}
	st.want = false
	st.syncing = true
	st.mu.Unlock()

	var end func(error)
	if d.tracer != nil {
		_, end = d.tracer(context.Background(), core.SpanSync, slog.Uint64("stream", uint64(st.no)))
	}
	start := d.clock.Elapsed()
	err := st.s.Sync()
	took := d.clock.Elapsed() - start
	if errors.Is(err, segment.ErrStreamClosed) {
		err = core.ErrClosed
	}

	st.mu.Lock()
	st.syncs++
	st.sum += took
	st.buckets[bucketOf(took)]++
	if err != nil {
		st.failures++
		if st.haveLast {
			st.failed[st.lastSeg] = err
		}
		d.log.Error("journal sync failed", "stream", st.no, "err", err)
	}
	st.syncing = false
	st.notifyLocked()
	st.mu.Unlock()
	if end != nil {
		end(err)
	}
}

// bucketOf returns the histogram bucket a sync of duration d falls in.
func bucketOf(d time.Duration) int {
	for i, b := range SyncDurationBounds {
		if d <= b {
			return i
		}
	}
	return len(SyncDurationBounds)
}

// complete drops every covered entry from the ledger (R87): each is marked
// done under its file's append domain, one domain at a time, so a record whose
// sync landed is never dropped in the gap between a writer's append and the
// accounting it does under that same domain. Only the worker compacts and
// Track only appends, so the indices collected stay valid between the steps.
func (d *Durability) complete(st *stream) {
	st.mu.Lock()
	st.scratch = st.scratch[:0]
	for i := range st.ledger {
		if e := &st.ledger[i]; !e.done && st.covered(e.w.Loc) {
			st.scratch = append(st.scratch, i)
		}
	}
	idx := st.scratch
	st.mu.Unlock()
	if len(idx) == 0 {
		return
	}

	for _, i := range idx {
		st.mu.Lock()
		f := st.ledger[i].w.File
		st.mu.Unlock()
		unlock := d.domains.Lock(f)
		st.mu.Lock()
		st.ledger[i].done = true
		st.mu.Unlock()
		unlock()
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.ledger[:0]
	live := map[core.SegmentID]struct{}{}
	for _, e := range st.ledger {
		if !e.done {
			kept = append(kept, e)
			live[e.w.Loc.Segment] = struct{}{}
		}
	}
	clear(st.ledger[len(kept):]) // drop the references the tail still holds
	st.ledger = kept
	for seg := range st.synced {
		if _, ok := live[seg]; !ok && (!st.haveLast || seg != st.lastSeg) {
			delete(st.synced, seg)
		}
	}
	for seg := range st.failed {
		if _, ok := live[seg]; !ok && (!st.haveLast || seg != st.lastSeg) {
			delete(st.failed, seg)
		}
	}
}

// stamp records when the worker first saw each entry. The clock is read
// outside mu, and only entries that existed before the read are stamped, so a
// stamp is never earlier than the record's arrival.
func (d *Durability) stamp(st *stream) {
	st.mu.Lock()
	n := len(st.ledger)
	need := false
	for i := range n {
		if !st.ledger[i].seenSet {
			need = true
			break
		}
	}
	st.mu.Unlock()
	if !need {
		return
	}
	now := d.clock.Elapsed()
	st.mu.Lock()
	for i := range n {
		if e := &st.ledger[i]; !e.seenSet {
			e.seen, e.seenSet = now, true
		}
	}
	st.mu.Unlock()
}

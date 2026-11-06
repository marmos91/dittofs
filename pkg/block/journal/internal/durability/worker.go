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
// A failure is recorded against the segment the sync was of, as the stream
// reports it, and is never retried: the segment's tracker froze at the
// failure and will not report it again.
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
	seg, issued, err := st.s.SyncSegment()
	took := d.clock.Elapsed() - start
	if d.afterSync != nil {
		d.afterSync()
	}

	st.mu.Lock()
	closing := errors.Is(err, segment.ErrStreamClosed)
	switch {
	case closing:
		// The stream closed under the worker: nothing failed, so waiters get
		// core.ErrClosed from check and no failure is counted or recorded.
		st.closed = true
		d.log.Debug("journal sync skipped, stream closed", "stream", st.no)
	case !issued:
		// The stream answered without an fsync, as for a sealed or an ended
		// segment: no sync was issued, so none is counted as issued or as
		// failed. A refusal still fails the waiters of that segment.
		if err != nil {
			st.failed[seg] = err
			d.log.Debug("journal sync refused, no fsync issued", "stream", st.no, "err", err)
		}
	default:
		st.syncs.Add(1)
		st.sum.Add(int64(took))
		st.buckets[bucketOf(took)].Add(1)
		if err != nil {
			st.failures.Add(1)
			// The failure belongs to the segment SyncSegment reported, which
			// a roll after the sync cannot move.
			st.failed[seg] = err
			d.log.Error("journal sync failed", "stream", st.no, "err", err)
		}
	}
	// The stream is active on the segment synced, so a record of an older
	// segment is no longer the active one's to lead a sync for. The mark only
	// moves forward: it is the newest segment the stream is known to have
	// reached, whether through a Track or a sync, and a segment that holds no
	// tracked record must not hand the lead back to an older segment whose
	// record no sync of the newer one can cover.
	if seg != 0 && (!st.haveLast || seg > st.lastSeg) {
		st.lastSeg, st.haveLast = seg, true
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

// complete drops every covered entry from the ledger: each is marked
// done under its file's append domain, one domain at a time, so a record whose
// sync landed is never dropped in the gap between a writer's append and the
// accounting it does under that same domain. Only the worker compacts and
// Track only appends, so the indices collected stay valid between the steps.
func (d *Durability) complete(st *stream) {
	st.mu.Lock()
	if !st.reported {
		st.mu.Unlock()
		return
	}
	st.reported = false
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
	live := st.live
	clear(live)
	stamped := 0
	for i, e := range st.ledger {
		if !e.done {
			kept = append(kept, e)
			live[e.w.Loc.Segment] = struct{}{}
			if i < st.stamped {
				stamped++
			}
		}
	}
	st.stamped = stamped
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
	n, from := len(st.ledger), st.stamped
	st.mu.Unlock()
	if n == from {
		return
	}
	now := d.clock.Elapsed()
	st.mu.Lock()
	for i := from; i < n; i++ {
		e := &st.ledger[i]
		e.seen, e.seenSet = now, true
	}
	st.stamped = n
	st.mu.Unlock()
}

package durability

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// run is one stream's worker: on each wake it takes the windows of the
// segments that ended, fails their content records, completes the entries a
// sync covered, stamps the ones it has not seen, issues the sync a waiter asked for
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
		d.takeWindows(st)
		d.failWindows(st)
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

	// The failure's time is read before the lock: the clock is never read
	// under mu.
	var failedAt time.Time
	var failedMono time.Duration
	if issued && err != nil && !errors.Is(err, segment.ErrStreamClosed) && !errors.Is(err, segment.ErrSegmentEnded) {
		failedAt, failedMono = d.clock.Now(), d.clock.Elapsed()
	}

	st.mu.Lock()
	failed := false
	closing := errors.Is(err, segment.ErrStreamClosed)
	ended := errors.Is(err, segment.ErrSegmentEnded)
	switch {
	case closing:
		// The stream closed under the worker: nothing failed, so waiters get
		// core.ErrClosed from check and no failure is counted or recorded.
		st.closed = true
		d.log.Debug("journal sync skipped, stream closed", "stream", st.no)
	case ended:
		// A failed append or seal ended the segment first: no sync was
		// issued for it and nothing new failed, so nothing is counted. Its
		// end event is on its way and takes the window.
		if _, kept := st.failed[seg]; !kept {
			st.ending[seg] = struct{}{}
		}
		d.log.Debug("journal sync skipped, segment ended", "stream", st.no, "segment", seg)
	case !issued:
		// The stream answered without an fsync, as for a sealed segment: no
		// sync was issued, so none is counted as issued or as failed. A
		// refusal still fails the waiters of that segment.
		if err != nil {
			st.failed[seg] = err
			d.log.Debug("journal sync refused, no fsync issued", "stream", st.no, "err", err)
		}
	default:
		st.syncs.Add(1)
		st.sum.Add(int64(took))
		st.buckets[bucketOf(took)].Add(1)
		if err != nil {
			failed = true
			st.failures.Add(1)
			st.lastFail.Store(&failStamp{SyncFailure: SyncFailure{Err: err, At: failedAt}, mono: failedMono})
			st.counted[seg] = struct{}{}
			// The failure belongs to the segment SyncSegment reported, which
			// a roll after the sync cannot move. The first failure of a
			// segment stands.
			if _, dup := st.failed[seg]; !dup {
				st.failed[seg] = err
			}
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
	d.notifyAll()
	st.mu.Unlock()
	if end != nil {
		end(err)
	}
	if failed {
		// The failed segment takes no further append, not even a seal marker
		// (R13). The failure stands whatever End answers: a failed sync is
		// never retried.
		if eerr := st.s.End(seg, err); eerr != nil && !errors.Is(eerr, segment.ErrStreamClosed) {
			d.log.Error("journal could not end the failed segment", "stream", st.no, "segment", seg, "err", eerr)
		}
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
// accounting it does under that same domain. A content record's superseded
// extents stop being retained under the same hold. Only the worker compacts and
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
		w := st.ledger[i].w
		st.mu.Unlock()
		unlock := d.domains.Lock(w.File)
		if isContent(w) {
			// st.mu is not held: the index takes the file's lock and the
			// account is the caller's. The entry is done only after the
			// lift, so a Sync that returns on it sees the lift.
			d.lift(w.File, w.Version)
		}
		st.mu.Lock()
		st.ledger[i].done = true
		st.mu.Unlock()
		unlock()
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.removeLocked(func(e *entry) bool { return e.done }, nil)
	// A Sync waits for its records to be completed, not only covered.
	d.notifyAll()
	live := st.live
	clear(live)
	// A segment with a pending window keeps its failure and its synced value.
	for _, w := range st.windows {
		live[w.seg] = struct{}{}
	}
	for i := range st.ledger {
		live[st.ledger[i].w.Loc.Segment] = struct{}{}
	}
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
	for seg := range st.ending {
		if _, ok := live[seg]; !ok {
			delete(st.ending, seg)
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

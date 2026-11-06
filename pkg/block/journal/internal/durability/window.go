package durability

import (
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// SyncFailure is the most recent sync failure, and when it happened (R64).
type SyncFailure struct {
	Err error
	// At is the clock's Now when the worker took the failure. A failure the
	// worker's own sync reported is stamped as it returns; a seal's failure is
	// stamped when the worker takes its end event, which the stream hands over
	// only after the segment's reservations and in-flight syncs drained, so At
	// can lag the failure by that drain.
	At time.Time
}

// failStamp is a stream's last sync failure, with the monotonic reading that
// orders it against the other streams' failures.
//
// decision: a failStamp is immutable once stored. The worker is its only
// writer, publishing a fresh one through st.lastFail's atomic pointer, and
// Stats reads that pointer without any lock, so a poll never waits on a lock
// an append takes. Withdrawn if a second goroutine ever stores one.
type failStamp struct {
	SyncFailure
	mono time.Duration
}

// window is a failed segment's records that no sync covered: taken from the
// ledger when the segment's end event reaches the worker (R13, R15, R85).
//
// decision: a window lives under the stream's leaf mutex, st.mu, like the
// ledger its entries came out of: it is read and listed in st.windows only
// with that mutex held, and only the stream's worker creates one (takeWindow)
// or drops one (resolve), so no other goroutine mutates it, and only the same
// worker removes an entry when failWindow fails it. cause is kept for the
// code that fails a window's records, which reads why the segment ended.
type window struct {
	id      uint64
	seg     core.SegmentID
	cause   segment.EndCause
	err     error   // why the segment ended
	entries []entry // moved from the ledger, in Track order
}

// onEnd is the consumer of every stream's end events (D13). It runs on the
// stream's notifier with no stream lock held, so it only queues the event for
// the stream's worker and wakes it: no clock, no domain, no blocking.
func (d *Durability) onEnd(ev segment.EndEvent) {
	d.mu.Lock()
	st := d.made[ev.Stream]
	d.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	st.ends = append(st.ends, ev)
	st.mu.Unlock()
	st.wake()
}

// takeWindows takes the window of every end event queued for the stream. It
// runs on the worker, before the completion pass, so a record a sync covered
// is completed there and only the uncovered ones are the window.
func (d *Durability) takeWindows(st *stream) {
	st.mu.Lock()
	evs := st.ends
	st.ends = nil
	st.mu.Unlock()
	for _, ev := range evs {
		d.takeWindow(st, ev)
	}
}

// removeLocked drops the ledger entries drop selects, in Track order, and
// hands each to take, which may be nil. It is the one compaction of the
// ledger, shared by the completion pass and the window: it recounts stamped
// over what stays, so the stamped prefix stays the entries the worker has
// stamped, and clears the references the tail still holds. st.mu must be held.
func (st *stream) removeLocked(drop func(*entry) bool, take func(entry)) {
	kept := st.ledger[:0]
	stamped := 0
	for i := range st.ledger {
		e := &st.ledger[i]
		if drop(e) {
			if take != nil {
				take(*e)
			}
			continue
		}
		kept = append(kept, *e)
		if i < st.stamped {
			stamped++
		}
	}
	clear(st.ledger[len(kept):])
	st.ledger, st.stamped = kept, stamped
}

func (d *Durability) takeWindow(st *stream, ev segment.EndEvent) {
	// A seal's failed sync is a sync failure no worker sync reported, so it is
	// counted here, unless the worker already recorded a failure for the
	// segment: a seal that finds that failure ends with EndSeal too, and the
	// one fsync failure is counted once. The clock is read before the lock.
	// An append's end counts nothing, and a requested end is the failure the
	// worker counted when it asked.
	//
	// decision: every EndSeal the worker holds no failure for is counted as a
	// sync failure. A seal ends its segment mostly through a failed sync, and
	// the sync failure is the count's subject (R62, R64). It is known to
	// overcount: the stream also ends a segment with EndSeal when a write
	// fails while a seal drains, or when the seal-marker write fails, and
	// EndEvent cannot tell these from a failed sync, so sync_failures and
	// LastSyncError can then show a write error. Withdrawn once EndEvent says
	// whether the cause was a sync, which the stream would have to add, and
	// only that case is counted. The same ceiling covers ErrBadCatalog and
	// ledger refusals, which end with EndSeal too. So sync_failures counts
	// seal fsyncs that syncs does not: a sync of an ended segment issues no
	// fsync yet its seal counts one (Syncs=0, SyncFailures=1), and the two
	// counters count different sets.
	// sync_failures also differs from RFC 1's "syncs issued, and failed" row:
	// it counts a seal's failure the worker never issued. The RFC row should
	// say the same; that is a follow-up for the RFC, not for this code.
	var failedAt time.Time
	var failedMono time.Duration
	if ev.Cause == segment.EndSeal {
		failedAt, failedMono = d.clock.Now(), d.clock.Elapsed()
	}

	st.mu.Lock()
	w := &window{id: d.windowSeq.Add(1), seg: ev.Segment, cause: ev.Cause, err: ev.Err}
	st.removeLocked(func(e *entry) bool {
		return e.w.Loc.Segment == ev.Segment && !st.covered(e.w.Loc)
	}, func(e entry) { w.entries = append(w.entries, e) })
	_, dup := st.failed[ev.Segment]
	if !dup {
		st.failed[ev.Segment] = ev.Err
	}
	_, wasCounted := st.counted[ev.Segment]
	delete(st.counted, ev.Segment)
	delete(st.ending, ev.Segment)
	st.windows = append(st.windows, w)
	if ev.Cause == segment.EndSeal && !dup && !wasCounted {
		st.failures.Add(1)
		st.lastFail.Store(&failStamp{SyncFailure: SyncFailure{Err: ev.Err, At: failedAt}, mono: failedMono})
	}
	st.d.notifyAll()
	empty := len(w.entries) == 0
	st.mu.Unlock()

	if ev.Cause == segment.EndRequested {
		d.log.Debug("journal segment ended", "stream", st.no, "segment", ev.Segment, "cause", ev.Cause, "err", ev.Err)
	} else {
		d.log.Error("journal segment ended", "stream", st.no, "segment", ev.Segment, "cause", ev.Cause, "err", ev.Err)
	}
	if empty {
		// Nothing is held from the segment, so there is nothing to fail.
		d.resolve(st, w)
	}
	// A stopped stream is a refused one, whatever stopped it, and takes no
	// further segment, so no later event reaches here.
	if ev.Stopped && d.readOnly != nil {
		d.readOnly(ev.Err)
	}
}

// resolve marks the window's records no longer pending: it drops the window
// and the failure kept for its segment once nothing else names it, then
// announces the segment through Config.Resolved (R19). It runs on the worker.
//
// decision: a window is resolved when it holds no record: at once when it is
// taken with none, or when failWindow has failed its last content record. A
// window holding a record that failWindow leaves, a header-only one, stays
// pending and is not announced, and every Sync naming a file with such a
// record answers a *core.SyncError, until the code that re-appends it lands.
// A pending window never reads durable, so nothing is lost by waiting; what
// is withdrawn is only the window's memory.
//
// A record failWindow dropped has left the window and the ledger, which would
// read it durable: st.lost keeps it out of that answer, and failWindow adds it
// there in the same critical section that removes it from the window.
func (d *Durability) resolve(st *stream, w *window) {
	st.mu.Lock()
	st.windows = slices.DeleteFunc(st.windows, func(o *window) bool { return o == w })
	held := false
	for i := range st.ledger {
		held = held || st.ledger[i].w.Loc.Segment == w.seg
	}
	for _, o := range st.windows {
		held = held || o.seg == w.seg
	}
	if !held {
		delete(st.failed, w.seg)
	}
	st.d.notifyAll()
	st.mu.Unlock()
	if d.resolved != nil {
		d.resolved(st.no, w.seg)
	}
}

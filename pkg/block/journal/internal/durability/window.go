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
	At  time.Time // the clock's Now at the failure
}

// failStamp is a stream's last sync failure, with the monotonic reading that
// orders it against the other streams' failures.
type failStamp struct {
	SyncFailure
	mono time.Duration
}

// window is a failed segment's records that no sync covered: taken from the
// ledger when the segment's end event reaches the worker (R13, R15, R85).
type window struct {
	seg     core.SegmentID
	cause   segment.EndCause
	err     error   // the failure that ended the segment
	entries []entry // moved from the ledger, in Track order
	// resolved is set once the window's records are no longer pending.
	resolved bool
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

func (d *Durability) takeWindow(st *stream, ev segment.EndEvent) {
	// A seal's failed sync is a sync failure no worker sync reported, so it is
	// counted here. The clock is read before the lock. An append's end
	// counts nothing, and a requested end is the failure the worker counted
	// when it asked.
	var failedAt time.Time
	var failedMono time.Duration
	if ev.Cause == segment.EndSeal {
		failedAt, failedMono = d.clock.Now(), d.clock.Elapsed()
	}

	st.mu.Lock()
	w := &window{seg: ev.Segment, cause: ev.Cause, err: ev.Err}
	kept := st.ledger[:0]
	stamped := 0
	for i, e := range st.ledger {
		if e.w.Loc.Segment == ev.Segment && !e.done && !st.covered(e.w.Loc) {
			w.entries = append(w.entries, e)
			continue
		}
		kept = append(kept, e)
		if i < st.stamped {
			stamped++
		}
	}
	clear(st.ledger[len(kept):]) // drop the references the tail still holds
	st.ledger, st.stamped = kept, stamped
	if _, dup := st.failed[ev.Segment]; !dup {
		st.failed[ev.Segment] = ev.Err
	}
	delete(st.ending, ev.Segment)
	st.windows = append(st.windows, w)
	if ev.Cause == segment.EndSeal {
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
}

// resolve marks the window's records no longer pending: it drops the window
// and the failure kept for its segment once nothing else names it. It runs on
// the worker.
//
// decision: in this layer only a window with no records is resolved, at the
// moment it is taken. A window that holds records stays pending, and every
// Sync naming a file with a record in it answers a *core.SyncError, until the
// code that fails a window's records through the index calls resolve. A
// pending window never reads durable, so nothing is lost by waiting; what is
// withdrawn is only the window's memory.
func (d *Durability) resolve(st *stream, w *window) {
	st.mu.Lock()
	w.resolved = true
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
}

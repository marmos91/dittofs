package durability

import (
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// The sync bound: a record is synced within core.SyncBound of the worker
// first seeing it, whether or not anyone calls Sync (R7, R8). One behaviour
// serves every share, and nothing weakens it (R9).
//
// ponytail: there is no per-share mode and no setting that weakens the bound.
// A bulk writer that never syncs pays at most one flush per stream per
// second. Upgrade to a per-share mode that syncs only on request once a
// check of a writer that never calls Sync shows that flush costing real
// throughput.
//
// decision: the bound is measured from when the worker first observes the
// record, not from inside Track, because Track runs under the segment's
// append domain and may not read the clock, which takes the clock's own
// mutex. The observation follows Track by one kick, so the bound errs late
// only by the worker's scheduling latency. Overturn this if a profile shows
// worker lag comparable to the bound.

// oldestLocked returns when the worker first saw the oldest record that a
// timer sync could still cover: not completed, not covered, and not of a
// segment whose sync failed, since nothing is retried (R10) and a timer
// that kept firing for it would sync every second for nothing. The ledger
// is in Track order, so the first match is the oldest. The caller holds mu.
func (st *stream) oldestLocked() (seen time.Duration, ok bool) {
	for i := range st.ledger {
		e := &st.ledger[i]
		if e.done || !e.seenSet || st.covered(e.w.Loc) {
			continue
		}
		if _, bad := st.failed[e.w.Loc.Segment]; bad {
			continue
		}
		return e.seen, true
	}
	return 0, false
}

// arm returns the channel of an After for the oldest uncovered record's
// deadline, or nil when nothing waits. The worker keeps at most one
// outstanding, since a pending After cannot be cancelled; deadlines only move
// later, so the one timer is never late, and a fire that finds the oldest
// record not yet due arms the next.
func (d *Durability) arm(st *stream) <-chan struct{} {
	st.mu.Lock()
	seen, ok := st.oldestLocked()
	st.mu.Unlock()
	if !ok {
		return nil
	}
	wait := seen + core.SyncBound - d.clock.Elapsed()
	if wait < 0 {
		wait = 0
	}
	return d.clock.After(wait)
}

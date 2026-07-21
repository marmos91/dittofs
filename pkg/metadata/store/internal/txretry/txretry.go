// Package txretry holds the transient-conflict backoff shared by the SQL
// metadata backends (sqlite, postgres).
//
// Both backends must backpressure under write contention — block-and-retry
// until a real time budget elapses — rather than surfacing EIO to the caller
// after a fixed handful of attempts (#1769). Every competitor (rclone/juicefs)
// goes slow under the same pressure but never errors; a fixed 3-attempt budget
// was routinely exceeded on hot rows (usedBytes counter, parent-dir mtime,
// quota) under concurrent writers, turning contention into NFS3ErrIO. Only the
// backend's already-classified transient conflicts (sqlite BUSY/LOCKED,
// postgres 40001/40P01) are retried; that classification stays backend-local.
//
// The deadline computation and the jittered exponential backoff between
// attempts are identical across the two backends, so they live here once. The
// badger backend uses a structurally different loop (closure-based db.Update,
// fixed attempt count, no time budget) and deliberately does not share this.
package txretry

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	// budget bounds how long a caller backpressures on a transient conflict
	// before giving up and returning the mapped error. Kept in line with the
	// sqlite busy_timeout (config default 5s) so a genuinely stuck conflict
	// still eventually surfaces — after a real budget, not 60ms.
	budget = 5 * time.Second
	// baseBackoff / maxBackoff bound the jittered exponential backoff between
	// attempts.
	baseBackoff = 5 * time.Millisecond
	maxBackoff  = 200 * time.Millisecond
)

// Deadline returns the backpressure deadline for a transaction: now+budget,
// tightened to an earlier ctx deadline when the caller set one, so a caller's
// own timeout is always honored ahead of the retry budget.
func Deadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(budget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

// Backoff waits a jittered exponential backoff before the next transaction
// attempt, bounded by deadline and ctx. It returns true if the caller should
// retry, or false when the retry budget is exhausted or ctx is done.
func Backoff(ctx context.Context, deadline time.Time, attempt int) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	// Full-jitter exponential backoff: base<<attempt capped at max, then a
	// uniform random in (0, d]. Spreads retries so contending writers don't
	// resynchronize into a thundering herd.
	d := maxBackoff
	if attempt < 16 {
		if s := baseBackoff << uint(attempt); s > 0 && s < maxBackoff {
			d = s
		}
	}
	if d > remaining {
		d = remaining
	}
	wait := time.Duration(rand.Int64N(int64(d)) + 1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

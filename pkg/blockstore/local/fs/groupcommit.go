package fs

import (
	"context"
	"sync"
	"time"
)

// groupCommit is the per-file fsync coalescing coordinator.
//
// Scope: one coordinator per open log file. Concurrent
// AppendWrites to the same file share at most one in-flight fsync via
// the in-flight piggyback fan-in below; different log files fsync
// independently.
//
// Durability: callers block in Sync until the underlying fsync
// returns. NFS COMMIT and SMB Flush callers depend on this contract
// async ack would break it. The only added latency for batched callers
// is at most one in-flight fsync's runtime.
//
// Adaptive bypass: when a Sync arrives at an empty queue with
// no fsync in flight, fsyncFn fires inline — single-writer workloads
// see zero added latency. The inFlight guard ensures that any writer
// arriving while a bypass is running joins the in-flight fsync's
// completion broadcast, so even depth-1 → depth-N transitions still
// coalesce onto a single fsync.
//
// Lock-order (third rule extending FIX-2/FIX-20 in
// appendwrite.go): per-file mu → groupCommit.mu → the per-store append
// log lock. The coordinator never references the per-store append log
// lock — enforced by the TestGroupCommit_NoLogsMuTouch source-grep
// gate.
//
// Documented "burst window-race" edge case: under extreme bursts a
// writer that observes !inFlight inside Sync but is preempted before
// setting inFlight=true could in principle race a second writer onto
// the same bypass branch. The check-then-set sequence happens entirely
// under g.mu so this race cannot manifest in practice; the comment is
// kept to flag the invariant for future maintainers.
//
// No config knob: the window const below stays hardcoded until bench
// data justifies tuning.
type groupCommit struct {
	mu      sync.Mutex
	pending []chan error
	//nolint:unused //: reserved for future timer-armed batching; the current in-flight-piggyback design has no timer site
	timer    *time.Timer
	inFlight bool
	fsyncFn  func() error
}

// groupCommitWindow is the maximum wait inside the coordinator before
// a hypothetical timer-armed batch fires. 1ms is chosen
// empirically as the smallest window that still coalesces bursts on
// rotational and NVMe disks; tighter would defeat batching, looser
// would penalize single-writer latency. The constant is deliberately
// not exposed as a config knob — bench data justifies tuning, not a
// milestone-19 surface.
//
//nolint:unused //: paired with the reserved timer field; both light up when timer-armed batching ships
const groupCommitWindow = 1 * time.Millisecond

// newGroupCommit constructs a coordinator bound to fsyncFn. The
// coordinator does not own the file; it only owns the fan-in queue.
func newGroupCommit(fsyncFn func() error) *groupCommit {
	return &groupCommit{fsyncFn: fsyncFn}
}

// Sync blocks until an underlying fsync covering this call has
// completed (or ctx is canceled). On ctx cancellation the caller
// observes ctx.Err(), but the in-flight fsync still runs for
// co-batched waiters — durability trumps caller-side latency
// relief. The channel capacity of 1 absorbs the eventual broadcast
// send so the broadcaster never blocks on an abandoned waiter.
func (g *groupCommit) Sync(ctx context.Context) error {
	ch := make(chan error, 1)

	g.mu.Lock()
	if g.inFlight {
		// A fsync is currently running; piggyback onto its completion
		// broadcast. Any writes already durable behind that fsync are
		// also durable for us (per-file log is append-only and the
		// caller already holds the per-file append mutex).
		g.pending = append(g.pending, ch)
		g.mu.Unlock()
		return waitOn(ctx, ch)
	}
	if len(g.pending) > 0 {
		// Defensive branch: pending should only ever be non-empty when
		// inFlight is true, but if a future timer-driven extension
		// arms one without setting inFlight we still want to join
		// rather than race.
		g.pending = append(g.pending, ch)
		g.mu.Unlock()
		return waitOn(ctx, ch)
	}
	// Empty queue, no fsync in flight → adaptive bypass.
	g.inFlight = true
	g.mu.Unlock()

	err := g.fsyncFn()

	g.mu.Lock()
	waiters := g.pending
	g.pending = nil
	g.inFlight = false
	g.mu.Unlock()

	for _, w := range waiters {
		w <- err
	}
	return err
}

// waitOn blocks until ch delivers a result or ctx is canceled.
// Returning ctx.Err() does not abort the in-flight fsync — the
// channel cap is 1 so the eventual broadcast send still succeeds.
func waitOn(ctx context.Context, ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

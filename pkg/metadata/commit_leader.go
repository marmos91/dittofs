package metadata

import (
	"context"
	"sync"
	"sync/atomic"
)

// commitLeader coalesces concurrent metadata durability barriers into as few
// backend Sync calls as possible. It is the store-agnostic sibling of the block
// store's syncLeader (pkg/block/local/fs/sync_leader.go).
//
// The one structural difference from syncLeader: syncLeader batches per-fd fsync
// closures (N distinct fds → N back-to-back fsyncs the FS journal coalesces),
// whereas a metadata store exposes ONE barrier that makes ALL committed-but-
// unsynced writes durable at once (Store.SyncDurable → badger db.Sync / sqlite
// WAL checkpoint). So the leader calls a single shared barrier once per drain
// batch and hands its result to every waiter in that batch.
//
// Why (#1573): every NFS COMMIT / stable SMB flush runs flushPendingWrite, which
// stages the write and then forces durability. Before this, each concurrent
// commit issued its own uncoalesced store.SyncDurable, so N files committing at
// once paid N independent fsyncs — the metadata wall. Routing them through one
// leader collapses a concurrent burst onto one barrier.
//
// Durability contract: Sync blocks until a barrier that ran AFTER this caller
// enqueued has completed, and returns that barrier's result. The barrier always
// completes before Sync returns — this is STRICT group-commit, not async write-
// back. NFS COMMIT depends on the synchronous ack. (Correctness of the "ran
// after enqueue" guarantee: a caller that enqueues while a leader is mid-barrier
// lands in the NEXT batch, which the leader always drains — it only exits after
// locking and finding the queue empty, and the enqueue-under-lock happens-before
// that check. So its write, committed before enqueue, is covered by a fresh
// barrier.)
//
// Adaptive bypass: a lone caller arriving at an idle leader runs the barrier
// inline (one drain pass) with no goroutine hand-off, mirroring syncLeader.
type commitLeader struct {
	// barrier makes every write committed so far on the owning store durable.
	barrier func() error

	mu      sync.Mutex
	pending []chan error
	running bool

	// drainPasses counts non-empty barrier batches. A burst that coalesces onto
	// one leader run shows far fewer passes than Sync calls; read by the
	// coalescing tests.
	drainPasses atomic.Int64
}

func newCommitLeader(barrier func() error) *commitLeader {
	return &commitLeader{barrier: barrier}
}

// Sync makes this caller's already-staged write durable, coalescing with any
// concurrent callers, and returns the shared barrier's result. On ctx
// cancellation the caller observes ctx.Err() but the barrier still runs
// (durability is never abandoned); the done channel is buffered so the leader
// never blocks on an abandoned waiter.
func (l *commitLeader) Sync(ctx context.Context) error {
	done := make(chan error, 1)

	l.mu.Lock()
	l.pending = append(l.pending, done)
	if l.running {
		// A leader is already draining; it will pick up our request on a later
		// pass. Follow.
		l.mu.Unlock()
		return commitWaitOn(ctx, done)
	}
	l.running = true
	l.mu.Unlock()

	// We are the leader: drain batches until the queue is empty. Requests that
	// arrive while we run the current barrier are collected on the next
	// iteration, so a concurrent burst collapses onto this single leader run.
	for {
		l.mu.Lock()
		batch := l.pending
		l.pending = nil
		if len(batch) == 0 {
			l.running = false
			l.mu.Unlock()
			break
		}
		l.mu.Unlock()

		l.drainPasses.Add(1)
		err := l.barrier()
		for _, d := range batch {
			d <- err
		}
	}
	return commitWaitOn(ctx, done)
}

// commitWaitOn blocks until ch delivers a result or ctx is canceled. Returning
// ctx.Err() does not abort the barrier — the channel cap is 1 so the leader's
// eventual send still succeeds.
func commitWaitOn(ctx context.Context, ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

package fs

import (
	"context"
	"sync"
	"sync/atomic"
)

// syncLeader batches concurrent per-fd append-log fsyncs into as few
// filesystem-journal commits as possible. It is a SINGLE coordinator per
// *FSStore (not per-file): every log fd's fsync — whether issued inline by a
// SyncEveryWrite AppendWrite or at COMMIT/CLOSE by SyncPayload — is submitted
// here.
//
// Why one store-level leader (PR3 / #1416): before this, each open log had its
// own coordinator, so N distinct payloads each doing "one write + fsync"
// produced N independent fsyncs. Independent-fd fsyncs do not naturally
// coalesce (ext4 data=ordered tends to serialize them), so the metadata / many
// small-files workload paid ~one journal commit per file. Routing every fd
// through one leader issues the pending fsyncs back-to-back in a single drain
// pass, so the filesystem journal collapses them into (ideally) one commit — N
// files amortize to ~one commit's latency.
//
// Durability contract: Sync blocks until THIS caller's fd fsync has completed
// and returns that fd's result. Unlike the old per-file coordinator, waiters do
// NOT share one fsync call — they hold different fds, so each fd is fsynced
// exactly once and each caller sees only its own fd's error. NFS COMMIT / SMB
// Flush depend on this synchronous contract (no async ack).
//
// Adaptive bypass: a lone caller arriving at an idle leader runs its fsync
// inline (one drain iteration) with zero added latency and no goroutine
// hand-off.
//
// Lock order: the submitting caller holds its per-file `mu` across Sync (so its
// fd cannot be closed under it), then leader.mu. The leader NEVER touches a
// shard lock — enforced by the TestSyncLeader_NoShardLockTouch grep gate.
//
// ponytail: the leader can run a follower's fsync after that follower already
// returned ctx.Err() and released its per-file mu; a racing DeleteAppendLog may
// then Close the fd mid-fsync. os.File serializes Sync/Close internally, so the
// fsync just returns ErrClosed into a buffered, unread channel — no corruption.
// This matches the pre-PR3 documented posture.
type syncLeader struct {
	mu      sync.Mutex
	pending []syncWaiter
	running bool

	// drainPasses counts non-empty batches the leader has processed. A burst
	// that coalesces onto one leader run shows far fewer passes than
	// submissions; read by TestSyncLeader coalescing assertions.
	drainPasses atomic.Int64
}

// syncWaiter is one enqueued fsync request: the fd's fsync closure and a
// buffered channel the leader delivers that fd's result on.
type syncWaiter struct {
	fsync func() error
	done  chan error
}

func newSyncLeader() *syncLeader { return &syncLeader{} }

// Sync submits fsync for this caller's fd and blocks until it (and the batch it
// joins) has run, returning this fd's fsync result. On ctx cancellation the
// caller observes ctx.Err(), but the fsync still runs for durability — the done
// channel is buffered so the leader never blocks on an abandoned waiter.
func (l *syncLeader) Sync(ctx context.Context, fsync func() error) error {
	w := syncWaiter{fsync: fsync, done: make(chan error, 1)}

	l.mu.Lock()
	l.pending = append(l.pending, w)
	if l.running {
		// A leader is already draining; it will pick up our request on its
		// next pass. Follow.
		l.mu.Unlock()
		return waitOn(ctx, w.done)
	}
	l.running = true
	l.mu.Unlock()

	// We are the leader: drain batches until the queue is empty. Requests that
	// arrive while we run the current batch are collected on the next
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
		// Issue the batch's fsyncs back-to-back so the filesystem journal
		// coalesces them, then hand each waiter its own fd's result.
		for _, bw := range batch {
			bw.done <- bw.fsync()
		}
	}
	return waitOn(ctx, w.done)
}

// waitOn blocks until ch delivers a result or ctx is canceled. Returning
// ctx.Err() does not abort the fsync — the channel cap is 1 so the leader's
// eventual send still succeeds.
func waitOn(ctx context.Context, ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

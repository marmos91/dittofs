package engine

import (
	"context"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/block/syncer"
)

// countingSink tracks how many CommitBlock calls are in flight at once — the
// quantity the upload window exists to bound, since each in-flight commit holds
// a live carver arena.
type countingSink struct {
	inner    BlockSink
	inFlight atomic.Int64
	peak     atomic.Int64
	calls    atomic.Int64
}

func (c *countingSink) CommitBlock(ctx context.Context, chunks []CarveChunk) error {
	c.calls.Add(1)
	cur := c.inFlight.Add(1)
	for {
		peak := c.peak.Load()
		if cur <= peak || c.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer c.inFlight.Add(-1)
	// A real remote takes time to accept a block, and that overlap is the whole
	// point: with an instant sink the goroutines finish before their successors
	// start, so an absent window looks identical to a working one.
	time.Sleep(25 * time.Millisecond)
	return c.inner.CommitBlock(ctx, chunks)
}

// TestUploadWindowBoundsConcurrentCommits is the guard whose absence let the
// window ship inert. #2551 moved the per-file bound out of journal's carve loop
// into the sink, behind a type assertion on an interface with unexported
// methods that nothing implemented — so it never engaged, and a file's blocks
// committed all at once with every arena live. Nothing failed, because nothing
// measured concurrency.
//
// The assertion is on observed concurrency rather than on wiring: a test that
// checks the semaphore is non-nil, or that some interface is satisfied, would
// have passed against the broken build too.
func TestUploadWindowBoundsConcurrentCommits(t *testing.T) {
	ctx := context.Background()
	const window = 2
	const blockSize = 32 << 10
	const blocks = 8

	f := newChunkedCarveFixture(t, remotememory.New(), blockSize,
		chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10})

	// Distinct bytes per block: identical payloads would dedup into one block
	// and there would be no concurrency to bound.
	rng := rand.New(rand.NewSource(7))
	for range blocks {
		payload := make([]byte, blockSize)
		if _, err := rng.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		f.storeChunk(t, ctx, payload)
	}

	sink := &countingSink{inner: engineBlockSink{
		sealer:      f.syncer.chunkSealer,
		rbs:         f.remote,
		committer:   f.syncer.blockCommitter,
		commitLocks: &carveCommitLocks{},
	}}
	closure, reap := newFlushClosure(f.local, chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
		blockSize, engineDeduper{synced: f.syncer.syncedHashStore}, sink, syncer.NewDynamicSemaphore(window))

	if err := f.local.Flush(ctx, carveFixturePayload,
		journal.FlushOptions{Force: true, AfterFile: reap}, closure); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Without more blocks than slots the bound is untested: the window could be
	// absent entirely and the peak would still sit at or below it.
	if got := sink.calls.Load(); got <= window {
		t.Fatalf("only %d blocks committed, need > window (%d) for the bound to mean anything", got, window)
	}
	if peak := sink.peak.Load(); peak > window {
		t.Errorf("peak concurrent CommitBlock = %d, want <= window %d: the upload window is not bounding in-flight arenas", peak, window)
	}
}

// nopSink accepts every block; the chain's own bookkeeping is what is on trial.
type nopSink struct{}

func (nopSink) CommitBlock(context.Context, []CarveChunk) error { return nil }

// TestUploadChainSurfacesCancelledSlotAcquire pins that a block dropped at the
// window gate still fails the pass.
//
// A cancelled acquire submits nothing, so the block gets no flight and the
// resolve loop finds every flight it does have in good order. Journal checks
// cancellation at the TOP of each run, so on the file's LAST run there is no
// next iteration to notice: the loop ends, Flush returns nil, and the caller
// reads a successful flush over bytes that never left. They stay dirty rather
// than lost, but a COMMIT answering "durable" for them is a lie.
func TestUploadChainSurfacesCancelledSlotAcquire(t *testing.T) {
	slots := syncer.NewDynamicSemaphore(1)
	if err := slots.Acquire(context.Background()); err != nil {
		t.Fatalf("seed acquire: %v", err)
	} // the only slot is taken, so the submit below must wait

	u := newUploadChain(nopSink{}, slots)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.submit(ctx, []CarveChunk{{FileOffset: 0, Size: 4 << 10}},
		[]journal.Extent{{Off: 0, Len: 4 << 10}})

	out, err := u.collect()
	if err == nil {
		t.Fatalf("collect returned (%v, nil): a block dropped at the window gate must fail the pass", out)
	}
	if len(out) != 0 {
		t.Errorf("collect returned extents %v: nothing was submitted, so nothing is durable", out)
	}
}

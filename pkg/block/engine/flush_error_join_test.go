package engine

import (
	"context"
	"errors"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/block/syncer"
)

var errCommitRefused = errors.New("commit refused")

// failFirstHeldSink refuses the first block and parks every other one inside
// CommitBlock until the test releases it. The first flight is the one collect()
// stops at, so the pass returns an error while its siblings are held — and they
// stay held, rather than finishing on a timer, so whether the test observes
// them does not depend on when a goroutine happens to be scheduled.
type failFirstHeldSink struct {
	release  chan struct{}
	calls    atomic.Int64
	inFlight atomic.Int64
}

func (s *failFirstHeldSink) CommitBlock(ctx context.Context, _ []CarveChunk) error {
	if s.calls.Add(1) == 1 {
		return errCommitRefused
	}
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}

// TestFlushErrorPathJoinsInFlightUploads is the guard for the gap #2778's
// shutdown fence does not cover: a carve pass that returns an error leaves the
// uploads it launched running, with nothing waiting on them. The fence then
// closes the stores those commits write through while they are still writing.
//
// The assertion is that Flush does not return while a sibling sits inside
// CommitBlock — observed work, not the presence of a drain call. A test that
// asserted the WaitGroup is waited, or that some join exists, would pass
// against a join placed where it cannot help.
//
// The window is wider than the pass can fill on purpose. submit() acquires its
// slot in the CALLER, so a window narrower than the block count parks the carve
// loop in Acquire and collect() is not reached until the last flight was
// spawned an instant earlier — which is the one state in which a missing join
// leaves nothing observable behind.
func TestFlushErrorPathJoinsInFlightUploads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const blockSize = 32 << 10
	const blocks = 4
	const window = 64 // never binding: see above

	params := chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10}
	f := newChunkedCarveFixture(t, remotememory.New(), blockSize, params)

	rng := rand.New(rand.NewSource(11))
	for range blocks {
		payload := make([]byte, blockSize)
		if _, err := rng.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		f.storeChunk(t, ctx, payload)
	}

	sink := &failFirstHeldSink{release: make(chan struct{})}
	closure, reap := newFlushClosure(f.local, params, blockSize,
		engineDeduper{synced: f.syncer.syncedHashStore}, sink, syncer.NewDynamicSemaphore(window))

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- f.local.Flush(ctx, carveFixturePayload,
			journal.FlushOptions{Force: true, AfterFile: reap}, closure)
	}()

	// Wait until two siblings are parked inside CommitBlock. Without a sibling
	// in flight there is nothing for the join to wait on, and the assertion
	// below would pass against both builds.
	deadline := time.After(30 * time.Second)
	for sink.inFlight.Load() < 2 {
		select {
		case err := <-flushDone:
			t.Fatalf("Flush returned before two siblings were in flight (err=%v, calls=%d): this run cannot detect a missing join", err, sink.calls.Load())
		case <-deadline:
			t.Fatalf("no two concurrent CommitBlock calls after 30s (calls=%d)", sink.calls.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// The failing flight resolves on its own, so a pass with no join returns
	// here within microseconds. The held siblings never resolve by themselves,
	// so a correct pass cannot return at all until the release below: load can
	// only delay the broken build's return, never produce one.
	select {
	case err := <-flushDone:
		close(sink.release)
		t.Fatalf("Flush returned (err=%v) with %d upload(s) still inside CommitBlock: the error path did not join its flights, so shutdown can close the stores underneath them", err, sink.inFlight.Load())
	case <-time.After(time.Second):
	}

	close(sink.release)
	select {
	case err := <-flushDone:
		if !errors.Is(err, errCommitRefused) {
			t.Fatalf("Flush error = %v, want %v", err, errCommitRefused)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Flush did not return after the held uploads were released")
	}
}

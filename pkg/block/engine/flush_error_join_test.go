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

// failFirstSlowSink fails the first block and makes every other block take
// long enough that it is unambiguously still uploading when the failure
// resolves. The first flight is the one collect() stops at, so without a join
// on the error path its siblings are still inside CommitBlock when Flush
// returns.
type failFirstSlowSink struct {
	delay    time.Duration
	calls    atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
}

func (s *failFirstSlowSink) CommitBlock(ctx context.Context, _ []CarveChunk) error {
	n := s.calls.Add(1)
	cur := s.inFlight.Add(1)
	for {
		peak := s.peak.Load()
		if cur <= peak || s.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer s.inFlight.Add(-1)
	if n == 1 {
		return errCommitRefused
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	return nil
}

// TestFlushErrorPathJoinsInFlightUploads is the guard for the gap #2778's
// shutdown fence does not cover: a carve pass that returns an error leaves the
// uploads it launched running, with nothing waiting on them. The fence then
// closes the stores those commits write through while they are still writing.
//
// The assertion is on observed in-flight work at the moment Flush returns, not
// on the presence of a drain call — a test that asserted the WaitGroup is
// waited, or that some join exists, would pass against a join placed where it
// cannot help.
//
// A loaded machine makes this test MORE decisive rather than flakier: delay is
// how long a sibling is still uploading when the first block fails, so
// saturation widens the window the bug needs and cannot manufacture a pass.
func TestFlushErrorPathJoinsInFlightUploads(t *testing.T) {
	ctx := context.Background()
	const window = 3
	const blockSize = 32 << 10
	const blocks = 8

	f := newChunkedCarveFixture(t, remotememory.New(), blockSize,
		chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10})

	rng := rand.New(rand.NewSource(11))
	for range blocks {
		payload := make([]byte, blockSize)
		if _, err := rng.Read(payload); err != nil {
			t.Fatalf("rand: %v", err)
		}
		f.storeChunk(t, ctx, payload)
	}

	sink := &failFirstSlowSink{delay: 300 * time.Millisecond}
	closure, reap := newFlushClosure(f.local, chunker.Params{Min: 4 << 10, Avg: 8 << 10, Max: 16 << 10},
		blockSize, engineDeduper{synced: f.syncer.syncedHashStore}, sink, syncer.NewDynamicSemaphore(window))

	err := f.local.Flush(ctx, carveFixturePayload,
		journal.FlushOptions{Force: true, AfterFile: reap}, closure)
	inFlight := sink.inFlight.Load()

	if err == nil {
		t.Fatal("Flush returned nil: the sink refused the first block, so the pass must fail")
	}

	// Without concurrent siblings there is nothing for the join to wait on and
	// the assertion below passes against both builds.
	if peak := sink.peak.Load(); peak < 2 {
		t.Fatalf("peak concurrent CommitBlock = %d: no sibling was ever in flight alongside the failing block, so this run cannot detect a missing join", peak)
	}

	if inFlight != 0 {
		t.Errorf("Flush returned with %d upload(s) still in CommitBlock: the error path did not join its flights, so shutdown can close the stores underneath them", inFlight)
	}
}

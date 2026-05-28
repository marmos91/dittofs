//go:build race

package fs

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// TestFSStore_OnChunkComplete_RaceFreeSwap guards against regression of
// audit finding C-1: SetOnChunkComplete used to write the field bare
// while rollup workers read it on the hot path (chunkstore.StoreChunk),
// which tripped go test -race.
//
// We don't need to exercise real rollup logic — the data race lives in
// the bare field swap vs the bare field read. Concurrent SetOnChunkComplete
// against goroutines that mimic chunkstore's load-and-invoke pattern is
// sufficient to surface the race under -race.
func TestFSStore_OnChunkComplete_RaceFreeSwap(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})

	const iterations = 500
	const readers = 2
	const writers = 2

	var stop atomic.Bool
	var invocations atomic.Int64
	var writersWG sync.WaitGroup
	var readersWG sync.WaitGroup

	// Reader goroutines: mimic chunkstore.StoreChunk's load-and-invoke
	// pattern. Run until writers signal stop.
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for !stop.Load() {
				if cb := bc.onChunkComplete.Load(); cb != nil && cb.fn != nil {
					cb.fn(blockstore.ContentHash{}, nil, "")
					invocations.Add(1)
				}
			}
		}()
	}

	// Writer goroutines: continuously swap fresh callbacks via the
	// setter, then exit. The variety of fn values (non-nil distinct
	// closures, nil) widens the window for races on both the holder
	// pointer and the inner fn read.
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(id int) {
			defer writersWG.Done()
			for i := 0; i < iterations; i++ {
				localID := id
				bc.SetOnChunkComplete(func(_ blockstore.ContentHash, _ []byte, _ string) {
					_ = localID
				})
				if i%17 == 0 {
					// Exercise the nil-fn install path too — readers must
					// gracefully skip without panic.
					bc.SetOnChunkComplete(nil)
				}
			}
		}(w)
	}

	writersWG.Wait()
	stop.Store(true)
	readersWG.Wait()

	// A non-flaky lower bound is hard to guarantee (scheduler-dependent),
	// but in practice readers fire many times. We assert only that the
	// test reached a clean end — the real signal here is -race not
	// reporting a data race on bc.onChunkComplete.
	_ = invocations.Load()
}

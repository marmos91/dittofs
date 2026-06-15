package engine

import (
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestDestroyCache_ConcurrentReads_NoRace exercises the cache-field swap in
// DestroyCache against concurrent readers (the OnChunkComplete rollup
// goroutine reads bs.cache with no closeMu, and data ops read it under
// closeMu.RLock, which does NOT serialize against the swap). Run under
// `go test -race`, an unsynchronized field write would be flagged here.
//
// The fix routes every cache access through cacheMu (loadCache/storeCache),
// so this test must be race-clean.
func TestDestroyCache_ConcurrentReads_NoRace(t *testing.T) {
	bs := newTestEngine(t, 1<<20, 1) // non-zero budget → real cache installed

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: mimic the OnChunkComplete closure (loadCache().Put) and the
	// data-op readers (loadCache().Stats / OnRead) hammering the field.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var h block.ContentHash
			for {
				select {
				case <-stop:
					return
				default:
					c := bs.loadCache()
					c.Put(h, []byte{0x1})
					_ = c.Stats()
				}
			}
		}()
	}

	// Writer: repeatedly destroy the cache (swaps in nullCache{}).
	for i := 0; i < 50; i++ {
		bs.DestroyCache()
	}
	close(stop)
	wg.Wait()
}

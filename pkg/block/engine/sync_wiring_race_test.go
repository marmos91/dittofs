package engine

import (
	"sync"
	"testing"

	memorylocal "github.com/marmos91/dittofs/pkg/block/local/memory"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestFlushFnReadsWiringUnderLock pins that building a flush closure and
// re-wiring the syncer cannot race.
//
// remoteBlockStore, chunkSealer, blockCommitter and syncedHashStore are written
// by the setters under m.mu — three of them say "Guarded by m.mu" on the struct
// — and flushFn used to read all four with no lock at all. The race detector
// stayed quiet because every current caller finishes wiring before Start, so
// the window is only reachable when a setter runs on an already-serving share.
// That is not hypothetical for this type: SetMetrics already back-fills on a
// serving share, which is why the syncer holds a metrics cell rather than a
// value.
//
// This drives the two concurrently on purpose. Run under -race it fails on the
// unlocked read; the assertion is the detector's, so there is nothing to check
// afterwards beyond not deadlocking.
func TestFlushFnReadsWiringUnderLock(t *testing.T) {
	local := memorylocal.New()
	t.Cleanup(func() { _ = local.Close() })
	rbs := remotememory.New()
	t.Cleanup(func() { _ = rbs.Close() })

	m := NewRemoteSync(local, rbs, newStubFileChunkStore(), RemoteSyncConfig{})

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)

	// Re-wire on a serving share, exactly what the setters exist to allow.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				m.SetRemoteBlockStore(rbs)
			} else {
				m.SetRemoteBlockStore(nil)
			}
			m.SetSyncedHashStore(nil)
		}
	}()

	// Build flush closures against whatever is wired at the time.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			flush, after := m.flushFn()
			if flush == nil || after == nil {
				t.Errorf("flushFn returned a nil closure")
				return
			}
		}
	}()

	wg.Wait()
}

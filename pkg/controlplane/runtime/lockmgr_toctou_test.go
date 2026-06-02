package runtime

import (
	"sync"
	"testing"
)

// TestRemoveShare_ConcurrentLockManagerLookup is the area-7 M1 -race reproducer
// at the Runtime façade. RemoveShare deregisters a share's lock manager from the
// MetadataService (via RemoveStoreForShare) while in-flight protocol ops may
// still resolve it through the GetLockManagerForShare lazy getter. Run under
// -race it exercises the register/remove TOCTOU on the per-share lock-manager
// registry: a RemoveShare must never leave a resurrected lock manager behind, and
// concurrent lookups must not race the removal.
//
// The fix lives in MetadataService.RegisterStoreForShare/RemoveStoreForShare (a
// per-share removal generation that makes a register decline to publish when a
// removal landed mid-flight); this test drives that path through the real
// AddShare/RemoveShare lifecycle the control plane uses.
func TestRemoveShare_ConcurrentLockManagerLookup(t *testing.T) {
	rt, s := setupTestRuntime(t)
	localID := createLocalBlockStoreConfig(t, s, "local-lockmgr-toctou")

	const shareName = "/lockmgr-toctou"
	addShareViaRuntime(t, rt, s, shareName, localID)

	meta := rt.GetMetadataService()

	var wg sync.WaitGroup

	// Reader: hammers the lazy getter the REVIEW names, concurrent with removal.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = meta.GetLockManagerForShare(shareName)
		}
	}()

	// Remover: removes the share while lookups are in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = rt.RemoveShare(shareName)
	}()

	wg.Wait()

	// After RemoveShare the lock manager must be fully gone — not resurrected by
	// a racing lookup or a late register publish.
	if lm := meta.GetLockManagerForShare(shareName); lm != nil {
		t.Fatalf("lock manager for removed share %q must not be resurrected", shareName)
	}
}

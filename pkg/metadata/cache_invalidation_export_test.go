package metadata

import (
	"sync"
	"testing"
)

// PauseCacheOnlyFlushForTest holds the real flush stripe between a cache-only
// flush's pop and republish, exposing precisely the window invalidation joins.
func PauseCacheOnlyFlushForTest(t testing.TB, svc *Service, handle FileHandle) func() {
	t.Helper()
	mu := svc.pendingWrites.GetFlushLock(handle)
	mu.Lock()
	state, ok := svc.pendingWrites.PopPending(handle)
	if !ok || state.CachedFile == nil || state.MaxSize != 0 || !state.LastMtime.IsZero() || state.ClearSetuidSetgid {
		mu.Unlock()
		t.Fatal("cache-only flush requires cached attributes without pending writes")
	}
	var once sync.Once
	resume := func() {
		once.Do(func() {
			svc.pendingWrites.SetCachedFile(handle, state.CachedFile)
			mu.Unlock()
		})
	}
	t.Cleanup(resume)
	return resume
}

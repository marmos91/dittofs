package fs

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// TestPut_BoundsReadThroughCache asserts that FSStore.Put — the read-through
// cache's write entry, whose only production callers are the syncer's
// remote-fetch paths in engine/fetch.go — reserves capacity via ensureSpace
// before storing. Before #1362's fix, Put delegated straight to StoreChunk,
// which never calls ensureSpace, so a read-only workload over a remote tier
// grew the local CAS store without bound past maxDisk (eviction only ran on
// the write/append path). This test fails without the Put-side ensureSpace.
func TestPut_BoundsReadThroughCache(t *testing.T) {
	const chunk = 200
	const maxDisk = 3 * chunk // exactly three chunks fit; a fourth must evict
	bc := newTestCacheWithDiskLimit(t, maxDisk)
	ctx := context.Background()

	// Seed three chunks via the canonical write path, then add a fourth via
	// the read-cache Put. With a nil SyncedHashStore every chunk is evictable.
	h1 := storeChunk(t, bc, bytes.Repeat([]byte{0x01}, chunk))
	_ = storeChunk(t, bc, bytes.Repeat([]byte{0x02}, chunk))
	_ = storeChunk(t, bc, bytes.Repeat([]byte{0x03}, chunk))
	if got := bc.diskUsed.Load(); got != maxDisk {
		t.Fatalf("seed diskUsed=%d, want %d", got, maxDisk)
	}

	fetched := bytes.Repeat([]byte{0x04}, chunk)
	hFetched := hashBytes(fetched)
	if err := bc.Put(ctx, hFetched, fetched); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if got := bc.diskUsed.Load(); got > maxDisk {
		t.Fatalf("after read-cache Put diskUsed=%d exceeds maxDisk=%d — cache unbounded (#1362)", got, maxDisk)
	}
	if _, err := os.Stat(bc.chunkPath(hFetched)); err != nil {
		t.Fatalf("freshly fetched chunk must be present after Put: %v", err)
	}
	if _, err := os.Stat(bc.chunkPath(h1)); !os.IsNotExist(err) {
		t.Errorf("LRU-oldest chunk should have been evicted to make room; stat err=%v", err)
	}
}

// TestPut_IdempotentRePutDoesNotEvict guards the !exists gate: re-Putting a
// chunk that is already present must not reserve capacity, because StoreChunk
// is idempotent and writes nothing. Reserving unconditionally would evict
// other live chunks to make room for bytes that are never added.
func TestPut_IdempotentRePutDoesNotEvict(t *testing.T) {
	const chunk = 200
	const maxDisk = 2 * chunk
	bc := newTestCacheWithDiskLimit(t, maxDisk)
	ctx := context.Background()

	a := bytes.Repeat([]byte{0xAA}, chunk)
	hA := hashBytes(a)
	if err := bc.Put(ctx, hA, a); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	hB := storeChunk(t, bc, bytes.Repeat([]byte{0xBB}, chunk))

	// Re-Put A (already present): must be a capacity no-op so B survives.
	if err := bc.Put(ctx, hA, a); err != nil {
		t.Fatalf("re-Put A: %v", err)
	}
	if _, err := os.Stat(bc.chunkPath(hB)); err != nil {
		t.Errorf("re-Put of an existing chunk must not evict others; B missing: %v", err)
	}
	if got := bc.diskUsed.Load(); got != maxDisk {
		t.Errorf("diskUsed=%d, want %d (no growth on idempotent re-Put)", got, maxDisk)
	}
}

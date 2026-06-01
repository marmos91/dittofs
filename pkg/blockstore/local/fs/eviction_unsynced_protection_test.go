package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newCacheWithSyncedStore builds an FSStore with a small disk limit (so
// eviction is easy to trigger) and a real in-memory SyncedHashStore wired
// via FSStoreOptions, so eviction can consult per-hash sync state.
func newCacheWithSyncedStore(t *testing.T, maxDisk int64) (*FSStore, *memory.MemoryMetadataStore) {
	t.Helper()
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(dir, maxDisk, 256*1024*1024, mds, FSStoreOptions{
		SyncedHashStore: mds,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)
	t.Cleanup(func() { _ = bc.Close() })
	return bc, mds
}

// TestLRU_UnsyncedChunk_lruEvictOneReturnsEmpty asserts that when every
// LRU candidate is unsynced, lruEvictOne evicts NONE of them and returns
// errLRUEmpty directly — no on-disk file is removed and no 30s back-
// pressure wait is incurred. Evicting an unsynced chunk before its first
// mirror destroys the only copy — this is the data-loss bug the fix
// closes. Tested at lruEvictOne (not ensureSpace) so the assertion is
// immediate.
func TestLRU_UnsyncedChunk_lruEvictOneReturnsEmpty(t *testing.T) {
	bc, _ := newCacheWithSyncedStore(t, 600)
	ctx := context.Background()

	hA := storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 200))
	hB := storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 200))
	hC := storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 200))

	// Every chunk is unsynced -> no evictable candidate -> errLRUEmpty,
	// with zero files removed and zero bytes freed.
	freed, err := bc.lruEvictOne(ctx)
	if !errors.Is(err, errLRUEmpty) {
		t.Fatalf("lruEvictOne over unsynced-only LRU: got (%d, %v), want (0, errLRUEmpty)", freed, err)
	}
	if freed != 0 {
		t.Fatalf("lruEvictOne freed %d bytes; want 0 (nothing evictable)", freed)
	}

	// No unsynced chunk file may have been deleted.
	for _, h := range []blockstore.ContentHash{hA, hB, hC} {
		if _, err := os.Stat(bc.chunkPath(h)); err != nil {
			t.Errorf("unsynced chunk %s was evicted (stat err=%v) — data loss", h, err)
		}
	}
}

// TestLRU_UnsyncedChunk_NotEvictedBeforeMirror asserts the same data-loss
// guard through the ensureSpace back-pressure path: an unsynced-only LRU
// yields ErrDiskFull and no file is deleted. evictMaxWait is shrunk so the
// back-pressure deadline trips in milliseconds instead of the 30s default.
func TestLRU_UnsyncedChunk_NotEvictedBeforeMirror(t *testing.T) {
	// maxDisk=600 holds two 200B chunks but not three; ensureSpace will
	// want to evict to make room. Every chunk is unsynced, so none may go.
	bc, _ := newCacheWithSyncedStore(t, 600)
	bc.evictMaxWait = 50 * time.Millisecond // avoid the 30s back-pressure wait
	ctx := context.Background()

	hA := storeChunk(t, bc, bytes.Repeat([]byte{0x01}, 200))
	hB := storeChunk(t, bc, bytes.Repeat([]byte{0x02}, 200))
	hC := storeChunk(t, bc, bytes.Repeat([]byte{0x03}, 200))

	// diskUsed=600, maxDisk=600, request 200 -> over budget, must evict.
	// All three chunks are unsynced -> no eviction candidate -> ErrDiskFull.
	if err := bc.ensureSpace(ctx, 200); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("ensureSpace over unsynced-only LRU: got %v, want ErrDiskFull", err)
	}

	// No unsynced chunk file may have been deleted.
	for _, h := range []blockstore.ContentHash{hA, hB, hC} {
		if _, err := os.Stat(bc.chunkPath(h)); err != nil {
			t.Errorf("unsynced chunk %s was evicted (stat err=%v) — data loss", h, err)
		}
	}
}

// TestLRU_SyncedChunk_EvictedBeforeUnsynced asserts that when only one of
// the LRU candidates is synced, eviction picks the SYNCED chunk and
// leaves the unsynced one on disk.
func TestLRU_SyncedChunk_EvictedBeforeUnsynced(t *testing.T) {
	// maxDisk=600 holds two 200B chunks; requesting more forces one evict.
	bc, mds := newCacheWithSyncedStore(t, 600)
	ctx := context.Background()

	// hSynced is inserted first (LRU back / oldest); hUnsynced second.
	hSynced := storeChunk(t, bc, bytes.Repeat([]byte{0x10}, 200))
	hUnsynced := storeChunk(t, bc, bytes.Repeat([]byte{0x11}, 200))

	if err := mds.MarkSynced(ctx, hSynced); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Touch the synced chunk so it becomes the LRU FRONT (most recent) —
	// i.e. the unsynced chunk is the oldest and would be the natural first
	// victim. The fix must skip it and evict the synced one instead.
	bc.lruTouch(hSynced, 200, bc.chunkPath(hSynced))

	// diskUsed=400, maxDisk=600, request 300 -> 700 > 600 -> evict one.
	if err := bc.ensureSpace(ctx, 300); err != nil {
		t.Fatalf("ensureSpace: %v", err)
	}

	if _, err := os.Stat(bc.chunkPath(hSynced)); !os.IsNotExist(err) {
		t.Errorf("synced chunk should have been evicted (stat err=%v)", err)
	}
	if _, err := os.Stat(bc.chunkPath(hUnsynced)); err != nil {
		t.Errorf("unsynced chunk must remain on disk: %v", err)
	}
}

// slowSyncedHashStore wraps a SyncedHashStore and adds a small delay to
// IsSynced, widening the unlocked window in lruEvictOne so a concurrent
// lruTouch is far more likely to interleave — surfacing the duplicate-
// element / ghost-entry race the optimistic peek-recheck closes.
type slowSyncedHashStore struct {
	inner metadata.SyncedHashStore
	delay time.Duration
}

func (s *slowSyncedHashStore) IsSynced(ctx context.Context, h blockstore.ContentHash) (bool, error) {
	time.Sleep(s.delay)
	return s.inner.IsSynced(ctx, h)
}

func (s *slowSyncedHashStore) MarkSynced(ctx context.Context, h blockstore.ContentHash) error {
	return s.inner.MarkSynced(ctx, h)
}

func (s *slowSyncedHashStore) DeleteSynced(ctx context.Context, h blockstore.ContentHash) error {
	return s.inner.DeleteSynced(ctx, h)
}

// TestLRU_EvictTouchRace hammers lruEvictOne against concurrent lruTouch
// for the SAME hashes while IsSynced is artificially slow. The old
// pop-before-IsSynced design left the victim absent from lruIndex during
// the unlocked IsSynced call, so a concurrent touch inserted a SECOND
// list element (ghost entry) — lruIndex would then point at only one of
// two duplicates and disk accounting would drift. The optimistic peek-
// recheck never removes the entry from the index across the unlocked
// call, so list and index stay in bijection. Run under -race; the post-
// condition asserts lruList.Len() == len(lruIndex) with every index
// element actually present in the list (no duplicates, no ghosts).
func TestLRU_EvictTouchRace(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(dir, 1<<30, 256*1024*1024, mds, FSStoreOptions{
		SyncedHashStore: &slowSyncedHashStore{inner: mds, delay: 200 * time.Microsecond},
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	bc.SetEvictionEnabled(true)
	bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)
	t.Cleanup(func() { _ = bc.Close() })
	ctx := context.Background()

	const n = 64
	hashes := make([]blockstore.ContentHash, n)
	for i := range hashes {
		h := storeChunk(t, bc, bytes.Repeat([]byte{byte(i)}, 64))
		hashes[i] = h
		// Mark half synced so eviction actually removes some entries while
		// the rest are moved-to-front — exercising both recheck branches.
		if i%2 == 0 {
			if err := mds.MarkSynced(ctx, h); err != nil {
				t.Fatalf("MarkSynced: %v", err)
			}
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Toucher goroutines: continuously re-touch the same hashes, racing the
	// evictor's unlocked IsSynced window.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, h := range hashes {
					bc.lruTouch(h, 64, bc.chunkPath(h))
				}
			}
		}()
	}

	// Evictor goroutines: repeatedly evict the tail.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 64; i++ {
				_, _ = bc.lruEvictOne(ctx)
			}
		}()
	}

	// Let evictors run, then stop touchers.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Integrity: the list and the index must remain in exact bijection —
	// every index element present in the list exactly once, no ghosts.
	bc.lruMu.Lock()
	defer bc.lruMu.Unlock()
	if bc.lruList.Len() != len(bc.lruIndex) {
		t.Fatalf("LRU list/index diverged: list=%d index=%d (ghost entries)", bc.lruList.Len(), len(bc.lruIndex))
	}
	seen := make(map[blockstore.ContentHash]int, bc.lruList.Len())
	for el := bc.lruList.Front(); el != nil; el = el.Next() {
		e := el.Value.(*lruEntry)
		seen[e.hash]++
		idxEl, ok := bc.lruIndex[e.hash]
		if !ok {
			t.Errorf("list element %s missing from index", e.hash)
		} else if idxEl != el {
			t.Errorf("index for %s points at a different element than the list (duplicate)", e.hash)
		}
	}
	for h, c := range seen {
		if c != 1 {
			t.Errorf("hash %s appears %d times in list (want 1) — duplicate element", h, c)
		}
	}
}

package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
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

// TestLRU_UnsyncedChunk_NotEvictedBeforeMirror asserts that when every
// LRU candidate is unsynced, ensureSpace evicts NONE of them and instead
// back-pressures with ErrDiskFull. Evicting an unsynced chunk before its
// first mirror destroys the only copy — this is the data-loss bug the
// fix closes.
func TestLRU_UnsyncedChunk_NotEvictedBeforeMirror(t *testing.T) {
	// maxDisk=600 holds two 200B chunks but not three; ensureSpace will
	// want to evict to make room. Every chunk is unsynced, so none may go.
	bc, _ := newCacheWithSyncedStore(t, 600)
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

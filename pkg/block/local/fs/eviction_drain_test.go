package fs

// Tests for DrainLocalSynced (#1595): the on-demand counterpart to reclaimSpace.
// reclaimSpace only frees blob bytes on the write/rollup path and only down to
// maxDisk, so with no disk pressure a sealed, fully-synced blob stays resident
// forever — the "sticky local store" that made the live-VM read-path sweep
// unmeasurable (reads served locally, s3_rx ≈ 0). DrainLocalSynced frees those
// blobs on demand while never dropping unsynced (remote-missing) bytes.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestDrainLocalSynced_FreesSealedSyncedBlobOnDemand is the headline: maxDisk=0
// means reclaimSpace/ensureSpace never fire, so a sealed fully-synced blob is
// sticky. DrainLocalSynced must still evict it on demand, dropping DiskUsed to 0
// and routing subsequent reads to the remote refetch path (ErrChunkNotFound).
func TestDrainLocalSynced_FreesSealedSyncedBlobOnDemand(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds) // 0 = no maxDisk cap → no auto-eviction
	ctx := context.Background()

	a := bytes.Repeat([]byte{0xA1}, 400)
	b := bytes.Repeat([]byte{0xA2}, 350)
	aH, bH := blake3ContentHash(a), blake3ContentHash(b)
	for _, kv := range []struct {
		h block.ContentHash
		d []byte
	}{{aH, a}, {bH, b}} {
		if err := bc.StoreChunk(ctx, kv.h, kv.d); err != nil {
			t.Fatalf("StoreChunk: %v", err)
		}
		if err := mds.MarkSynced(ctx, kv.h, block.ChunkLocator{}); err != nil {
			t.Fatalf("MarkSynced: %v", err)
		}
	}
	if err := bc.logBlob.Rotate(); err != nil { // seal so the blob is evictable
		t.Fatalf("Rotate: %v", err)
	}
	if got := bc.Stats().DiskUsed; got != 750 {
		t.Fatalf("DiskUsed before drain = %d, want 750", got)
	}

	freed, err := bc.DrainLocalSynced(ctx)
	if err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	if freed != 750 {
		t.Fatalf("DrainLocalSynced freed = %d, want 750", freed)
	}
	if got := bc.Stats().DiskUsed; got != 0 {
		t.Fatalf("DiskUsed after drain = %d, want 0 (fully drained)", got)
	}
	for _, h := range []block.ContentHash{aH, bH} {
		if _, err := bc.ReadChunk(ctx, h); !errors.Is(err, block.ErrChunkNotFound) {
			t.Fatalf("ReadChunk after drain = %v, want ErrChunkNotFound (routes to remote refetch)", err)
		}
	}

	// Idempotent: nothing left evictable → frees 0, no error.
	if freed, err := bc.DrainLocalSynced(ctx); err != nil || freed != 0 {
		t.Fatalf("second DrainLocalSynced = (%d, %v), want (0, nil)", freed, err)
	}
}

// TestDrainLocalSynced_SealsActiveBlob pins the small-store case (#1465): data
// below the 1 GiB roll threshold all lives in the still-open ACTIVE blob, which
// blobEvictOne refuses (sealed-only). Without an active-blob seal step the drain
// frees nothing and DiskUsed stays put — the exact e2e blocks-flip failure. Note
// this test does NOT call Rotate: DrainLocalSynced must seal on its own.
func TestDrainLocalSynced_SealsActiveBlob(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds) // 0 = no maxDisk cap → active blob never rolls
	ctx := context.Background()

	a := bytes.Repeat([]byte{0xB1}, 400)
	b := bytes.Repeat([]byte{0xB2}, 350)
	aH, bH := blake3ContentHash(a), blake3ContentHash(b)
	for _, kv := range []struct {
		h block.ContentHash
		d []byte
	}{{aH, a}, {bH, b}} {
		if err := bc.StoreChunk(ctx, kv.h, kv.d); err != nil {
			t.Fatalf("StoreChunk: %v", err)
		}
		if err := mds.MarkSynced(ctx, kv.h, block.ChunkLocator{}); err != nil {
			t.Fatalf("MarkSynced: %v", err)
		}
	}
	if got := bc.Stats().DiskUsed; got != 750 {
		t.Fatalf("DiskUsed before drain = %d, want 750", got)
	}

	freed, err := bc.DrainLocalSynced(ctx)
	if err != nil {
		t.Fatalf("DrainLocalSynced: %v", err)
	}
	if freed != 750 {
		t.Fatalf("DrainLocalSynced freed = %d, want 750 (must seal the active blob)", freed)
	}
	if got := bc.Stats().DiskUsed; got != 0 {
		t.Fatalf("DiskUsed after drain = %d, want 0 (active blob sealed + evicted)", got)
	}
}

// TestDrainLocalSynced_KeepsUnsyncedSurvivor pins the data-safety invariant: a
// sealed blob mixing synced (evictable) and unsynced (only copy) chunks is
// drained via compaction — the synced remainder is reclaimed, the unsynced
// survivor is relocated and stays readable, never lost.
func TestDrainLocalSynced_KeepsUnsyncedSurvivor(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)
	ctx := context.Background()

	synced := bytes.Repeat([]byte{0xC1}, 400)
	syncedH := blake3ContentHash(synced)
	unsynced := bytes.Repeat([]byte{0xD0}, 100)
	unsyncedH := blake3ContentHash(unsynced)
	if err := bc.StoreChunk(ctx, syncedH, synced); err != nil {
		t.Fatalf("StoreChunk(synced): %v", err)
	}
	if err := mds.MarkSynced(ctx, syncedH, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	if err := bc.StoreChunk(ctx, unsyncedH, unsynced); err != nil {
		t.Fatalf("StoreChunk(unsynced): %v", err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if freed, err := bc.DrainLocalSynced(ctx); err != nil || freed <= 0 {
		t.Fatalf("DrainLocalSynced = (%d, %v), want (>0, nil)", freed, err)
	}
	// Only the relocated unsynced survivor remains resident.
	if got := bc.Stats().DiskUsed; got != 100 {
		t.Fatalf("DiskUsed after drain = %d, want 100 (unsynced survivor only)", got)
	}
	if got, err := bc.ReadChunk(ctx, unsyncedH); err != nil || !bytes.Equal(got, unsynced) {
		t.Fatalf("unsynced survivor lost/corrupted after drain: err=%v", err)
	}
	if _, err := bc.ReadChunk(ctx, syncedH); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadChunk(drained synced) = %v, want ErrChunkNotFound", err)
	}
}

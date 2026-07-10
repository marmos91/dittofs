package fs

import (
	"bytes"
	"context"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestReclaimDeadBlobs_FreesReadThroughStagedActiveBlob reproduces the SMB
// blocks-flip local-disk leak: read-through-staged chunks (FSStore.Put) land in
// the still-open ACTIVE log blob. The mark-sweep GC drops their index entries
// (DeleteChunk) but blob bytes are reclaimed only by blob-level eviction, which
// skips the active blob — so DiskUsed stays non-zero after the sweep.
// ReclaimDeadBlobs seals the now-fully-dead active blob and evicts it → 0.
func TestReclaimDeadBlobs_FreesReadThroughStagedActiveBlob(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)

	// Stage chunks via the read-through Put path (what the syncer's inline
	// fetch / prefetch use) — they append to the active blob.
	payloads := [][]byte{
		bytes.Repeat([]byte{0x01}, 1024),
		bytes.Repeat([]byte{0x02}, 2048),
		bytes.Repeat([]byte{0x03}, 4096),
	}
	var hashes []block.ContentHash
	var want int64
	for _, p := range payloads {
		h := block.ContentHash(blake3.Sum256(p))
		if err := bc.Put(ctx, h, p); err != nil {
			t.Fatalf("Put: %v", err)
		}
		hashes = append(hashes, h)
		want += int64(len(p))
	}
	if got := bc.Stats().DiskUsed; got != want {
		t.Fatalf("DiskUsed after staging = %d, want %d", got, want)
	}

	// Simulate the GC mark-sweep dropping the (now-orphaned) chunks' index
	// entries — exactly what CollectGarbageLocal does via Delete/DeleteChunk.
	for _, h := range hashes {
		if err := bc.DeleteChunk(ctx, h); err != nil {
			t.Fatalf("DeleteChunk: %v", err)
		}
	}
	// The leak: index entries gone, but blob bytes remain (active blob unsealed).
	if got := bc.Stats().DiskUsed; got != want {
		t.Fatalf("DiskUsed after sweep = %d, want %d (leak precondition)", got, want)
	}

	freed, err := bc.ReclaimDeadBlobs(ctx)
	if err != nil {
		t.Fatalf("ReclaimDeadBlobs: %v", err)
	}
	if freed != want {
		t.Fatalf("ReclaimDeadBlobs freed = %d, want %d", freed, want)
	}
	if got := bc.Stats().DiskUsed; got != 0 {
		t.Fatalf("DiskUsed after reclaim = %d, want 0 (local leak not freed)", got)
	}
}

// TestReclaimDeadBlobs_KeepsBlobWithLiveChunk asserts the reclaim never drops a
// blob that still has a live index entry (no live-data loss).
func TestReclaimDeadBlobs_KeepsBlobWithLiveChunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)

	dead := bytes.Repeat([]byte{0x01}, 1024)
	live := bytes.Repeat([]byte{0x02}, 2048)
	hDead := block.ContentHash(blake3.Sum256(dead))
	hLive := block.ContentHash(blake3.Sum256(live))
	if err := bc.Put(ctx, hDead, dead); err != nil {
		t.Fatalf("Put dead: %v", err)
	}
	if err := bc.Put(ctx, hLive, live); err != nil {
		t.Fatalf("Put live: %v", err)
	}
	// Sweep only the dead chunk's index entry; the live one stays.
	if err := bc.DeleteChunk(ctx, hDead); err != nil {
		t.Fatalf("DeleteChunk: %v", err)
	}

	freed, err := bc.ReclaimDeadBlobs(ctx)
	if err != nil {
		t.Fatalf("ReclaimDeadBlobs: %v", err)
	}
	if freed != 0 {
		t.Fatalf("ReclaimDeadBlobs freed = %d, want 0 (blob has a live chunk)", freed)
	}
	// Live chunk must still be readable.
	got, err := bc.Get(ctx, hLive)
	if err != nil {
		t.Fatalf("Get live after reclaim: %v", err)
	}
	if !bytes.Equal(got, live) {
		t.Fatalf("live chunk corrupted after reclaim")
	}
}

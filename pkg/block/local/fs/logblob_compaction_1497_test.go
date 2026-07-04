package fs

// Tests for #1497 (part of the blocks-only epic #1493): local log-blob
// compaction. A sealed log blob mixes cold synced chunks (evictable, refetchable
// from the remote) with unsynced chunks (the only durable copy). blobEvictOne
// refuses the whole blob if any chunk is unsynced, so one small unsynced chunk
// pins a whole ~1 GB blob and --local-store-size becomes unenforceable.
//
// compactBlobOne relocates the unsynced survivors into the active blob
// (fsynced BEFORE the index is rewritten and BEFORE the old blob is dropped, so
// the only durable copy is never stranded) and then drops the old blob whole,
// reclaiming the cold remainder at sub-blob granularity.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCompaction_ReclaimsSyncedRemainderKeepsUnsynced is the core case: a sealed
// blob holding one synced chunk (cold, evictable) and one unsynced chunk (only
// copy). blobEvictOne refuses it; compaction relocates the unsynced survivor and
// reclaims the synced chunk's bytes.
func TestCompaction_ReclaimsSyncedRemainderKeepsUnsynced(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)
	ctx := context.Background()

	syncedData := bytes.Repeat([]byte{0x11}, 400)
	syncedHash := blake3ContentHash(syncedData)
	if err := bc.StoreChunk(ctx, syncedHash, syncedData); err != nil {
		t.Fatalf("StoreChunk(synced): %v", err)
	}
	if err := mds.MarkSynced(ctx, syncedHash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	unsyncedData := bytes.Repeat([]byte{0x22}, 100)
	unsyncedHash := blake3ContentHash(unsyncedData)
	if err := bc.StoreChunk(ctx, unsyncedHash, unsyncedData); err != nil {
		t.Fatalf("StoreChunk(unsynced): %v", err)
	}

	// Seal the blob so both chunks live in a sealed (compactable) blob.
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got := bc.Stats().DiskUsed; got != 500 {
		t.Fatalf("DiskUsed before compaction = %d, want 500", got)
	}

	// blobEvictOne must REFUSE (the blob is not fully synced).
	if freed, err := bc.blobEvictOne(ctx); !errors.Is(err, errLRUEmpty) || freed != 0 {
		t.Fatalf("blobEvictOne = (%d, %v), want (0, errLRUEmpty) for a partly-unsynced blob", freed, err)
	}

	// Compaction reclaims the synced remainder, relocating the unsynced survivor.
	if freed, err := bc.compactBlobOne(ctx); err != nil || freed <= 0 {
		t.Fatalf("compactBlobOne = (%d, %v), want (>0, nil)", freed, err)
	}

	// Net on-disk usage is now just the relocated unsynced survivor.
	if got := bc.Stats().DiskUsed; got != 100 {
		t.Fatalf("DiskUsed after compaction = %d, want 100 (only the unsynced survivor remains)", got)
	}

	// The unsynced survivor must still read back byte-identical from its new home.
	got, err := bc.ReadChunk(ctx, unsyncedHash)
	if err != nil {
		t.Fatalf("ReadChunk(unsynced survivor) after compaction: %v", err)
	}
	if !bytes.Equal(got, unsyncedData) {
		t.Fatalf("relocated unsynced survivor returned wrong bytes")
	}

	// The synced chunk was dropped (durable on the remote): a clean local miss
	// routes the engine to refetch.
	if _, err := bc.ReadChunk(ctx, syncedHash); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadChunk(synced, dropped) = %v, want ErrChunkNotFound", err)
	}
}

// TestCompaction_EntirelyUnsyncedBlob_Untouched pins the crash-stranded-data
// invariant: a sealed blob whose every chunk is unsynced (the only durable copy)
// has no reclaimable dead weight, so compaction must leave it entirely alone —
// never relocate-then-drop for zero net gain, and never lose the bytes.
func TestCompaction_EntirelyUnsyncedBlob_Untouched(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)
	ctx := context.Background()

	a := bytes.Repeat([]byte{0x33}, 200)
	aHash := blake3ContentHash(a)
	b := bytes.Repeat([]byte{0x44}, 150)
	bHash := blake3ContentHash(b)
	for _, d := range [][]byte{a, b} {
		if err := bc.StoreChunk(ctx, blake3ContentHash(d), d); err != nil {
			t.Fatalf("StoreChunk: %v", err)
		}
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Nothing to reclaim: compaction reports errLRUEmpty and touches nothing.
	if freed, err := bc.compactBlobOne(ctx); !errors.Is(err, errLRUEmpty) || freed != 0 {
		t.Fatalf("compactBlobOne on all-unsynced blob = (%d, %v), want (0, errLRUEmpty)", freed, err)
	}
	if got := bc.Stats().DiskUsed; got != 350 {
		t.Fatalf("DiskUsed = %d, want 350 (untouched)", got)
	}
	for _, tc := range []struct {
		h    block.ContentHash
		want []byte
	}{{aHash, a}, {bHash, b}} {
		got, err := bc.ReadChunk(ctx, tc.h)
		if err != nil {
			t.Fatalf("ReadChunk(unsynced) after compaction: %v", err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Fatalf("unsynced chunk bytes changed after compaction")
		}
	}
}

// TestCompaction_EnforcesLocalStoreSizeViaEnsureSpace is the headline: a Put
// that pushes past --local-store-size succeeds because ensureSpace compacts a
// sealed blob pinned by a small unsynced chunk — the exact enforcement gap
// #1497 closes. Before compaction the same Put would fail with ErrDiskFull.
func TestCompaction_EnforcesLocalStoreSizeViaEnsureSpace(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 600, mds)
	bc.evictMaxWait = 50 * time.Millisecond
	ctx := context.Background()

	// Sealed blob: 400 synced (cold) + 50 unsynced (pins the whole blob).
	synced := bytes.Repeat([]byte{0x55}, 400)
	syncedHash := blake3ContentHash(synced)
	if err := bc.StoreChunk(ctx, syncedHash, synced); err != nil {
		t.Fatalf("StoreChunk(synced): %v", err)
	}
	if err := mds.MarkSynced(ctx, syncedHash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	unsynced := bytes.Repeat([]byte{0x66}, 50)
	unsyncedHash := blake3ContentHash(unsynced)
	if err := bc.StoreChunk(ctx, unsyncedHash, unsynced); err != nil {
		t.Fatalf("StoreChunk(unsynced): %v", err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// used=450, +300 = 750 > 600. blobEvictOne can't touch the pinned blob;
	// compaction reclaims the 400 synced bytes so the Put fits.
	newData := bytes.Repeat([]byte{0x77}, 300)
	newHash := blake3ContentHash(newData)
	if err := bc.Put(ctx, newHash, newData); err != nil {
		t.Fatalf("Put over limit: %v (compaction should have reclaimed the synced remainder)", err)
	}

	// Final footprint: relocated survivor (50) + new chunk (300).
	if got := bc.Stats().DiskUsed; got != 350 {
		t.Fatalf("DiskUsed after compacting Put = %d, want 350", got)
	}
	got, err := bc.ReadChunk(ctx, unsyncedHash)
	if err != nil || !bytes.Equal(got, unsynced) {
		t.Fatalf("unsynced survivor unreadable/wrong after compacting Put: err=%v", err)
	}
	if got, err := bc.ReadChunk(ctx, newHash); err != nil || !bytes.Equal(got, newData) {
		t.Fatalf("new chunk unreadable/wrong after Put: err=%v", err)
	}
	if _, err := bc.ReadChunk(ctx, syncedHash); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadChunk(synced, dropped) = %v, want ErrChunkNotFound", err)
	}
}

// TestCompaction_TornSurvivor_LeavesBlobInPlace covers the corruption guard:
// if a must-keep (unsynced) survivor's bytes are unreadable, compaction must
// NOT drop the blob (that would destroy the only remaining trace of the
// survivor). It skips the blob and leaves it on disk.
func TestCompaction_TornSurvivor_LeavesBlobInPlace(t *testing.T) {
	dir := t.TempDir()
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc := newBlobStoreWithLimit(t, dir, 0, mds)
	ctx := context.Background()

	// Synced chunk first, unsynced survivor LAST so truncation removes only the
	// survivor's tail bytes.
	synced := bytes.Repeat([]byte{0x88}, 300)
	syncedHash := blake3ContentHash(synced)
	if err := bc.StoreChunk(ctx, syncedHash, synced); err != nil {
		t.Fatalf("StoreChunk(synced): %v", err)
	}
	if err := mds.MarkSynced(ctx, syncedHash, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	unsynced := bytes.Repeat([]byte{0x99}, 100)
	unsyncedHash := blake3ContentHash(unsynced)
	if err := bc.StoreChunk(ctx, unsyncedHash, unsynced); err != nil {
		t.Fatalf("StoreChunk(unsynced): %v", err)
	}
	loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, unsyncedHash)
	if err != nil || !ok {
		t.Fatalf("GetLocalLocation(unsynced): ok=%v err=%v", ok, err)
	}
	if err := bc.logBlob.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Physically truncate the sealed blob to lop off the unsynced survivor's
	// bytes: its ReadAt now short-reads (torn). No cached fd exists yet (the
	// blob has not been read since it sealed).
	blobPath := filepath.Join(dir, "blobs", loc.LogBlobID+".blob")
	if err := os.Truncate(blobPath, loc.RawOffset); err != nil {
		t.Fatalf("truncate sealed blob: %v", err)
	}

	// Compaction cannot relocate the torn survivor, so it skips the blob and
	// reports nothing reclaimable — the blob file must remain on disk.
	if freed, err := bc.compactBlobOne(ctx); !errors.Is(err, errLRUEmpty) || freed != 0 {
		t.Fatalf("compactBlobOne with torn survivor = (%d, %v), want (0, errLRUEmpty)", freed, err)
	}
	if _, statErr := os.Stat(blobPath); statErr != nil {
		t.Fatalf("sealed blob was removed despite an un-relocatable survivor: %v", statErr)
	}
}

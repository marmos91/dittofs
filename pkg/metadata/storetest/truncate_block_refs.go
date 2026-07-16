package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// runTruncateChunkRefTests dispatches the truncate-down block-ref pruning
// scenario against the provided factory. It runs against Memory, Badger, and
// Postgres via RunConformanceSuite, so every backend is held to the same
// "no stale-tail refs past EOF" contract.
func runTruncateChunkRefTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("TruncateDownPrunesChunkRefs", func(t *testing.T) {
		testTruncateDownPrunesChunkRefs(t, factory)
	})

	t.Run("ChmodDoesNotRewriteRefs", func(t *testing.T) {
		testChunkRef_ChmodDoesNotRewriteRefs(t, factory)
	})
}

// manifestWriteCounter is an optional, test-only capability the SQL backends
// implement to expose how many times PutFile actually persisted the
// file_block_refs manifest (i.e. ran past the BlocksDirty gate). It is what
// lets ChmodDoesNotRewriteRefs prove ZERO manifest writes — a row-count check
// alone cannot, because a DELETE+INSERT of the same M rows leaves the same
// count. Memory/Badger do not implement it (they hold Blocks inline and have
// no separate manifest table to rewrite), so the count assertions are skipped
// for those backends and only the row-count invariants are checked.
type manifestWriteCounter interface {
	PutFileChunkRefsCallCount() int64
}

// testChunkRef_ChmodDoesNotRewriteRefs is the write-amplification proof for
// #1715 #8: an attr-only SetFileAttributes (mode change, no size change) on an
// M-chunk file must NOT rewrite the block manifest — zero file_block_refs
// writes — while a subsequent truncate on the same file still prunes the rows.
func testChunkRef_ChmodDoesNotRewriteRefs(t *testing.T, factory StoreFactory) {
	store := factory(t)

	const shareName = "/chmod-refs"
	rootHandle := createTestShare(t, store, shareName)
	handle := createTestFile(t, store, shareName, rootHandle, "big.bin", 0644)

	ctx := t.Context()

	const mib = uint64(1 << 20)
	const nBlocks = 4

	// Seed a 4 MiB file with four 1 MiB blocks (the post-rollup manifest).
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	file.Size = nBlocks * mib
	file.Blocks = make([]block.ChunkRef, nBlocks)
	for i := range file.Blocks {
		var h block.ContentHash
		h[0] = byte(i + 1)
		file.Blocks[i] = block.ChunkRef{Hash: h, Offset: uint64(i) * mib, Size: uint32(mib)}
	}
	file.ObjectID = block.ComputeObjectID(file.Blocks)
	file.BlocksDirty = true // seeding the manifest IS a manifest write
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() seeding %d blocks failed: %v", nBlocks, err)
	}

	// Baseline the manifest-write counter AFTER seeding, so we measure only
	// what the chmod does. nil counter => backend does not gate (Memory/Badger).
	counter, hasCounter := store.(manifestWriteCounter)
	var baseline int64
	if hasCounter {
		baseline = counter.PutFileChunkRefsCallCount()
	}

	// chmod through the service's SetFileAttributes path: Mode only, no Size.
	// This is the hot attr-only write that used to rewrite the whole manifest.
	svc := metadata.New()
	if err := svc.RegisterStoreForShare(shareName, store); err != nil {
		t.Fatalf("RegisterStoreForShare() failed: %v", err)
	}
	rootUID := uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootUID},
	}
	newMode := uint32(0600)
	if _, err := svc.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{Mode: &newMode}); err != nil {
		t.Fatalf("SetFileAttributes(mode) failed: %v", err)
	}

	// The mode must have changed...
	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after chmod failed: %v", err)
	}
	if got.Mode&0o777 != 0600 {
		t.Errorf("Mode = %o, want 0600 after chmod", got.Mode&0o777)
	}
	// ...but the manifest must be byte-identical: all M blocks intact.
	if len(got.Blocks) != nBlocks {
		t.Fatalf("len(Blocks) = %d, want %d — chmod must not drop refs", len(got.Blocks), nBlocks)
	}
	// The real proof: ZERO manifest writes occurred for the chmod. Row-count
	// alone cannot show this (a DELETE+INSERT of the same 4 rows is invisible).
	if hasCounter {
		if delta := counter.PutFileChunkRefsCallCount() - baseline; delta != 0 {
			t.Errorf("chmod performed %d manifest write(s), want 0", delta)
		}
	}

	// A real manifest-changing op still works: truncate to 1 MiB prunes to one
	// block AND performs exactly one manifest write.
	newSize := mib
	if _, err := svc.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{Size: &newSize}); err != nil {
		t.Fatalf("SetFileAttributes(size=1MiB) failed: %v", err)
	}
	got, err = store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after truncate failed: %v", err)
	}
	if len(got.Blocks) != 1 {
		t.Fatalf("len(Blocks) = %d, want 1 after truncate to 1 MiB", len(got.Blocks))
	}
	if hasCounter {
		if delta := counter.PutFileChunkRefsCallCount() - baseline; delta != 1 {
			t.Errorf("after chmod+truncate, manifest writes = %d, want exactly 1 (the truncate)", delta)
		}
	}
}

// testTruncateDownPrunesChunkRefs reproduces the snapshot over-reference bug:
// a 4 MiB file is reduced to 1 MiB via a size-down SetAttr, then the surviving
// FileAttr.Blocks must cover only [0, 1 MiB). Higher-offset blocks from the old
// size must not linger — otherwise the snapshot manifest (built from
// FileAttr.Blocks) over-references content past EOF, the GC holds extra blocks,
// and a restore would emit a file longer than the current size.
//
// The scenario drives the real SetFileAttributes truncate path through
// MetadataService over the backend store, so it validates pruning on every
// backend's FileAttr.Blocks / file_block_refs representation.
func testTruncateDownPrunesChunkRefs(t *testing.T, factory StoreFactory) {
	store := factory(t)

	const shareName = "/trunc"
	rootHandle := createTestShare(t, store, shareName)
	handle := createTestFile(t, store, shareName, rootHandle, "big.bin", 0644)

	ctx := t.Context()

	const mib = uint64(1 << 20)

	// Simulate the post-rollup state: a 4 MiB file with four 1 MiB blocks at
	// offsets 0, 1M, 2M, 3M (distinct hashes so each ref is identifiable).
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	file.Size = 4 * mib
	file.Blocks = make([]block.ChunkRef, 4)
	for i := range file.Blocks {
		var h block.ContentHash
		h[0] = byte(i + 1)
		file.Blocks[i] = block.ChunkRef{
			Hash:   h,
			Offset: uint64(i) * mib,
			Size:   uint32(mib),
		}
	}
	// Quiesce the file: a non-zero ObjectID (Merkle root over Blocks) means the
	// truncate must keep it consistent with the trimmed list, not leave it stale.
	file.ObjectID = block.ComputeObjectID(file.Blocks)
	// Seeding the manifest is a manifest-changing write — the SQL backends
	// gate file_block_refs persistence on BlocksDirty.
	file.BlocksDirty = true
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() with 4 MiB block list failed: %v", err)
	}

	// Truncate down to 1 MiB through the service's SetFileAttributes path.
	svc := metadata.New()
	if err := svc.RegisterStoreForShare(shareName, store); err != nil {
		t.Fatalf("RegisterStoreForShare() failed: %v", err)
	}
	rootUID := uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootUID},
	}
	newSize := mib
	if _, err := svc.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{Size: &newSize}); err != nil {
		t.Fatalf("SetFileAttributes(size=1MiB) failed: %v", err)
	}

	// The surviving block list must cover only [0, 1 MiB): exactly the single
	// block at offset 0, with nothing at or beyond the new EOF.
	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after truncate failed: %v", err)
	}
	if got.Size != newSize {
		t.Errorf("Size = %d, want %d", got.Size, newSize)
	}
	if len(got.Blocks) != 1 {
		t.Fatalf("len(Blocks) = %d, want 1 (only the [0,1MiB) block)", len(got.Blocks))
	}
	if got.Blocks[0].Offset != 0 {
		t.Errorf("Blocks[0].Offset = %d, want 0", got.Blocks[0].Offset)
	}
	for _, b := range got.Blocks {
		if b.Offset >= newSize {
			t.Errorf("stale-tail ref survived truncate: offset %d >= new size %d", b.Offset, newSize)
		}
	}

	// The snapshot manifest is the union of every file's FileAttr.Blocks
	// hashes (see each backend's Backup implementation). The pruned-away tail
	// blocks must therefore no longer be referenced by the file.
	referenced := make(map[block.ContentHash]struct{}, len(got.Blocks))
	for _, b := range got.Blocks {
		referenced[b.Hash] = struct{}{}
	}
	for _, b := range file.Blocks[1:] {
		if _, found := referenced[b.Hash]; found {
			t.Errorf("pruned tail block %x still referenced after truncate", b.Hash[:4])
		}
	}

	// ObjectID must stay consistent with the trimmed block list (the dedup
	// invariant: a non-zero ObjectID equals ComputeObjectID(Blocks)).
	if want := block.ComputeObjectID(got.Blocks); got.ObjectID != want {
		t.Errorf("ObjectID = %s, want recompute(Blocks) %s after truncate",
			got.ObjectID.String(), want.String())
	}
}

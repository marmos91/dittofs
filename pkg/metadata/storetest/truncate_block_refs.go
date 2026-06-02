package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// runTruncateBlockRefTests dispatches the truncate-down block-ref pruning
// scenario against the provided factory. It runs against Memory, Badger, and
// Postgres via RunConformanceSuite, so every backend is held to the same
// "no stale-tail refs past EOF" contract.
func runTruncateBlockRefTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("TruncateDownPrunesBlockRefs", func(t *testing.T) {
		testTruncateDownPrunesBlockRefs(t, factory)
	})
}

// testTruncateDownPrunesBlockRefs reproduces the snapshot over-reference bug:
// a 4 MiB file is reduced to 1 MiB via a size-down SetAttr, then the surviving
// FileAttr.Blocks must cover only [0, 1 MiB). Higher-offset blocks from the old
// size must not linger — otherwise the snapshot manifest (built from
// FileAttr.Blocks) over-references content past EOF, the GC holds extra blocks,
// and a restore would emit a file longer than the current size.
//
// The scenario drives the real SetFileAttributes truncate path through
// MetadataService over the backend store, so it validates pruning on every
// backend's FileAttr.Blocks / file_block_refs representation.
func testTruncateDownPrunesBlockRefs(t *testing.T, factory StoreFactory) {
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
	file.Blocks = make([]blockstore.BlockRef, 4)
	for i := range file.Blocks {
		var h blockstore.ContentHash
		h[0] = byte(i + 1)
		file.Blocks[i] = blockstore.BlockRef{
			Hash:   h,
			Offset: uint64(i) * mib,
			Size:   uint32(mib),
		}
	}
	// Quiesce the file: a non-zero ObjectID (Merkle root over Blocks) means the
	// truncate must keep it consistent with the trimmed list, not leave it stale.
	file.ObjectID = blockstore.ComputeObjectID(file.Blocks)
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
	referenced := make(map[blockstore.ContentHash]struct{}, len(got.Blocks))
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
	if want := blockstore.ComputeObjectID(got.Blocks); got.ObjectID != want {
		t.Errorf("ObjectID = %s, want recompute(Blocks) %s after truncate",
			got.ObjectID.String(), want.String())
	}
}

package storetest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// FileBlockRefsAccessor is an optional capability backends may implement to
// expose direct row-count access to the file_block_refs join table. The
// conformance suite uses it only for the FK-cascade scenario, which is
// meaningful exclusively on Postgres — Memory and Badger have no schema-level
// concept of a separate refs table, so they skip the cascade test cleanly via
// type-assertion failure.
//
// Postgres satisfies this via *PostgresMetadataStore.CountFileBlockRefs
// (defined in pkg/metadata/store/postgres/file_block_refs.go).
type FileBlockRefsAccessor interface {
	// CountFileBlockRefs returns the number of file_block_refs rows for the
	// given fileID. Test-only; never call from production code.
	CountFileBlockRefs(ctx context.Context, fileID uuid.UUID) (int, error)
}

// runBlockRefOpsTests dispatches the META-04 BlockRef round-trip conformance
// scenarios against the provided factory. Each backend wires
// RunConformanceSuite into its *_conformance_test.go, so adding scenarios here
// automatically runs them against Memory, Badger, and Postgres.
//
// Phase 12 META-04: every metadata backend MUST round-trip FileAttr.Blocks
// across PutFile/GetFile (including replace and nil semantics). Postgres
// additionally exercises the FK ON DELETE CASCADE behavior from Plan 02.
func runBlockRefOpsTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("RoundTripBasic", func(t *testing.T) {
		testBlockRef_RoundTripBasic(t, factory)
	})

	t.Run("NilBlocks", func(t *testing.T) {
		testBlockRef_NilBlocks(t, factory)
	})

	t.Run("ReplaceBlocks", func(t *testing.T) {
		testBlockRef_ReplaceBlocks(t, factory)
	})

	t.Run("CascadeDeleteOnFileDelete", func(t *testing.T) {
		testBlockRef_CascadeDeleteOnFileDelete(t, factory)
	})
}

// testBlockRef_RoundTripBasic asserts that a file with three sorted-by-offset
// BlockRefs survives a PutFile/GetFile round-trip with deep equality on every
// field of every BlockRef. Catches encoding drift between backends.
func testBlockRef_RoundTripBasic(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "blockref-roundtrip")
	fileHandle := createTestFile(t, store, "blockref-roundtrip", rootHandle, "round.bin", 0o644)

	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("ref-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("ref-1"), Offset: 4 << 20, Size: 4 << 20},
		{Hash: hashOfSeed("ref-2"), Offset: 8 << 20, Size: 1 << 20},
	}

	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (pre-put): %v", err)
	}
	file.FileAttr.Blocks = blocks
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (round-trip): %v", err)
	}
	if len(got.FileAttr.Blocks) != len(blocks) {
		t.Fatalf("Blocks len: got %d, want %d", len(got.FileAttr.Blocks), len(blocks))
	}
	for i, want := range blocks {
		g := got.FileAttr.Blocks[i]
		if g.Hash != want.Hash {
			t.Errorf("Blocks[%d].Hash = %x, want %x", i, g.Hash[:8], want.Hash[:8])
		}
		if g.Offset != want.Offset {
			t.Errorf("Blocks[%d].Offset = %d, want %d", i, g.Offset, want.Offset)
		}
		if g.Size != want.Size {
			t.Errorf("Blocks[%d].Size = %d, want %d", i, g.Size, want.Size)
		}
	}
}

// testBlockRef_NilBlocks asserts that a regular file with no BlockRefs
// (Blocks == nil) round-trips without error. The retrieved Blocks slice
// must be empty (nil or zero-length both pass — backends differ on
// nil-vs-empty representation, but len() == 0 is the contract).
func testBlockRef_NilBlocks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "blockref-nil")
	fileHandle := createTestFile(t, store, "blockref-nil", rootHandle, "empty.bin", 0o644)

	// createTestFile does not set Blocks; verify the round-trip yields
	// an empty Blocks slice.
	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if len(got.FileAttr.Blocks) != 0 {
		t.Errorf("Blocks len: got %d, want 0 (nil-Blocks file)", len(got.FileAttr.Blocks))
	}

	// Now explicitly set Blocks to nil and PutFile; round-trip should
	// remain empty.
	got.FileAttr.Blocks = nil
	if err := store.PutFile(ctx, got); err != nil {
		t.Fatalf("PutFile (nil Blocks): %v", err)
	}

	got2, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (post-nil-put): %v", err)
	}
	if len(got2.FileAttr.Blocks) != 0 {
		t.Errorf("Blocks len after nil PutFile: got %d, want 0", len(got2.FileAttr.Blocks))
	}
}

// testBlockRef_ReplaceBlocks asserts that PutFile fully replaces the
// previous BlockRefs list — no leftover rows from prior PutFile calls.
// Plan 02 Postgres backend implements this via DELETE+INSERT in the same tx;
// Memory and Badger replace the slice trivially (single-blob encoding).
func testBlockRef_ReplaceBlocks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "blockref-replace")
	fileHandle := createTestFile(t, store, "blockref-replace", rootHandle, "replace.bin", 0o644)

	// Initial PutFile with 5 blocks.
	five := []blockstore.BlockRef{
		{Hash: hashOfSeed("rep-0"), Offset: 0, Size: 1 << 20},
		{Hash: hashOfSeed("rep-1"), Offset: 1 << 20, Size: 1 << 20},
		{Hash: hashOfSeed("rep-2"), Offset: 2 << 20, Size: 1 << 20},
		{Hash: hashOfSeed("rep-3"), Offset: 3 << 20, Size: 1 << 20},
		{Hash: hashOfSeed("rep-4"), Offset: 4 << 20, Size: 1 << 20},
	}
	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (pre 5): %v", err)
	}
	file.FileAttr.Blocks = five
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile (5 blocks): %v", err)
	}

	got5, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (5 blocks): %v", err)
	}
	if len(got5.FileAttr.Blocks) != 5 {
		t.Fatalf("Blocks len after 5-put: got %d, want 5", len(got5.FileAttr.Blocks))
	}

	// Replace with 2 different blocks at different offsets. After the
	// second PutFile the GetFile must return exactly the new 2 — no
	// leftover rows from the prior list.
	two := []blockstore.BlockRef{
		{Hash: hashOfSeed("rep-X"), Offset: 0, Size: 2 << 20},
		{Hash: hashOfSeed("rep-Y"), Offset: 2 << 20, Size: 2 << 20},
	}
	got5.FileAttr.Blocks = two
	if err := store.PutFile(ctx, got5); err != nil {
		t.Fatalf("PutFile (2 blocks replace): %v", err)
	}

	got2, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (post-replace): %v", err)
	}
	if len(got2.FileAttr.Blocks) != 2 {
		t.Fatalf("Blocks len after replace: got %d, want 2 (no leftovers from prior 5)",
			len(got2.FileAttr.Blocks))
	}
	for i, want := range two {
		g := got2.FileAttr.Blocks[i]
		if g.Hash != want.Hash || g.Offset != want.Offset || g.Size != want.Size {
			t.Errorf("Blocks[%d] = %+v, want %+v", i, g, want)
		}
	}
}

// testBlockRef_CascadeDeleteOnFileDelete asserts that deleting a file row
// cascades to the file_block_refs join table (Plan 02 D-03 FK ON DELETE
// CASCADE). Postgres-only via the FileBlockRefsAccessor capability hook;
// Memory and Badger have no separate refs table and skip cleanly.
func testBlockRef_CascadeDeleteOnFileDelete(t *testing.T, factory StoreFactory) {
	store := factory(t)

	accessor, ok := store.(FileBlockRefsAccessor)
	if !ok {
		t.Skip("backend does not implement FileBlockRefsAccessor — no separate refs table to cascade")
	}

	ctx := t.Context()

	rootHandle := createTestShare(t, store, "blockref-cascade")
	fileHandle := createTestFile(t, store, "blockref-cascade", rootHandle, "cascade.bin", 0o644)

	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("cas-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("cas-1"), Offset: 4 << 20, Size: 4 << 20},
		{Hash: hashOfSeed("cas-2"), Offset: 8 << 20, Size: 4 << 20},
	}
	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.FileAttr.Blocks = blocks
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	// Capture the underlying file ID from the handle for direct row counting.
	_, fileID, err := metadata.DecodeFileHandle(fileHandle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}

	pre, err := accessor.CountFileBlockRefs(ctx, fileID)
	if err != nil {
		t.Fatalf("CountFileBlockRefs (pre-delete): %v", err)
	}
	if pre != len(blocks) {
		t.Fatalf("pre-delete row count: got %d, want %d", pre, len(blocks))
	}

	// Remove the parent's child mapping first (matches the Plan 02 test pattern;
	// DeleteFile expects the file row to be detachable).
	parent, err := store.GetParent(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetParent: %v", err)
	}
	if err := store.DeleteChild(ctx, parent, "cascade.bin"); err != nil {
		t.Fatalf("DeleteChild: %v", err)
	}
	if err := store.DeleteFile(ctx, fileHandle); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	post, err := accessor.CountFileBlockRefs(ctx, fileID)
	if err != nil {
		t.Fatalf("CountFileBlockRefs (post-delete): %v", err)
	}
	if post != 0 {
		t.Fatalf("post-delete row count: got %d, want 0 (FK ON DELETE CASCADE)", post)
	}
}

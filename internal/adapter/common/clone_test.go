package common

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCloneRange_WholeFile_O1 asserts the whole-file fast path is a pure
// metadata reflink: the destination inherits the source's BlockRef list and
// each unique source hash is RefCount-incremented exactly once (no data
// movement). This is the headline `cp --reflink` case.
func TestCloneRange_WholeFile_O1(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.BlockRef{
		{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096},
		{Hash: block.ContentHash{0x02}, Offset: 4096, Size: 4096},
		{Hash: block.ContentHash{0x03}, Offset: 8192, Size: 2048},
	}
	const srcSize = 4096 + 4096 + 2048
	srcHandle := putTestFile(t, ms, "/src.bin", "src-pid", srcBlocks, srcSize)
	dstHandle := putTestFile(t, ms, "/dst.bin", "dst-pid", nil, 0)
	cache := &recordingInvalidator{}

	// count == 0 => whole file from src offset 0.
	if err := CloneRange(ctx, bs, ms, cache, srcHandle, dstHandle, "src-pid", "dst-pid", 0, 0, 0); err != nil {
		t.Fatalf("CloneRange failed: %v", err)
	}

	// O(1): one IncrementRefCount per unique source hash, no per-byte work.
	if len(coord.incrementCalls) != 3 {
		t.Fatalf("got %d IncrementRefCount calls, want 3", len(coord.incrementCalls))
	}

	dstFile, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatalf("GetFile(dst): %v", err)
	}
	if len(dstFile.Blocks) != len(srcBlocks) {
		t.Fatalf("dst has %d blocks, want %d", len(dstFile.Blocks), len(srcBlocks))
	}
	for i := range srcBlocks {
		if dstFile.Blocks[i].Hash != srcBlocks[i].Hash {
			t.Errorf("dst.Blocks[%d].Hash = %v, want %v", i, dstFile.Blocks[i].Hash, srcBlocks[i].Hash)
		}
	}
	if dstFile.Size != srcSize {
		t.Errorf("dst.Size = %d, want %d", dstFile.Size, srcSize)
	}
	if dstFile.Ctime.Before(dstFile.Mtime) {
		t.Error("dst.Ctime must advance with the content change")
	}

	// Cache invalidated POST-txn for the destination payload.
	if len(cache.calls) != 1 || cache.calls[0].payloadID != metadata.PayloadID("dst-pid") {
		t.Errorf("InvalidateFile calls = %+v, want one for dst-pid", cache.calls)
	}
}

// TestCloneRange_ExplicitWholeRange takes the fast path when src/dst offsets are
// 0 and count exactly equals the source size (an explicit, non-zero whole-file
// request, as some clients issue).
func TestCloneRange_ExplicitWholeRange(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.BlockRef{
		{Hash: block.ContentHash{0xAA}, Offset: 0, Size: 4096},
	}
	srcHandle := putTestFile(t, ms, "/src.bin", "src-pid", srcBlocks, 4096)
	dstHandle := putTestFile(t, ms, "/dst.bin", "dst-pid", nil, 0)

	if err := CloneRange(ctx, bs, ms, nil, srcHandle, dstHandle, "src-pid", "dst-pid", 0, 0, 4096); err != nil {
		t.Fatalf("CloneRange failed: %v", err)
	}
	if len(coord.incrementCalls) != 1 {
		t.Fatalf("got %d IncrementRefCount calls, want 1 (fast path)", len(coord.incrementCalls))
	}
	dstFile, _ := ms.GetFile(ctx, dstHandle)
	if dstFile.Size != 4096 || len(dstFile.Blocks) != 1 {
		t.Errorf("dst size=%d blocks=%d, want 4096/1", dstFile.Size, len(dstFile.Blocks))
	}
}

// TestCloneRange_SrcRangeOutOfBounds rejects a source range past EOF with an
// invalid-argument error before any txn work.
func TestCloneRange_SrcRangeOutOfBounds(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcHandle := putTestFile(t, ms, "/src.bin", "src-pid", []block.BlockRef{
		{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096},
	}, 4096)
	dstHandle := putTestFile(t, ms, "/dst.bin", "dst-pid", nil, 0)

	t.Run("offset past EOF", func(t *testing.T) {
		err := CloneRange(ctx, bs, ms, nil, srcHandle, dstHandle, "src-pid", "dst-pid", 8192, 0, 0)
		if err == nil {
			t.Fatal("expected error for src offset past EOF")
		}
	})

	t.Run("range past EOF", func(t *testing.T) {
		err := CloneRange(ctx, bs, ms, nil, srcHandle, dstHandle, "src-pid", "dst-pid", 0, 0, 8192)
		if err == nil {
			t.Fatal("expected error for src range past EOF")
		}
	})

	// No increments should have happened on the rejected paths.
	if len(coord.incrementCalls) != 0 {
		t.Errorf("got %d IncrementRefCount calls on rejected clones, want 0", len(coord.incrementCalls))
	}
}

package common

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCloneWholeFile_O1 asserts the reflink is a pure metadata operation: the
// destination inherits the source's BlockRef list and each unique source hash
// is RefCount-incremented exactly once (no data movement). This is the headline
// `cp --reflink` case.
func TestCloneWholeFile_O1(t *testing.T) {
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

	if err := CloneWholeFile(ctx, bs, ms, cache, srcHandle, dstHandle, "dst-pid"); err != nil {
		t.Fatalf("CloneWholeFile failed: %v", err)
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

// TestCloneWholeFile_SelfCloneNoOp asserts that cloning a payload onto itself is
// a no-op: no RefCount bumps (which would inflate the count) and no cache
// invalidation. This is the defense-in-depth guard for the shared primitive.
func TestCloneWholeFile_SelfCloneNoOp(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.BlockRef{{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096}}
	selfHandle := putTestFile(t, ms, "/self.bin", "same-pid", srcBlocks, 4096)
	cache := &recordingInvalidator{}

	if err := CloneWholeFile(ctx, bs, ms, cache, selfHandle, selfHandle, "same-pid"); err != nil {
		t.Fatalf("CloneWholeFile self-clone failed: %v", err)
	}
	if len(coord.incrementCalls) != 0 {
		t.Errorf("self-clone made %d IncrementRefCount calls, want 0", len(coord.incrementCalls))
	}
	if len(cache.calls) != 0 {
		t.Errorf("self-clone fired %d InvalidateFile calls, want 0", len(cache.calls))
	}
}

// TestCloneWholeFile_RollsBackOnIncrementError pins the atomicity contract: a
// mid-loop IncrementRefCount failure rolls back the destination PutFile and all
// RefCount bumps, and skips the POST-txn cache invalidation.
func TestCloneWholeFile_RollsBackOnIncrementError(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{
		failOnNthIncrTrip: 2, // fail the 2nd unique-hash increment
		failOnNthIncrErr:  errors.New("synthetic increment failure"),
	}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.BlockRef{
		{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096},
		{Hash: block.ContentHash{0x02}, Offset: 4096, Size: 4096},
	}
	srcHandle := putTestFile(t, ms, "/src.bin", "src-pid", srcBlocks, 8192)
	dstHandle := putTestFile(t, ms, "/dst.bin", "dst-pid", nil, 0)
	cache := &recordingInvalidator{}

	if err := CloneWholeFile(ctx, bs, ms, cache, srcHandle, dstHandle, "dst-pid"); err == nil {
		t.Fatal("expected CloneWholeFile to fail on IncrementRefCount error")
	}

	// Destination must be untouched (rollback) and the cache must NOT fire.
	dstFile, err := ms.GetFile(ctx, dstHandle)
	if err != nil {
		t.Fatalf("GetFile(dst): %v", err)
	}
	if len(dstFile.Blocks) != 0 || dstFile.Size != 0 {
		t.Errorf("dst mutated after rollback: blocks=%d size=%d", len(dstFile.Blocks), dstFile.Size)
	}
	if len(cache.calls) != 0 {
		t.Errorf("InvalidateFile fired %d times after rollback, want 0", len(cache.calls))
	}
}

package common

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCloneWholeFile_O1 asserts the reflink is a pure metadata operation: the
// destination inherits the source's ChunkRef list and each unique source hash
// is RefCount-incremented exactly once (no data movement). This is the headline
// `cp --reflink` case.
func TestCloneWholeFile_O1(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.ChunkRef{
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

	srcBlocks := []block.ChunkRef{{Hash: block.ContentHash{0x01}, Offset: 0, Size: 4096}}
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
// mid-loop IncrementRefCount failure rolls back the destination UpdateAttrs and all
// RefCount bumps, and skips the POST-txn cache invalidation.
func TestCloneWholeFile_RollsBackOnIncrementError(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	coord := &fakeCoordinator{
		failOnNthIncrTrip: 2, // fail the 2nd unique-hash increment
		failOnNthIncrErr:  errors.New("synthetic increment failure"),
	}
	bs := newCopyTestEngineWithMS(t, coord, ms)

	srcBlocks := []block.ChunkRef{
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

// TestCloneWholeFile_SeedsDestinationRanges pins the wiring the residency
// accounting depends on: the reflink moves no bytes, so the destination's ranges
// land in the manifest and would land nowhere in the local tier's index without
// the post-commit seed. Everything that reports remote-only bytes reads that
// index, so an unseeded destination is invisible to all of it.
func TestCloneWholeFile_SeedsDestinationRanges(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, local := newCopyTestEngineWithLocal(t, &fakeCoordinator{}, ms)

	srcBlocks := []block.ChunkRef{
		{Hash: block.ContentHash{0x11}, Offset: 0, Size: 4096},
		{Hash: block.ContentHash{0x22}, Offset: 4096, Size: 4096},
		{Hash: block.ContentHash{0x33}, Offset: 8192, Size: 2048},
	}
	const srcSize = 4096 + 4096 + 2048
	srcHandle := putTestFile(t, ms, "/seed-src.bin", "seed-src-pid", srcBlocks, srcSize)
	dstHandle := putTestFile(t, ms, "/seed-dst.bin", "seed-dst-pid", nil, 0)

	// Nothing describes the destination before the clone.
	before, err := local.DataExtents(ctx, "seed-dst-pid", srcSize)
	if err != nil {
		t.Fatalf("DataExtents before the clone: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("the destination already had %d index extents before the clone", len(before))
	}

	if err := CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, "seed-dst-pid"); err != nil {
		t.Fatalf("CloneWholeFile: %v", err)
	}

	described, err := local.DataExtents(ctx, "seed-dst-pid", srcSize)
	if err != nil {
		t.Fatalf("DataExtents after the clone: %v", err)
	}
	var describedBytes uint64
	for _, e := range described {
		describedBytes += e[1] - e[0]
	}
	if describedBytes != srcSize {
		t.Errorf("the index describes %d bytes of the clone's destination, want %d (extents %v)",
			describedBytes, srcSize, described)
	}
}

// TestCloneWholeFile_SelfCloneSeedsNothing keeps the self-clone no-op whole: the
// destination is the source, its ranges are already accounted for, and seeding
// them would describe the source's own bytes as remote-only.
func TestCloneWholeFile_SelfCloneSeedsNothing(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, local := newCopyTestEngineWithLocal(t, &fakeCoordinator{}, ms)

	srcBlocks := []block.ChunkRef{{Hash: block.ContentHash{0x44}, Offset: 0, Size: 4096}}
	selfHandle := putTestFile(t, ms, "/seed-self.bin", "seed-self-pid", srcBlocks, 4096)

	if err := CloneWholeFile(ctx, bs, ms, nil, selfHandle, selfHandle, "seed-self-pid"); err != nil {
		t.Fatalf("CloneWholeFile self-clone: %v", err)
	}

	described, err := local.DataExtents(ctx, "seed-self-pid", 4096)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}
	if len(described) != 0 {
		t.Errorf("the self-clone seeded %v; it copied nothing and must describe nothing", described)
	}
}

// TestCloneWholeFile_DropsTheDestinationsStaleLocalRanges pins the post-commit
// step the destination's correctness depends on.
//
// The reflink replaces the destination's content wholesale and moves no byte,
// so every local range the destination still holds describes content it no
// longer has. Those ranges are what the read path resolves first — a covered
// warm range reports neither hole nor cold, so the read never reaches the new
// manifest — and without this step the destination serves its pre-clone content
// indefinitely, with nothing logged.
//
// The bystander is the control: another payload holding local ranges of its
// own, which the clone has nothing to do with and must leave described.
func TestCloneWholeFile_DropsTheDestinationsStaleLocalRanges(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, local := newCopyTestEngineWithLocal(t, &fakeCoordinator{}, ms)

	srcBlocks := []block.ChunkRef{
		{Hash: block.ContentHash{0x11}, Offset: 0, Size: 4096},
		{Hash: block.ContentHash{0x22}, Offset: 4096, Size: 4096},
	}
	const size = 8192
	srcHandle := putTestFile(t, ms, "/stale-src.bin", "stale-src-pid", srcBlocks, size)
	dstHandle := putTestFile(t, ms, "/stale-dst.bin", "stale-dst-pid", nil, size)
	putTestFile(t, ms, "/stale-bystander.bin", "stale-bystander-pid", nil, size)

	// The destination and the bystander each hold their own bytes locally.
	for _, pid := range []string{"stale-dst-pid", "stale-bystander-pid"} {
		if _, err := bs.WriteAt(ctx, pid, nil, bytes.Repeat([]byte{0xAB}, size), 0); err != nil {
			t.Fatalf("WriteAt(%s): %v", pid, err)
		}
		extents, err := local.DataExtents(ctx, pid, size)
		if err != nil {
			t.Fatalf("DataExtents(%s): %v", pid, err)
		}
		if len(extents) == 0 {
			t.Fatalf("%s describes nothing before the clone, so the drop below would prove nothing", pid)
		}
	}

	if err := CloneWholeFile(ctx, bs, ms, nil, srcHandle, dstHandle, "stale-dst-pid"); err != nil {
		t.Fatalf("CloneWholeFile: %v", err)
	}

	described, err := local.DataExtents(ctx, "stale-dst-pid", size)
	if err != nil {
		t.Fatalf("DataExtents(dst) after the clone: %v", err)
	}
	if len(described) != 0 {
		t.Errorf("the destination still describes %v locally; those ranges hold the content the clone replaced", described)
	}

	bystander, err := local.DataExtents(ctx, "stale-bystander-pid", size)
	if err != nil {
		t.Fatalf("DataExtents(bystander): %v", err)
	}
	if len(bystander) == 0 {
		t.Error("the clone dropped a payload it had nothing to do with")
	}
}

// TestCloneWholeFile_SelfCloneKeepsItsLocalRanges keeps the self-clone no-op
// whole. The destination is the source: it holds exactly the content its
// manifest describes, and dropping its local ranges would throw away bytes
// nothing replaced.
func TestCloneWholeFile_SelfCloneKeepsItsLocalRanges(t *testing.T) {
	ctx := context.Background()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bs, local := newCopyTestEngineWithLocal(t, &fakeCoordinator{}, ms)

	const size = 4096
	selfHandle := putTestFile(t, ms, "/self.bin", "self-pid",
		[]block.ChunkRef{{Hash: block.ContentHash{0x44}, Offset: 0, Size: size}}, size)
	if _, err := bs.WriteAt(ctx, "self-pid", nil, bytes.Repeat([]byte{0xCD}, size), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if err := CloneWholeFile(ctx, bs, ms, nil, selfHandle, selfHandle, "self-pid"); err != nil {
		t.Fatalf("CloneWholeFile self-clone: %v", err)
	}

	described, err := local.DataExtents(ctx, "self-pid", size)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}
	if len(described) == 0 {
		t.Error("the self-clone dropped the payload's own local ranges; it replaced nothing")
	}
}

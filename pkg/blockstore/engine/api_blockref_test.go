package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// TestCopyPayload_O1_IncrementsRefCountPerUniqueHash asserts the Plan
// 07 D-11 contract: CopyPayload bumps RefCount once per unique source
// hash and copies no bytes — purely a metadata operation.
func TestCopyPayload_O1_IncrementsRefCountPerUniqueHash(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)
	ctx := context.Background()

	// 3 distinct hashes, 2 of them duplicated → 3 unique → 3 increments.
	h1 := blockstore.ContentHash{0x01}
	h2 := blockstore.ContentHash{0x02}
	h3 := blockstore.ContentHash{0x03}
	src := []blockstore.BlockRef{
		{Hash: h1, Offset: 0, Size: 1024},
		{Hash: h2, Offset: 1024, Size: 1024},
		{Hash: h1, Offset: 2048, Size: 1024}, // duplicate of h1
		{Hash: h3, Offset: 3072, Size: 1024},
		{Hash: h2, Offset: 4096, Size: 1024}, // duplicate of h2
	}

	dst, err := bs.CopyPayload(ctx, "src", "dst", src)
	if err != nil {
		t.Fatalf("CopyPayload: %v", err)
	}

	if len(dst) != len(src) {
		t.Errorf("dst len = %d, want %d (BlockRef sequence preserved)", len(dst), len(src))
	}
	for i := range src {
		if dst[i] != src[i] {
			t.Errorf("dst[%d] = %+v, want %+v", i, dst[i], src[i])
		}
	}

	// Exactly 3 unique-hash IncrementRefCount calls.
	if got := len(fc.incHashes); got != 3 {
		t.Errorf("IncrementRefCount calls = %d, want 3", got)
	}
	seen := make(map[blockstore.ContentHash]bool, 3)
	for _, h := range fc.incHashes {
		seen[h] = true
	}
	for _, want := range []blockstore.ContentHash{h1, h2, h3} {
		if !seen[want] {
			t.Errorf("expected IncrementRefCount on %s, not seen", want.String())
		}
	}
}

// TestCopyPayload_FailureRollsBack pins the Plan 07 D-11 mid-failure
// contract: any IncrementRefCount error is surfaced immediately and no
// further increments are attempted (caller's metadata txn rolls back
// every committed bump).
func TestCopyPayload_FailureRollsBack(t *testing.T) {
	fc := &fakeCoordinator{failOnNthIncrement: 3}
	bs := newTestEngineWithCoordinator(t, fc)
	ctx := context.Background()

	src := []blockstore.BlockRef{
		{Hash: blockstore.ContentHash{0x01}, Offset: 0, Size: 1024},
		{Hash: blockstore.ContentHash{0x02}, Offset: 1024, Size: 1024},
		{Hash: blockstore.ContentHash{0x03}, Offset: 2048, Size: 1024}, // 3rd → induced failure
		{Hash: blockstore.ContentHash{0x04}, Offset: 3072, Size: 1024},
		{Hash: blockstore.ContentHash{0x05}, Offset: 4096, Size: 1024},
	}

	dst, err := bs.CopyPayload(ctx, "src", "dst", src)
	if err == nil {
		t.Fatal("CopyPayload: want error from failOnNthIncrement=3, got nil")
	}
	if dst != nil {
		t.Errorf("dst = %v, want nil on failure (caller's txn rolls back)", dst)
	}

	// 2 increments recorded (1 + 2 succeeded; 3rd failed; no further
	// attempts on 4 and 5). incCallCount tracks attempts including the
	// failure → 3.
	if fc.incCallCount != 3 {
		t.Errorf("incCallCount = %d, want 3 (loop must stop at first error)", fc.incCallCount)
	}
	if got := len(fc.incHashes); got != 2 {
		t.Errorf("incHashes recorded = %d, want 2 (only successful increments)", got)
	}
}

// TestCopyPayload_NilCoordinator returns ErrMetadataCoordinatorNotWired
// when called with a non-empty source on a coordinator-less engine.
func TestCopyPayload_NilCoordinator(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	src := []blockstore.BlockRef{{Hash: blockstore.ContentHash{0x01}, Offset: 0, Size: 1024}}
	_, err := bs.CopyPayload(ctx, "src", "dst", src)
	if !errors.Is(err, ErrMetadataCoordinatorNotWired) {
		t.Errorf("err = %v, want ErrMetadataCoordinatorNotWired", err)
	}
}

// TestDelete_DecrementsRefCounts asserts the Plan 07 D-17 contract:
// engine.Delete invokes coordinator.DecrementRefCount for every
// BlockRef hash in the input slice.
func TestDelete_DecrementsRefCounts(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)
	ctx := context.Background()

	blocks := []blockstore.BlockRef{
		{Hash: blockstore.ContentHash{0x01}, Offset: 0, Size: 1024},
		{Hash: blockstore.ContentHash{0x02}, Offset: 1024, Size: 1024},
		{Hash: blockstore.ContentHash{0x03}, Offset: 2048, Size: 1024},
	}

	if err := bs.Delete(ctx, "delete-test", blocks); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := len(fc.decHashes); got != 3 {
		t.Errorf("DecrementRefCount calls = %d, want 3", got)
	}
}

// TestDelete_EmptyBlocksSkipsCoordinator confirms the legacy / dual-
// read path: nil/empty blocks bypass the coordinator entirely.
func TestDelete_EmptyBlocksSkipsCoordinator(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)
	ctx := context.Background()

	if err := bs.Delete(ctx, "delete-legacy", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fc.decHashes) != 0 {
		t.Errorf("DecrementRefCount called %d times on legacy delete (want 0)", len(fc.decHashes))
	}
}

// TestTruncate_DropsBlocksPastNewSize asserts the Plan 07 D-15 contract:
// blocks whose Offset >= newSize are dropped from the returned BlockRef
// list and their hashes flow through DecrementRefCount.
func TestTruncate_DropsBlocksPastNewSize(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)
	ctx := context.Background()

	blocks := []blockstore.BlockRef{
		{Hash: blockstore.ContentHash{0x01}, Offset: 0, Size: 4096},          // kept
		{Hash: blockstore.ContentHash{0x02}, Offset: 4096, Size: 4096},       // kept (Offset < newSize=8192)
		{Hash: blockstore.ContentHash{0x03}, Offset: 8192, Size: 4096},       // dropped
		{Hash: blockstore.ContentHash{0x04}, Offset: 12288, Size: 4096},      // dropped
	}

	kept, err := bs.Truncate(ctx, "trunc-test", blocks, 8192)
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	if len(kept) != 2 {
		t.Errorf("kept = %d blocks, want 2", len(kept))
	}
	if len(fc.decHashes) != 2 {
		t.Errorf("DecrementRefCount calls = %d, want 2 (one per dropped block)", len(fc.decHashes))
	}
}

// TestTruncate_EmptyBlocksLegacyPath asserts that nil currentBlocks
// runs the legacy local+remote truncate without coordinator side effects.
func TestTruncate_EmptyBlocksLegacyPath(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)
	ctx := context.Background()

	kept, err := bs.Truncate(ctx, "trunc-legacy", nil, 0)
	if err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if kept != nil {
		t.Errorf("kept = %v, want nil for legacy path", kept)
	}
	if len(fc.decHashes) != 0 {
		t.Errorf("DecrementRefCount called %d times on legacy path (want 0)", len(fc.decHashes))
	}
}

// TestWriteAt_ReturnsCurrentBlocks asserts the Plan 07 contract: until
// Plan 09 wires the FastCDC re-chunking, WriteAt returns
// currentBlocks unchanged so the caller's PutFile sees a stable list.
func TestWriteAt_ReturnsCurrentBlocks(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	current := []blockstore.BlockRef{
		{Hash: blockstore.ContentHash{0xAA}, Offset: 0, Size: 1024},
	}
	got, err := bs.WriteAt(ctx, "write-test", current, []byte("hello"), 0)
	if err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if len(got) != len(current) || got[0] != current[0] {
		t.Errorf("got = %+v, want %+v (Plan 07 returns currentBlocks unchanged)", got, current)
	}
}

// TestSyncer_PostFlushHook_Direct asserts the syncer post-Flush wiring
// point (Plan 07 lays the seam; Plan 13-12 wires Flush -> hook). Engine.New
// plumbs the coordinator into the syncer; persistFileBlocksAfterFlush
// invokes PersistFileBlocks when called directly. Fine-grained unit
// coverage of the seam — the end-to-end Flush()-driven assertion lives in
// pkg/blockstore/engine/syncer_flush_test.go::TestSyncer_Flush_InvokesPostFlushHook
// (it requires a real FileBlockStore + LocalStore + RemoteStore which the
// lightweight stubFileBlockStore harness here cannot provide).
func TestSyncer_PostFlushHook_Direct(t *testing.T) {
	fc := &fakeCoordinator{}
	bs := newTestEngineWithCoordinator(t, fc)

	if bs.syncer.coordinator == nil {
		t.Fatal("engine.New did not wire syncer.coordinator from cfg.Coordinator")
	}

	// persistFileBlocksAfterFlush is the seam — invoke directly to pin
	// the post-Flush contract.
	ctx := context.Background()
	blocks := []blockstore.BlockRef{{Hash: blockstore.ContentHash{0xCD}, Offset: 0, Size: 1024}}
	if err := bs.syncer.persistFileBlocksAfterFlush(ctx, "post-flush-test", blocks); err != nil {
		t.Fatalf("persistFileBlocksAfterFlush: %v", err)
	}
	if len(fc.persistCalls) != 1 {
		t.Errorf("persistCalls = %d, want 1", len(fc.persistCalls))
	}
	if fc.persistCalls[0].payloadID != "post-flush-test" {
		t.Errorf("payloadID = %q, want post-flush-test", fc.persistCalls[0].payloadID)
	}
	// Plan 13-12 invariant: the direct-hook path also computes and
	// passes the BLAKE3 Merkle root ObjectID.
	if got, want := fc.persistCalls[0].objectID, blockstore.ComputeObjectID(blocks); got != want {
		t.Errorf("objectID = %s, want ComputeObjectID(blocks) = %s",
			got.String(), want.String())
	}
}

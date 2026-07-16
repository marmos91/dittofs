package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// seedPackedBlock writes a block object to rbs and records, in st, the block
// record (LiveChunkCount = len(chunks)) and each chunk's synced block-locator
// backdated past the grace window. It mirrors what the carver's
// DefaultCommitBlock produces, minus the real codec framing the GC reclaim path
// never inspects.
func seedPackedBlock(t *testing.T, st metadata.Store, rbs remote.RemoteBlockStore, blockID string, chunks []block.ContentHash) {
	t.Helper()
	ctx := t.Context()
	data := []byte("block-bytes-" + blockID)
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock(%s): %v", blockID, err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        blockID,
		Length:         int64(len(data)),
		LiveChunkCount: uint32(len(chunks)),
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(%s): %v", blockID, err)
	}
	for i, h := range chunks {
		if err := st.MarkSynced(ctx, h, block.ChunkLocator{BlockID: blockID, WireOffset: int64(i) * 80, WireLength: 80}); err != nil {
			t.Fatalf("MarkSynced(%s): %v", h, err)
		}
		// Backdate past grace so the steady-state index sweep treats it as
		// eligible (the live-set check is then the only thing that can save it).
		st.(*metadatamemory.MemoryMetadataStore).MarkSyncedAtForTest(h, time.Now().Add(-2*time.Hour))
	}
}

func newBlockGCReclaimer(st metadata.Store, rbs remote.RemoteBlockStore) *BlockGCReclaimer {
	return &BlockGCReclaimer{Locators: st, Records: st, RemoteBlocks: rbs}
}

// ---------------------------------------------------------------------------
// Unit tests: BlockGCReclaimer.ReclaimDeadChunk
// ---------------------------------------------------------------------------

// TestBlockReclaimer_NoLocatorNotHandled proves a synced hash with no block
// locator is NOT handled — post-#1493 the sweep treats that as metadata drift
// and keeps the marker fail-closed. No block bookkeeping is touched.
func TestBlockReclaimer_NoLocatorNotHandled(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	h := hashFromString("standalone-chunk")
	if err := st.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil { // standalone
		t.Fatalf("MarkSynced: %v", err)
	}

	handled, freed, err := newBlockGCReclaimer(st, rbs).ReclaimDeadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReclaimDeadChunk: %v", err)
	}
	if handled {
		t.Errorf("handled = true for a locator-less hash; the sweep must record drift instead")
	}
	if freed != 0 {
		t.Errorf("bytesFreed = %d, want 0 for a locator-less hash", freed)
	}
}

// TestBlockReclaimer_PartialDecrement proves reclaiming one of a block's two
// chunks decrements LiveChunkCount but leaves the block object + record intact
// (the other chunk is still live).
func TestBlockReclaimer_PartialDecrement(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	h1 := hashFromString("blk-chunk-1")
	h2 := hashFromString("blk-chunk-2")
	seedPackedBlock(t, st, rbs, "blk-partial", []block.ContentHash{h1, h2})

	handled, freed, err := newBlockGCReclaimer(st, rbs).ReclaimDeadChunk(ctx, h1)
	if err != nil {
		t.Fatalf("ReclaimDeadChunk: %v", err)
	}
	if !handled {
		t.Fatalf("handled = false, want true for a block-resident chunk")
	}
	if freed != 0 {
		t.Errorf("bytesFreed = %d, want 0 (block not freed on partial decrement)", freed)
	}

	rec, ok, err := st.GetBlockRecord(ctx, "blk-partial")
	if err != nil || !ok {
		t.Fatalf("GetBlockRecord: ok=%v err=%v (block must survive a partial decrement)", ok, err)
	}
	if rec.LiveChunkCount != 1 {
		t.Errorf("LiveChunkCount = %d, want 1 after one chunk reaped", rec.LiveChunkCount)
	}
	if _, err := rbs.GetBlock(ctx, "blk-partial"); err != nil {
		t.Errorf("block object wrongly deleted on partial decrement: %v", err)
	}
}

// TestBlockReclaimer_FreesBlockAtZero proves reclaiming the LAST live chunk
// frees the remote block object AND the record, reporting the block bytes freed.
func TestBlockReclaimer_FreesBlockAtZero(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	h := hashFromString("only-chunk")
	seedPackedBlock(t, st, rbs, "blk-solo", []block.ContentHash{h})

	handled, freed, err := newBlockGCReclaimer(st, rbs).ReclaimDeadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReclaimDeadChunk: %v", err)
	}
	if !handled {
		t.Fatalf("handled = false, want true")
	}
	if freed <= 0 {
		t.Errorf("bytesFreed = %d, want the block Length when the last chunk is freed", freed)
	}
	if _, ok, _ := st.GetBlockRecord(ctx, "blk-solo"); ok {
		t.Errorf("block record still present after last chunk freed")
	}
	if _, err := rbs.GetBlock(ctx, "blk-solo"); err == nil {
		t.Errorf("block object still present on remote after last chunk freed")
	}
}

// TestBlockReclaimer_IdempotentAlreadyFreed proves reclaiming a block-resident
// hash whose block record is already gone (a sibling chunk freed it earlier in
// the same sweep) returns handled=true without error and is a no-op on the
// remote — so the caller still skips the CAS delete.
func TestBlockReclaimer_IdempotentAlreadyFreed(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	h := hashFromString("orphan-locator")
	// Synced block-locator present, but NO block record (already freed).
	if err := st.MarkSynced(ctx, h, block.ChunkLocator{BlockID: "blk-gone", WireOffset: 0, WireLength: 80}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	handled, freed, err := newBlockGCReclaimer(st, rbs).ReclaimDeadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReclaimDeadChunk: %v", err)
	}
	if !handled {
		t.Errorf("handled = false; an already-freed block-resident hash must still be handled (no CAS object exists)")
	}
	if freed != 0 {
		t.Errorf("bytesFreed = %d, want 0", freed)
	}
}

// ---------------------------------------------------------------------------
// Integration tests: block reclaim through the real GC index sweep.
// ---------------------------------------------------------------------------

// blockGCEnv wires one memory metadata store (live-set + synced index + reclaim
// surfaces) and one memory remote (CAS + block-keyed) for the index-sweep tests.
type blockGCEnv struct {
	st  *metadatamemory.MemoryMetadataStore
	rs  *remotememory.Store
	rec *gcMSReconciler
}

func newBlockGCEnv(t *testing.T) *blockGCEnv {
	t.Helper()
	rec := newGCMSReconciler()
	st := rec.addShare("share-a").(*metadatamemory.MemoryMetadataStore)
	rs := remotememory.New()
	t.Cleanup(func() { _ = rs.Close() })
	return &blockGCEnv{st: st, rs: rs, rec: rec}
}

func (e *blockGCEnv) runGC(t *testing.T) *GCStats {
	t.Helper()
	return CollectGarbage(t.Context(), e.rec, &Options{
		GCStateRoot:     t.TempDir(),
		GracePeriod:     time.Hour,
		SyncedHashIndex: e.st,
		BlockReclaimer:  newBlockGCReclaimer(e.st, e.rs),
	})
}

// TestGCBlockSweep_DedupSiblingKeepsBlockAlive is the dedup-safety proof: a
// block-resident hash shared by two files (dedup) must NOT free its block when
// only the FIRST file unlinks — the sibling FileChunk row keeps the hash in the
// live set. Freeing it here would corrupt the sibling. The block is freed only
// when the SECOND (last) referencing file also unlinks.
func TestGCBlockSweep_DedupSiblingKeepsBlockAlive(t *testing.T) {
	ctx := t.Context()
	env := newBlockGCEnv(t)

	h := hashFromString("dedup-shared-chunk")
	seedPackedBlock(t, env.st, env.rs, "blk-dedup", []block.ContentHash{h})
	// Two files reference the SAME packed chunk (cross-file dedup): one block
	// chunk, one synced locator, two live FileChunk rows.
	putBlock(t, env.st, "fileA/0", h)
	putBlock(t, env.st, "fileB/0", h)

	// Phase 1: unlink fileA only. h is still live via fileB → block kept.
	if err := env.st.Delete(ctx, "fileA/0"); err != nil {
		t.Fatalf("unlink fileA: %v", err)
	}
	stats := env.runGC(t)
	if stats.ErrorCount != 0 {
		t.Fatalf("phase-1 ErrorCount = %d (%v)", stats.ErrorCount, stats.FirstErrors)
	}
	if stats.ObjectsSwept != 0 {
		t.Fatalf("phase-1 ObjectsSwept = %d, want 0 — block freed while a dedup sibling still references it (DATA LOSS)", stats.ObjectsSwept)
	}
	if _, err := env.rs.GetBlock(ctx, "blk-dedup"); err != nil {
		t.Fatalf("block wrongly freed while fileB still references it: %v", err)
	}
	if rec, ok, _ := env.st.GetBlockRecord(ctx, "blk-dedup"); !ok || rec.LiveChunkCount != 1 {
		t.Fatalf("block record disturbed by a sibling-kept sweep: ok=%v count=%d", ok, rec.LiveChunkCount)
	}

	// Phase 2: unlink fileB (the last reference). h is now globally dead → freed.
	if err := env.st.Delete(ctx, "fileB/0"); err != nil {
		t.Fatalf("unlink fileB: %v", err)
	}
	stats = env.runGC(t)
	if stats.ErrorCount != 0 {
		t.Fatalf("phase-2 ErrorCount = %d (%v)", stats.ErrorCount, stats.FirstErrors)
	}
	if stats.ObjectsSwept != 1 {
		t.Fatalf("phase-2 ObjectsSwept = %d, want 1 (the now-dead block chunk)", stats.ObjectsSwept)
	}
	if _, err := env.rs.GetBlock(ctx, "blk-dedup"); err == nil {
		t.Errorf("block not freed after its last reference unlinked")
	}
	if _, ok, _ := env.st.GetBlockRecord(ctx, "blk-dedup"); ok {
		t.Errorf("block record not deleted after last chunk freed")
	}
	if ok, _ := env.st.IsSynced(ctx, h); ok {
		t.Errorf("synced marker not cleared after block reclaim (ListUnsynced would skip it forever)")
	}
}

// TestGCBlockSweep_PartialUnlinkLeavesIntact proves the index sweep decrements a
// block's LiveChunkCount for the dead chunk while keeping the block object alive
// for the still-live chunk — the LiveChunkCount invariant holds across a partial
// unlink.
func TestGCBlockSweep_PartialUnlinkLeavesIntact(t *testing.T) {
	ctx := t.Context()
	env := newBlockGCEnv(t)

	h1 := hashFromString("partial-chunk-1")
	h2 := hashFromString("partial-chunk-2")
	seedPackedBlock(t, env.st, env.rs, "blk-part", []block.ContentHash{h1, h2})
	putBlock(t, env.st, "file/0", h1)
	putBlock(t, env.st, "file/64", h2)

	// Unlink only h1's row; h2 stays live.
	if err := env.st.Delete(ctx, "file/0"); err != nil {
		t.Fatalf("unlink h1 row: %v", err)
	}
	stats := env.runGC(t)
	if stats.ErrorCount != 0 {
		t.Fatalf("ErrorCount = %d (%v)", stats.ErrorCount, stats.FirstErrors)
	}
	if stats.ObjectsSwept != 1 {
		t.Fatalf("ObjectsSwept = %d, want 1 (only h1)", stats.ObjectsSwept)
	}

	rec, ok, err := env.st.GetBlockRecord(ctx, "blk-part")
	if err != nil || !ok {
		t.Fatalf("block record missing after partial unlink: ok=%v err=%v", ok, err)
	}
	if rec.LiveChunkCount != 1 {
		t.Errorf("LiveChunkCount = %d, want 1 (invariant: one live chunk remains)", rec.LiveChunkCount)
	}
	if _, err := env.rs.GetBlock(ctx, "blk-part"); err != nil {
		t.Errorf("block wrongly freed on partial unlink: %v", err)
	}
	if ok, _ := env.st.IsSynced(ctx, h1); ok {
		t.Errorf("h1 synced marker not cleared")
	}
	if ok, _ := env.st.IsSynced(ctx, h2); !ok {
		t.Errorf("h2 synced marker wrongly cleared (still live)")
	}
}

// TestGCBlockSweep_LocatorlessMarkerFailsClosed proves the post-#1493 drift
// handling in the sweep: a dead synced hash whose marker carries NO block
// locator (post-migration this cannot legitimately exist) is recorded as an
// error and its marker is KEPT — the sweep never issues a per-hash remote
// delete.
func TestGCBlockSweep_LocatorlessMarkerFailsClosed(t *testing.T) {
	ctx := t.Context()
	env := newBlockGCEnv(t)

	h := hashFromString("standalone-dead")
	if err := env.st.MarkSynced(ctx, h, block.ChunkLocator{}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	env.st.MarkSyncedAtForTest(h, time.Now().Add(-2*time.Hour))
	// No FileChunk row → not in the live set → dead, but locator-less.

	stats := env.runGC(t)
	if stats.ErrorCount == 0 {
		t.Fatalf("ErrorCount = 0, want > 0 (locator-less dead marker is drift)")
	}
	if stats.ObjectsSwept != 0 {
		t.Fatalf("ObjectsSwept = %d, want 0 (fail-closed keep)", stats.ObjectsSwept)
	}
	if ok, _ := env.st.IsSynced(ctx, h); !ok {
		t.Errorf("marker cleared despite fail-closed drift handling")
	}
}

// TestBlockReclaimer_RerunAfterCrashNoDoubleDecrement is the critical
// data-loss regression test for the partial-reclaim re-entry bug. If the GC is
// killed between ReclaimDeadChunk's committed decrement and the sweep's own
// marker-clear, the next sweep could re-visit the same hash. A second decrement
// would drive live_chunk_count to 0 for a block that still has a live sibling →
// DeleteBlock fires → the sibling chunk's reads break permanently (silent data
// loss).
//
// The fix makes the SYNCED MARKER the decrement's idempotency token: DeleteSynced
// runs BEFORE (and thus commits with) the decrement, so on re-entry GetLocator
// reports the hash unsynced and the reclaimer is a no-op (handled=false) — no
// second decrement. (In the real sweep a cleared marker also drops the hash from
// EnumerateSynced, so it is never re-visited at all.)
func TestBlockReclaimer_RerunAfterCrashNoDoubleDecrement(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	h1 := hashFromString("crash-chunk-dead")
	h2 := hashFromString("crash-chunk-live")
	// 2-chunk block: h1 will be reclaimed, h2 is the live sibling that must survive.
	seedPackedBlock(t, st, rbs, "blk-crash", []block.ContentHash{h1, h2})

	reclaimer := newBlockGCReclaimer(st, rbs)

	// First reclaim of h1: normal path — clears h1's marker, decrements count,
	// deletes h1's local location.
	handled, freed, err := reclaimer.ReclaimDeadChunk(ctx, h1)
	if err != nil {
		t.Fatalf("first ReclaimDeadChunk: %v", err)
	}
	if !handled {
		t.Fatalf("first ReclaimDeadChunk: handled=false, want true")
	}
	if freed != 0 {
		t.Errorf("first pass bytesFreed=%d, want 0 (block still alive, one chunk remains)", freed)
	}

	// Verify intermediate state: count decremented to 1, h1 marker+local gone.
	rec, ok, _ := st.GetBlockRecord(ctx, "blk-crash")
	if !ok || rec.LiveChunkCount != 1 {
		t.Fatalf("after first pass: ok=%v LiveChunkCount=%d, want 1", ok, rec.LiveChunkCount)
	}
	if ok, _ := st.IsSynced(ctx, h1); ok {
		t.Fatalf("after first pass: h1 marker still present; it is the re-entry token and must be cleared")
	}

	// Crash-recovery re-entry: the marker was cleared with the decrement, so the
	// reclaimer must be a no-op — NOT a second decrement.
	handled, freed, err = reclaimer.ReclaimDeadChunk(ctx, h1)
	if err != nil {
		t.Fatalf("second ReclaimDeadChunk (crash recovery): %v", err)
	}
	if handled {
		t.Errorf("second ReclaimDeadChunk: handled=true, want false (marker already cleared → nothing to reclaim)")
	}

	// CRITICAL: the block must NOT have been freed. A second decrement would
	// drive live_chunk_count to 0 → DeleteBlock → data loss for h2.
	if _, err := rbs.GetBlock(ctx, "blk-crash"); err != nil {
		t.Errorf("block prematurely freed on crash-recovery reclaim (DATA LOSS): GetBlock: %v", err)
	}
	rec, ok, _ = st.GetBlockRecord(ctx, "blk-crash")
	if !ok {
		t.Errorf("block record deleted on crash-recovery reclaim (DATA LOSS)")
	} else if rec.LiveChunkCount != 1 {
		t.Errorf("LiveChunkCount=%d after crash-recovery reclaim, want 1 (double-decrement = data loss)", rec.LiveChunkCount)
	}
	if freed != 0 {
		t.Errorf("crash-recovery bytesFreed=%d, want 0 (block must not be freed)", freed)
	}
	// h2's synced marker must remain intact — it is still live.
	if ok, _ := st.IsSynced(ctx, h2); !ok {
		t.Errorf("h2 synced marker wrongly cleared during crash-recovery reclaim (still live)")
	}
}

// compile-time: the memory metadata store satisfies the reclaimer surfaces.
var (
	_ blockSyncedMarkerGC = (*metadatamemory.MemoryMetadataStore)(nil)
	_ blockRecordGC       = (*metadatamemory.MemoryMetadataStore)(nil)
	_ context.Context     = nil
)

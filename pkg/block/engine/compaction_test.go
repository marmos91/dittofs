package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// seedRealPackedBlock writes a real codec-framed block (nil Sealer, so each
// wire body is the verbatim chunk plaintext) to rbs and records its block
// record + a live synced locator for every chunk. It returns each chunk's hash
// so a test can later kill a subset. Mirrors what the carver commits, with real
// framing the compactor's Parse can decode.
func seedRealPackedBlock(t *testing.T, st metadata.Store, rbs remote.RemoteBlockStore, blockID string, chunkData [][]byte) []block.ContentHash {
	t.Helper()
	ctx := t.Context()

	var buf bytes.Buffer
	builder, err := blockcodec.NewBuilder(&buf, blockID, nil)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	hashes := make([]block.ContentHash, len(chunkData))
	locs := make([]block.ChunkLocator, len(chunkData))
	for i, d := range chunkData {
		h := block.ContentHash(blake3.Sum256(d))
		hashes[i] = h
		loc, err := builder.Add(h, d)
		if err != nil {
			t.Fatalf("builder.Add: %v", err)
		}
		locs[i] = loc
	}
	if _, err := builder.Finish(); err != nil {
		t.Fatalf("builder.Finish: %v", err)
	}
	blockBytes := buf.Bytes()

	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(blockBytes)); err != nil {
		t.Fatalf("PutBlock(%s): %v", blockID, err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      block.ContentHash(blake3.Sum256(blockBytes)),
		Length:         int64(len(blockBytes)),
		LiveChunkCount: uint32(len(chunkData)),
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(%s): %v", blockID, err)
	}
	for i, h := range hashes {
		if err := st.MarkSynced(ctx, h, locs[i]); err != nil {
			t.Fatalf("MarkSynced(%s): %v", h, err)
		}
		st.(*metadatamemory.MemoryMetadataStore).MarkSyncedAtForTest(h, time.Now().Add(-2*time.Hour))
	}
	return hashes
}

// killChunk simulates the post-sweep state of a dead chunk: the sweep has
// cleared its synced marker (DeleteSynced) and decremented the block's live
// count. The chunk bytes remain physically in the old block object.
func killChunk(t *testing.T, st metadata.Store, blockID string, h block.ContentHash) {
	t.Helper()
	ctx := t.Context()
	if err := st.DeleteSynced(ctx, h); err != nil {
		t.Fatalf("DeleteSynced(%s): %v", h, err)
	}
	if _, err := st.DecrLiveChunkCount(ctx, blockID, 1); err != nil {
		t.Fatalf("DecrLiveChunkCount(%s): %v", blockID, err)
	}
}

// liveChunkBytes resolves a chunk through its current locator and returns the
// wire body (== plaintext for a nil-Sealer block), proving the chunk is still
// readable and byte-identical after a relocation.
func liveChunkBytes(t *testing.T, st metadata.Store, rbs remote.RemoteBlockStore, h block.ContentHash) []byte {
	t.Helper()
	ctx := t.Context()
	loc, ok, err := st.GetLocator(ctx, h)
	if err != nil || !ok {
		t.Fatalf("GetLocator(%s): ok=%v err=%v", h, ok, err)
	}
	data, err := rbs.GetBlock(ctx, loc.BlockID)
	if err != nil {
		t.Fatalf("GetBlock(%s): %v", loc.BlockID, err)
	}
	return data[loc.WireOffset : loc.WireOffset+loc.WireLength]
}

// TestCompactBlocks_PartiallyDeadBlock is the core #1487 proof: a block whose
// live bytes fall below the threshold is repacked so the surviving chunks stay
// readable and byte-identical, the dead space is reclaimed, and the old block
// (object + record) is gone.
func TestCompactBlocks_PartiallyDeadBlock(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	live := bytes.Repeat([]byte("L"), 100)
	dead1 := bytes.Repeat([]byte("D"), 100)
	dead2 := bytes.Repeat([]byte("E"), 100)
	dead3 := bytes.Repeat([]byte("F"), 100)
	hashes := seedRealPackedBlock(t, st, rbs, "blk-partial", [][]byte{live, dead1, dead2, dead3})
	liveHash := hashes[0]

	// Kill three of four chunks: block is now ~25% live by bytes.
	killChunk(t, st, "blk-partial", hashes[1])
	killChunk(t, st, "blk-partial", hashes[2])
	killChunk(t, st, "blk-partial", hashes[3])

	oldRec, _, _ := st.GetBlockRecord(ctx, "blk-partial")

	rep, err := CompactBlocks(ctx, []CompactMetaView{st}, rbs, CompactOptions{LiveRatio: 0.5})
	if err != nil {
		t.Fatalf("CompactBlocks: %v", err)
	}
	if rep.BlocksCompacted != 1 || rep.ChunksMoved != 1 || rep.Errors != 0 {
		t.Fatalf("report = %+v; want 1 compacted, 1 moved, 0 errors", rep)
	}
	if rep.BytesReclaimed <= 0 {
		t.Errorf("BytesReclaimed = %d; want > 0", rep.BytesReclaimed)
	}

	// Old block is gone: record and remote object.
	if _, ok, _ := st.GetBlockRecord(ctx, "blk-partial"); ok {
		t.Error("old block record still present after compaction")
	}
	if _, err := rbs.GetBlock(ctx, "blk-partial"); err == nil {
		t.Error("old block object still present after compaction")
	}

	// The live chunk moved to a fresh block and reads back byte-identical.
	loc, ok, err := st.GetLocator(ctx, liveHash)
	if err != nil || !ok {
		t.Fatalf("live chunk locator missing: ok=%v err=%v", ok, err)
	}
	if loc.BlockID == "blk-partial" || loc.BlockID == "" {
		t.Errorf("live chunk locator points at %q; want a fresh block", loc.BlockID)
	}
	if got := liveChunkBytes(t, st, rbs, liveHash); !bytes.Equal(got, live) {
		t.Errorf("live chunk bytes changed after compaction: got %q", got)
	}

	// The dead chunks are unreferenced (no locator).
	for _, h := range hashes[1:] {
		if _, ok, _ := st.GetLocator(ctx, h); ok {
			t.Errorf("dead chunk %s still has a locator after compaction", h)
		}
	}

	// The new block is strictly smaller than the old (dead space reclaimed).
	newRec, ok, _ := st.GetBlockRecord(ctx, loc.BlockID)
	if !ok {
		t.Fatal("new block record missing")
	}
	if newRec.Length >= oldRec.Length {
		t.Errorf("new block length %d not smaller than old %d", newRec.Length, oldRec.Length)
	}
	if newRec.LiveChunkCount != 1 {
		t.Errorf("new block LiveChunkCount = %d; want 1", newRec.LiveChunkCount)
	}

	// Idempotent: a second pass has nothing to do (the block is now fully live).
	rep2, err := CompactBlocks(ctx, []CompactMetaView{st}, rbs, CompactOptions{LiveRatio: 0.5})
	if err != nil {
		t.Fatalf("CompactBlocks (2nd): %v", err)
	}
	if rep2.BlocksCompacted != 0 {
		t.Errorf("second pass compacted %d blocks; want 0", rep2.BlocksCompacted)
	}
}

// TestCompactBlocks_SkipsHealthyBlock proves a block above the live-ratio
// threshold is left untouched.
func TestCompactBlocks_SkipsHealthyBlock(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	c0 := bytes.Repeat([]byte("A"), 100)
	c1 := bytes.Repeat([]byte("B"), 100)
	c2 := bytes.Repeat([]byte("C"), 100)
	c3 := bytes.Repeat([]byte("D"), 100)
	hashes := seedRealPackedBlock(t, st, rbs, "blk-healthy", [][]byte{c0, c1, c2, c3})
	killChunk(t, st, "blk-healthy", hashes[3]) // 3 of 4 live → ~55% live by bytes

	// Threshold 0.5: the block is above it, so nothing is compacted.
	rep, err := CompactBlocks(ctx, []CompactMetaView{st}, rbs, CompactOptions{LiveRatio: 0.5})
	if err != nil {
		t.Fatalf("CompactBlocks: %v", err)
	}
	if rep.BlocksCompacted != 0 {
		t.Errorf("compacted %d blocks; want 0 (block is above threshold)", rep.BlocksCompacted)
	}
	if _, ok, _ := st.GetBlockRecord(ctx, "blk-healthy"); !ok {
		t.Error("healthy block was wrongly deleted")
	}
}

// TestCompactBlocks_DisabledAndNilRemote proves the pass no-ops when compaction
// is disabled (ratio <= 0) or the remote cannot hold blocks.
func TestCompactBlocks_DisabledAndNilRemote(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	rep, err := CompactBlocks(ctx, []CompactMetaView{st}, rbs, CompactOptions{LiveRatio: 0})
	if err != nil || rep.BlocksScanned != 0 {
		t.Errorf("disabled pass: rep=%+v err=%v; want no-op", rep, err)
	}
	rep, err = CompactBlocks(ctx, []CompactMetaView{st}, nil, CompactOptions{LiveRatio: 0.5})
	if err != nil || rep.BlocksCompacted != 0 {
		t.Errorf("nil-remote pass: rep=%+v err=%v; want no-op", rep, err)
	}
}

// compactNestedGuardView models the sqlite MaxOpenConns(1) hazard: a metadata
// query issued while the EnumerateSynced cursor is open would deadlock. It fails
// loudly instead, so CompactBlocks must drain EnumerateSynced before resolving
// locators (the collect-then-query rule).
type compactNestedGuardView struct {
	t           *testing.T
	inEnumerate bool
}

func (v *compactNestedGuardView) EnumerateSynced(_ context.Context, fn func(block.ContentHash, time.Time) error) error {
	v.inEnumerate = true
	defer func() { v.inEnumerate = false }()
	return fn(hashFromString("live-chunk"), time.Now().Add(-2*time.Hour))
}

func (v *compactNestedGuardView) GetLocator(_ context.Context, _ block.ContentHash) (block.ChunkLocator, bool, error) {
	if v.inEnumerate {
		v.t.Fatal("GetLocator called while EnumerateSynced cursor is open — deadlocks on sqlite MaxOpenConns(1)")
	}
	return block.ChunkLocator{BlockID: "blk-x", WireLength: 10}, true, nil
}

func (v *compactNestedGuardView) WalkBlockRecords(_ context.Context, fn func(block.BlockRecord) error) error {
	// Length 1000 with only 10 live bytes → far below any threshold, but the
	// GetBlock/parse never runs here (no remote), so the point is only that the
	// candidate scan completes without a nested-query deadlock.
	return fn(block.BlockRecord{BlockID: "blk-x", Length: 1000, LiveChunkCount: 1, SyncState: block.BlockStateRemote})
}

func (v *compactNestedGuardView) GetBlockRecord(_ context.Context, _ string) (block.BlockRecord, bool, error) {
	return block.BlockRecord{}, false, nil // candidate vanished before compaction — clean skip
}

func (v *compactNestedGuardView) DeleteBlockRecord(_ context.Context, _ string) error { return nil }

func (v *compactNestedGuardView) WithTransaction(_ context.Context, _ func(metadata.Transaction) error) error {
	return nil
}

// TestCompactBlocks_NoNestedQueryDuringEnumerate guards the collect-then-query
// rule so a GetLocator is never issued inside the EnumerateSynced callback.
func TestCompactBlocks_NoNestedQueryDuringEnumerate(t *testing.T) {
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()
	v := &compactNestedGuardView{t: t}
	// The candidate's GetBlockRecord returns false, so compaction cleanly skips
	// it without touching the remote — the point is only that the candidate scan
	// completes without a nested-query deadlock.
	if _, err := CompactBlocks(t.Context(), []CompactMetaView{v}, rbs, CompactOptions{LiveRatio: 0.5}); err != nil {
		t.Fatalf("CompactBlocks: %v", err)
	}
}

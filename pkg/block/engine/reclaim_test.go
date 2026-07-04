package engine

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// putZeroRefRecord seeds a class-1 orphan: a remote object plus a block record
// with LiveChunkCount==0 and no synced locator pointing at it.
func putZeroRefRecord(t *testing.T, st *metadatamemory.MemoryMetadataStore, rbs *remotememory.Store, blockID string, length int64) {
	t.Helper()
	ctx := t.Context()
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader([]byte(blockID))); err != nil {
		t.Fatalf("PutBlock(%s): %v", blockID, err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        blockID,
		Length:         length,
		LiveChunkCount: 0,
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(%s): %v", blockID, err)
	}
}

func recordExists(t *testing.T, st *metadatamemory.MemoryMetadataStore, blockID string) bool {
	t.Helper()
	_, ok, err := st.GetBlockRecord(t.Context(), blockID)
	if err != nil {
		t.Fatalf("GetBlockRecord(%s): %v", blockID, err)
	}
	return ok
}

func remoteHasBlock(t *testing.T, rbs *remotememory.Store, blockID string) bool {
	t.Helper()
	found := false
	if err := rbs.WalkBlocks(t.Context(), func(id string, _ block.Meta) error {
		if id == blockID {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	return found
}

// TestReclaimZeroRefRecords_DeletesOnlyZeroRef proves the reclaimer deletes a
// zero-ref record AND its remote object, leaves a healthy block and a leaked
// (class-2) block untouched, and is idempotent on a second pass.
func TestReclaimZeroRefRecords_DeletesOnlyZeroRef(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	// Healthy: record + live locator + object → must survive.
	hHealthy := hashFromString("healthy-chunk")
	seedPackedBlock(t, st, rbs, "blk-healthy", []block.ContentHash{hHealthy})

	// Class 1: zero-ref record + its object → must be deleted.
	putZeroRefRecord(t, st, rbs, "blk-zeroref", 111)

	// Class 2: leaked block (LiveChunkCount>0, no live locator) → NOT this
	// stage's job; must be left intact for PR5c.
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID: "blk-leaked", Length: 222, LiveChunkCount: 2, SyncState: block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(blk-leaked): %v", err)
	}

	rep, err := ReclaimZeroRefRecords(ctx, []ReclaimMetaView{st}, rbs, ReclaimOptions{})
	if err != nil {
		t.Fatalf("ReclaimZeroRefRecords: %v", err)
	}

	if rep.Reclaimed.Count != 1 || len(rep.Reclaimed.Sample) != 1 || rep.Reclaimed.Sample[0] != "blk-zeroref" {
		t.Errorf("Reclaimed = %+v; want count 1 sample [blk-zeroref]", rep.Reclaimed)
	}
	if rep.Reclaimed.Bytes != 111 {
		t.Errorf("Reclaimed.Bytes = %d; want 111", rep.Reclaimed.Bytes)
	}
	if rep.Errors != 0 {
		t.Errorf("Errors = %d; want 0", rep.Errors)
	}

	// Zero-ref record and its object are gone.
	if recordExists(t, st, "blk-zeroref") {
		t.Error("blk-zeroref record still present after reclaim")
	}
	if remoteHasBlock(t, rbs, "blk-zeroref") {
		t.Error("blk-zeroref remote object still present after reclaim")
	}
	// Healthy and leaked survive.
	if !recordExists(t, st, "blk-healthy") || !remoteHasBlock(t, rbs, "blk-healthy") {
		t.Error("healthy block was wrongly reclaimed")
	}
	if !recordExists(t, st, "blk-leaked") {
		t.Error("leaked (class-2) record was wrongly reclaimed")
	}

	// Idempotent: a second pass finds nothing to reclaim.
	rep2, err := ReclaimZeroRefRecords(ctx, []ReclaimMetaView{st}, rbs, ReclaimOptions{})
	if err != nil {
		t.Fatalf("ReclaimZeroRefRecords (2nd): %v", err)
	}
	if rep2.Reclaimed.Count != 0 {
		t.Errorf("second pass Reclaimed.Count = %d; want 0", rep2.Reclaimed.Count)
	}
}

// TestReclaimZeroRefRecords_DryRun proves a dry run tallies the same set but
// deletes nothing.
func TestReclaimZeroRefRecords_DryRun(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	putZeroRefRecord(t, st, rbs, "blk-zeroref", 500)

	rep, err := ReclaimZeroRefRecords(ctx, []ReclaimMetaView{st}, rbs, ReclaimOptions{DryRun: true})
	if err != nil {
		t.Fatalf("ReclaimZeroRefRecords: %v", err)
	}
	if !rep.DryRun || rep.Reclaimed.Count != 1 || rep.Reclaimed.Bytes != 500 {
		t.Errorf("dry-run report = %+v; want DryRun count 1 bytes 500", rep)
	}
	if !recordExists(t, st, "blk-zeroref") || !remoteHasBlock(t, rbs, "blk-zeroref") {
		t.Error("dry run deleted something; must be non-mutating")
	}
}

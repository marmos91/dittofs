package engine

import (
	"bytes"
	"context"
	"iter"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// fakeUnsynced is a hermetic ReconcileLocalView yielding a fixed set of
// unsynced chunk hashes. The real implementation is *fs.FSStore.ListUnsynced;
// the class-4 classification only needs the iterator surface, so this keeps the
// engine test off-disk.
type fakeUnsynced struct{ hashes []block.ContentHash }

func (f fakeUnsynced) ListUnsynced(context.Context) iter.Seq2[block.ContentHash, error] {
	return func(yield func(block.ContentHash, error) bool) {
		for _, h := range f.hashes {
			if !yield(h, nil) {
				return
			}
		}
	}
}

// putBareBlock writes a remote object with no backing block record, stamping its
// LastModified at the given time so the grace-window filter can be exercised.
func putBareBlock(t *testing.T, rbs *remotememory.Store, blockID string, modTime time.Time) {
	t.Helper()
	rbs.SetNowFnForTest(func() time.Time { return modTime })
	if err := rbs.PutBlock(context.Background(), blockID, bytes.NewReader([]byte(blockID))); err != nil {
		t.Fatalf("PutBlock(%s): %v", blockID, err)
	}
	rbs.SetNowFnForTest(nil) // reset to real clock
}

// TestReconcile_ClassifiesEachOrphanClass seeds one instance of every orphan
// class plus healthy blocks/objects/chunks and asserts each is classified
// correctly, healthy items are NOT flagged, the fresh (within-grace) record-less
// object is preserved, and the scan mutates nothing.
func TestReconcile_ClassifiesEachOrphanClass(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	// --- HEALTHY block: record + live synced locator + remote object. ---
	// seedPackedBlock writes the object, the record (LiveChunkCount=1), and a
	// synced block-locator pointing at blk-healthy → in refSet → not flagged.
	hHealthy := hashFromString("healthy-chunk")
	seedPackedBlock(t, st, rbs, "blk-healthy", []block.ContentHash{hHealthy})

	// --- CLASS 1: zero-ref record (crash between decr and delete-record). ---
	// A record with LiveChunkCount==0 and no live locator pointing at it.
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        "blk-zeroref",
		Length:         111,
		LiveChunkCount: 0,
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(blk-zeroref): %v", err)
	}

	// --- CLASS 2: leaked block (#1525 re-carve residue). ---
	// A record with LiveChunkCount>0 whose hash was moved onto another block by
	// a last-wins commit, so no live locator points at blk-leaked anymore.
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID:        "blk-leaked",
		Length:         222,
		LiveChunkCount: 2,
		SyncState:      block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(blk-leaked): %v", err)
	}
	// The re-carved hash now points at the healthy block (last-wins). Its
	// presence must not rescue blk-leaked.
	hRecarve := hashFromString("recarved-chunk")
	if err := st.MarkSynced(ctx, hRecarve, block.ChunkLocator{BlockID: "blk-healthy", WireOffset: 80, WireLength: 80}); err != nil {
		t.Fatalf("MarkSynced(recarve): %v", err)
	}

	// --- CLASS 3: record-less remote objects. ---
	putBareBlock(t, rbs, "blk-orphan-aged", time.Now().Add(-2*time.Hour)) // aged → orphan
	putBareBlock(t, rbs, "blk-orphan-fresh", time.Now())                  // within grace → preserved

	// --- CLASS 4: stranded local-only chunk (unsynced, local-durable). ---
	hStranded := hashFromString("stranded-local-chunk")
	local := fakeUnsynced{hashes: []block.ContentHash{hStranded}}

	// --- Snapshot state to prove the scan is non-mutating. ---
	before := captureState(t, st, rbs)

	rep, err := Reconcile(ctx, []ReconcileMetaView{st}, rbs, []ReconcileLocalView{local}, ReconcileOptions{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Class 1.
	if rep.ZeroRefRecords.Count != 1 {
		t.Errorf("ZeroRefRecords.Count = %d; want 1", rep.ZeroRefRecords.Count)
	}
	if len(rep.ZeroRefRecords.Sample) != 1 || rep.ZeroRefRecords.Sample[0] != "blk-zeroref" {
		t.Errorf("ZeroRefRecords.Sample = %v; want [blk-zeroref]", rep.ZeroRefRecords.Sample)
	}
	if rep.ZeroRefRecords.Bytes != 111 {
		t.Errorf("ZeroRefRecords.Bytes = %d; want 111", rep.ZeroRefRecords.Bytes)
	}
	// Class 2.
	if rep.LeakedBlocks.Count != 1 || rep.LeakedBlocks.Sample[0] != "blk-leaked" {
		t.Errorf("LeakedBlocks = %+v; want count 1 sample [blk-leaked]", rep.LeakedBlocks)
	}
	if rep.LeakedBlocks.Bytes != 222 {
		t.Errorf("LeakedBlocks.Bytes = %d; want 222", rep.LeakedBlocks.Bytes)
	}
	// Class 3: only the aged object, never the fresh one and never the healthy
	// object (which has a record).
	if rep.OrphanRemoteObjects.Count != 1 || rep.OrphanRemoteObjects.Sample[0] != "blk-orphan-aged" {
		t.Errorf("OrphanRemoteObjects = %+v; want count 1 sample [blk-orphan-aged]", rep.OrphanRemoteObjects)
	}
	// Class 4.
	if rep.StrandedLocalChunks.Count != 1 || rep.StrandedLocalChunks.Sample[0] != hStranded.String() {
		t.Errorf("StrandedLocalChunks = %+v; want count 1 sample [%s]", rep.StrandedLocalChunks, hStranded)
	}

	// Non-mutation: state identical after the scan.
	after := captureState(t, st, rbs)
	before.assertEqual(t, after)
}

// TestReconcile_GraceWindowBoundary asserts the grace filter uses the
// configured window: an object just inside it is preserved, one just outside is
// reported.
func TestReconcile_GraceWindowBoundary(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	// 30-minute grace: 20m-old object preserved, 40m-old object reported.
	putBareBlock(t, rbs, "blk-young", time.Now().Add(-20*time.Minute))
	putBareBlock(t, rbs, "blk-old", time.Now().Add(-40*time.Minute))

	rep, err := Reconcile(ctx, []ReconcileMetaView{st}, rbs, nil, ReconcileOptions{GracePeriod: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rep.OrphanRemoteObjects.Count != 1 || rep.OrphanRemoteObjects.Sample[0] != "blk-old" {
		t.Errorf("OrphanRemoteObjects = %+v; want count 1 sample [blk-old]", rep.OrphanRemoteObjects)
	}
	if rep.GracePeriod != 30*time.Minute {
		t.Errorf("GracePeriod = %v; want 30m", rep.GracePeriod)
	}
}

// reconcileState is a snapshot of the mutable storage the reporter reads, used
// to prove Reconcile does not touch it.
type reconcileState struct {
	blockRecords map[string]uint32 // blockID → LiveChunkCount
	remoteBlocks map[string]struct{}
	syncedHashes map[string]struct{}
}

func captureState(t *testing.T, st *metadatamemory.MemoryMetadataStore, rbs *remotememory.Store) reconcileState {
	t.Helper()
	ctx := context.Background()
	s := reconcileState{
		blockRecords: map[string]uint32{},
		remoteBlocks: map[string]struct{}{},
		syncedHashes: map[string]struct{}{},
	}
	if err := st.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
		s.blockRecords[rec.BlockID] = rec.LiveChunkCount
		return nil
	}); err != nil {
		t.Fatalf("WalkBlockRecords: %v", err)
	}
	if err := rbs.WalkBlocks(ctx, func(blockID string, _ block.Meta) error {
		s.remoteBlocks[blockID] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	if err := st.EnumerateSynced(ctx, func(h block.ContentHash, _ time.Time) error {
		s.syncedHashes[h.String()] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("EnumerateSynced: %v", err)
	}
	return s
}

func (s reconcileState) assertEqual(t *testing.T, other reconcileState) {
	t.Helper()
	if len(s.blockRecords) != len(other.blockRecords) {
		t.Fatalf("block record count changed: %d → %d", len(s.blockRecords), len(other.blockRecords))
	}
	for id, cnt := range s.blockRecords {
		if other.blockRecords[id] != cnt {
			t.Errorf("block record %s LiveChunkCount changed: %d → %d", id, cnt, other.blockRecords[id])
		}
	}
	if len(s.remoteBlocks) != len(other.remoteBlocks) {
		t.Errorf("remote block set changed: %d → %d objects", len(s.remoteBlocks), len(other.remoteBlocks))
	}
	for id := range s.remoteBlocks {
		if _, ok := other.remoteBlocks[id]; !ok {
			t.Errorf("remote block %s removed by scan", id)
		}
	}
	if len(s.syncedHashes) != len(other.syncedHashes) {
		t.Errorf("synced hash set changed: %d → %d", len(s.syncedHashes), len(other.syncedHashes))
	}
	for h := range s.syncedHashes {
		if _, ok := other.syncedHashes[h]; !ok {
			t.Errorf("synced hash %s removed by scan", h)
		}
	}
}

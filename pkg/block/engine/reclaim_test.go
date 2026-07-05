package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

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

// TestReclaimRecords_DeletesUnreferencedRecords proves the reclaimer deletes both
// a zero-ref (class-1) and a leaked (class-2) record along with their remote
// objects, tallies them separately, leaves a healthy block untouched, and is
// idempotent on a second pass.
func TestReclaimRecords_DeletesUnreferencedRecords(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	// Healthy: record + live locator + object → must survive.
	hHealthy := hashFromString("healthy-chunk")
	seedPackedBlock(t, st, rbs, "blk-healthy", []block.ContentHash{hHealthy})

	// Class 1: zero-ref record + its object → must be deleted.
	putZeroRefRecord(t, st, rbs, "blk-zeroref", 111)

	// Class 2: leaked block (LiveChunkCount>0, no live locator) → the stale count
	// is a lie; with no locator it is terminally dead and must be reclaimed (#1525).
	if err := rbs.PutBlock(ctx, "blk-leaked", bytes.NewReader([]byte("blk-leaked"))); err != nil {
		t.Fatalf("PutBlock(blk-leaked): %v", err)
	}
	if err := st.PutBlockRecord(ctx, block.BlockRecord{
		BlockID: "blk-leaked", Length: 222, LiveChunkCount: 2, SyncState: block.BlockStateRemote,
	}); err != nil {
		t.Fatalf("PutBlockRecord(blk-leaked): %v", err)
	}

	rep, err := ReclaimRecords(ctx, []ReclaimMetaView{st}, rbs, ReclaimOptions{})
	if err != nil {
		t.Fatalf("ReclaimRecords: %v", err)
	}

	if rep.Reclaimed.Count != 1 || len(rep.Reclaimed.Sample) != 1 || rep.Reclaimed.Sample[0] != "blk-zeroref" {
		t.Errorf("Reclaimed = %+v; want count 1 sample [blk-zeroref]", rep.Reclaimed)
	}
	if rep.Reclaimed.Bytes != 111 {
		t.Errorf("Reclaimed.Bytes = %d; want 111", rep.Reclaimed.Bytes)
	}
	if rep.LeakedReclaimed.Count != 1 || len(rep.LeakedReclaimed.Sample) != 1 || rep.LeakedReclaimed.Sample[0] != "blk-leaked" {
		t.Errorf("LeakedReclaimed = %+v; want count 1 sample [blk-leaked]", rep.LeakedReclaimed)
	}
	if rep.LeakedReclaimed.Bytes != 222 {
		t.Errorf("LeakedReclaimed.Bytes = %d; want 222", rep.LeakedReclaimed.Bytes)
	}
	if rep.Errors != 0 {
		t.Errorf("Errors = %d; want 0", rep.Errors)
	}

	// Both unreferenced records and their objects are gone.
	for _, id := range []string{"blk-zeroref", "blk-leaked"} {
		if recordExists(t, st, id) {
			t.Errorf("%s record still present after reclaim", id)
		}
		if remoteHasBlock(t, rbs, id) {
			t.Errorf("%s remote object still present after reclaim", id)
		}
	}
	// Healthy survives.
	if !recordExists(t, st, "blk-healthy") || !remoteHasBlock(t, rbs, "blk-healthy") {
		t.Error("healthy block was wrongly reclaimed")
	}

	// Idempotent: a second pass finds nothing to reclaim.
	rep2, err := ReclaimRecords(ctx, []ReclaimMetaView{st}, rbs, ReclaimOptions{})
	if err != nil {
		t.Fatalf("ReclaimRecords (2nd): %v", err)
	}
	if rep2.Reclaimed.Count != 0 || rep2.LeakedReclaimed.Count != 0 {
		t.Errorf("second pass reclaimed %d zero-ref + %d leaked; want 0", rep2.Reclaimed.Count, rep2.LeakedReclaimed.Count)
	}
}

// TestReclaimOrphanObjects_GraceWindow proves the object sweep deletes an aged
// record-less object, spares one still inside the grace window, spares one whose
// age cannot be evaluated (zero LastModified, fail-closed), and never touches an
// object that still has a backing record.
func TestReclaimOrphanObjects_GraceWindow(t *testing.T) {
	ctx := t.Context()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	// blk-known has a record; the union protects it. blk-aged-orphan is old and
	// record-less → reclaimed. blk-fresh-orphan is record-less but recently
	// written, so it is inside the grace window → spared.
	putBareBlock(t, rbs, "blk-known", time.Now().Add(-2*time.Hour))
	putBareBlock(t, rbs, "blk-aged-orphan", time.Now().Add(-2*time.Hour))
	putBareBlock(t, rbs, "blk-fresh-orphan", time.Now())
	// blk-noage-orphan is record-less with a zero LastModified: its age cannot
	// be evaluated, so the sweep must fail closed and spare it.
	putBareBlock(t, rbs, "blk-noage-orphan", time.Time{})
	metaBlockIDs := map[string]struct{}{"blk-known": {}}

	rep, err := ReclaimOrphanObjects(ctx, metaBlockIDs, rbs, ReclaimOptions{GracePeriod: time.Hour})
	if err != nil {
		t.Fatalf("ReclaimOrphanObjects: %v", err)
	}

	if rep.OrphanObjectsReclaimed.Count != 1 || rep.OrphanObjectsReclaimed.Sample[0] != "blk-aged-orphan" {
		t.Errorf("OrphanObjectsReclaimed = %+v; want count 1 sample [blk-aged-orphan]", rep.OrphanObjectsReclaimed)
	}
	if remoteHasBlock(t, rbs, "blk-aged-orphan") {
		t.Error("aged orphan object still present after reclaim")
	}
	if !remoteHasBlock(t, rbs, "blk-fresh-orphan") {
		t.Error("fresh orphan inside grace window was wrongly reclaimed")
	}
	if !remoteHasBlock(t, rbs, "blk-noage-orphan") {
		t.Error("zero-LastModified orphan (unevaluable age) was wrongly reclaimed; must fail closed")
	}
	if !remoteHasBlock(t, rbs, "blk-known") {
		t.Error("object with a backing record was wrongly reclaimed")
	}
}

// TestReclaimRecords_DryRun proves a dry run tallies the same set but
// deletes nothing.
func TestReclaimRecords_DryRun(t *testing.T) {
	ctx := t.Context()
	st := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	rbs := remotememory.New()
	defer func() { _ = rbs.Close() }()

	putZeroRefRecord(t, st, rbs, "blk-zeroref", 500)

	rep, err := ReclaimRecords(ctx, []ReclaimMetaView{st}, rbs, ReclaimOptions{DryRun: true})
	if err != nil {
		t.Fatalf("ReclaimRecords: %v", err)
	}
	if !rep.DryRun || rep.Reclaimed.Count != 1 || rep.Reclaimed.Bytes != 500 {
		t.Errorf("dry-run report = %+v; want DryRun count 1 bytes 500", rep)
	}
	if !recordExists(t, st, "blk-zeroref") || !remoteHasBlock(t, rbs, "blk-zeroref") {
		t.Error("dry run deleted something; must be non-mutating")
	}
}

// raceView simulates a client commit (DefaultCommitBlock — record + locator in
// one transaction) landing BETWEEN the reclaimer's two metadata scans. Block "R"
// is invisible to the first scan the reclaimer performs and visible to the second.
// A correct reclaimer walks records BEFORE building the live set, so it either
// never sees R (skipped this run, safe) or sees both R and its locator (survives).
// A reclaimer that builds the live set first would capture an empty set, then walk
// R (LiveChunkCount>0, absent from the stale set) and delete a live block — the
// #1525 TOCTOU. This test fails if the scan order is ever reverted.
type raceView struct {
	scans   int
	deleted []string
}

func (v *raceView) reveal() bool { visible := v.scans > 0; v.scans++; return visible }

func (v *raceView) WalkBlockRecords(_ context.Context, fn func(block.BlockRecord) error) error {
	if v.reveal() {
		return fn(block.BlockRecord{BlockID: "R", Length: 10, LiveChunkCount: 3, SyncState: block.BlockStateRemote})
	}
	return nil
}

func (v *raceView) EnumerateSynced(_ context.Context, fn func(block.ContentHash, time.Time) error) error {
	if v.reveal() {
		return fn(hashFromString("R-chunk"), time.Time{})
	}
	return nil
}

func (v *raceView) GetLocator(_ context.Context, _ block.ContentHash) (block.ChunkLocator, bool, error) {
	return block.ChunkLocator{BlockID: "R"}, true, nil
}

func (v *raceView) DeleteBlockRecord(_ context.Context, blockID string) error {
	v.deleted = append(v.deleted, blockID)
	return nil
}

// nestedGuardView models the sqlite metadata pool's MaxOpenConns(1) hazard: while
// an EnumerateSynced cursor is open it holds the only connection, so any query
// issued from inside the callback (e.g. GetLocator) would block forever waiting
// for a second. This view fails loudly instead of deadlocking, so the reclaimer
// must drain EnumerateSynced before resolving locators. blk-leaked has a record
// with no locator (class 2); blk-live has a record and a locator (must survive).
type nestedGuardView struct {
	t           *testing.T
	inEnumerate bool
	deleted     []string
}

func (v *nestedGuardView) EnumerateSynced(_ context.Context, fn func(block.ContentHash, time.Time) error) error {
	v.inEnumerate = true
	defer func() { v.inEnumerate = false }()
	return fn(hashFromString("live-chunk"), time.Time{})
}

func (v *nestedGuardView) GetLocator(_ context.Context, _ block.ContentHash) (block.ChunkLocator, bool, error) {
	if v.inEnumerate {
		v.t.Fatal("GetLocator called while EnumerateSynced cursor is open — deadlocks on sqlite MaxOpenConns(1)")
	}
	return block.ChunkLocator{BlockID: "blk-live"}, true, nil
}

func (v *nestedGuardView) WalkBlockRecords(_ context.Context, fn func(block.BlockRecord) error) error {
	for _, r := range []block.BlockRecord{
		{BlockID: "blk-live", Length: 5, LiveChunkCount: 1},
		{BlockID: "blk-leaked", Length: 7, LiveChunkCount: 3},
	} {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func (v *nestedGuardView) DeleteBlockRecord(_ context.Context, blockID string) error {
	v.deleted = append(v.deleted, blockID)
	return nil
}

// TestReclaimRecords_NoNestedQueryDuringEnumerate guards against re-introducing a
// GetLocator call inside the EnumerateSynced callback, which deadlocks on the
// single-connection sqlite pool (found via a real sqlite backend on a live VM).
func TestReclaimRecords_NoNestedQueryDuringEnumerate(t *testing.T) {
	v := &nestedGuardView{t: t}
	rep, err := ReclaimRecords(t.Context(), []ReclaimMetaView{v}, nil, ReclaimOptions{})
	if err != nil {
		t.Fatalf("ReclaimRecords: %v", err)
	}
	// Only the unreferenced leaked record is reclaimed; the located one survives.
	if len(v.deleted) != 1 || v.deleted[0] != "blk-leaked" {
		t.Errorf("deleted = %v; want [blk-leaked]", v.deleted)
	}
	if rep.LeakedReclaimed.Count != 1 || rep.Reclaimed.Count != 0 {
		t.Errorf("tally: leaked=%d zeroRef=%d; want leaked=1 zeroRef=0", rep.LeakedReclaimed.Count, rep.Reclaimed.Count)
	}
}

// TestReclaimRecords_CommitDuringScanNotDeleted is the #1525 TOCTOU regression:
// a block committed while a reclaim is scanning must never be reclaimed.
func TestReclaimRecords_CommitDuringScanNotDeleted(t *testing.T) {
	v := &raceView{}
	rep, err := ReclaimRecords(t.Context(), []ReclaimMetaView{v}, nil, ReclaimOptions{})
	if err != nil {
		t.Fatalf("ReclaimRecords: %v", err)
	}
	if len(v.deleted) != 0 {
		t.Errorf("deleted live block(s) committed mid-scan: %v", v.deleted)
	}
	if rep.Reclaimed.Count != 0 || rep.LeakedReclaimed.Count != 0 {
		t.Errorf("reclaimed a mid-scan commit: zeroRef=%d leaked=%d", rep.Reclaimed.Count, rep.LeakedReclaimed.Count)
	}
}

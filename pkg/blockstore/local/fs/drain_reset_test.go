package fs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestDrainRollups_ForcesManifestPopulation reproduces C1 (empty/racy
// manifest). With a LARGE stabilization window and NO rollup worker pool
// started, the async rollup never fires before a snapshot would run, so
// the ObjectIDPersister (which writes FileBlock rows + FileAttr.Blocks)
// is never invoked and the snapshot manifest is empty. DrainRollups must
// force ALL dirty payloads through rollup to completion regardless of the
// stabilization gate, so the persister fires and the manifest is
// non-empty.
func TestDrainRollups_ForcesManifestPopulation(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()

	var mu sync.Mutex
	var persisted []blockstore.BlockRef
	persister := func(_ context.Context, _ string, blocks []blockstore.BlockRef, _ blockstore.ObjectID) error {
		mu.Lock()
		defer mu.Unlock()
		persisted = append(persisted, blocks...)
		return nil
	}

	// Stabilization window is enormous (1 hour) so the ticker/stable path
	// can NEVER consume the interval; only a forced drain can.
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})
	// NOTE: intentionally NOT calling StartRollup — mirrors the snapshot
	// race where a snapshot is taken before the async rollup catches up.

	ctx := context.Background()
	payload := bytes.Repeat([]byte{0x5A}, 8*1024*1024)
	if err := bc.AppendWrite(ctx, "file1", payload, 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Before the drain the persister must NOT have run (proves the bug
	// would yield an empty manifest at snapshot time).
	mu.Lock()
	pre := len(persisted)
	mu.Unlock()
	if pre != 0 {
		t.Fatalf("persister fired before drain (%d blocks); test cannot prove the C1 race", pre)
	}

	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups: %v", err)
	}

	mu.Lock()
	post := len(persisted)
	mu.Unlock()
	if post == 0 {
		t.Fatal("DrainRollups did not force the rollup: persister never fired, manifest would be empty (C1)")
	}

	// Rollup offset must have advanced past the header, proving the log
	// was consumed.
	off, err := rs.GetRollupOffset(ctx, "file1")
	if err != nil {
		t.Fatalf("GetRollupOffset: %v", err)
	}
	if off <= uint64(logHeaderSize) {
		t.Fatalf("rollup_offset did not advance after DrainRollups: got %d", off)
	}
}

// TestDrainRollups_RealResidualReturnsErrDrainIncomplete verifies that
// when a no-progress drain pass leaves dirty intervals backed by REAL log
// data (payload not tombstoned, logIndex CAN back the interval),
// DrainRollups returns ErrDrainIncomplete instead of silently succeeding
// with a partial manifest.
//
// Deterministic construction: lower the reconstruct ceiling, then write
// contiguous records that merge into a single dirty interval whose span
// exceeds it. rollupFile reads the on-disk frames, but reconstructStream
// refuses the over-ceiling buffer and returns nil WITHOUT consuming the
// interval — a no-progress pass that leaves a genuine, logIndex-backed
// dirty interval in place.
func TestDrainRollups_RealResidualReturnsErrDrainIncomplete(t *testing.T) {
	defer func(old uint64) { maxReconstructBytes = old }(maxReconstructBytes)
	maxReconstructBytes = 128

	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		StabilizationMS: 1,
		RollupStore:     memmeta.NewMemoryMetadataStoreWithDefaults(),
	})
	ctx := context.Background()
	const payloadID = "file-real-residual"

	// One 200-byte record spans [0,200), exceeding the 128-byte test
	// ceiling, so reconstructStream refuses every pass.
	if err := bc.AppendWrite(ctx, payloadID, bytes.Repeat([]byte{0x33}, 200), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Sanity: the interval is logIndex-backed (real data), so the
	// classifier must flag it.
	if got := bc.payloadsWithRealResidual([]string{payloadID}); len(got) != 1 {
		t.Fatalf("payloadsWithRealResidual = %v, want exactly [%q]", got, payloadID)
	}

	err := bc.DrainRollups(ctx)
	if !errors.Is(err, ErrDrainIncomplete) {
		t.Fatalf("DrainRollups err = %v, want errors.Is(ErrDrainIncomplete)", err)
	}
}

// TestDrainRollups_TombstonedResidualSucceeds verifies that residual dirty
// intervals on a TOMBSTONED payload are legitimately skipped: DrainRollups
// returns nil (the bytes belong to a deleted payload and never need to
// reach the manifest).
func TestDrainRollups_TombstonedResidualSucceeds(t *testing.T) {
	defer func(old uint64) { maxReconstructBytes = old }(maxReconstructBytes)
	maxReconstructBytes = 128

	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		StabilizationMS: 1,
		RollupStore:     memmeta.NewMemoryMetadataStoreWithDefaults(),
	})
	ctx := context.Background()
	const payloadID = "file-tombstoned-residual"

	// Same un-drainable real residual as above (span > test ceiling).
	if err := bc.AppendWrite(ctx, payloadID, bytes.Repeat([]byte{0x44}, 200), 0); err != nil {
		t.Fatalf("AppendWrite: %v", err)
	}

	// Tombstone the payload directly (mirrors a concurrent DeleteAppendLog
	// having set the tombstone). The classifier must now exclude it.
	bcSh := bc.shardFor(payloadID)
	bcSh.mu.Lock()
	bcSh.tombstones[payloadID] = struct{}{}
	bcSh.mu.Unlock()

	if got := bc.payloadsWithRealResidual([]string{payloadID}); len(got) != 0 {
		t.Fatalf("payloadsWithRealResidual on tombstoned payload = %v, want empty", got)
	}

	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups on tombstoned-only residual = %v, want nil", err)
	}
}

// TestDrainRollups_DivergentResidualSucceeds verifies that a tree-only
// dirty interval with NO logIndex backing (a tree/logIndex divergence —
// the DropExact case) is treated as benign: rollupFile drops it and
// DrainRollups returns nil.
func TestDrainRollups_DivergentResidualSucceeds(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		StabilizationMS: 1,
		RollupStore:     memmeta.NewMemoryMetadataStoreWithDefaults(),
	})
	ctx := context.Background()
	const payloadID = "file-divergent-residual"

	// Materialize per-payload state without any logIndex entry, then inject
	// a tree-only interval — divergent by construction (no backing).
	_, mu, tree, idx, err := bc.getOrCreateLog(payloadID)
	if err != nil {
		t.Fatalf("getOrCreateLog: %v", err)
	}
	mu.Lock()
	tree.Insert(0, 4096, time.Now().Add(-time.Hour))
	mu.Unlock()

	// Sanity: no logIndex entry backs the interval, so it is NOT a real
	// residual.
	if len(idx.EntriesForInterval(0, 4096, nil)) != 0 {
		t.Fatalf("expected no logIndex entries for divergent interval")
	}
	if got := bc.payloadsWithRealResidual([]string{payloadID}); len(got) != 0 {
		t.Fatalf("payloadsWithRealResidual on divergent interval = %v, want empty", got)
	}

	// DrainRollups drops the divergent interval (rollupFile DropExact) and
	// succeeds.
	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups on divergent-only residual = %v, want nil", err)
	}
}

// TestResetLocalState_DropsStaleLog reproduces C2 (restore corrupts
// in-place-modified files). A file is written + drained into CAS (the
// snapshot state). Then the same payload is modified in place via a fresh
// AppendWrite — the new bytes land ONLY in the append log (not yet rolled
// up). A restore resets metadata to the snapshot, but unless the block
// store's per-payload log is also reset, ReadPayloadAt's replayLogIntoDest
// overlays the post-snapshot log record on top of the restored CAS bytes
// ("last record wins"), returning the MUTATED bytes. ResetLocalState must
// drop the stale log so reads go purely through CAS.
func TestResetLocalState_DropsStaleLog(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()

	// Real FileBlock store so ReadPayloadAt's CAS manifest path resolves
	// post-rollup bytes.
	fbs := newMemFileBlockStore()
	persister := func(ctx context.Context, payloadID string, blocks []blockstore.BlockRef, _ blockstore.ObjectID) error {
		return fbs.persist(ctx, payloadID, blocks)
	}

	bc := newFSStoreForTestWithFBS(t, fbs, FSStoreOptions{
		MaxLogBytes:       1 << 30,
		RollupWorkers:     2,
		StabilizationMS:   3_600_000,
		RollupStore:       rs,
		ObjectIDPersister: persister,
	})

	ctx := context.Background()

	// --- snapshot state: write "AAAA..." and drain to CAS ---
	snapBytes := bytes.Repeat([]byte{'A'}, 4096)
	if err := bc.AppendWrite(ctx, "fileA", snapBytes, 0); err != nil {
		t.Fatalf("AppendWrite snapshot bytes: %v", err)
	}
	if err := bc.DrainRollups(ctx); err != nil {
		t.Fatalf("DrainRollups (snapshot): %v", err)
	}

	// Sanity: reading now returns the snapshot bytes.
	got := make([]byte, 4096)
	if _, err := bc.ReadPayloadAt(ctx, "fileA", got, 0); err != nil {
		t.Fatalf("ReadPayloadAt after snapshot drain: %v", err)
	}
	if !bytes.Equal(got, snapBytes) {
		t.Fatal("post-drain read did not return snapshot bytes")
	}

	// --- post-snapshot in-place modification (log-only, NOT rolled up) ---
	mutBytes := bytes.Repeat([]byte{'B'}, 4096)
	if err := bc.AppendWrite(ctx, "fileA", mutBytes, 0); err != nil {
		t.Fatalf("AppendWrite mutation: %v", err)
	}
	// Confirm the mutation is observable (log overlay wins).
	if _, err := bc.ReadPayloadAt(ctx, "fileA", got, 0); err != nil {
		t.Fatalf("ReadPayloadAt after mutation: %v", err)
	}
	if !bytes.Equal(got, mutBytes) {
		t.Fatal("post-mutation read did not return mutated bytes; test setup invalid")
	}

	// --- restore: reset block-store local state (metadata reset is
	// modeled by the CAS manifest still holding the snapshot blocks) ---
	if err := bc.ResetLocalState(ctx); err != nil {
		t.Fatalf("ResetLocalState: %v", err)
	}

	// After ResetLocalState the stale log overlay must be gone; the read
	// resolves purely through the CAS manifest = snapshot bytes.
	clear(got)
	if _, err := bc.ReadPayloadAt(ctx, "fileA", got, 0); err != nil {
		t.Fatalf("ReadPayloadAt after reset: %v", err)
	}
	if !bytes.Equal(got, snapBytes) {
		t.Fatalf("ResetLocalState did not drop stale log: read returned mutated bytes (C2 corruption)\n got[:8]=%q want[:8]=%q", got[:8], snapBytes[:8])
	}
}

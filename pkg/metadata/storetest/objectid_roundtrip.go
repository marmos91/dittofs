package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ObjectIDIndexAccessor is an optional capability backends may implement to
// expose the per-backend ObjectID secondary index row count for assertions.
//
// Used by the ConcurrentQuiesceRace scenario to verify that exactly one
// row remains indexed under a contested ObjectID after the
// first-committer-wins resolution (Phase 13 D-14). Backends that do not
// implement the capability skip the scenario via type-assertion failure.
//
// Memory, Badger, and Postgres all satisfy this in Phase 13 Plan 05.
type ObjectIDIndexAccessor interface {
	// CountObjectIDIndexRows returns the number of files indexed under
	// the given objectID. Test-only; never call from production code.
	//
	// Zero-valued objectID inputs MUST return (0, nil) without backend
	// access — the partial/skip-zero discipline mirrors FindByObjectID.
	CountObjectIDIndexRows(ctx context.Context, objectID blockstore.ObjectID) (int, error)
}

// runObjectIDOpsTests dispatches the META-02 ObjectID conformance scenarios
// (Phase 13 D-04 / D-12). Each backend wires RunConformanceSuite into its
// *_conformance_test.go, so adding scenarios here automatically runs them
// against Memory, Badger, and Postgres.
//
// Scenarios fall into two files for readability:
//   - objectid_roundtrip.go: round-trip + lifecycle (this file).
//   - objectid_lookup.go: FindByObjectID + restart + concurrent-race +
//     cross-share scope.
func runObjectIDOpsTests(t *testing.T, factory StoreFactory) {
	t.Helper()
	t.Run("RoundTripBasic", func(t *testing.T) { testObjectID_RoundTripBasic(t, factory) })
	t.Run("ZeroSentinel", func(t *testing.T) { testObjectID_ZeroSentinel(t, factory) })
	t.Run("MutationLifecycle", func(t *testing.T) { testObjectID_MutationLifecycle(t, factory) })
	t.Run("SortStability", func(t *testing.T) { testObjectID_SortStability(t, factory) })
	t.Run("FindByObjectID", func(t *testing.T) { testObjectID_FindByObjectID(t, factory) })
	t.Run("RestartStability", func(t *testing.T) { testObjectID_RestartStability(t, factory) })
	t.Run("ConcurrentQuiesceRace", func(t *testing.T) { testObjectID_ConcurrentQuiesceRace(t, factory) })
	t.Run("CrossShareDedupScope_DEDUP02", func(t *testing.T) { testObjectID_CrossShareDedupScope(t, factory) })
}

// testObjectID_RoundTripBasic asserts that a non-zero ObjectID computed over
// three sorted-by-offset BlockRefs survives a PutFile/GetFile round-trip
// with byte-equal payload. Catches encoding drift between backends.
func testObjectID_RoundTripBasic(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "objid-roundtrip")
	fileHandle := createTestFile(t, store, "objid-roundtrip", rootHandle, "round.bin", 0o644)

	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-rt-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("oid-rt-1"), Offset: 4 << 20, Size: 4 << 20},
		{Hash: hashOfSeed("oid-rt-2"), Offset: 8 << 20, Size: 1 << 20},
	}
	wantOID := blockstore.ComputeObjectID(blocks)
	if wantOID.IsZero() {
		t.Fatalf("ComputeObjectID returned zero — fixture is broken")
	}

	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (pre-put): %v", err)
	}
	file.FileAttr.Blocks = blocks
	file.FileAttr.ObjectID = wantOID
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (round-trip): %v", err)
	}
	if got.FileAttr.ObjectID != wantOID {
		t.Errorf("ObjectID round-trip: got %s, want %s",
			got.FileAttr.ObjectID.String(), wantOID.String())
	}
}

// testObjectID_ZeroSentinel asserts that the all-zero ObjectID sentinel is
// accepted on PutFile and read back as zero, and that FindByObjectID with
// a zero argument short-circuits to (nil, nil) without backend access
// (Phase 13 D-03 / D-12 — partial/skip-zero discipline).
func testObjectID_ZeroSentinel(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "objid-zero")
	fileHandle := createTestFile(t, store, "objid-zero", rootHandle, "zero.bin", 0o644)

	// createTestFile leaves Blocks/ObjectID at zero; verify round-trip
	// preserves the zero sentinel.
	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !got.FileAttr.ObjectID.IsZero() {
		t.Errorf("ObjectID: got %s, want zero (never-quiesced sentinel)",
			got.FileAttr.ObjectID.String())
	}

	// Explicit PutFile with the zero sentinel.
	got.FileAttr.Blocks = nil
	got.FileAttr.ObjectID = blockstore.ObjectID{}
	if err := store.PutFile(ctx, got); err != nil {
		t.Fatalf("PutFile (zero ObjectID): %v", err)
	}
	got2, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (post-zero-put): %v", err)
	}
	if !got2.FileAttr.ObjectID.IsZero() {
		t.Errorf("ObjectID after zero PutFile: got %s, want zero",
			got2.FileAttr.ObjectID.String())
	}

	// FindByObjectID(zero) MUST return (nil, nil) without indexing.
	refs, err := store.FindByObjectID(ctx, blockstore.ObjectID{})
	if err != nil {
		t.Errorf("FindByObjectID(zero): unexpected error: %v", err)
	}
	if refs != nil {
		t.Errorf("FindByObjectID(zero): got %d refs, want nil (sentinel skip)", len(refs))
	}
}

// testObjectID_MutationLifecycle exercises the D-07 lifecycle: a quiesced
// file has a non-zero ObjectID and a populated index entry; mutating it to
// the zero sentinel (simulating a fresh dirty write) MUST clear the
// secondary index so subsequent FindByObjectID returns (nil, nil).
func testObjectID_MutationLifecycle(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "objid-mutate")
	fileHandle := createTestFile(t, store, "objid-mutate", rootHandle, "mutate.bin", 0o644)

	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-mut-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("oid-mut-1"), Offset: 4 << 20, Size: 4 << 20},
	}
	wantOID := blockstore.ComputeObjectID(blocks)

	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.FileAttr.Blocks = blocks
	file.FileAttr.ObjectID = wantOID
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile (quiesced): %v", err)
	}

	// Pre-mutation: index lookup hits.
	refs, err := store.FindByObjectID(ctx, wantOID)
	if err != nil {
		t.Fatalf("FindByObjectID (pre-mutation): %v", err)
	}
	if len(refs) != len(blocks) {
		t.Fatalf("FindByObjectID (pre-mutation): got %d refs, want %d", len(refs), len(blocks))
	}

	// Simulate the D-07 dirty-write event: caller (engine) clears
	// ObjectID to zero in the same metadata txn that mutates Blocks.
	mutated, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (pre-mutate): %v", err)
	}
	mutated.FileAttr.ObjectID = blockstore.ObjectID{}
	if err := store.PutFile(ctx, mutated); err != nil {
		t.Fatalf("PutFile (mutation): %v", err)
	}

	// Post-mutation: index lookup MUST miss.
	refs, err = store.FindByObjectID(ctx, wantOID)
	if err != nil {
		t.Errorf("FindByObjectID (post-mutation): %v", err)
	}
	if refs != nil {
		t.Errorf("FindByObjectID (post-mutation): got %d refs, want nil (index cleared on D-07 mutation)",
			len(refs))
	}

	// And the round-tripped FileAttr.ObjectID is zero.
	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (post-mutation): %v", err)
	}
	if !got.FileAttr.ObjectID.IsZero() {
		t.Errorf("ObjectID post-mutation: got %s, want zero", got.FileAttr.ObjectID.String())
	}
}

// testObjectID_SortStability asserts ComputeObjectID determinism (two
// independent calls over the same sorted BlockRef list yield byte-equal
// ObjectIDs) and survives a PutFile/GetFile/recompute cycle.
func testObjectID_SortStability(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "objid-sort")
	fileHandle := createTestFile(t, store, "objid-sort", rootHandle, "sort.bin", 0o644)

	// Five blocks already in canonical sorted-by-offset order.
	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-srt-0"), Offset: 0, Size: 1 << 20},
		{Hash: hashOfSeed("oid-srt-1"), Offset: 1 << 20, Size: 1 << 20},
		{Hash: hashOfSeed("oid-srt-2"), Offset: 2 << 20, Size: 1 << 20},
		{Hash: hashOfSeed("oid-srt-3"), Offset: 3 << 20, Size: 1 << 20},
		{Hash: hashOfSeed("oid-srt-4"), Offset: 4 << 20, Size: 1 << 20},
	}

	// Determinism: independent ComputeObjectID calls over the same input
	// produce byte-identical outputs.
	a := blockstore.ComputeObjectID(blocks)
	b := blockstore.ComputeObjectID(blocks)
	if a != b {
		t.Fatalf("ComputeObjectID determinism violated: %s vs %s", a.String(), b.String())
	}

	// Persist + reload + recompute from stored Blocks.
	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	file.FileAttr.Blocks = blocks
	file.FileAttr.ObjectID = a
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (round-trip): %v", err)
	}
	recomputed := blockstore.ComputeObjectID(got.FileAttr.Blocks)
	if recomputed != got.FileAttr.ObjectID {
		t.Errorf("recompute(stored Blocks) != stored ObjectID: %s vs %s",
			recomputed.String(), got.FileAttr.ObjectID.String())
	}
	if recomputed != a {
		t.Errorf("recompute(stored Blocks) != original ObjectID: %s vs %s",
			recomputed.String(), a.String())
	}
}

// concurrentRaceErrIsConflict accepts both Memory/Badger ErrConflict
// (D-14 first-committer-wins; surfaced via mderrors.NewConflictError) and
// Postgres ErrAlreadyExists (the partial UNIQUE index 23505 maps to
// ErrAlreadyExists in mapPgErrorCode). Either is the documented loser
// signal across backends.
func concurrentRaceErrIsConflict(err error) bool {
	if err == nil {
		return false
	}
	if mderrors.IsConflictError(err) {
		return true
	}
	var storeErr *metadata.StoreError
	if errors.As(err, &storeErr) {
		return storeErr.Code == metadata.ErrAlreadyExists
	}
	return false
}

// raceWorkerResult captures one goroutine's PutFile outcome for the
// ConcurrentQuiesceRace scenario. Used by both objectid_roundtrip.go and
// objectid_lookup.go via the shared runner below.
type raceWorkerResult struct {
	id  int
	err error
}

// runConcurrentQuiesceRace launches two goroutines that each PutFile a
// distinct file but with the same target ObjectID. Returns both worker
// outcomes once both goroutines complete. The barrier (`start`) ensures
// they race rather than serialize.
func runConcurrentQuiesceRace(
	ctx context.Context,
	store metadata.MetadataStore,
	files [2]*metadata.File,
) (raceWorkerResult, raceWorkerResult) {
	var wg sync.WaitGroup
	results := [2]raceWorkerResult{{id: 0}, {id: 1}}
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx].err = store.PutFile(ctx, files[idx])
		}(i)
	}
	close(start)
	wg.Wait()
	return results[0], results[1]
}

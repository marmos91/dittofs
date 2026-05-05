package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// testObjectID_FindByObjectID asserts that two distinct files with two
// distinct ObjectIDs are individually retrievable via FindByObjectID and
// that an unindexed ObjectID returns (nil, nil).
func testObjectID_FindByObjectID(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "objid-find")

	fileA := createTestFile(t, store, "objid-find", rootHandle, "a.bin", 0o644)
	fileB := createTestFile(t, store, "objid-find", rootHandle, "b.bin", 0o644)

	blocksA := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-find-a-0"), Offset: 0, Size: 4096},
		{Hash: hashOfSeed("oid-find-a-1"), Offset: 4096, Size: 4096},
	}
	blocksB := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-find-b-0"), Offset: 0, Size: 4096},
		{Hash: hashOfSeed("oid-find-b-1"), Offset: 4096, Size: 4096},
		{Hash: hashOfSeed("oid-find-b-2"), Offset: 8192, Size: 4096},
	}
	oidA := blockstore.ComputeObjectID(blocksA)
	oidB := blockstore.ComputeObjectID(blocksB)
	if oidA == oidB {
		t.Fatalf("fixture broken: distinct block fixtures produced equal ObjectID %s", oidA.String())
	}

	// PutFile A + B with their respective ObjectIDs.
	for _, pair := range []struct {
		handle metadata.FileHandle
		blocks []blockstore.BlockRef
		oid    blockstore.ObjectID
		label  string
	}{
		{fileA, blocksA, oidA, "A"},
		{fileB, blocksB, oidB, "B"},
	} {
		f, err := store.GetFile(ctx, pair.handle)
		if err != nil {
			t.Fatalf("GetFile %s: %v", pair.label, err)
		}
		f.Blocks = pair.blocks
		f.ObjectID = pair.oid
		if err := store.PutFile(ctx, f); err != nil {
			t.Fatalf("PutFile %s: %v", pair.label, err)
		}
	}

	// FindByObjectID(oidA) returns A's BlockRef list.
	gotA, err := store.FindByObjectID(ctx, oidA)
	if err != nil {
		t.Fatalf("FindByObjectID(A): %v", err)
	}
	if len(gotA) != len(blocksA) {
		t.Fatalf("FindByObjectID(A): got %d refs, want %d", len(gotA), len(blocksA))
	}
	for i, want := range blocksA {
		if gotA[i].Hash != want.Hash || gotA[i].Offset != want.Offset || gotA[i].Size != want.Size {
			t.Errorf("FindByObjectID(A)[%d] = %+v, want %+v", i, gotA[i], want)
		}
	}

	// FindByObjectID(oidB) returns B's BlockRef list.
	gotB, err := store.FindByObjectID(ctx, oidB)
	if err != nil {
		t.Fatalf("FindByObjectID(B): %v", err)
	}
	if len(gotB) != len(blocksB) {
		t.Fatalf("FindByObjectID(B): got %d refs, want %d", len(gotB), len(blocksB))
	}

	// Miss: an ObjectID nobody indexed.
	missOID := blockstore.ComputeObjectID([]blockstore.BlockRef{
		{Hash: hashOfSeed("oid-find-miss"), Offset: 0, Size: 1},
	})
	gotMiss, err := store.FindByObjectID(ctx, missOID)
	if err != nil {
		t.Errorf("FindByObjectID(miss): unexpected error: %v", err)
	}
	if gotMiss != nil {
		t.Errorf("FindByObjectID(miss): got %d refs, want nil (no row indexed)", len(gotMiss))
	}
}

// testObjectID_RestartStability asserts that a quiesced ObjectID survives
// a recompute over the round-tripped Blocks slice. For backends that have
// no persistence concept (Memory) this is equivalent to the round-trip
// scenario; for Badger/Postgres it additionally proves the byte-encoding
// is deterministic and recompute-stable.
//
// The factory contract creates a fresh store per call (suite.go), so we
// cannot literally close+reopen the same backing path here; the
// recompute-after-PutFile-GetFile cycle is the closest equivalent that
// runs uniformly across all three backends. Conformance harnesses that
// specifically exercise close+reopen live in the per-backend integration
// tests.
func testObjectID_RestartStability(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootHandle := createTestShare(t, store, "objid-restart")
	fileHandle := createTestFile(t, store, "objid-restart", rootHandle, "restart.bin", 0o644)

	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-rst-0"), Offset: 0, Size: 4 << 20},
		{Hash: hashOfSeed("oid-rst-1"), Offset: 4 << 20, Size: 4 << 20},
		{Hash: hashOfSeed("oid-rst-2"), Offset: 8 << 20, Size: 1 << 20},
	}
	wantOID := blockstore.ComputeObjectID(blocks)

	file, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (pre-put): %v", err)
	}
	file.Blocks = blocks
	file.ObjectID = wantOID
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (post-put): %v", err)
	}
	recomputed := blockstore.ComputeObjectID(got.Blocks)
	if recomputed != got.ObjectID {
		t.Errorf("recompute(stored Blocks) != stored ObjectID: %s vs %s",
			recomputed.String(), got.ObjectID.String())
	}
	if recomputed != wantOID {
		t.Errorf("recompute(stored Blocks) != original ObjectID: %s vs %s",
			recomputed.String(), wantOID.String())
	}

	// FindByObjectID resolves the same Blocks that PutFile stored.
	refs, err := store.FindByObjectID(ctx, wantOID)
	if err != nil {
		t.Fatalf("FindByObjectID: %v", err)
	}
	if len(refs) != len(blocks) {
		t.Fatalf("FindByObjectID: got %d refs, want %d", len(refs), len(blocks))
	}
	for i, want := range blocks {
		if refs[i].Hash != want.Hash || refs[i].Offset != want.Offset || refs[i].Size != want.Size {
			t.Errorf("FindByObjectID[%d] = %+v, want %+v", i, refs[i], want)
		}
	}
}

// testObjectID_ConcurrentQuiesceRace asserts D-14 first-committer-wins
// semantics: two PutFile calls racing to claim the SAME ObjectID for
// DIFFERENT files settle so exactly one survives in the secondary index.
//
// Detection is per-backend (Memory/Badger surface ErrConflict; Postgres
// surfaces ErrAlreadyExists from the 23505 unique-violation), and
// concurrentRaceErrIsConflict accepts both.
//
// Index row count is verified via the optional ObjectIDIndexAccessor
// capability — required by all three backends in Plan 05; type-assertion
// failure means the backend forgot to implement it.
func testObjectID_ConcurrentQuiesceRace(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "objid-race"
	rootHandle := createTestShare(t, store, shareName)
	handleA := createTestFile(t, store, shareName, rootHandle, "race-a.bin", 0o644)
	handleB := createTestFile(t, store, shareName, rootHandle, "race-b.bin", 0o644)

	// Both files target the same contested ObjectID.
	contested := blockstore.ComputeObjectID([]blockstore.BlockRef{
		{Hash: hashOfSeed("oid-race-contest"), Offset: 0, Size: 4096},
	})

	// Pre-load each file with its current state from the store and stage
	// the contested ObjectID. Use distinct Blocks so each file's PutFile
	// updates a unique row but both attempt to claim `contested`.
	loadAndStage := func(handle metadata.FileHandle, seed string) *metadata.File {
		f, err := store.GetFile(ctx, handle)
		if err != nil {
			t.Fatalf("GetFile (stage %s): %v", seed, err)
		}
		f.Blocks = []blockstore.BlockRef{
			{Hash: hashOfSeed(seed), Offset: 0, Size: 4096},
		}
		f.ObjectID = contested
		return f
	}
	files := [2]*metadata.File{
		loadAndStage(handleA, "oid-race-a"),
		loadAndStage(handleB, "oid-race-b"),
	}

	r0, r1 := runConcurrentQuiesceRace(ctx, store, files)

	// Outcome accounting: exactly one winner (nil err) and one loser
	// (concurrentRaceErrIsConflict-positive).
	winners, losers := 0, 0
	for _, r := range []raceWorkerResult{r0, r1} {
		switch {
		case r.err == nil:
			winners++
		case concurrentRaceErrIsConflict(r.err):
			losers++
		default:
			t.Errorf("worker %d: unexpected error %v (want nil OR conflict-class)", r.id, r.err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("D-14 outcome: winners=%d, losers=%d (want 1+1); errs: %v / %v",
			winners, losers, r0.err, r1.err)
	}

	// Index integrity: exactly one row indexed under `contested`.
	accessor, ok := store.(ObjectIDIndexAccessor)
	if !ok {
		t.Skipf("backend %T does not implement ObjectIDIndexAccessor — skipping index-row count assertion", store)
	}
	n, err := accessor.CountObjectIDIndexRows(ctx, contested)
	if err != nil {
		t.Fatalf("CountObjectIDIndexRows: %v", err)
	}
	if n != 1 {
		t.Errorf("CountObjectIDIndexRows: got %d, want 1 (D-14 first-committer-wins)", n)
	}
}

// testObjectID_CrossShareDedupScope verifies D-13 / DEDUP-02: FindByObjectID
// resolves at the metadata-store layer, NOT at the share layer. Two shares are
// created against the SAME factory-produced store, each receives a quiesced
// file with a DIFFERENT ObjectID, and the store-level FindByObjectID returns
// each share's BlockRef list correctly when looked up by its respective
// ObjectID — proving cross-share dedup hits are addressable. A negative
// lookup against a never-persisted ObjectID returns nil (no false hit
// across the share boundary either way).
//
// Phase 13 plan 13-08 strengthens this scenario from the plan-05 baseline
// (single file in share-A) to two files in two shares with distinct
// ObjectIDs, locking the symmetric per-store invariant.
func testObjectID_CrossShareDedupScope(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootA := createTestShare(t, store, "objid-cross-a")
	rootB := createTestShare(t, store, "objid-cross-b")

	fileA := createTestFile(t, store, "objid-cross-a", rootA, "shared.bin", 0o644)
	fileB := createTestFile(t, store, "objid-cross-b", rootB, "other.bin", 0o644)

	blocksA := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-cross-a-0"), Offset: 0, Size: 4096},
		{Hash: hashOfSeed("oid-cross-a-1"), Offset: 4096, Size: 4096},
	}
	blocksB := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-cross-b-0"), Offset: 0, Size: 8192},
		{Hash: hashOfSeed("oid-cross-b-1"), Offset: 8192, Size: 4096},
		{Hash: hashOfSeed("oid-cross-b-2"), Offset: 12288, Size: 1024},
	}
	oidA := blockstore.ComputeObjectID(blocksA)
	oidB := blockstore.ComputeObjectID(blocksB)
	if oidA == oidB {
		t.Fatalf("fixture broken: distinct block fixtures produced equal ObjectID %s", oidA.String())
	}

	// PutFile each share's file with its respective ObjectID.
	for _, pair := range []struct {
		handle metadata.FileHandle
		blocks []blockstore.BlockRef
		oid    blockstore.ObjectID
		label  string
	}{
		{fileA, blocksA, oidA, "share-A"},
		{fileB, blocksB, oidB, "share-B"},
	} {
		f, err := store.GetFile(ctx, pair.handle)
		if err != nil {
			t.Fatalf("GetFile %s: %v", pair.label, err)
		}
		f.Blocks = pair.blocks
		f.ObjectID = pair.oid
		if err := store.PutFile(ctx, f); err != nil {
			t.Fatalf("PutFile %s: %v", pair.label, err)
		}
	}

	// Per D-13: the lookup operates at the metadata-store layer, not the
	// share layer; either share's ObjectID resolves to its persisted
	// BlockRef list regardless of which share's file owns it.

	// Lookup A: returns share-A's BlockRefs.
	gotA, err := store.FindByObjectID(ctx, oidA)
	if err != nil {
		t.Fatalf("FindByObjectID share-A (cross-share): %v", err)
	}
	if !blockRefSlicesEqual(gotA, blocksA) {
		t.Errorf("DEDUP-02: cross-share FindByObjectID(A) returned wrong blocks: got %+v, want %+v",
			gotA, blocksA)
	}

	// Lookup B: returns share-B's BlockRefs.
	gotB, err := store.FindByObjectID(ctx, oidB)
	if err != nil {
		t.Fatalf("FindByObjectID share-B (cross-share): %v", err)
	}
	if !blockRefSlicesEqual(gotB, blocksB) {
		t.Errorf("DEDUP-02: cross-share FindByObjectID(B) returned wrong blocks: got %+v, want %+v",
			gotB, blocksB)
	}

	// Negative: an ObjectID never persisted by either share returns nil.
	missOID := blockstore.ComputeObjectID([]blockstore.BlockRef{
		{Hash: hashOfSeed("oid-cross-miss"), Offset: 0, Size: 1},
	})
	if missOID == oidA || missOID == oidB {
		t.Fatalf("fixture broken: miss ObjectID collides with A/B")
	}
	gotMiss, err := store.FindByObjectID(ctx, missOID)
	if err != nil {
		t.Fatalf("FindByObjectID (cross-share miss): unexpected error: %v", err)
	}
	if gotMiss != nil {
		t.Errorf("DEDUP-02: cross-share miss should return nil, got %d refs", len(gotMiss))
	}
}

// blockRefSlicesEqual returns true if a and b have identical BlockRef
// fields (Hash, Offset, Size) in order. Defined as a local helper so
// objectid_lookup.go scenarios stay self-contained.
func blockRefSlicesEqual(a, b []blockstore.BlockRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Hash != b[i].Hash || a[i].Offset != b[i].Offset || a[i].Size != b[i].Size {
			return false
		}
	}
	return true
}

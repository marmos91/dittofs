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
		f.FileAttr.Blocks = pair.blocks
		f.FileAttr.ObjectID = pair.oid
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
	file.FileAttr.Blocks = blocks
	file.FileAttr.ObjectID = wantOID
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile (post-put): %v", err)
	}
	recomputed := blockstore.ComputeObjectID(got.FileAttr.Blocks)
	if recomputed != got.FileAttr.ObjectID {
		t.Errorf("recompute(stored Blocks) != stored ObjectID: %s vs %s",
			recomputed.String(), got.FileAttr.ObjectID.String())
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
		f.FileAttr.Blocks = []blockstore.BlockRef{
			{Hash: hashOfSeed(seed), Offset: 0, Size: 4096},
		}
		f.FileAttr.ObjectID = contested
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

// testObjectID_CrossShareDedupScope verifies D-13: FindByObjectID returns a
// hit for a file in share-A even when the lookup origin would (semantically)
// be share-B, because the secondary index is per-metadata-store, not
// per-share. Two shares are created against the SAME factory-produced
// store, share-A's file is quiesced with a known ObjectID, and the
// store-level FindByObjectID resolves that ObjectID regardless of share
// boundary — establishing the DEDUP-02 contract at the conformance tier.
func testObjectID_CrossShareDedupScope(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rootA := createTestShare(t, store, "objid-cross-a")
	_ = createTestShare(t, store, "objid-cross-b")

	fileA := createTestFile(t, store, "objid-cross-a", rootA, "shared.bin", 0o644)

	blocks := []blockstore.BlockRef{
		{Hash: hashOfSeed("oid-cross-0"), Offset: 0, Size: 4096},
		{Hash: hashOfSeed("oid-cross-1"), Offset: 4096, Size: 4096},
	}
	wantOID := blockstore.ComputeObjectID(blocks)

	f, err := store.GetFile(ctx, fileA)
	if err != nil {
		t.Fatalf("GetFile share-A: %v", err)
	}
	f.FileAttr.Blocks = blocks
	f.FileAttr.ObjectID = wantOID
	if err := store.PutFile(ctx, f); err != nil {
		t.Fatalf("PutFile share-A: %v", err)
	}

	// Per D-13: the lookup operates at the metadata-store layer, not the
	// share layer; share-B's existence does not isolate share-A's
	// ObjectID. The hit returns share-A's BlockRef list.
	refs, err := store.FindByObjectID(ctx, wantOID)
	if err != nil {
		t.Fatalf("FindByObjectID (cross-share): %v", err)
	}
	if len(refs) != len(blocks) {
		t.Fatalf("FindByObjectID (cross-share): got %d refs, want %d (D-13 per-store scope)",
			len(refs), len(blocks))
	}
	for i, want := range blocks {
		if refs[i].Hash != want.Hash || refs[i].Offset != want.Offset || refs[i].Size != want.Size {
			t.Errorf("cross-share refs[%d] = %+v, want %+v", i, refs[i], want)
		}
	}
}

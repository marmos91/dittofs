package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// runFileBlockOpsTests runs the FileBlockStore conformance suite.
// MetadataStore embeds FileBlockStore, so the StoreFactory works directly.
func runFileBlockOpsTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("ListLocalBlocks", func(t *testing.T) {
		testListLocalBlocks(t, factory)
	})

	t.Run("ListLocalBlocks_Limit", func(t *testing.T) {
		testListLocalBlocksLimit(t, factory)
	})

	t.Run("ListLocalBlocks_OlderThan", func(t *testing.T) {
		testListLocalBlocksOlderThan(t, factory)
	})

	t.Run("ListLocalBlocks_EmptyStore", func(t *testing.T) {
		testListLocalBlocksEmptyStore(t, factory)
	})

	t.Run("ListRemoteBlocks", func(t *testing.T) {
		testListRemoteBlocks(t, factory)
	})

	t.Run("ListRemoteBlocks_Limit", func(t *testing.T) {
		testListRemoteBlocksLimit(t, factory)
	})

	t.Run("ListRemoteBlocks_EmptyStore", func(t *testing.T) {
		testListRemoteBlocksEmptyStore(t, factory)
	})

	t.Run("ListFileBlocks", func(t *testing.T) {
		testListFileBlocks(t, factory)
	})

	t.Run("ListFileBlocks_Ordering", func(t *testing.T) {
		testListFileBlocksOrdering(t, factory)
	})

	t.Run("ListFileBlocks_MixedStates", func(t *testing.T) {
		testListFileBlocksMixedStates(t, factory)
	})

	t.Run("ListFileBlocks_EmptyStore", func(t *testing.T) {
		testListFileBlocksEmptyStore(t, factory)
	})

	// Phase 11 Plan 02 (D-13/D-14): the syncer claim cycle stamps
	// LastSyncAttemptAt = now when flipping a block to Syncing, and the
	// restart-recovery janitor compares it against ClaimTimeout. Every
	// metadata backend MUST round-trip the field — otherwise a process
	// restart cannot tell stale Syncing rows from fresh ones.
	t.Run("PutGet_LastSyncAttemptAt", func(t *testing.T) {
		testPutGet_LastSyncAttemptAt(t, factory)
	})

	t.Run("PutGet_LastSyncAttemptAt_Zero", func(t *testing.T) {
		testPutGet_LastSyncAttemptAt_Zero(t, factory)
	})

	// Phase 11 WR-4-01: the dedup short-circuit (engine.uploadOne) writes a
	// second FileBlock with a fresh ID but the same ContentHash whenever two
	// file regions hash-match. Hash is NOT a uniqueness key at the contract
	// level (see FileBlockStore.PutFileBlock godoc). Backends that enforce
	// hash uniqueness reject the second writer, leak the donor's RefCount,
	// and leave the FileBlock in Syncing forever. This regression test pins
	// the contract across all three backends.
	t.Run("PutFileBlock_TwoIDsSameHash", func(t *testing.T) {
		testPutFileBlock_TwoIDsSameHash(t, factory)
	})

	// Phase 11 Plan 06 (GC-01 / D-02): the GC mark phase calls
	// EnumerateFileBlocks on the metadata store to stream every live
	// ContentHash into the disk-backed live set. Every backend MUST yield
	// every block — under-yield risks the sweep deleting referenced data.
	t.Run("EnumerateFileBlocks_Empty", func(t *testing.T) {
		testEnumerateFileBlocks_Empty(t, factory)
	})

	t.Run("EnumerateFileBlocks_SingleFile", func(t *testing.T) {
		testEnumerateFileBlocks_SingleFile(t, factory)
	})

	t.Run("EnumerateFileBlocks_LargeFanout", func(t *testing.T) {
		testEnumerateFileBlocks_LargeFanout(t, factory)
	})

	t.Run("EnumerateFileBlocks_FnErrorAborts", func(t *testing.T) {
		testEnumerateFileBlocks_FnErrorAborts(t, factory)
	})

	t.Run("EnumerateFileBlocks_ContextCancellation", func(t *testing.T) {
		testEnumerateFileBlocks_ContextCancellation(t, factory)
	})

	t.Run("EnumerateFileBlocks_ZeroHashEmitted", func(t *testing.T) {
		testEnumerateFileBlocks_ZeroHashEmitted(t, factory)
	})

	// Phase 11 INV-04 (mark fail-closed): backends that store the
	// ContentHash as text (Postgres) MUST surface a parse error when a
	// row's hash column holds a malformed value. Coercing the row to the
	// zero hash would let GC reap a still-live CAS object once the grace
	// TTL lapses. Backends that physically cannot represent a malformed
	// hash (memory/badger store [32]byte directly) skip via the optional
	// CorruptHashInjector capability.
	t.Run("EnumerateFileBlocks_CorruptHashFailsClosed", func(t *testing.T) {
		testEnumerateFileBlocks_CorruptHashFailsClosed(t, factory)
	})
}

// ============================================================================
// ListLocalBlocks Tests
// ============================================================================

func testListLocalBlocks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 5 blocks with different states
	blocks := []*blockstore.FileBlock{
		{ID: "file-a/0", State: blockstore.BlockStatePending, LocalPath: "/cache/a0", DataSize: 100, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-a/1", State: blockstore.BlockStatePending, LocalPath: "/cache/a1", DataSize: 200, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-b/0", State: blockstore.BlockStatePending, LocalPath: "/cache/b0", DataSize: 300, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-c/0", State: blockstore.BlockStateRemote, LocalPath: "/cache/c0", BlockStoreKey: "s3://c0", DataSize: 400, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-d/0", State: blockstore.BlockStateSyncing, LocalPath: "/cache/d0", DataSize: 500, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
	}
	for _, b := range blocks {
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListLocalBlocks(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListLocalBlocks() error: %v", err)
	}

	// Phase 11 (STATE-01) collapsed Dirty + Local into Pending; ListLocalBlocks
	// now returns every Pending row with a LocalPath. Three of the seeded
	// blocks (file-a/0, file-a/1, file-b/0) match.
	if len(result) != 3 {
		t.Fatalf("ListLocalBlocks() returned %d blocks, want 3 (all Pending+LocalPath)", len(result))
	}

	for _, b := range result {
		if b.State != blockstore.BlockStatePending {
			t.Errorf("ListLocalBlocks() returned block %s with state %v, want Pending", b.ID, b.State)
		}
	}
}

func testListLocalBlocksLimit(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 3 Local blocks
	for i := 0; i < 3; i++ {
		b := &blockstore.FileBlock{
			ID: fmt.Sprintf("file-x/%d", i), State: blockstore.BlockStatePending,
			LocalPath: fmt.Sprintf("/cache/x%d", i), DataSize: 100, RefCount: 1,
			LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour),
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListLocalBlocks(ctx, 0, 1)
	if err != nil {
		t.Fatalf("ListLocalBlocks(limit=1) error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("ListLocalBlocks(limit=1) returned %d blocks, want 1", len(result))
	}
}

func testListLocalBlocksOlderThan(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 2 blocks: one old, one recent
	old := &blockstore.FileBlock{
		ID: "file-old/0", State: blockstore.BlockStatePending, LocalPath: "/cache/old",
		DataSize: 100, RefCount: 1,
		LastAccess: time.Now().Add(-2 * time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	recent := &blockstore.FileBlock{
		ID: "file-recent/0", State: blockstore.BlockStatePending, LocalPath: "/cache/recent",
		DataSize: 100, RefCount: 1,
		LastAccess: time.Now(), CreatedAt: time.Now(),
	}
	if err := store.PutFileBlock(ctx, old); err != nil {
		t.Fatalf("PutFileBlock(old) failed: %v", err)
	}
	if err := store.PutFileBlock(ctx, recent); err != nil {
		t.Fatalf("PutFileBlock(recent) failed: %v", err)
	}

	// olderThan=1h should only return the old block
	result, err := store.ListLocalBlocks(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("ListLocalBlocks(olderThan=1h) error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("ListLocalBlocks(olderThan=1h) returned %d blocks, want 1", len(result))
	}
	if result[0].ID != "file-old/0" {
		t.Errorf("ListLocalBlocks(olderThan=1h) returned %s, want file-old/0", result[0].ID)
	}
}

func testListLocalBlocksEmptyStore(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	result, err := store.ListLocalBlocks(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListLocalBlocks(empty) error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ListLocalBlocks(empty) returned %d blocks, want 0", len(result))
	}
}

// ============================================================================
// ListRemoteBlocks Tests
// ============================================================================

func testListRemoteBlocks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 5 blocks with different states
	blocks := []*blockstore.FileBlock{
		{ID: "file-a/0", State: blockstore.BlockStateRemote, LocalPath: "/cache/a0", BlockStoreKey: "s3://a0", DataSize: 100, RefCount: 1, LastAccess: time.Now().Add(-2 * time.Hour), CreatedAt: time.Now()},
		{ID: "file-a/1", State: blockstore.BlockStateRemote, LocalPath: "/cache/a1", BlockStoreKey: "s3://a1", DataSize: 200, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now()},
		{ID: "file-b/0", State: blockstore.BlockStateRemote, LocalPath: "", BlockStoreKey: "s3://b0", DataSize: 300, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},                 // Not cached
		{ID: "file-c/0", State: blockstore.BlockStatePending, LocalPath: "/cache/c0", DataSize: 400, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)}, // Local, not Remote
		{ID: "file-d/0", State: blockstore.BlockStatePending, LocalPath: "/cache/d0", DataSize: 500, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)}, // Dirty
	}
	for _, b := range blocks {
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListRemoteBlocks(ctx, 0)
	if err != nil {
		t.Fatalf("ListRemoteBlocks() error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("ListRemoteBlocks() returned %d blocks, want 2 (Remote + cached)", len(result))
	}

	// Should be ordered by LastAccess (oldest first = LRU)
	if result[0].LastAccess.After(result[1].LastAccess) {
		t.Errorf("ListRemoteBlocks() not ordered by LRU: %v > %v", result[0].LastAccess, result[1].LastAccess)
	}
}

func testListRemoteBlocksLimit(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 3 Remote + cached blocks
	for i := 0; i < 3; i++ {
		b := &blockstore.FileBlock{
			ID: fmt.Sprintf("file-r/%d", i), State: blockstore.BlockStateRemote,
			LocalPath: fmt.Sprintf("/cache/r%d", i), BlockStoreKey: fmt.Sprintf("s3://r%d", i),
			DataSize: 100, RefCount: 1,
			LastAccess: time.Now().Add(-time.Duration(i) * time.Hour), CreatedAt: time.Now(),
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListRemoteBlocks(ctx, 1)
	if err != nil {
		t.Fatalf("ListRemoteBlocks(limit=1) error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("ListRemoteBlocks(limit=1) returned %d blocks, want 1", len(result))
	}
}

func testListRemoteBlocksEmptyStore(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	result, err := store.ListRemoteBlocks(ctx, 0)
	if err != nil {
		t.Fatalf("ListRemoteBlocks(empty) error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ListRemoteBlocks(empty) returned %d blocks, want 0", len(result))
	}
}

// ============================================================================
// ListFileBlocks Tests
// ============================================================================

func testListFileBlocks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks for 2 different files
	blocks := []*blockstore.FileBlock{
		{ID: "file-A/0", State: blockstore.BlockStatePending, LocalPath: "/cache/a0", DataSize: 100, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-A/1", State: blockstore.BlockStatePending, LocalPath: "/cache/a1", DataSize: 200, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-A/2", State: blockstore.BlockStateRemote, LocalPath: "/cache/a2", BlockStoreKey: "s3://a2", DataSize: 300, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-B/0", State: blockstore.BlockStatePending, LocalPath: "/cache/b0", DataSize: 400, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-B/1", State: blockstore.BlockStatePending, LocalPath: "/cache/b1", DataSize: 500, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
	}
	for _, b := range blocks {
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	// Query file-A
	resultA, err := store.ListFileBlocks(ctx, "file-A")
	if err != nil {
		t.Fatalf("ListFileBlocks(file-A) error: %v", err)
	}
	if len(resultA) != 3 {
		t.Fatalf("ListFileBlocks(file-A) returned %d blocks, want 3", len(resultA))
	}

	// Verify ordering by block index
	for i, b := range resultA {
		expectedID := fmt.Sprintf("file-A/%d", i)
		if b.ID != expectedID {
			t.Errorf("ListFileBlocks(file-A)[%d].ID = %s, want %s", i, b.ID, expectedID)
		}
	}

	// Query file-B
	resultB, err := store.ListFileBlocks(ctx, "file-B")
	if err != nil {
		t.Fatalf("ListFileBlocks(file-B) error: %v", err)
	}
	if len(resultB) != 2 {
		t.Fatalf("ListFileBlocks(file-B) returned %d blocks, want 2", len(resultB))
	}

	// Query nonexistent
	resultN, err := store.ListFileBlocks(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListFileBlocks(nonexistent) error: %v", err)
	}
	if len(resultN) != 0 {
		t.Errorf("ListFileBlocks(nonexistent) returned %d blocks, want 0", len(resultN))
	}

	// Verify data integrity
	if resultA[0].DataSize != 100 {
		t.Errorf("ListFileBlocks(file-A)[0].DataSize = %d, want 100", resultA[0].DataSize)
	}
	if resultA[2].State != blockstore.BlockStateRemote {
		t.Errorf("ListFileBlocks(file-A)[2].State = %v, want Remote", resultA[2].State)
	}
}

func testListFileBlocksOrdering(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks for one file with out-of-order indices
	indices := []int{0, 5, 10, 2, 7}
	for _, idx := range indices {
		b := &blockstore.FileBlock{
			ID: fmt.Sprintf("file-sort/%d", idx), State: blockstore.BlockStatePending,
			LocalPath: fmt.Sprintf("/cache/s%d", idx), DataSize: uint32(idx * 100),
			RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now(),
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListFileBlocks(ctx, "file-sort")
	if err != nil {
		t.Fatalf("ListFileBlocks(file-sort) error: %v", err)
	}
	if len(result) != 5 {
		t.Fatalf("ListFileBlocks(file-sort) returned %d blocks, want 5", len(result))
	}

	// Expected order: 0, 2, 5, 7, 10
	expectedOrder := []int{0, 2, 5, 7, 10}
	for i, expected := range expectedOrder {
		expectedID := fmt.Sprintf("file-sort/%d", expected)
		if result[i].ID != expectedID {
			t.Errorf("ListFileBlocks(file-sort)[%d].ID = %s, want %s", i, result[i].ID, expectedID)
		}
	}
}

func testListFileBlocksMixedStates(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks in all 4 states for same file
	states := []blockstore.BlockState{
		blockstore.BlockStatePending,
		blockstore.BlockStatePending,
		blockstore.BlockStateSyncing,
		blockstore.BlockStateRemote,
	}
	for i, state := range states {
		b := &blockstore.FileBlock{
			ID: fmt.Sprintf("file-mix/%d", i), State: state,
			LocalPath: fmt.Sprintf("/cache/m%d", i), DataSize: uint32((i + 1) * 100),
			RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now(),
		}
		if state == blockstore.BlockStateRemote {
			b.BlockStoreKey = "s3://mix"
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	// ListFileBlocks should return ALL blocks regardless of state
	result, err := store.ListFileBlocks(ctx, "file-mix")
	if err != nil {
		t.Fatalf("ListFileBlocks(file-mix) error: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("ListFileBlocks(file-mix) returned %d blocks, want 4", len(result))
	}

	// Verify each state is present
	statesSeen := make(map[blockstore.BlockState]bool)
	for _, b := range result {
		statesSeen[b.State] = true
	}
	for _, state := range states {
		if !statesSeen[state] {
			t.Errorf("ListFileBlocks(file-mix) missing state %v", state)
		}
	}
}

func testListFileBlocksEmptyStore(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	result, err := store.ListFileBlocks(ctx, "any")
	if err != nil {
		t.Fatalf("ListFileBlocks(empty) error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ListFileBlocks(empty) returned %d blocks, want 0", len(result))
	}
}

// ============================================================================
// LastSyncAttemptAt Round-Trip (Phase 11 Plan 02 — D-13/D-14)
// ============================================================================

// testPutGet_LastSyncAttemptAt asserts that a non-zero LastSyncAttemptAt
// round-trips through Put/Get for every metadata backend. The syncer's
// restart-recovery janitor (D-14) reads this field on Start and requeues
// stale Syncing rows; a backend that drops the field would silently break
// the recovery contract.
func testPutGet_LastSyncAttemptAt(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	stamp := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	in := &blockstore.FileBlock{
		ID:                "file-sync-attempt/0",
		State:             blockstore.BlockStateSyncing,
		LocalPath:         "/cache/sa0",
		DataSize:          128,
		RefCount:          1,
		LastAccess:        time.Now(),
		CreatedAt:         time.Now(),
		LastSyncAttemptAt: stamp,
	}

	if err := store.PutFileBlock(ctx, in); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}

	out, err := store.GetFileBlock(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetFileBlock failed: %v", err)
	}

	if !out.LastSyncAttemptAt.Equal(stamp) {
		t.Errorf("LastSyncAttemptAt round-trip: got %v, want %v",
			out.LastSyncAttemptAt, stamp)
	}
}

// testPutGet_LastSyncAttemptAt_Zero asserts that a default-zero
// LastSyncAttemptAt survives the round-trip without being silently set
// to "now" or some other non-zero value. The janitor uses IsZero as a
// proxy for "this row was never claimed", which only works if zero
// stays zero through serialization.
func testPutGet_LastSyncAttemptAt_Zero(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	in := &blockstore.FileBlock{
		ID:         "file-sync-zero/0",
		State:      blockstore.BlockStatePending,
		LocalPath:  "/cache/sz0",
		DataSize:   64,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
		// LastSyncAttemptAt deliberately left zero.
	}

	if err := store.PutFileBlock(ctx, in); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}

	out, err := store.GetFileBlock(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetFileBlock failed: %v", err)
	}

	if !out.LastSyncAttemptAt.IsZero() {
		t.Errorf("LastSyncAttemptAt should be zero on round-trip, got %v",
			out.LastSyncAttemptAt)
	}
}

// testPutFileBlock_TwoIDsSameHash asserts that two distinct FileBlock IDs
// sharing the same ContentHash both round-trip through PutFileBlock without
// error. Phase 11 WR-4-01: the dedup short-circuit (engine.uploadOne) emits
// such pairs whenever two file regions hash-match (e.g. all-zero blocks
// across distinct VM image files). A backend that rejects the second
// writer breaks the dedup path, leaves the FileBlock stuck in Syncing,
// and leaks the donor block's RefCount.
//
// The contract permits FindFileBlockByHash to return either of the
// colliding rows (memory + badger overwrite the hash→id map; postgres
// returns one of the two rows non-deterministically). The assertion
// scope is therefore: both PutFileBlock calls return nil AND
// FindFileBlockByHash returns one of the two IDs (no error, no nil).
func testPutFileBlock_TwoIDsSameHash(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("shared-content")
	keyA := "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String()
	keyB := keyA // CAS keys are identical for the same hash; that's the point

	a := &blockstore.FileBlock{
		ID:            "file-A/0",
		Hash:          hash,
		State:         blockstore.BlockStateRemote,
		LocalPath:     "/cache/A0",
		BlockStoreKey: keyA,
		DataSize:      4096,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	b := &blockstore.FileBlock{
		ID:            "file-B/0",
		Hash:          hash, // SAME content hash, different ID
		State:         blockstore.BlockStateRemote,
		LocalPath:     "/cache/B0",
		BlockStoreKey: keyB,
		DataSize:      4096,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}

	if err := store.PutFileBlock(ctx, a); err != nil {
		t.Fatalf("PutFileBlock(A) failed: %v", err)
	}
	// THE assertion: the second writer with a colliding hash must NOT error.
	if err := store.PutFileBlock(ctx, b); err != nil {
		t.Fatalf("PutFileBlock(B) with duplicate hash failed: %v "+
			"(WR-4-01: hash is not a uniqueness key — backends MUST tolerate "+
			"cross-row hash duplicates from the dedup short-circuit)", err)
	}

	// Both rows must be retrievable by their IDs.
	gotA, err := store.GetFileBlock(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(A) failed: %v", err)
	}
	if gotA.Hash != hash {
		t.Errorf("GetFileBlock(A).Hash = %x, want %x", gotA.Hash[:8], hash[:8])
	}
	gotB, err := store.GetFileBlock(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(B) failed: %v", err)
	}
	if gotB.Hash != hash {
		t.Errorf("GetFileBlock(B).Hash = %x, want %x", gotB.Hash[:8], hash[:8])
	}

	// FindFileBlockByHash must return one of the two — exact identity is
	// implementation-defined (memory + badger return whichever wrote the
	// hash→id map last; postgres returns whichever row the planner picks).
	found, err := store.FindFileBlockByHash(ctx, hash)
	if err != nil {
		t.Fatalf("FindFileBlockByHash failed: %v", err)
	}
	if found == nil {
		t.Fatal("FindFileBlockByHash returned nil; expected one of the two colliding rows")
	}
	if found.ID != a.ID && found.ID != b.ID {
		t.Errorf("FindFileBlockByHash returned ID %q; want one of [%q, %q]",
			found.ID, a.ID, b.ID)
	}
}

// ============================================================================
// EnumerateFileBlocks Tests (Phase 11 Plan 06 — GC-01 / D-02)
// ============================================================================

// hashOf returns a deterministic non-zero ContentHash from a seed string.
// Used by enumerate tests to seed unique hashes per block.
func hashOfSeed(seed string) blockstore.ContentHash {
	var h blockstore.ContentHash
	src := []byte(seed)
	// Spread bytes into the 32-byte hash deterministically.
	for i := 0; i < blockstore.HashSize; i++ {
		h[i] = src[i%len(src)] ^ byte(i)
	}
	return h
}

// testEnumerateFileBlocks_Empty: invokes fn 0 times on an empty store.
func testEnumerateFileBlocks_Empty(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	count := 0
	err := store.EnumerateFileBlocks(ctx, func(_ blockstore.ContentHash) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileBlocks(empty) error: %v", err)
	}
	if count != 0 {
		t.Errorf("EnumerateFileBlocks(empty): fn invoked %d times, want 0", count)
	}
}

// testEnumerateFileBlocks_SingleFile: fn invoked exactly N times for a file
// with N blocks; the yielded hash set equals the stored hash set.
func testEnumerateFileBlocks_SingleFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const n = 5
	want := make(map[blockstore.ContentHash]bool, n)
	for i := 0; i < n; i++ {
		h := hashOfSeed(fmt.Sprintf("single-%d", i))
		want[h] = true
		b := &blockstore.FileBlock{
			ID:            fmt.Sprintf("file-single/%d", i),
			Hash:          h,
			State:         blockstore.BlockStateRemote,
			LocalPath:     fmt.Sprintf("/cache/single-%d", i),
			BlockStoreKey: "cas/" + h.String()[0:2] + "/" + h.String()[2:4] + "/" + h.String(),
			DataSize:      128,
			RefCount:      1,
			LastAccess:    time.Now(),
			CreatedAt:     time.Now(),
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
		}
	}

	got := make(map[blockstore.ContentHash]bool, n)
	err := store.EnumerateFileBlocks(ctx, func(h blockstore.ContentHash) error {
		got[h] = true
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileBlocks error: %v", err)
	}
	if len(got) != n {
		t.Fatalf("EnumerateFileBlocks: got %d distinct hashes, want %d", len(got), n)
	}
	for h := range want {
		if !got[h] {
			t.Errorf("EnumerateFileBlocks: missing hash %x", h[:8])
		}
	}
}

// testEnumerateFileBlocks_LargeFanout: 50 files * 20 blocks = 1000 blocks; fn
// invoked exactly 1000 times; no duplicates, no omissions; iteration completes
// within 5s on the memory backend (sanity bound).
func testEnumerateFileBlocks_LargeFanout(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const files = 50
	const perFile = 20
	want := make(map[blockstore.ContentHash]int, files*perFile)
	for f := 0; f < files; f++ {
		for i := 0; i < perFile; i++ {
			h := hashOfSeed(fmt.Sprintf("fanout-%d-%d", f, i))
			want[h]++
			b := &blockstore.FileBlock{
				ID:            fmt.Sprintf("file-fan-%d/%d", f, i),
				Hash:          h,
				State:         blockstore.BlockStateRemote,
				LocalPath:     fmt.Sprintf("/cache/fan-%d-%d", f, i),
				BlockStoreKey: "cas/" + h.String()[0:2] + "/" + h.String()[2:4] + "/" + h.String(),
				DataSize:      64,
				RefCount:      1,
				LastAccess:    time.Now(),
				CreatedAt:     time.Now(),
			}
			if err := store.PutFileBlock(ctx, b); err != nil {
				t.Fatalf("PutFileBlock(%s) failed: %v", b.ID, err)
			}
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	got := 0
	seen := make(map[blockstore.ContentHash]int, files*perFile)
	err := store.EnumerateFileBlocks(ctx, func(h blockstore.ContentHash) error {
		got++
		seen[h]++
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileBlocks error: %v", err)
	}
	if time.Now().After(deadline) {
		t.Errorf("EnumerateFileBlocks took longer than 5s sanity bound")
	}
	if got != files*perFile {
		t.Errorf("EnumerateFileBlocks: fn invoked %d times, want %d", got, files*perFile)
	}
	if len(seen) != len(want) {
		t.Errorf("EnumerateFileBlocks: %d distinct hashes seen, want %d", len(seen), len(want))
	}
	for h, want := range want {
		if seen[h] != want {
			t.Errorf("EnumerateFileBlocks: hash %x seen %d times, want %d", h[:8], seen[h], want)
		}
	}
}

// testEnumerateFileBlocks_FnErrorAborts: fn returns a sentinel error on the
// 7th invocation; EnumerateFileBlocks returns that error (possibly wrapped).
// fn is invoked at most a small batch beyond the sentinel — tolerant of
// PrefetchSize batching but never iterates the full set.
func testEnumerateFileBlocks_FnErrorAborts(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const n = 50
	for i := 0; i < n; i++ {
		h := hashOfSeed(fmt.Sprintf("fn-err-%d", i))
		b := &blockstore.FileBlock{
			ID:         fmt.Sprintf("file-fnerr/%d", i),
			Hash:       h,
			State:      blockstore.BlockStateRemote,
			LocalPath:  fmt.Sprintf("/cache/fnerr-%d", i),
			DataSize:   64,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock failed: %v", err)
		}
	}

	sentinel := errors.New("sentinel error from fn")
	calls := 0
	err := store.EnumerateFileBlocks(ctx, func(_ blockstore.ContentHash) error {
		calls++
		if calls == 7 {
			return sentinel
		}
		return nil
	})
	if err == nil {
		t.Fatalf("EnumerateFileBlocks returned nil, want sentinel error")
	}
	if !errors.Is(err, sentinel) {
		// Some impls may wrap; accept exact equality OR errors.Is.
		if err.Error() != sentinel.Error() && err != sentinel {
			t.Errorf("EnumerateFileBlocks returned %v, want sentinel %v", err, sentinel)
		}
	}
	if calls < 7 {
		t.Errorf("EnumerateFileBlocks: fn invoked %d times, want >= 7", calls)
	}
	if calls >= n {
		t.Errorf("EnumerateFileBlocks: fn invoked %d times — iteration did not abort", calls)
	}
}

// testEnumerateFileBlocks_ContextCancellation: cancel mid-iteration; method
// returns ctx.Err (possibly wrapped) and stops invoking fn.
func testEnumerateFileBlocks_ContextCancellation(t *testing.T, factory StoreFactory) {
	store := factory(t)
	parent := t.Context()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	const n = 50
	for i := 0; i < n; i++ {
		h := hashOfSeed(fmt.Sprintf("ctx-cancel-%d", i))
		b := &blockstore.FileBlock{
			ID:         fmt.Sprintf("file-ctx/%d", i),
			Hash:       h,
			State:      blockstore.BlockStateRemote,
			LocalPath:  fmt.Sprintf("/cache/ctx-%d", i),
			DataSize:   64,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
		}
		if err := store.PutFileBlock(ctx, b); err != nil {
			t.Fatalf("PutFileBlock failed: %v", err)
		}
	}

	calls := 0
	err := store.EnumerateFileBlocks(ctx, func(_ blockstore.ContentHash) error {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Fatalf("EnumerateFileBlocks: expected non-nil error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("EnumerateFileBlocks: error %v does not wrap context.Canceled", err)
	}
	if calls >= n {
		t.Errorf("EnumerateFileBlocks: fn invoked %d times — cancellation ignored", calls)
	}
}

// testEnumerateFileBlocks_ZeroHashEmitted: blocks with zero hash (legacy rows)
// are still enumerated. The GC mark phase decides whether to skip them.
func testEnumerateFileBlocks_ZeroHashEmitted(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Seed one zero-hash legacy block + one finalized block.
	legacy := &blockstore.FileBlock{
		ID:         "file-zero/0",
		State:      blockstore.BlockStatePending,
		LocalPath:  "/cache/zero",
		DataSize:   64,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
		// Hash deliberately zero.
	}
	if err := store.PutFileBlock(ctx, legacy); err != nil {
		t.Fatalf("PutFileBlock(zero) failed: %v", err)
	}
	finalized := &blockstore.FileBlock{
		ID:            "file-zero/1",
		Hash:          hashOfSeed("non-zero"),
		State:         blockstore.BlockStateRemote,
		LocalPath:     "/cache/nz",
		BlockStoreKey: "cas/12/34/" + hashOfSeed("non-zero").String(),
		DataSize:      64,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := store.PutFileBlock(ctx, finalized); err != nil {
		t.Fatalf("PutFileBlock(finalized) failed: %v", err)
	}

	zeroSeen, finalizedSeen := false, false
	err := store.EnumerateFileBlocks(ctx, func(h blockstore.ContentHash) error {
		if h.IsZero() {
			zeroSeen = true
		} else {
			finalizedSeen = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileBlocks error: %v", err)
	}
	if !zeroSeen {
		t.Errorf("EnumerateFileBlocks did not emit zero-hash block (legacy row missed)")
	}
	if !finalizedSeen {
		t.Errorf("EnumerateFileBlocks did not emit non-zero hash block")
	}
}

// CorruptHashInjector is an optional capability backends implement when their
// physical row format admits malformed hashes (e.g., Postgres stores the hash
// as TEXT). Backends whose row format is type-safe (`[32]byte` directly, e.g.
// memory and badger) cannot represent corruption and skip the test.
type CorruptHashInjector interface {
	// InjectCorruptHashRow stores a file_blocks row whose hash column holds
	// a syntactically malformed value (e.g., truncated, wrong charset, wrong
	// length). The row is otherwise well-formed; only the hash is bad.
	InjectCorruptHashRow(ctx context.Context, blockID string, badHash string) error
}

// testEnumerateFileBlocks_CorruptHashFailsClosed asserts that a malformed CAS
// hash on disk surfaces as an error from EnumerateFileBlocks rather than being
// silently coerced to the zero ContentHash. INV-04 mark fail-closed: the GC
// mark phase MUST abort on enumeration error so the sweep cannot reap a live
// CAS object whose live-set hash was lost in transit.
func testEnumerateFileBlocks_CorruptHashFailsClosed(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	injector, ok := store.(CorruptHashInjector)
	if !ok {
		t.Skip("backend does not implement CorruptHashInjector — type-safe row format cannot represent a malformed hash")
	}

	// Seed one well-formed Remote block so enumeration has something to walk
	// past before reaching the corrupt row.
	good := &blockstore.FileBlock{
		ID:            "file-corrupt/0",
		Hash:          hashOfSeed("good"),
		State:         blockstore.BlockStateRemote,
		LocalPath:     "/cache/good",
		BlockStoreKey: "cas/aa/bb/" + hashOfSeed("good").String(),
		DataSize:      64,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := store.PutFileBlock(ctx, good); err != nil {
		t.Fatalf("PutFileBlock(good) failed: %v", err)
	}

	// Inject a corrupt-hash row directly. The exact "malformed" payload is
	// backend-defined; truncated hex is a representative case.
	if err := injector.InjectCorruptHashRow(ctx, "file-corrupt/1", "deadbeef"); err != nil {
		t.Fatalf("InjectCorruptHashRow failed: %v", err)
	}

	calls := 0
	err := store.EnumerateFileBlocks(ctx, func(_ blockstore.ContentHash) error {
		calls++
		return nil
	})
	if err == nil {
		t.Fatalf("EnumerateFileBlocks returned nil; expected parse error from corrupt-hash row (INV-04 fail-closed)")
	}
	// We do not constrain how many rows are emitted before the failure —
	// only that an error is returned so the GC mark phase aborts.
}

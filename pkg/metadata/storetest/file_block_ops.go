package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// fileBlockStoreLegacy captures the legacy GetFileBlock + ListFileBlocks
// methods that removed from the public
// FileBlockStore interface but kept on each backend struct for engine-
// internal callers. The conformance suite type-asserts the factory's
// MetadataStore to this interface so the existing tests can still drive
// the methods without depending on a concrete backend type.
type fileBlockStoreLegacy interface {
	GetFileBlock(ctx context.Context, id string) (*blockstore.FileBlock, error)
	ListFileBlocks(ctx context.Context, payloadID string) ([]*blockstore.FileBlock, error)
}

// asLegacy returns the legacy backend interface for a MetadataStore, or
// fails the test with a clear message when the backend does not provide
// the kept-but-not-on-interface methods.
func asLegacy(t *testing.T, store metadata.MetadataStore) fileBlockStoreLegacy {
	t.Helper()
	legacy, ok := store.(fileBlockStoreLegacy)
	if !ok {
		t.Skipf("backend %T does not implement fileBlockStoreLegacy (GetFileBlock/ListFileBlocks); engine-internal methods unavailable on this backend", store)
	}
	return legacy
}

// runFileBlockOpsTests runs the FileBlockStore conformance suite.
// MetadataStore embeds FileBlockStore, so the StoreFactory works directly.
func runFileBlockOpsTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("ListPending", func(t *testing.T) {
		testListPending(t, factory)
	})

	t.Run("ListPending_Limit", func(t *testing.T) {
		testListPendingLimit(t, factory)
	})

	t.Run("ListPending_OlderThan", func(t *testing.T) {
		testListPendingOlderThan(t, factory)
	})

	t.Run("ListPending_EmptyStore", func(t *testing.T) {
		testListPendingEmptyStore(t, factory)
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

	// the syncer claim cycle stamps
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

	// the dedup short-circuit (engine.uploadOne) writes a
	// second FileBlock with a fresh ID but the same ContentHash whenever two
	// file regions hash-match. Hash is NOT a uniqueness key at the contract
	// level (see FileBlockStore.Put godoc). Backends that enforce
	// hash uniqueness reject the second writer, leak the donor's RefCount,
	// and leave the FileBlock in Syncing forever. This regression test pins
	// the contract across all three backends.
	t.Run("Put_TwoIDsSameHash", func(t *testing.T) {
		testPut_TwoIDsSameHash(t, factory)
	})

	// the GC mark phase calls
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

	// (mark fail-closed): backends that store the
	// ContentHash as text (Postgres) MUST surface a parse error when a
	// row's hash column holds a malformed value. Coercing the row to the
	// zero hash would let GC reap a still-live CAS object once the grace
	// TTL lapses. Backends that physically cannot represent a malformed
	// hash (memory/badger store [32]byte directly) skip via the optional
	// CorruptHashInjector capability.
	t.Run("EnumerateFileBlocks_CorruptHashFailsClosed", func(t *testing.T) {
		testEnumerateFileBlocks_CorruptHashFailsClosed(t, factory)
	})

	// (review iteration 1): IncrementRefCount /
	// DecrementRefCount called via a metadata.Transaction MUST roll back
	// when the wrapping WithTransaction returns an error. Memory backend
	// has documented best-effort semantics (single mutex, no rollback
	// buffer) so its expected behavior differs; Postgres + Badger MUST
	// honor durable rollback.
	t.Run("Tx_IncrementRefCount_RollsBack", func(t *testing.T) {
		testTx_IncrementRefCount_RollsBack(t, factory)
	})

	// AddRef is the LRU-hit refcount path for the
	// in-memory hash dedup LRU (Opt 1). Three scenarios pin the contract
	// across all backends: existing-hash RefCount bumps (state preserved
	// per), missing-hash returns the ErrUnknownHash sentinel and
	// creates no row, and concurrent AddRef vs DecrementRefCount cascade
	// preserves the TOCTOU-free serialization invariant.
	t.Run("AddRef_ExistingHash_BumpsRefCount", func(t *testing.T) {
		testAddRef_ExistingHash_BumpsRefCount(t, factory)
	})

	t.Run("AddRef_MissingHash_ReturnsErrUnknownHash", func(t *testing.T) {
		testAddRef_MissingHash_ReturnsErrUnknownHash(t, factory)
	})

	t.Run("AddRef_Concurrent_With_DecrementRefCountCascade", func(t *testing.T) {
		testAddRef_Concurrent_With_DecrementRefCountCascade(t, factory)
	})
}

// ============================================================================
// ListPending Tests
// ============================================================================

func testListPending(t *testing.T, factory StoreFactory) {
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
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListPending(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListPending() error: %v", err)
	}

	// collapsed Dirty + Local into Pending; ListLocalBlocks
	// now returns every Pending row with a LocalPath. Three of the seeded
	// blocks (file-a/0, file-a/1, file-b/0) match.
	if len(result) != 3 {
		t.Fatalf("ListPending() returned %d blocks, want 3 (all Pending+LocalPath)", len(result))
	}

	for _, b := range result {
		if b.State != blockstore.BlockStatePending {
			t.Errorf("ListPending() returned block %s with state %v, want Pending", b.ID, b.State)
		}
	}
}

func testListPendingLimit(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 3 Local blocks
	for i := 0; i < 3; i++ {
		b := &blockstore.FileBlock{
			ID: fmt.Sprintf("file-x/%d", i), State: blockstore.BlockStatePending,
			LocalPath: fmt.Sprintf("/cache/x%d", i), DataSize: 100, RefCount: 1,
			LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour),
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	result, err := store.ListPending(ctx, 0, 1)
	if err != nil {
		t.Fatalf("ListPending(limit=1) error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("ListPending(limit=1) returned %d blocks, want 1", len(result))
	}
}

func testListPendingOlderThan(t *testing.T, factory StoreFactory) {
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
	if err := store.Put(ctx, old); err != nil {
		t.Fatalf("Put(old) failed: %v", err)
	}
	if err := store.Put(ctx, recent); err != nil {
		t.Fatalf("Put(recent) failed: %v", err)
	}

	// olderThan=1h should only return the old block
	result, err := store.ListPending(ctx, time.Hour, 0)
	if err != nil {
		t.Fatalf("ListPending(olderThan=1h) error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("ListPending(olderThan=1h) returned %d blocks, want 1", len(result))
	}
	if result[0].ID != "file-old/0" {
		t.Errorf("ListPending(olderThan=1h) returned %s, want file-old/0", result[0].ID)
	}
}

func testListPendingEmptyStore(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	result, err := store.ListPending(ctx, 0, 0)
	if err != nil {
		t.Fatalf("ListPending(empty) error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ListPending(empty) returned %d blocks, want 0", len(result))
	}
}

// ============================================================================
// ListFileBlocks Tests
//
// ListFileBlocks is no longer on the public
// FileBlockStore interface but is retained as a backend method for engine-
// internal callers. Tests use the legacyFileBlockStore type assertion to
// reach the method on each backend; backends that don't implement it
// (none today) skip cleanly.
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
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	// Query file-A
	resultA, err := asLegacy(t, store).ListFileBlocks(ctx, "file-A")
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
	resultB, err := asLegacy(t, store).ListFileBlocks(ctx, "file-B")
	if err != nil {
		t.Fatalf("ListFileBlocks(file-B) error: %v", err)
	}
	if len(resultB) != 2 {
		t.Fatalf("ListFileBlocks(file-B) returned %d blocks, want 2", len(resultB))
	}

	// Query nonexistent
	resultN, err := asLegacy(t, store).ListFileBlocks(ctx, "nonexistent")
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
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	result, err := asLegacy(t, store).ListFileBlocks(ctx, "file-sort")
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
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	// ListFileBlocks should return ALL blocks regardless of state
	result, err := asLegacy(t, store).ListFileBlocks(ctx, "file-mix")
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

	result, err := asLegacy(t, store).ListFileBlocks(ctx, "any")
	if err != nil {
		t.Fatalf("ListFileBlocks(empty) error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ListFileBlocks(empty) returned %d blocks, want 0", len(result))
	}
}

// ============================================================================
// LastSyncAttemptAt Round-Trip
// ============================================================================

// testPutGet_LastSyncAttemptAt asserts that a non-zero LastSyncAttemptAt
// round-trips through Put/Get for every metadata backend. The syncer's
// restart-recovery janitor reads this field on Start and requeues
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

	if err := store.Put(ctx, in); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}

	out, err := asLegacy(t, store).GetFileBlock(ctx, in.ID)
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

	if err := store.Put(ctx, in); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}

	out, err := asLegacy(t, store).GetFileBlock(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetFileBlock failed: %v", err)
	}

	if !out.LastSyncAttemptAt.IsZero() {
		t.Errorf("LastSyncAttemptAt should be zero on round-trip, got %v",
			out.LastSyncAttemptAt)
	}
}

// testPut_TwoIDsSameHash asserts that two distinct FileBlock IDs
// sharing the same ContentHash both round-trip through PutFileBlock without
// error. the dedup short-circuit (engine.uploadOne) emits
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
func testPut_TwoIDsSameHash(t *testing.T, factory StoreFactory) {
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

	if err := store.Put(ctx, a); err != nil {
		t.Fatalf("Put(A) failed: %v", err)
	}
	// THE assertion: the second writer with a colliding hash must NOT error.
	if err := store.Put(ctx, b); err != nil {
		t.Fatalf("Put(B) with duplicate hash failed: %v "+
			"(WR-4-01: hash is not a uniqueness key — backends MUST tolerate "+
			"cross-row hash duplicates from the dedup short-circuit)", err)
	}

	// Both rows must be retrievable by their IDs.
	gotA, err := asLegacy(t, store).GetFileBlock(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(A) failed: %v", err)
	}
	if gotA.Hash != hash {
		t.Errorf("GetFileBlock(A).Hash = %x, want %x", gotA.Hash[:8], hash[:8])
	}
	gotB, err := asLegacy(t, store).GetFileBlock(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetFileBlock(B) failed: %v", err)
	}
	if gotB.Hash != hash {
		t.Errorf("GetFileBlock(B).Hash = %x, want %x", gotB.Hash[:8], hash[:8])
	}

	// FindFileBlockByHash must return one of the two — exact identity is
	// implementation-defined (memory + badger return whichever wrote the
	// hash→id map last; postgres returns whichever row the planner picks).
	found, err := store.GetByHash(ctx, hash)
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
// EnumerateFileBlocks Tests ()
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
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
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
			if err := store.Put(ctx, b); err != nil {
				t.Fatalf("Put(%s) failed: %v", b.ID, err)
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
		if err := store.Put(ctx, b); err != nil {
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
		if err := store.Put(ctx, b); err != nil {
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
	if err := store.Put(ctx, legacy); err != nil {
		t.Fatalf("Put(zero) failed: %v", err)
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
	if err := store.Put(ctx, finalized); err != nil {
		t.Fatalf("Put(finalized) failed: %v", err)
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
// silently coerced to the zero ContentHash. mark fail-closed: the GC
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
	if err := store.Put(ctx, good); err != nil {
		t.Fatalf("Put(good) failed: %v", err)
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

// ============================================================================
// Tx Rollback Tests (review iteration 1)
// ============================================================================

// testTx_IncrementRefCount_RollsBack pins the contract:
// when IncrementRefCount is invoked through a metadata.Transaction and the
// wrapping WithTransaction returns an error, the per-row RefCount UPDATE
// MUST be rolled back atomically. This is the conformance-level pin for
// the same property that pkg/controlplane/runtime/shares/coordinator_test
// exercises at the coordinator layer.
//
// Memory backend documented limitation: the memory tx has no rollback
// buffer (see memory/transaction.go). The test detects this and adjusts
// expectations — memory leaves the increments stuck on rollback (best-
// effort txn). Postgres and Badger must roll back durably.
func testTx_IncrementRefCount_RollsBack(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Detect best-effort txn backends (memory) by name. The memory store
	// has WithTransaction that holds a global mutex but does not maintain
	// a rollback buffer; durable rollback is impossible by construction.
	storeType := fmt.Sprintf("%T", store)
	bestEffortTxn := storeType == "*memory.MemoryMetadataStore"

	// Seed three FileBlocks with RefCount=1 each.
	type seed struct {
		id   string
		hash blockstore.ContentHash
	}
	seeds := []seed{
		{id: "tx-rollback/0", hash: blockstore.ContentHash{0x10, 0x11, 0x12}},
		{id: "tx-rollback/1", hash: blockstore.ContentHash{0x20, 0x21, 0x22}},
		{id: "tx-rollback/2", hash: blockstore.ContentHash{0x30, 0x31, 0x32}},
	}
	for _, s := range seeds {
		fb := &blockstore.FileBlock{
			ID:         s.id,
			Hash:       s.hash,
			DataSize:   4096,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
			State:      blockstore.BlockStateRemote,
		}
		if err := store.Put(ctx, fb); err != nil {
			t.Fatalf("seed Put(%s): %v", s.id, err)
		}
	}

	// Bump every refcount through the txn, then return error to roll back.
	injected := errors.New("synthetic rollback trigger")
	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		for _, s := range seeds {
			if err := tx.IncrementRefCount(ctx, s.id); err != nil {
				return fmt.Errorf("tx.IncrementRefCount(%s): %w", s.id, err)
			}
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("WithTransaction returned %v, want injected %v", err, injected)
	}

	// Post-rollback: every refcount MUST equal its seeded value (1) on
	// durable backends. Memory tolerates the documented best-effort
	// semantics.
	for _, s := range seeds {
		got, err := store.GetByHash(ctx, s.hash)
		if err != nil {
			t.Fatalf("GetByHash(%x): %v", s.hash[:4], err)
		}
		if got == nil {
			t.Fatalf("GetByHash(%x) returned nil after rollback", s.hash[:4])
		}
		if bestEffortTxn {
			// Memory: increment stuck (no rollback). RefCount=2.
			if got.RefCount != 2 {
				t.Errorf("memory backend: RefCount(%s)=%d, want 2 (best-effort txn leaves the increment in place)", s.id, got.RefCount)
			}
			continue
		}
		// Durable backends MUST roll back.
		if got.RefCount != 1 {
			t.Errorf("RefCount(%s)=%d after rollback; want 1 (txn must undo IncrementRefCount)", s.id, got.RefCount)
		}
	}
}

// ============================================================================
// AddRef Tests
//
// AddRef is the LRU-hit refcount path for the in-memory hash dedup
// LRU. It atomically bumps RefCount on the FileBlock row indexed by
// hash; BlockState is left unchanged. The LRU never creates blocks —
// it only references already-stored ones — so AddRef returns
// ErrUnknownHash when the hash is not yet in the store (caller falls
// back to the full Put path).
// ============================================================================

// testAddRef_ExistingHash_BumpsRefCount: seed a single FileBlock with a
// known hash at RefCount=1 and BlockState=Remote, AddRef once, assert
// RefCount becomes 2 AND BlockState stays Remote (state
// preservation is the load-bearing contract).
func testAddRef_ExistingHash_BumpsRefCount(t *testing.T, factory StoreFactory) {
	t.Helper()
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("addref-existing-hash")
	casKey := "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String()
	seed := &blockstore.FileBlock{
		ID:            "file-addref/0",
		Hash:          hash,
		State:         blockstore.BlockStateRemote,
		LocalPath:     "/cache/addref0",
		BlockStoreKey: casKey,
		DataSize:      4096,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := store.Put(ctx, seed); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	blockRef := blockstore.BlockRef{Hash: hash, Offset: 0, Size: 4096}
	if err := store.AddRef(ctx, hash, "file-addref", blockRef); err != nil {
		t.Fatalf("AddRef(existing hash): %v", err)
	}

	got, err := store.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash post-AddRef: %v", err)
	}
	if got == nil {
		t.Fatal("GetByHash returned nil after AddRef on existing hash")
	}
	if got.RefCount != 2 {
		t.Errorf("RefCount = %d after AddRef on RefCount=1 seed; want 2", got.RefCount)
	}
	// BlockState UNCHANGED. AddRef MUST NOT fire any
	// Pending→Syncing→Remote transition; the hit path references an
	// existing block, it never creates one.
	if got.State != blockstore.BlockStateRemote {
		t.Errorf("BlockState = %v after AddRef; want Remote (D-27: state preserved across AddRef)", got.State)
	}
}

// testAddRef_MissingHash_ReturnsErrUnknownHash: AddRef on a hash that
// has never been Put must return blockstore.ErrUnknownHash (also
// re-exported as metadata.ErrUnknownHash) AND must NOT materialize a
// row for that hash. caller falls back to the full Put path on
// this sentinel.
func testAddRef_MissingHash_ReturnsErrUnknownHash(t *testing.T, factory StoreFactory) {
	t.Helper()
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("addref-missing-hash-never-put")
	blockRef := blockstore.BlockRef{Hash: hash, Offset: 0, Size: 1024}

	err := store.AddRef(ctx, hash, "file-missing", blockRef)
	if err == nil {
		t.Fatal("AddRef(missing hash) returned nil; want metadata.ErrUnknownHash")
	}
	if !errors.Is(err, metadata.ErrUnknownHash) {
		t.Errorf("AddRef(missing hash) returned %v; want errors.Is(...,metadata.ErrUnknownHash)", err)
	}

	// AddRef MUST NOT create a row on the missing-hash
	// path. GetByHash returns (nil, nil) for an absent hash by contract.
	got, err := store.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash(missing) errored: %v", err)
	}
	if got != nil {
		t.Errorf("GetByHash(missing) returned a row %+v after AddRef-ErrUnknownHash; want nil (no row created)", got)
	}
}

// testAddRef_Concurrent_With_DecrementRefCountCascade: seed a single
// FileBlock at RefCount=10 (high enough that 8 concurrent decrements
// cannot underflow), spawn 8 AddRef goroutines + 8 DecrementRefCount
// goroutines all targeting the same row, assert final RefCount is
// exactly 10 (TOCTOU-free serialization invariant from AddRef
// matches IncrementRefCount's atomicity contract). Mirrors the
// ConcurrentMonotone subtest shape from rollup_store_suite.go.
func testAddRef_Concurrent_With_DecrementRefCountCascade(t *testing.T, factory StoreFactory) {
	t.Helper()
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("addref-concurrent-cascade")
	casKey := "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String()
	seed := &blockstore.FileBlock{
		ID:            "file-addref-conc/0",
		Hash:          hash,
		State:         blockstore.BlockStateRemote,
		LocalPath:     "/cache/addref-conc0",
		BlockStoreKey: casKey,
		DataSize:      4096,
		RefCount:      10,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	if err := store.Put(ctx, seed); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	// Resolve the row ID (DecrementRefCount is id-keyed, not hash-keyed)
	// via the setup goroutine — backends that hash-collide will return
	// any one matching row, which is the row we just Put.
	resolved, err := store.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash for id resolution: %v", err)
	}
	if resolved == nil {
		t.Fatal("GetByHash returned nil for freshly-Put row")
	}
	rowID := resolved.ID

	const halfN = 8
	blockRef := blockstore.BlockRef{Hash: hash, Offset: 0, Size: 4096}

	var wg sync.WaitGroup
	wg.Add(2 * halfN)
	// 8 AddRef goroutines.
	for i := 0; i < halfN; i++ {
		go func() {
			defer wg.Done()
			if err := store.AddRef(ctx, hash, "file-addref-conc", blockRef); err != nil {
				t.Errorf("concurrent AddRef: %v", err)
			}
		}()
	}
	// 8 DecrementRefCount goroutines on the same id.
	for i := 0; i < halfN; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.DecrementRefCount(ctx, rowID); err != nil {
				t.Errorf("concurrent DecrementRefCount: %v", err)
			}
		}()
	}
	wg.Wait()

	// TOCTOU-free serialization invariant: 10 + 8 (AddRef) - 8
	// (Decrement) = 10. Any backend that races read+compare+write
	// outside the native concurrency primitive will land off-by-N.
	got, err := store.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetByHash post-cascade: %v", err)
	}
	if got == nil {
		t.Fatal("GetByHash post-cascade returned nil; row was orphan-deleted (D-04 violation — RefCount never reached 0)")
	}
	if got.RefCount != 10 {
		t.Errorf("RefCount post-cascade = %d; want 10 (8 AddRef + 8 Decrement on RefCount=10 seed; TOCTOU-free)", got.RefCount)
	}
}

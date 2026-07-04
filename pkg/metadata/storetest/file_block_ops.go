package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// fileChunkStoreLegacy captures the legacy GetFileChunk + ListFileChunks
// methods that removed from the public
// FileChunkStore interface but kept on each backend struct for engine-
// internal callers. The conformance suite type-asserts the factory's
// MetadataStore to this interface so the existing tests can still drive
// the methods without depending on a concrete backend type.
type fileChunkStoreLegacy interface {
	GetFileChunk(ctx context.Context, id string) (*block.FileChunk, error)
	ListFileChunks(ctx context.Context, payloadID string) ([]*block.FileChunk, error)
}

// asLegacy returns the legacy backend interface for a MetadataStore, or
// fails the test with a clear message when the backend does not provide
// the kept-but-not-on-interface methods.
func asLegacy(t *testing.T, store metadata.Store) fileChunkStoreLegacy {
	t.Helper()
	legacy, ok := store.(fileChunkStoreLegacy)
	if !ok {
		t.Skipf("backend %T does not implement fileChunkStoreLegacy (GetFileChunk/ListFileChunks); engine-internal methods unavailable on this backend", store)
	}
	return legacy
}

// runFileChunkOpsTests runs the FileChunkStore conformance suite.
// MetadataStore embeds FileChunkStore, so the StoreFactory works directly.
func runFileChunkOpsTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("ListFileChunks", func(t *testing.T) {
		testListFileChunks(t, factory)
	})

	t.Run("ListFileChunks_Ordering", func(t *testing.T) {
		testListFileChunksOrdering(t, factory)
	})

	t.Run("ListFileChunks_MixedStates", func(t *testing.T) {
		testListFileChunksMixedStates(t, factory)
	})

	t.Run("ListFileChunks_EmptyStore", func(t *testing.T) {
		testListFileChunksEmptyStore(t, factory)
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
	// second FileChunk with a fresh ID but the same ContentHash whenever two
	// file regions hash-match. Hash is NOT a uniqueness key at the contract
	// level (see FileChunkStore.Put godoc). Backends that enforce
	// hash uniqueness reject the second writer, leak the donor's RefCount,
	// and leave the FileChunk in Syncing forever. This regression test pins
	// the contract across all three backends.
	t.Run("Put_TwoIDsSameHash", func(t *testing.T) {
		testPut_TwoIDsSameHash(t, factory)
	})

	// the GC mark phase calls
	// EnumerateFileChunks on the metadata store to stream every live
	// ContentHash into the disk-backed live set. Every backend MUST yield
	// every block — under-yield risks the sweep deleting referenced data.
	t.Run("EnumerateFileChunks_Empty", func(t *testing.T) {
		testEnumerateFileChunks_Empty(t, factory)
	})

	t.Run("EnumerateFileChunks_SingleFile", func(t *testing.T) {
		testEnumerateFileChunks_SingleFile(t, factory)
	})

	t.Run("EnumerateFileChunks_LargeFanout", func(t *testing.T) {
		testEnumerateFileChunks_LargeFanout(t, factory)
	})

	t.Run("EnumerateFileChunks_FnErrorAborts", func(t *testing.T) {
		testEnumerateFileChunks_FnErrorAborts(t, factory)
	})

	t.Run("EnumerateFileChunks_ContextCancellation", func(t *testing.T) {
		testEnumerateFileChunks_ContextCancellation(t, factory)
	})

	t.Run("EnumerateFileChunks_ZeroHashEmitted", func(t *testing.T) {
		testEnumerateFileChunks_ZeroHashEmitted(t, factory)
	})

	// (mark fail-closed): backends that store the
	// ContentHash as text (Postgres) MUST surface a parse error when a
	// row's hash column holds a malformed value. Coercing the row to the
	// zero hash would let GC reap a still-live CAS object once the grace
	// TTL lapses. Backends that physically cannot represent a malformed
	// hash (memory/badger store [32]byte directly) skip via the optional
	// CorruptHashInjector capability.
	t.Run("EnumerateFileChunks_CorruptHashFailsClosed", func(t *testing.T) {
		testEnumerateFileChunks_CorruptHashFailsClosed(t, factory)
	})

	// `share warm` and block-store stats enumerate payloads from the
	// authoritative metadata (EnumeratePayloads) rather than the local block
	// store's ListFiles, which goes empty after rollup. Every backend MUST
	// yield exactly the distinct payloadIDs derived from the FileChunk row IDs
	// ({payloadID}/{blockIdx}), deduped and order-independent (#1374).
	t.Run("EnumeratePayloads", func(t *testing.T) {
		testEnumeratePayloads(t, factory)
	})

	t.Run("EnumeratePayloads_Empty", func(t *testing.T) {
		testEnumeratePayloads_Empty(t, factory)
	})

	// EnumerateLivePayloadIDs reads the namespace (inodes), so the difference
	// against EnumeratePayloads (which reads file_blocks) is exactly the
	// stranded-payload set the GC reconcile reaps (#1433).
	t.Run("EnumerateLivePayloadIDs", func(t *testing.T) {
		testEnumerateLivePayloadIDs(t, factory)
	})

	// The GC mark live set unions the CAS index with the per-file manifest, but
	// a manifest on an nlink=0 (unlinked) inode is dead and must NOT keep its
	// chunks live — else GC can never reclaim deleted files (#1433).
	t.Run("EnumerateFileChunks_UnlinkedFileExcludesManifest", func(t *testing.T) {
		testEnumerateFileChunks_UnlinkedFileExcludesManifest(t, factory)
	})
	t.Run("EnumerateFileChunks_HardLinkSurvivesOneRemoval", func(t *testing.T) {
		testEnumerateFileChunks_HardLinkSurvivesOneRemoval(t, factory)
	})
	t.Run("EnumerateLivePayloadIDs_ExcludesNlinkZero", func(t *testing.T) {
		testEnumerateLivePayloadIDs_ExcludesNlinkZero(t, factory)
	})

	// IncrementRefCount / DecrementRefCount called via a
	// metadata.Transaction MUST roll back when the wrapping WithTransaction
	// returns an error. All backends — memory, badger, postgres — honor the
	// unconditional all-or-nothing contract (interface.go: error → roll
	// back); memory does so via a snapshot/restore buffer.
	t.Run("Tx_IncrementRefCount_RollsBack", func(t *testing.T) {
		testTx_IncrementRefCount_RollsBack(t, factory)
	})

	// A tx.Put followed by tx.ListFileChunks / tx.EnumerateFileChunks in the
	// SAME WithTransaction MUST observe the uncommitted write (read-after-write
	// within the tx). Badger previously delegated these list methods to a fresh
	// db.View snapshot taken at call time, so the pending Put was invisible.
	t.Run("Tx_ListReadAfterWrite", func(t *testing.T) {
		testTx_ListReadAfterWrite(t, factory)
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

	// A FileChunk's content hash is valid the moment the chunk is hashed
	// at rollup time, long before it reaches the remote (BlockStateRemote).
	// The engine's CAS read path resolves (payloadID, offset) -> Hash via
	// ListFileChunks / GetFileChunk, so a Pending row MUST round-trip its
	// hash. A backend that only persists the hash for finalized rows leaves
	// the per-file read index hash-less; once the local cache is cold
	// (restart + eviction, or a snapshot restore that resets local state)
	// reads can no longer resolve the chunk and the file reads as zeros.
	t.Run("PutGet_PendingHashRoundTrips", func(t *testing.T) {
		testPutGet_PendingHashRoundTrips(t, factory)
	})

	// DecrementRefCountAndReap is the engine Delete/Truncate reclaim path
	// (#832): once a hash has no live references its FileChunk index row is
	// deleted in the same critical section as the decrement, so the hash
	// leaves EnumerateFileChunks and the GC sweep can collect the remote
	// chunk. Three cases pin the contract across all backends: reap-at-zero
	// (row + hash index gone), survive-when-still-referenced, and
	// tolerate-missing-row.
	t.Run("DecrementRefCountAndReap", func(t *testing.T) {
		testDecrementRefCountAndReap(t, factory)
	})
}

// testDecrementRefCountAndReap pins the DecrementRefCountAndReap contract:
//
//	(a) refcount 1 → reap: returns 0, GetByHash==nil, GetFileChunk→NotFound.
//	(b) refcount 2 → no reap: returns 1, row + hash index still present.
//	(c) non-existent id → returns 0, nil (a swept row is not a caller error).
func testDecrementRefCountAndReap(t *testing.T, factory StoreFactory) {
	t.Helper()

	// (a) refcount 1: the decrement drops to 0 → the row is reaped.
	t.Run("ReapsAtZero", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()
		legacy := asLegacy(t, store)

		hash := hashOfSeed("reap-at-zero")
		fb := &block.FileChunk{
			ID:            "file-reap/0",
			Hash:          hash,
			State:         block.BlockStateRemote,
			BlockStoreKey: "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String(),
			LocalPath:     "/cache/reap0",
			DataSize:      4096,
			RefCount:      1,
			LastAccess:    time.Now(),
			CreatedAt:     time.Now(),
		}
		if err := store.Put(ctx, fb); err != nil {
			t.Fatalf("Put: %v", err)
		}

		got, err := store.DecrementRefCountAndReap(ctx, fb.ID)
		if err != nil {
			t.Fatalf("DecrementRefCountAndReap: %v", err)
		}
		if got != 0 {
			t.Errorf("new count = %d, want 0 (reaped)", got)
		}

		// Hash index entry must be gone: GetByHash returns (nil, nil).
		byHash, err := store.GetByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetByHash post-reap: %v", err)
		}
		if byHash != nil {
			t.Errorf("GetByHash returned %+v after reap; want nil (hash index entry must be gone so the hash leaves EnumerateFileChunks)", byHash)
		}

		// The row itself must be gone: GetFileChunk → ErrFileChunkNotFound.
		if _, err := legacy.GetFileChunk(ctx, fb.ID); !errors.Is(err, metadata.ErrFileChunkNotFound) {
			t.Errorf("GetFileChunk post-reap err = %v; want ErrFileChunkNotFound (row reaped)", err)
		}
	})

	// (b) refcount 2: the decrement leaves 1 → the row survives.
	t.Run("SurvivesWhenStillReferenced", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()
		legacy := asLegacy(t, store)

		hash := hashOfSeed("reap-survives")
		fb := &block.FileChunk{
			ID:            "file-survive/0",
			Hash:          hash,
			State:         block.BlockStateRemote,
			BlockStoreKey: "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String(),
			LocalPath:     "/cache/survive0",
			DataSize:      4096,
			RefCount:      1,
			LastAccess:    time.Now(),
			CreatedAt:     time.Now(),
		}
		if err := store.Put(ctx, fb); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Second reference (refcount 1 → 2).
		if err := store.IncrementRefCount(ctx, fb.ID); err != nil {
			t.Fatalf("IncrementRefCount: %v", err)
		}

		got, err := store.DecrementRefCountAndReap(ctx, fb.ID)
		if err != nil {
			t.Fatalf("DecrementRefCountAndReap: %v", err)
		}
		if got != 1 {
			t.Errorf("new count = %d, want 1 (still referenced — no reap)", got)
		}

		// Row + hash index must survive.
		if _, err := legacy.GetFileChunk(ctx, fb.ID); err != nil {
			t.Errorf("GetFileChunk after non-reap decrement: %v; want row still present", err)
		}
		byHash, err := store.GetByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetByHash after non-reap decrement: %v", err)
		}
		if byHash == nil {
			t.Error("GetByHash returned nil after non-reap decrement; want the still-referenced row")
		}
	})

	// (c) non-existent id: a swept row is not a caller error.
	t.Run("ToleratesMissingRow", func(t *testing.T) {
		store := factory(t)
		ctx := t.Context()

		got, err := store.DecrementRefCountAndReap(ctx, "file-never-existed/0")
		if err != nil {
			t.Fatalf("DecrementRefCountAndReap(missing) err = %v; want nil (tolerated)", err)
		}
		if got != 0 {
			t.Errorf("DecrementRefCountAndReap(missing) count = %d, want 0", got)
		}
	})
}

// ============================================================================
// ListFileChunks Tests
//
// ListFileChunks is no longer on the public
// FileChunkStore interface but is retained as a backend method for engine-
// internal callers. Tests use the legacyFileChunkStore type assertion to
// reach the method on each backend; backends that don't implement it
// (none today) skip cleanly.
// ============================================================================

func testListFileChunks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks for 2 different files
	blocks := []*block.FileChunk{
		{ID: "file-A/0", State: block.BlockStatePending, LocalPath: "/cache/a0", DataSize: 100, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-A/1", State: block.BlockStatePending, LocalPath: "/cache/a1", DataSize: 200, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-A/2", State: block.BlockStateRemote, LocalPath: "/cache/a2", BlockStoreKey: "s3://a2", DataSize: 300, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-B/0", State: block.BlockStatePending, LocalPath: "/cache/b0", DataSize: 400, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-B/1", State: block.BlockStatePending, LocalPath: "/cache/b1", DataSize: 500, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
	}
	for _, b := range blocks {
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	// Query file-A
	resultA, err := asLegacy(t, store).ListFileChunks(ctx, "file-A")
	if err != nil {
		t.Fatalf("ListFileChunks(file-A) error: %v", err)
	}
	if len(resultA) != 3 {
		t.Fatalf("ListFileChunks(file-A) returned %d blocks, want 3", len(resultA))
	}

	// Verify ordering by block index
	for i, b := range resultA {
		expectedID := fmt.Sprintf("file-A/%d", i)
		if b.ID != expectedID {
			t.Errorf("ListFileChunks(file-A)[%d].ID = %s, want %s", i, b.ID, expectedID)
		}
	}

	// Query file-B
	resultB, err := asLegacy(t, store).ListFileChunks(ctx, "file-B")
	if err != nil {
		t.Fatalf("ListFileChunks(file-B) error: %v", err)
	}
	if len(resultB) != 2 {
		t.Fatalf("ListFileChunks(file-B) returned %d blocks, want 2", len(resultB))
	}

	// Query nonexistent
	resultN, err := asLegacy(t, store).ListFileChunks(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListFileChunks(nonexistent) error: %v", err)
	}
	if len(resultN) != 0 {
		t.Errorf("ListFileChunks(nonexistent) returned %d blocks, want 0", len(resultN))
	}

	// Verify data integrity
	if resultA[0].DataSize != 100 {
		t.Errorf("ListFileChunks(file-A)[0].DataSize = %d, want 100", resultA[0].DataSize)
	}
	if resultA[2].State != block.BlockStateRemote {
		t.Errorf("ListFileChunks(file-A)[2].State = %v, want Remote", resultA[2].State)
	}
}

func testListFileChunksOrdering(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks for one file with out-of-order indices
	indices := []int{0, 5, 10, 2, 7}
	for _, idx := range indices {
		b := &block.FileChunk{
			ID: fmt.Sprintf("file-sort/%d", idx), State: block.BlockStatePending,
			LocalPath: fmt.Sprintf("/cache/s%d", idx), DataSize: uint32(idx * 100),
			RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now(),
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	result, err := asLegacy(t, store).ListFileChunks(ctx, "file-sort")
	if err != nil {
		t.Fatalf("ListFileChunks(file-sort) error: %v", err)
	}
	if len(result) != 5 {
		t.Fatalf("ListFileChunks(file-sort) returned %d blocks, want 5", len(result))
	}

	// Expected order: 0, 2, 5, 7, 10
	expectedOrder := []int{0, 2, 5, 7, 10}
	for i, expected := range expectedOrder {
		expectedID := fmt.Sprintf("file-sort/%d", expected)
		if result[i].ID != expectedID {
			t.Errorf("ListFileChunks(file-sort)[%d].ID = %s, want %s", i, result[i].ID, expectedID)
		}
	}
}

func testListFileChunksMixedStates(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks in all 4 states for same file
	states := []block.BlockState{
		block.BlockStatePending,
		block.BlockStatePending,
		block.BlockStateSyncing,
		block.BlockStateRemote,
	}
	for i, state := range states {
		b := &block.FileChunk{
			ID: fmt.Sprintf("file-mix/%d", i), State: state,
			LocalPath: fmt.Sprintf("/cache/m%d", i), DataSize: uint32((i + 1) * 100),
			RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now(),
		}
		if state == block.BlockStateRemote {
			b.BlockStoreKey = "s3://mix"
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s) failed: %v", b.ID, err)
		}
	}

	// ListFileChunks should return ALL blocks regardless of state
	result, err := asLegacy(t, store).ListFileChunks(ctx, "file-mix")
	if err != nil {
		t.Fatalf("ListFileChunks(file-mix) error: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("ListFileChunks(file-mix) returned %d blocks, want 4", len(result))
	}

	// Verify each state is present
	statesSeen := make(map[block.BlockState]bool)
	for _, b := range result {
		statesSeen[b.State] = true
	}
	for _, state := range states {
		if !statesSeen[state] {
			t.Errorf("ListFileChunks(file-mix) missing state %v", state)
		}
	}
}

func testListFileChunksEmptyStore(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	result, err := asLegacy(t, store).ListFileChunks(ctx, "any")
	if err != nil {
		t.Fatalf("ListFileChunks(empty) error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("ListFileChunks(empty) returned %d blocks, want 0", len(result))
	}
}

// ============================================================================
// EnumeratePayloads Tests
//
// EnumeratePayloads streams every DISTINCT payloadID that has at least one
// FileChunk row. It is the rollup-durable enumeration surface used by
// `share warm` and block-store stats: unlike the local block store's
// ListFiles, the FileChunk metadata rows survive after an append log rolls up,
// so this still reports payloads whose local payload tracking has gone empty
// (#1374).
// ============================================================================

// testEnumeratePayloads seeds blocks for several distinct payloads (multiple
// blocks each, including a multi-digit block index to exercise numeric suffix
// handling, AND payloadIDs that themselves CONTAIN slashes — the production
// shape, BuildPayloadID(shareName, filePath), where the trailing "/{offset}"
// component is the chunk offset and the payloadID is everything before the
// LAST slash) and asserts EnumeratePayloads yields exactly those distinct
// payloadIDs, deduped and order-independent. A backend that split on the FIRST
// slash would truncate "myshare/dir/sub/file.bin" to "myshare" and fail here.
func testEnumeratePayloads(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// payload -> number of blocks. The slash-containing payloadIDs are the
	// normal case for any file in a subdirectory.
	want := map[string]int{
		"payload-alpha":            3,
		"payload-beta":             1,
		"payload-gamma":            12, // exercises a multi-digit block index suffix
		"myshare/dir/sub/file.bin": 2,  // payloadID itself contains slashes
		"export/docs/report.pdf":   1,  // single-block subdirectory file
	}
	for payloadID, n := range want {
		for i := 0; i < n; i++ {
			b := &block.FileChunk{
				ID:         fmt.Sprintf("%s/%d", payloadID, i),
				State:      block.BlockStatePending,
				LocalPath:  fmt.Sprintf("/cache/%s-%d", payloadID, i),
				DataSize:   128,
				RefCount:   1,
				LastAccess: time.Now(),
				CreatedAt:  time.Now(),
			}
			if err := store.Put(ctx, b); err != nil {
				t.Fatalf("Put(%s) failed: %v", b.ID, err)
			}
		}
	}

	got := make(map[string]int)
	err := store.EnumeratePayloads(ctx, func(payloadID string) error {
		got[payloadID]++
		return nil
	})
	if err != nil {
		t.Fatalf("EnumeratePayloads error: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("EnumeratePayloads yielded %d distinct payloads %v, want %d %v",
			len(got), got, len(want), want)
	}
	for payloadID := range want {
		switch got[payloadID] {
		case 0:
			t.Errorf("EnumeratePayloads missing payload %q", payloadID)
		case 1:
			// exactly once — correct
		default:
			t.Errorf("EnumeratePayloads yielded payload %q %d times, want exactly 1 (must dedupe)",
				payloadID, got[payloadID])
		}
	}
}

// testEnumeratePayloads_Empty asserts EnumeratePayloads invokes fn zero times
// on an empty store.
func testEnumeratePayloads_Empty(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	count := 0
	err := store.EnumeratePayloads(ctx, func(_ string) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("EnumeratePayloads(empty) error: %v", err)
	}
	if count != 0 {
		t.Errorf("EnumeratePayloads(empty): fn invoked %d times, want 0", count)
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
	in := &block.FileChunk{
		ID:                "file-sync-attempt/0",
		State:             block.BlockStateSyncing,
		LocalPath:         "/cache/sa0",
		DataSize:          128,
		RefCount:          1,
		LastAccess:        time.Now(),
		CreatedAt:         time.Now(),
		LastSyncAttemptAt: stamp,
	}

	if err := store.Put(ctx, in); err != nil {
		t.Fatalf("PutFileChunk failed: %v", err)
	}

	out, err := asLegacy(t, store).GetFileChunk(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetFileChunk failed: %v", err)
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

	in := &block.FileChunk{
		ID:         "file-sync-zero/0",
		State:      block.BlockStatePending,
		LocalPath:  "/cache/sz0",
		DataSize:   64,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
		// LastSyncAttemptAt deliberately left zero.
	}

	if err := store.Put(ctx, in); err != nil {
		t.Fatalf("PutFileChunk failed: %v", err)
	}

	out, err := asLegacy(t, store).GetFileChunk(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetFileChunk failed: %v", err)
	}

	if !out.LastSyncAttemptAt.IsZero() {
		t.Errorf("LastSyncAttemptAt should be zero on round-trip, got %v",
			out.LastSyncAttemptAt)
	}
}

// testPut_TwoIDsSameHash asserts that two distinct FileChunk IDs
// sharing the same ContentHash both round-trip through PutFileChunk without
// error. the dedup short-circuit (engine.uploadOne) emits
// such pairs whenever two file regions hash-match (e.g. all-zero blocks
// across distinct VM image files). A backend that rejects the second
// writer breaks the dedup path, leaves the FileChunk stuck in Syncing,
// and leaks the donor block's RefCount.
//
// The contract permits FindFileChunkByHash to return either of the
// colliding rows (memory + badger overwrite the hash→id map; postgres
// returns one of the two rows non-deterministically). The assertion
// scope is therefore: both PutFileChunk calls return nil AND
// FindFileChunkByHash returns one of the two IDs (no error, no nil).
func testPut_TwoIDsSameHash(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("shared-content")
	keyA := "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String()
	keyB := keyA // CAS keys are identical for the same hash; that's the point

	a := &block.FileChunk{
		ID:            "file-A/0",
		Hash:          hash,
		State:         block.BlockStateRemote,
		LocalPath:     "/cache/A0",
		BlockStoreKey: keyA,
		DataSize:      4096,
		RefCount:      1,
		LastAccess:    time.Now(),
		CreatedAt:     time.Now(),
	}
	b := &block.FileChunk{
		ID:            "file-B/0",
		Hash:          hash, // SAME content hash, different ID
		State:         block.BlockStateRemote,
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
	gotA, err := asLegacy(t, store).GetFileChunk(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetFileChunk(A) failed: %v", err)
	}
	if gotA.Hash != hash {
		t.Errorf("GetFileChunk(A).Hash = %x, want %x", gotA.Hash[:8], hash[:8])
	}
	gotB, err := asLegacy(t, store).GetFileChunk(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetFileChunk(B) failed: %v", err)
	}
	if gotB.Hash != hash {
		t.Errorf("GetFileChunk(B).Hash = %x, want %x", gotB.Hash[:8], hash[:8])
	}

	// FindFileChunkByHash must return one of the two — exact identity is
	// implementation-defined (memory + badger return whichever wrote the
	// hash→id map last; postgres returns whichever row the planner picks).
	found, err := store.GetByHash(ctx, hash)
	if err != nil {
		t.Fatalf("FindFileChunkByHash failed: %v", err)
	}
	if found == nil {
		t.Fatal("FindFileChunkByHash returned nil; expected one of the two colliding rows")
	}
	if found.ID != a.ID && found.ID != b.ID {
		t.Errorf("FindFileChunkByHash returned ID %q; want one of [%q, %q]",
			found.ID, a.ID, b.ID)
	}
}

// testPutGet_PendingHashRoundTrips pins the contract that a FileChunk's
// content hash survives a Put/Get round-trip regardless of block state.
// Both per-file read accessors (ListFileChunks and GetFileChunk) must
// surface the hash for a Pending row, because the engine CAS read path
// resolves chunks through that index, not just through finalized rows.
func testPutGet_PendingHashRoundTrips(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("pending-hash-roundtrip")
	fb := &block.FileChunk{
		ID:         "file-pending/0",
		Hash:       hash,
		State:      block.BlockStatePending,
		DataSize:   4096,
		RefCount:   1,
		LastAccess: time.Now(),
		CreatedAt:  time.Now(),
	}
	if err := store.Put(ctx, fb); err != nil {
		t.Fatalf("Put(pending) failed: %v", err)
	}

	legacy := asLegacy(t, store)

	got, err := legacy.GetFileChunk(ctx, fb.ID)
	if err != nil {
		t.Fatalf("GetFileChunk failed: %v", err)
	}
	if got.Hash != hash {
		t.Errorf("GetFileChunk: Pending row Hash = %x, want %x "+
			"(the content hash must persist for Pending blocks — the CAS "+
			"read path resolves chunks via this hash even before the block "+
			"reaches the remote)", got.Hash[:8], hash[:8])
	}

	rows, err := legacy.ListFileChunks(ctx, "file-pending")
	if err != nil {
		t.Fatalf("ListFileChunks failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListFileChunks returned %d rows, want 1", len(rows))
	}
	if rows[0].Hash != hash {
		t.Errorf("ListFileChunks: Pending row Hash = %x, want %x",
			rows[0].Hash[:8], hash[:8])
	}
}

// ============================================================================
// EnumerateFileChunks Tests ()
// ============================================================================

// hashOf returns a deterministic non-zero ContentHash from a seed string.
// Used by enumerate tests to seed unique hashes per block.
func hashOfSeed(seed string) block.ContentHash {
	var h block.ContentHash
	src := []byte(seed)
	// Spread bytes into the 32-byte hash deterministically.
	for i := 0; i < block.HashSize; i++ {
		h[i] = src[i%len(src)] ^ byte(i)
	}
	return h
}

// testEnumerateFileChunks_Empty: invokes fn 0 times on an empty store.
func testEnumerateFileChunks_Empty(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	count := 0
	err := store.EnumerateFileChunks(ctx, func(_ block.ContentHash) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileChunks(empty) error: %v", err)
	}
	if count != 0 {
		t.Errorf("EnumerateFileChunks(empty): fn invoked %d times, want 0", count)
	}
}

// testEnumerateFileChunks_SingleFile: fn invoked exactly N times for a file
// with N blocks; the yielded hash set equals the stored hash set.
func testEnumerateFileChunks_SingleFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const n = 5
	want := make(map[block.ContentHash]bool, n)
	for i := 0; i < n; i++ {
		h := hashOfSeed(fmt.Sprintf("single-%d", i))
		want[h] = true
		b := &block.FileChunk{
			ID:            fmt.Sprintf("file-single/%d", i),
			Hash:          h,
			State:         block.BlockStateRemote,
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

	got := make(map[block.ContentHash]bool, n)
	err := store.EnumerateFileChunks(ctx, func(h block.ContentHash) error {
		got[h] = true
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileChunks error: %v", err)
	}
	if len(got) != n {
		t.Fatalf("EnumerateFileChunks: got %d distinct hashes, want %d", len(got), n)
	}
	for h := range want {
		if !got[h] {
			t.Errorf("EnumerateFileChunks: missing hash %x", h[:8])
		}
	}
}

// testEnumerateFileChunks_LargeFanout: 50 files * 20 blocks = 1000 blocks; fn
// invoked exactly 1000 times; no duplicates, no omissions; iteration completes
// within 5s on the memory backend (sanity bound).
func testEnumerateFileChunks_LargeFanout(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const files = 50
	const perFile = 20
	want := make(map[block.ContentHash]int, files*perFile)
	for f := 0; f < files; f++ {
		for i := 0; i < perFile; i++ {
			h := hashOfSeed(fmt.Sprintf("fanout-%d-%d", f, i))
			want[h]++
			b := &block.FileChunk{
				ID:            fmt.Sprintf("file-fan-%d/%d", f, i),
				Hash:          h,
				State:         block.BlockStateRemote,
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
	seen := make(map[block.ContentHash]int, files*perFile)
	err := store.EnumerateFileChunks(ctx, func(h block.ContentHash) error {
		got++
		seen[h]++
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileChunks error: %v", err)
	}
	if time.Now().After(deadline) {
		t.Errorf("EnumerateFileChunks took longer than 5s sanity bound")
	}
	if got != files*perFile {
		t.Errorf("EnumerateFileChunks: fn invoked %d times, want %d", got, files*perFile)
	}
	if len(seen) != len(want) {
		t.Errorf("EnumerateFileChunks: %d distinct hashes seen, want %d", len(seen), len(want))
	}
	for h, want := range want {
		if seen[h] != want {
			t.Errorf("EnumerateFileChunks: hash %x seen %d times, want %d", h[:8], seen[h], want)
		}
	}
}

// testEnumerateFileChunks_FnErrorAborts: fn returns a sentinel error on the
// 7th invocation; EnumerateFileChunks returns that error (possibly wrapped).
// fn is invoked at most a small batch beyond the sentinel — tolerant of
// PrefetchSize batching but never iterates the full set.
func testEnumerateFileChunks_FnErrorAborts(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const n = 50
	for i := 0; i < n; i++ {
		h := hashOfSeed(fmt.Sprintf("fn-err-%d", i))
		b := &block.FileChunk{
			ID:         fmt.Sprintf("file-fnerr/%d", i),
			Hash:       h,
			State:      block.BlockStateRemote,
			LocalPath:  fmt.Sprintf("/cache/fnerr-%d", i),
			DataSize:   64,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("PutFileChunk failed: %v", err)
		}
	}

	sentinel := errors.New("sentinel error from fn")
	calls := 0
	err := store.EnumerateFileChunks(ctx, func(_ block.ContentHash) error {
		calls++
		if calls == 7 {
			return sentinel
		}
		return nil
	})
	if err == nil {
		t.Fatalf("EnumerateFileChunks returned nil, want sentinel error")
	}
	if !errors.Is(err, sentinel) {
		// Some impls may wrap; accept exact equality OR errors.Is.
		if err.Error() != sentinel.Error() && err != sentinel {
			t.Errorf("EnumerateFileChunks returned %v, want sentinel %v", err, sentinel)
		}
	}
	if calls < 7 {
		t.Errorf("EnumerateFileChunks: fn invoked %d times, want >= 7", calls)
	}
	if calls >= n {
		t.Errorf("EnumerateFileChunks: fn invoked %d times — iteration did not abort", calls)
	}
}

// testEnumerateFileChunks_ContextCancellation: cancel mid-iteration; method
// returns ctx.Err (possibly wrapped) and stops invoking fn.
func testEnumerateFileChunks_ContextCancellation(t *testing.T, factory StoreFactory) {
	store := factory(t)
	parent := t.Context()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	const n = 50
	for i := 0; i < n; i++ {
		h := hashOfSeed(fmt.Sprintf("ctx-cancel-%d", i))
		b := &block.FileChunk{
			ID:         fmt.Sprintf("file-ctx/%d", i),
			Hash:       h,
			State:      block.BlockStateRemote,
			LocalPath:  fmt.Sprintf("/cache/ctx-%d", i),
			DataSize:   64,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("PutFileChunk failed: %v", err)
		}
	}

	calls := 0
	err := store.EnumerateFileChunks(ctx, func(_ block.ContentHash) error {
		calls++
		if calls == 3 {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Fatalf("EnumerateFileChunks: expected non-nil error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("EnumerateFileChunks: error %v does not wrap context.Canceled", err)
	}
	if calls >= n {
		t.Errorf("EnumerateFileChunks: fn invoked %d times — cancellation ignored", calls)
	}
}

// testEnumerateFileChunks_ZeroHashEmitted: blocks with zero hash (legacy rows)
// are still enumerated. The GC mark phase decides whether to skip them.
func testEnumerateFileChunks_ZeroHashEmitted(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Seed one zero-hash legacy block + one finalized block.
	legacy := &block.FileChunk{
		ID:         "file-zero/0",
		State:      block.BlockStatePending,
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
	finalized := &block.FileChunk{
		ID:            "file-zero/1",
		Hash:          hashOfSeed("non-zero"),
		State:         block.BlockStateRemote,
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
	err := store.EnumerateFileChunks(ctx, func(h block.ContentHash) error {
		if h.IsZero() {
			zeroSeen = true
		} else {
			finalizedSeen = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("EnumerateFileChunks error: %v", err)
	}
	if !zeroSeen {
		t.Errorf("EnumerateFileChunks did not emit zero-hash block (legacy row missed)")
	}
	if !finalizedSeen {
		t.Errorf("EnumerateFileChunks did not emit non-zero hash block")
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

// testEnumerateFileChunks_CorruptHashFailsClosed asserts that a malformed CAS
// hash on disk surfaces as an error from EnumerateFileChunks rather than being
// silently coerced to the zero ContentHash. mark fail-closed: the GC
// mark phase MUST abort on enumeration error so the sweep cannot reap a live
// CAS object whose live-set hash was lost in transit.
func testEnumerateFileChunks_CorruptHashFailsClosed(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	injector, ok := store.(CorruptHashInjector)
	if !ok {
		t.Skip("backend does not implement CorruptHashInjector — type-safe row format cannot represent a malformed hash")
	}

	// Seed one well-formed Remote block so enumeration has something to walk
	// past before reaching the corrupt row.
	good := &block.FileChunk{
		ID:            "file-corrupt/0",
		Hash:          hashOfSeed("good"),
		State:         block.BlockStateRemote,
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
	err := store.EnumerateFileChunks(ctx, func(_ block.ContentHash) error {
		calls++
		return nil
	})
	if err == nil {
		t.Fatalf("EnumerateFileChunks returned nil; expected parse error from corrupt-hash row (INV-04 fail-closed)")
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
// All backends are held to the same all-or-nothing contract: memory rolls
// back via a snapshot/restore buffer in WithTransaction, badger and postgres
// via native transaction rollback.
func testTx_IncrementRefCount_RollsBack(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Seed three FileChunks with RefCount=1 each.
	type seed struct {
		id   string
		hash block.ContentHash
	}
	seeds := []seed{
		{id: "tx-rollback/0", hash: block.ContentHash{0x10, 0x11, 0x12}},
		{id: "tx-rollback/1", hash: block.ContentHash{0x20, 0x21, 0x22}},
		{id: "tx-rollback/2", hash: block.ContentHash{0x30, 0x31, 0x32}},
	}
	for _, s := range seeds {
		fb := &block.FileChunk{
			ID:         s.id,
			Hash:       s.hash,
			DataSize:   4096,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
			State:      block.BlockStateRemote,
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

	// Post-rollback: every refcount MUST equal its seeded value (1) on all
	// backends — the transaction must undo the increment.
	for _, s := range seeds {
		got, err := store.GetByHash(ctx, s.hash)
		if err != nil {
			t.Fatalf("GetByHash(%x): %v", s.hash[:4], err)
		}
		if got == nil {
			t.Fatalf("GetByHash(%x) returned nil after rollback", s.hash[:4])
		}
		if got.RefCount != 1 {
			t.Errorf("RefCount(%s)=%d after rollback; want 1 (txn must undo IncrementRefCount)", s.id, got.RefCount)
		}
	}
}

// testTx_ListReadAfterWrite pins read-after-write within a transaction: a
// FileChunk Put through the tx MUST be visible to ListFileChunks and
// EnumerateFileChunks issued later in the same WithTransaction. Backends that
// open a fresh snapshot per list call (the original badger behavior) miss the
// pending write and fail here.
func testTx_ListReadAfterWrite(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const payloadID = "raw-tx-file"
	hash := block.ContentHash{0xa1, 0xb2, 0xc3, 0xd4}

	err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		// ListFileChunks / EnumerateFileChunks live on each backend's tx
		// struct but not on the narrowed metadata.Transaction interface; the
		// engine-internal callers reach them via the concrete type. Assert to
		// a local interface so the conformance test can drive them.
		lister, ok := tx.(interface {
			ListFileChunks(ctx context.Context, payloadID string) ([]*block.FileChunk, error)
			EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error
		})
		if !ok {
			return errTxListUnsupported
		}

		fb := &block.FileChunk{
			ID:            payloadID + "/0",
			Hash:          hash,
			State:         block.BlockStateRemote,
			LocalPath:     "/cache/raw",
			BlockStoreKey: "cas/a1/b2/" + hash.String(),
			DataSize:      4096,
			RefCount:      1,
			LastAccess:    time.Now(),
			CreatedAt:     time.Now(),
		}
		if putErr := tx.Put(ctx, fb); putErr != nil {
			return fmt.Errorf("tx.Put: %w", putErr)
		}

		// ListFileChunks must see the just-Put block.
		listed, listErr := lister.ListFileChunks(ctx, payloadID)
		if listErr != nil {
			return fmt.Errorf("tx.ListFileChunks: %w", listErr)
		}
		if len(listed) != 1 {
			return fmt.Errorf("tx.ListFileChunks returned %d blocks; want 1 (uncommitted write invisible)", len(listed))
		}

		// EnumerateFileChunks must also see it.
		var seen bool
		enumErr := lister.EnumerateFileChunks(ctx, func(h block.ContentHash) error {
			if h == hash {
				seen = true
			}
			return nil
		})
		if enumErr != nil {
			return fmt.Errorf("tx.EnumerateFileChunks: %w", enumErr)
		}
		if !seen {
			return fmt.Errorf("tx.EnumerateFileChunks did not yield the uncommitted block hash")
		}
		return nil
	})
	if errors.Is(err, errTxListUnsupported) {
		t.Skip("backend tx does not expose ListFileChunks/EnumerateFileChunks")
	}
	if err != nil {
		t.Fatalf("read-after-write within tx failed: %v", err)
	}
}

// errTxListUnsupported signals testTx_ListReadAfterWrite that the backend's
// transaction does not expose the engine-internal ListFileChunks /
// EnumerateFileChunks methods, so the scenario is skipped rather than failed.
var errTxListUnsupported = errors.New("backend tx does not expose ListFileChunks/EnumerateFileChunks")

// ============================================================================
// AddRef Tests
//
// AddRef is the LRU-hit refcount path for the in-memory hash dedup
// LRU. It atomically bumps RefCount on the FileChunk row indexed by
// hash; BlockState is left unchanged. The LRU never creates blocks —
// it only references already-stored ones — so AddRef returns
// ErrUnknownHash when the hash is not yet in the store (caller falls
// back to the full Put path).
// ============================================================================

// testAddRef_ExistingHash_BumpsRefCount: seed a single FileChunk with a
// known hash at RefCount=1 and BlockState=Remote, AddRef once, assert
// RefCount becomes 2 AND BlockState stays Remote (state
// preservation is the load-bearing contract).
func testAddRef_ExistingHash_BumpsRefCount(t *testing.T, factory StoreFactory) {
	t.Helper()
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("addref-existing-hash")
	casKey := "cas/" + hash.String()[0:2] + "/" + hash.String()[2:4] + "/" + hash.String()
	seed := &block.FileChunk{
		ID:            "file-addref/0",
		Hash:          hash,
		State:         block.BlockStateRemote,
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

	blockRef := block.ChunkRef{Hash: hash, Offset: 0, Size: 4096}
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
	if got.State != block.BlockStateRemote {
		t.Errorf("BlockState = %v after AddRef; want Remote (D-27: state preserved across AddRef)", got.State)
	}
}

// testAddRef_MissingHash_ReturnsErrUnknownHash: AddRef on a hash that
// has never been Put must return block.ErrUnknownHash (also
// re-exported as metadata.ErrUnknownHash) AND must NOT materialize a
// row for that hash. caller falls back to the full Put path on
// this sentinel.
func testAddRef_MissingHash_ReturnsErrUnknownHash(t *testing.T, factory StoreFactory) {
	t.Helper()
	store := factory(t)
	ctx := t.Context()

	hash := hashOfSeed("addref-missing-hash-never-put")
	blockRef := block.ChunkRef{Hash: hash, Offset: 0, Size: 1024}

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
// FileChunk at RefCount=10 (high enough that 8 concurrent decrements
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
	seed := &block.FileChunk{
		ID:            "file-addref-conc/0",
		Hash:          hash,
		State:         block.BlockStateRemote,
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
	blockRef := block.ChunkRef{Hash: hash, Offset: 0, Size: 4096}

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

// testEnumerateLivePayloadIDs verifies the namespace-derived live set used by
// the GC reconcile (#1433). A "stranded" payload — file_blocks rows whose
// owning inode is gone (the historical leak) — must appear in EnumeratePayloads
// but NOT in EnumerateLivePayloadIDs, so the reconcile can reap it. A live
// file's payload must appear in both.
func testEnumerateLivePayloadIDs(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	root := createTestShare(t, store, "myshare")

	// A live file carrying a PayloadID, plus its file_blocks rows.
	const livePID = "myshare/live.bin"
	h := createTestFile(t, store, "myshare", root, "live.bin", 0o644)
	f, err := store.GetFile(ctx, h)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	f.PayloadID = metadata.PayloadID(livePID)
	if err := store.PutFile(ctx, f); err != nil {
		t.Fatalf("PutFile (set payload): %v", err)
	}
	seedStrandedBlocks(t, ctx, store, livePID, 2)

	// A STRANDED payload: file_blocks rows exist with no inode referencing them
	// (owning file deleted without reaping — the pre-fix leak).
	const strandedPID = "myshare/ghost.bin"
	seedStrandedBlocks(t, ctx, store, strandedPID, 3)

	// EnumerateLivePayloadIDs reads the namespace: live present, stranded absent.
	live := make(map[string]int)
	if err := store.EnumerateLivePayloadIDs(ctx, func(p string) error {
		live[p]++
		return nil
	}); err != nil {
		t.Fatalf("EnumerateLivePayloadIDs: %v", err)
	}
	if live[livePID] != 1 {
		t.Errorf("live payload %q yielded %d times, want exactly 1", livePID, live[livePID])
	}
	if live[strandedPID] != 0 {
		t.Errorf("stranded payload %q reported live — reconcile would never reap it", strandedPID)
	}

	// EnumeratePayloads reads file_blocks: BOTH present (stranded-inclusive).
	all := make(map[string]int)
	if err := store.EnumeratePayloads(ctx, func(p string) error {
		all[p]++
		return nil
	}); err != nil {
		t.Fatalf("EnumeratePayloads: %v", err)
	}
	if all[livePID] == 0 {
		t.Errorf("EnumeratePayloads missing live payload %q", livePID)
	}
	if all[strandedPID] == 0 {
		t.Errorf("EnumeratePayloads missing stranded payload %q (test setup invalid)", strandedPID)
	}
}

// seedStrandedBlocks puts n file_blocks rows for payloadID without requiring an
// inode — the exact shape of a leaked/stranded manifest.
func seedStrandedBlocks(t *testing.T, ctx context.Context, store metadata.Store, payloadID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		b := &block.FileChunk{
			ID:         fmt.Sprintf("%s/%d", payloadID, i),
			State:      block.BlockStatePending,
			LocalPath:  fmt.Sprintf("/cache/%s-%d", payloadID, i),
			DataSize:   128,
			RefCount:   1,
			LastAccess: time.Now(),
			CreatedAt:  time.Now(),
		}
		if err := store.Put(ctx, b); err != nil {
			t.Fatalf("Put(%s): %v", b.ID, err)
		}
	}
}

// testEnumerateFileChunks_UnlinkedFileExcludesManifest proves that once a file
// is unlinked (nlink=0) its manifest blocks leave the GC mark live set, so the
// sweep can reclaim the orphaned chunks. This is the core of the #1433 fix: the
// manifest (file_block_refs / f: File.Blocks) lingers on the nlink=0 inode, but
// the file is dead and must not pin its chunks live.
func testEnumerateFileChunks_UnlinkedFileExcludesManifest(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	shareName := "nlink0-exclude"
	root := createTestShare(t, store, shareName)
	h := createTestFile(t, store, shareName, root, "dead.bin", 0o644)

	want := hashOfSeed("nlink0-manifest-hash")
	f, err := store.GetFile(ctx, h)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	f.Blocks = []block.ChunkRef{{Hash: want, Offset: 0, Size: 4 << 20}}
	f.Size = 4 << 20
	if err := store.PutFile(ctx, f); err != nil {
		t.Fatalf("PutFile (linked): %v", err)
	}
	if !enumerateContains(t, store, want) {
		t.Fatalf("baseline: hash %s absent from live set while nlink>0", want)
	}

	// Unlink: drop the dir edge and set nlink=0 (what RemoveFile does on the last
	// link). The embedded File.Nlink is not the authoritative link count (#1166):
	// SetLinkCount is the only API that updates the source of truth (SQL
	// inodes.nlink, badger l: key, memory linkCounts), so it is the faithful
	// simulation — mutating File.Nlink + PutFile would not move it.
	if err := store.DeleteChild(ctx, root, "dead.bin"); err != nil {
		t.Fatalf("DeleteChild: %v", err)
	}
	if err := store.SetLinkCount(ctx, h, 0); err != nil {
		t.Fatalf("SetLinkCount(0): %v", err)
	}
	if enumerateContains(t, store, want) {
		t.Errorf("hash %s still in GC live set after unlink (nlink=0); its manifest must be excluded so the sweep can reclaim it (#1433)", want)
	}
}

// testEnumerateFileChunks_HardLinkSurvivesOneRemoval guards against an
// over-eager exclusion: a file with two hard links that loses one (nlink 2→1)
// is still alive, so its blocks MUST remain in the live set.
func testEnumerateFileChunks_HardLinkSurvivesOneRemoval(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	shareName := "hardlink-survive"
	root := createTestShare(t, store, shareName)
	h := createTestFile(t, store, shareName, root, "shared.bin", 0o644)

	want := hashOfSeed("hardlink-hash")
	f, err := store.GetFile(ctx, h)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	f.Blocks = []block.ChunkRef{{Hash: want, Offset: 0, Size: 4 << 20}}
	f.Size = 4 << 20
	if err := store.PutFile(ctx, f); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	// Two hard links.
	if err := store.SetLinkCount(ctx, h, 2); err != nil {
		t.Fatalf("SetLinkCount(2): %v", err)
	}
	if !enumerateContains(t, store, want) {
		t.Fatalf("baseline: hash %s absent from live set while nlink=2", want)
	}

	// Remove one link: nlink 2→1, still alive.
	if err := store.SetLinkCount(ctx, h, 1); err != nil {
		t.Fatalf("SetLinkCount(1): %v", err)
	}
	if !enumerateContains(t, store, want) {
		t.Errorf("hash %s dropped from live set at nlink=1; a still-linked file's blocks must stay live", want)
	}
}

// testEnumerateLivePayloadIDs_ExcludesNlinkZero proves the reconcile live-set
// query also excludes nlink=0 inodes, so their payload is classified stranded
// (and its pre-fix file_blocks rows get reaped on upgrade).
func testEnumerateLivePayloadIDs_ExcludesNlinkZero(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	shareName := "livepayload-nlink0"
	root := createTestShare(t, store, shareName)
	h := createTestFile(t, store, shareName, root, "payload.bin", 0o644)

	const payloadID = "pl-nlink0-dead"
	f, err := store.GetFile(ctx, h)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	f.PayloadID = payloadID
	if err := store.PutFile(ctx, f); err != nil {
		t.Fatalf("PutFile (linked): %v", err)
	}
	if !livePayloadContains(t, store, payloadID) {
		t.Fatalf("baseline: payload %s absent from live payloads while nlink>0", payloadID)
	}

	// Unlink → nlink=0; the payload is now dead and must not be reported live.
	if err := store.DeleteChild(ctx, root, "payload.bin"); err != nil {
		t.Fatalf("DeleteChild: %v", err)
	}
	if err := store.SetLinkCount(ctx, h, 0); err != nil {
		t.Fatalf("SetLinkCount(0): %v", err)
	}
	if livePayloadContains(t, store, payloadID) {
		t.Errorf("payload %s still reported live after unlink (nlink=0); reconcile must treat it as stranded (#1433)", payloadID)
	}
}

// enumerateContains reports whether hash h appears in the store's GC mark live
// set (EnumerateFileChunks).
func enumerateContains(t *testing.T, store metadata.Store, h block.ContentHash) bool {
	t.Helper()
	found := false
	if err := store.EnumerateFileChunks(t.Context(), func(got block.ContentHash) error {
		if got == h {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("EnumerateFileChunks: %v", err)
	}
	return found
}

// livePayloadContains reports whether payloadID appears in EnumerateLivePayloadIDs.
func livePayloadContains(t *testing.T, store metadata.Store, payloadID string) bool {
	t.Helper()
	found := false
	if err := store.EnumerateLivePayloadIDs(t.Context(), func(got string) error {
		if got == payloadID {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatalf("EnumerateLivePayloadIDs: %v", err)
	}
	return found
}

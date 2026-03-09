package storetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runFileBlockOpsTests runs conformance tests for FileBlockStore query methods:
// ListLocalBlocks, ListRemoteBlocks, and ListFileBlocks.
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
}

// ============================================================================
// ListLocalBlocks Tests
// ============================================================================

func testListLocalBlocks(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 5 blocks with different states
	blocks := []*metadata.FileBlock{
		{ID: "file-a/0", State: metadata.BlockStateLocal, CachePath: "/cache/a0", DataSize: 100, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-a/1", State: metadata.BlockStateLocal, CachePath: "/cache/a1", DataSize: 200, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-b/0", State: metadata.BlockStateDirty, CachePath: "/cache/b0", DataSize: 300, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-c/0", State: metadata.BlockStateRemote, CachePath: "/cache/c0", BlockStoreKey: "s3://c0", DataSize: 400, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "file-d/0", State: metadata.BlockStateSyncing, CachePath: "/cache/d0", DataSize: 500, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)},
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

	if len(result) != 2 {
		t.Fatalf("ListLocalBlocks() returned %d blocks, want 2", len(result))
	}

	// Both should be Local state
	for _, b := range result {
		if b.State != metadata.BlockStateLocal {
			t.Errorf("ListLocalBlocks() returned block %s with state %v, want Local", b.ID, b.State)
		}
	}
}

func testListLocalBlocksLimit(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create 3 Local blocks
	for i := 0; i < 3; i++ {
		b := &metadata.FileBlock{
			ID: fmt.Sprintf("file-x/%d", i), State: metadata.BlockStateLocal,
			CachePath: fmt.Sprintf("/cache/x%d", i), DataSize: 100, RefCount: 1,
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
	old := &metadata.FileBlock{
		ID: "file-old/0", State: metadata.BlockStateLocal, CachePath: "/cache/old",
		DataSize: 100, RefCount: 1,
		LastAccess: time.Now().Add(-2 * time.Hour), CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	recent := &metadata.FileBlock{
		ID: "file-recent/0", State: metadata.BlockStateLocal, CachePath: "/cache/recent",
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
	if result != nil && len(result) != 0 {
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
	blocks := []*metadata.FileBlock{
		{ID: "file-a/0", State: metadata.BlockStateRemote, CachePath: "/cache/a0", BlockStoreKey: "s3://a0", DataSize: 100, RefCount: 1, LastAccess: time.Now().Add(-2 * time.Hour), CreatedAt: time.Now()},
		{ID: "file-a/1", State: metadata.BlockStateRemote, CachePath: "/cache/a1", BlockStoreKey: "s3://a1", DataSize: 200, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now()},
		{ID: "file-b/0", State: metadata.BlockStateRemote, CachePath: "", BlockStoreKey: "s3://b0", DataSize: 300, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},          // Not cached
		{ID: "file-c/0", State: metadata.BlockStateLocal, CachePath: "/cache/c0", DataSize: 400, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)}, // Local, not Remote
		{ID: "file-d/0", State: metadata.BlockStateDirty, CachePath: "/cache/d0", DataSize: 500, RefCount: 1, LastAccess: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour)}, // Dirty
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
		b := &metadata.FileBlock{
			ID: fmt.Sprintf("file-r/%d", i), State: metadata.BlockStateRemote,
			CachePath: fmt.Sprintf("/cache/r%d", i), BlockStoreKey: fmt.Sprintf("s3://r%d", i),
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
	if result != nil && len(result) != 0 {
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
	blocks := []*metadata.FileBlock{
		{ID: "file-A/0", State: metadata.BlockStateLocal, CachePath: "/cache/a0", DataSize: 100, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-A/1", State: metadata.BlockStateLocal, CachePath: "/cache/a1", DataSize: 200, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-A/2", State: metadata.BlockStateRemote, CachePath: "/cache/a2", BlockStoreKey: "s3://a2", DataSize: 300, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-B/0", State: metadata.BlockStateLocal, CachePath: "/cache/b0", DataSize: 400, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
		{ID: "file-B/1", State: metadata.BlockStateDirty, CachePath: "/cache/b1", DataSize: 500, RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now()},
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
	if resultA[2].State != metadata.BlockStateRemote {
		t.Errorf("ListFileBlocks(file-A)[2].State = %v, want Remote", resultA[2].State)
	}
}

func testListFileBlocksOrdering(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// Create blocks for one file with out-of-order indices
	indices := []int{0, 5, 10, 2, 7}
	for _, idx := range indices {
		b := &metadata.FileBlock{
			ID: fmt.Sprintf("file-sort/%d", idx), State: metadata.BlockStateLocal,
			CachePath: fmt.Sprintf("/cache/s%d", idx), DataSize: uint32(idx * 100),
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
	states := []metadata.BlockState{
		metadata.BlockStateDirty,
		metadata.BlockStateLocal,
		metadata.BlockStateSyncing,
		metadata.BlockStateRemote,
	}
	for i, state := range states {
		b := &metadata.FileBlock{
			ID: fmt.Sprintf("file-mix/%d", i), State: state,
			CachePath: fmt.Sprintf("/cache/m%d", i), DataSize: uint32((i + 1) * 100),
			RefCount: 1, LastAccess: time.Now(), CreatedAt: time.Now(),
		}
		if state == metadata.BlockStateRemote {
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
	statesSeen := make(map[metadata.BlockState]bool)
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

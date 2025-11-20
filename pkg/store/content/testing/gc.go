package testing

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunGCTests executes all GarbageCollectableStore operation tests.
func (suite *StoreTestSuite) RunGCTests(t *testing.T) {
	t.Run("ListAllContent_Empty", suite.testListAllContentEmpty)
	t.Run("ListAllContent_Single", suite.testListAllContentSingle)
	t.Run("ListAllContent_Multiple", suite.testListAllContentMultiple)
	t.Run("DeleteBatch_Empty", suite.testDeleteBatchEmpty)
	t.Run("DeleteBatch_Single", suite.testDeleteBatchSingle)
	t.Run("DeleteBatch_Multiple", suite.testDeleteBatchMultiple)
	t.Run("DeleteBatch_PartialFailure", suite.testDeleteBatchPartialFailure)
	t.Run("GarbageCollection_FullCycle", suite.testGarbageCollectionFullCycle)
}

// ============================================================================
// ListAllContent Tests
// ============================================================================

func (suite *StoreTestSuite) testListAllContentEmpty(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	// List should be empty initially
	ids, err := gc.ListAllContent(testContext())
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func (suite *StoreTestSuite) testListAllContentSingle(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("list-single")
	mustWriteContent(t, writable, id, []byte("data"))

	// List should contain one item
	ids, err := gc.ListAllContent(testContext())
	require.NoError(t, err)
	assert.Len(t, ids, 1)
	assert.Contains(t, ids, id)
}

func (suite *StoreTestSuite) testListAllContentMultiple(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	// Create multiple content items
	ids := []metadata.ContentID{
		generateTestID("list-1"),
		generateTestID("list-2"),
		generateTestID("list-3"),
	}

	for _, id := range ids {
		mustWriteContent(t, writable, id, []byte("data"))
	}

	// List should contain all items
	allIds, err := gc.ListAllContent(testContext())
	require.NoError(t, err)
	assert.Len(t, allIds, len(ids))

	for _, id := range ids {
		assert.Contains(t, allIds, id)
	}
}

// ============================================================================
// DeleteBatch Tests
// ============================================================================

func (suite *StoreTestSuite) testDeleteBatchEmpty(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	// Delete empty batch should succeed
	failures, err := gc.DeleteBatch(testContext(), []metadata.ContentID{})
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func (suite *StoreTestSuite) testDeleteBatchSingle(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("delete-batch-single")
	mustWriteContent(t, writable, id, []byte("data"))

	// Delete batch with one item
	failures, err := gc.DeleteBatch(testContext(), []metadata.ContentID{id})
	require.NoError(t, err)
	assert.Empty(t, failures)

	// Verify deleted
	assertContentExists(t, store, id, false)
}

func (suite *StoreTestSuite) testDeleteBatchMultiple(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	// Create multiple content items
	ids := []metadata.ContentID{
		generateTestID("delete-batch-1"),
		generateTestID("delete-batch-2"),
		generateTestID("delete-batch-3"),
	}

	for _, id := range ids {
		mustWriteContent(t, writable, id, []byte("data"))
	}

	// Delete batch
	failures, err := gc.DeleteBatch(testContext(), ids)
	require.NoError(t, err)
	assert.Empty(t, failures)

	// Verify all deleted
	for _, id := range ids {
		assertContentExists(t, store, id, false)
	}
}

func (suite *StoreTestSuite) testDeleteBatchPartialFailure(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	// Create some content items
	existingIDs := []metadata.ContentID{
		generateTestID("delete-batch-exists-1"),
		generateTestID("delete-batch-exists-2"),
	}

	for _, id := range existingIDs {
		mustWriteContent(t, writable, id, []byte("data"))
	}

	// Mix of existing and non-existing IDs
	allIDs := []metadata.ContentID{
		existingIDs[0],
		generateTestID("delete-batch-nonexist-1"),
		existingIDs[1],
		generateTestID("delete-batch-nonexist-2"),
	}

	// Delete batch (non-existent items should not cause failure due to idempotency)
	failures, err := gc.DeleteBatch(testContext(), allIDs)
	require.NoError(t, err)
	assert.Empty(t, failures) // All should succeed (delete is idempotent)

	// Verify all are gone
	for _, id := range allIDs {
		assertContentExists(t, store, id, false)
	}
}

// ============================================================================
// Garbage Collection Integration Test
// ============================================================================

func (suite *StoreTestSuite) testGarbageCollectionFullCycle(t *testing.T) {
	store := suite.NewStore()
	gc, ok := store.(content.GarbageCollectableStore)
	if !ok {
		t.Skip("Store does not implement GarbageCollectableStore")
	}

	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	// Create some content items
	ids := []metadata.ContentID{
		generateTestID("gc-1"),
		generateTestID("gc-2"),
		generateTestID("gc-3"),
		generateTestID("gc-4"),
		generateTestID("gc-5"),
	}

	for _, id := range ids {
		mustWriteContent(t, writable, id, []byte("data"))
	}

	// Simulate: IDs 1, 2, 3 are referenced by metadata
	referencedIDs := map[metadata.ContentID]bool{
		ids[0]: true,
		ids[1]: true,
		ids[2]: true,
	}

	// List all content
	allIDs, err := gc.ListAllContent(testContext())
	require.NoError(t, err)
	assert.Len(t, allIDs, 5)

	// Find unreferenced IDs
	unreferencedIDs := []metadata.ContentID{}
	for _, id := range allIDs {
		if !referencedIDs[id] {
			unreferencedIDs = append(unreferencedIDs, id)
		}
	}

	assert.Len(t, unreferencedIDs, 2)

	// Delete unreferenced content
	failures, err := gc.DeleteBatch(testContext(), unreferencedIDs)
	require.NoError(t, err)
	assert.Empty(t, failures)

	// Verify referenced content still exists
	for id := range referencedIDs {
		assertContentExists(t, store, id, true)
	}

	// Verify unreferenced content is deleted
	for _, id := range unreferencedIDs {
		assertContentExists(t, store, id, false)
	}

	// List should now contain only referenced IDs
	remainingIDs, err := gc.ListAllContent(testContext())
	require.NoError(t, err)
	assert.Len(t, remainingIDs, 3)
}

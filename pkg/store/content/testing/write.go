package testing

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunWriteTests executes all WritableContentStore operation tests.
//
// Note: WriteAt tests have been removed because not all backends support it.
// Object storage backends (S3, etc.) cannot efficiently implement random-access writes.
// For backends that do support WriteAt (filesystem), test it separately in backend-specific tests.
func (suite *StoreTestSuite) RunWriteTests(t *testing.T) {
	t.Run("WriteContent_Basic", suite.testWriteContentBasic)
	t.Run("WriteContent_Overwrite", suite.testWriteContentOverwrite)
	t.Run("Truncate_Shrink", suite.testTruncateShrink)
	t.Run("Truncate_Grow", suite.testTruncateGrow)
	t.Run("Truncate_NotFound", suite.testTruncateNotFound)
	t.Run("Delete_Success", suite.testDeleteSuccess)
	t.Run("Delete_Idempotent", suite.testDeleteIdempotent)
}

// ============================================================================
// WriteContent Tests
// ============================================================================

func (suite *StoreTestSuite) testWriteContentBasic(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("write-basic")
	testData := []byte("Hello, World!")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Verify content
	assertContentEquals(t, store, id, testData)
	assertContentSize(t, store, id, uint64(len(testData)))
}

func (suite *StoreTestSuite) testWriteContentOverwrite(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("write-overwrite")
	oldData := []byte("Old data")
	newData := []byte("New data that is longer")

	// Write initial content
	mustWriteContent(t, writable, id, oldData)
	assertContentEquals(t, store, id, oldData)

	// Overwrite with new content
	mustWriteContent(t, writable, id, newData)
	assertContentEquals(t, store, id, newData)
	assertContentSize(t, store, id, uint64(len(newData)))
}

// ============================================================================
// Truncate Tests
// ============================================================================

func (suite *StoreTestSuite) testTruncateShrink(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("truncate-shrink")
	testData := []byte("Hello, World!")

	// Write content
	mustWriteContent(t, writable, id, testData)
	assertContentSize(t, store, id, uint64(len(testData)))

	// Truncate to 5 bytes
	mustTruncate(t, writable, id, 5)

	// Verify
	assertContentSize(t, store, id, 5)
	assertContentEquals(t, store, id, []byte("Hello"))
}

func (suite *StoreTestSuite) testTruncateGrow(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("truncate-grow")
	testData := []byte("Hello")

	// Write content
	mustWriteContent(t, writable, id, testData)
	assertContentSize(t, store, id, uint64(len(testData)))

	// Truncate to 10 bytes (extend with zeros)
	mustTruncate(t, writable, id, 10)

	// Verify
	assertContentSize(t, store, id, 10)
	data := mustReadContent(t, store, id)
	assert.Equal(t, []byte("Hello"), data[0:5])
	assert.Equal(t, []byte{0, 0, 0, 0, 0}, data[5:10])
}

func (suite *StoreTestSuite) testTruncateNotFound(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("truncate-notfound")

	// Truncate non-existent content should error
	err := writable.Truncate(testContext(), id, 100)
	AssertErrorIs(t, content.ErrContentNotFound, err)
}

// ============================================================================
// Delete Tests
// ============================================================================

func (suite *StoreTestSuite) testDeleteSuccess(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("delete-success")
	testData := []byte("To be deleted")

	// Write content
	mustWriteContent(t, writable, id, testData)
	assertContentExists(t, store, id, true)

	// Delete content
	mustDelete(t, writable, id)

	// Verify deleted
	assertContentExists(t, store, id, false)

	// Read should fail
	_, err := store.ReadContent(testContext(), id)
	AssertErrorIs(t, content.ErrContentNotFound, err)
}

func (suite *StoreTestSuite) testDeleteIdempotent(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("delete-idempotent")

	// Delete non-existent content should succeed (idempotent)
	err := writable.Delete(testContext(), id)
	require.NoError(t, err)

	// Delete again should still succeed
	err = writable.Delete(testContext(), id)
	require.NoError(t, err)
}

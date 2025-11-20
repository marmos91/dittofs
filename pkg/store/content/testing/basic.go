package testing

import (
	"io"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunBasicTests executes all basic ContentStore operation tests.
func (suite *StoreTestSuite) RunBasicTests(t *testing.T) {
	t.Run("ReadContent_NotFound", suite.testReadContentNotFound)
	t.Run("ReadContent_Success", suite.testReadContentSuccess)
	t.Run("GetContentSize_NotFound", suite.testGetContentSizeNotFound)
	t.Run("GetContentSize_Success", suite.testGetContentSizeSuccess)
	t.Run("ContentExists_NotFound", suite.testContentExistsNotFound)
	t.Run("ContentExists_Success", suite.testContentExistsSuccess)
	t.Run("ReadContent_EmptyContent", suite.testReadContentEmpty)
	t.Run("ReadContent_LargeContent", suite.testReadContentLarge)
}

// ============================================================================
// ReadContent Tests
// ============================================================================

func (suite *StoreTestSuite) testReadContentNotFound(t *testing.T) {
	store := suite.NewStore()

	id := generateTestID("nonexistent")
	_, err := store.ReadContent(testContext(), id)

	AssertErrorIs(t, content.ErrContentNotFound, err)
}

func (suite *StoreTestSuite) testReadContentSuccess(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("read-success")
	testData := []byte("Hello, World!")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Read content
	reader, err := store.ReadContent(testContext(), id)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	// Verify data
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, testData, data)
}

func (suite *StoreTestSuite) testReadContentEmpty(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("empty")
	testData := []byte{}

	// Write empty content
	mustWriteContent(t, writable, id, testData)

	// Read content
	data := mustReadContent(t, store, id)
	assert.Equal(t, 0, len(data))
}

func (suite *StoreTestSuite) testReadContentLarge(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("large")
	// 10MB test data
	testData := generateTestData(10 * 1024 * 1024)

	// Write large content
	mustWriteContent(t, writable, id, testData)

	// Read content
	data := mustReadContent(t, store, id)
	assert.Equal(t, testData, data)
}

// ============================================================================
// GetContentSize Tests
// ============================================================================

func (suite *StoreTestSuite) testGetContentSizeNotFound(t *testing.T) {
	store := suite.NewStore()

	id := generateTestID("nonexistent-size")
	_, err := store.GetContentSize(testContext(), id)

	AssertErrorIs(t, content.ErrContentNotFound, err)
}

func (suite *StoreTestSuite) testGetContentSizeSuccess(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("size-success")
	testData := []byte("Test data for size")

	// Write content
	mustWriteContent(t, writable, id, testData)

	// Get size
	size := mustGetSize(t, store, id)
	assert.Equal(t, uint64(len(testData)), size)
}

// ============================================================================
// ContentExists Tests
// ============================================================================

func (suite *StoreTestSuite) testContentExistsNotFound(t *testing.T) {
	store := suite.NewStore()

	id := generateTestID("nonexistent-exists")
	assertContentExists(t, store, id, false)
}

func (suite *StoreTestSuite) testContentExistsSuccess(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	id := generateTestID("exists-success")
	testData := []byte("Exists test")

	// Before write
	assertContentExists(t, store, id, false)

	// Write content
	mustWriteContent(t, writable, id, testData)

	// After write
	assertContentExists(t, store, id, true)
}

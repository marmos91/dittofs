package testing

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunStatsTests executes all storage statistics tests.
func (suite *StoreTestSuite) RunStatsTests(t *testing.T) {
	t.Run("GetStorageStats_Empty", suite.testGetStorageStatsEmpty)
	t.Run("GetStorageStats_WithContent", suite.testGetStorageStatsWithContent)
	t.Run("GetStorageStats_AfterDelete", suite.testGetStorageStatsAfterDelete)
}

// ============================================================================
// GetStorageStats Tests
// ============================================================================

func (suite *StoreTestSuite) testGetStorageStatsEmpty(t *testing.T) {
	store := suite.NewStore()

	stats, err := store.GetStorageStats(testContext())
	require.NoError(t, err)
	assert.NotNil(t, stats)

	// Empty store should have zero used size and count
	assert.Equal(t, uint64(0), stats.UsedSize)
	assert.Equal(t, uint64(0), stats.ContentCount)
	assert.Equal(t, uint64(0), stats.AverageSize)
}

func (suite *StoreTestSuite) testGetStorageStatsWithContent(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	// Create some content items of different sizes
	id1 := generateTestID("stats-1")
	id2 := generateTestID("stats-2")
	id3 := generateTestID("stats-3")

	data1 := generateTestData(100) // 100 bytes
	data2 := generateTestData(200) // 200 bytes
	data3 := generateTestData(300) // 300 bytes

	mustWriteContent(t, writable, id1, data1)
	mustWriteContent(t, writable, id2, data2)
	mustWriteContent(t, writable, id3, data3)

	// Get stats
	stats, err := store.GetStorageStats(testContext())
	require.NoError(t, err)
	assert.NotNil(t, stats)

	// Verify stats
	expectedUsedSize := uint64(100 + 200 + 300)
	expectedCount := uint64(3)
	expectedAverage := expectedUsedSize / expectedCount

	assert.Equal(t, expectedUsedSize, stats.UsedSize)
	assert.Equal(t, expectedCount, stats.ContentCount)
	assert.Equal(t, expectedAverage, stats.AverageSize)
}

func (suite *StoreTestSuite) testGetStorageStatsAfterDelete(t *testing.T) {
	store := suite.NewStore()
	writable, ok := store.(content.WritableContentStore)
	if !ok {
		t.Skip("Store does not implement WritableContentStore")
	}

	// Create content
	id1 := generateTestID("stats-delete-1")
	id2 := generateTestID("stats-delete-2")
	id3 := generateTestID("stats-delete-3")

	data1 := generateTestData(100) // 100 bytes
	data2 := generateTestData(200) // 200 bytes
	data3 := generateTestData(300) // 300 bytes

	mustWriteContent(t, writable, id1, data1)
	mustWriteContent(t, writable, id2, data2)
	mustWriteContent(t, writable, id3, data3)

	// Get initial stats
	stats1, err := store.GetStorageStats(testContext())
	require.NoError(t, err)
	assert.Equal(t, uint64(600), stats1.UsedSize)
	assert.Equal(t, uint64(3), stats1.ContentCount)

	// Delete one item
	mustDelete(t, writable, id2)

	// Get stats after delete
	stats2, err := store.GetStorageStats(testContext())
	require.NoError(t, err)
	assert.Equal(t, uint64(400), stats2.UsedSize)
	assert.Equal(t, uint64(2), stats2.ContentCount)
	assert.Equal(t, uint64(200), stats2.AverageSize)

	// Delete all
	mustDelete(t, writable, id1)
	mustDelete(t, writable, id3)

	// Get final stats
	stats3, err := store.GetStorageStats(testContext())
	require.NoError(t, err)
	assert.Equal(t, uint64(0), stats3.UsedSize)
	assert.Equal(t, uint64(0), stats3.ContentCount)
	assert.Equal(t, uint64(0), stats3.AverageSize)
}

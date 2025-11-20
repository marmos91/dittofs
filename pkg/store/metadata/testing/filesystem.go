//go:build obsolete

package testing

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (suite *StoreTestSuite) RunFilesystemTests(test *testing.T) {
	test.Run("GetFilesystemCapabilities_Success", suite.TestGetFilesystemCapabilities_Success)
	test.Run("GetFilesystemCapabilities_Consistency", suite.TestGetFilesystemCapabilities_Consistency)
	test.Run("GetFilesystemCapabilities_Values", suite.TestGetFilesystemCapabilities_Values)
	test.Run("GetFilesystemStatistics_Success", suite.TestGetFilesystemStatistics_Success)
	test.Run("GetFilesystemStatistics_Consistency", suite.TestGetFilesystemStatistics_Consistency)
	test.Run("GetFilesystemStatistics_FileCountTracking", suite.TestGetFilesystemStatistics_FileCountTracking)
}

// TestGetFilesystemCapabilities_Success verifies that filesystem capabilities can be retrieved.
func (suite *StoreTestSuite) TestGetFilesystemCapabilities_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create a share to get a valid file handle
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Act
	caps, err := store.GetFilesystemCapabilities(ctx, rootHandle)

	// Assert
	require.NoError(test, err, "Getting filesystem capabilities should succeed")
	assert.NotNil(test, caps, "Capabilities should not be nil")
}

// TestGetFilesystemCapabilities_Consistency verifies that capabilities remain consistent.
func (suite *StoreTestSuite) TestGetFilesystemCapabilities_Consistency(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create a share
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Get capabilities multiple times
	caps1, err := store.GetFilesystemCapabilities(ctx, rootHandle)
	require.NoError(test, err)

	caps2, err := store.GetFilesystemCapabilities(ctx, rootHandle)
	require.NoError(test, err)

	caps3, err := store.GetFilesystemCapabilities(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - All calls should return identical values
	assert.Equal(test, caps1.MaxReadSize, caps2.MaxReadSize, "MaxReadSize should be consistent")
	assert.Equal(test, caps1.MaxReadSize, caps3.MaxReadSize, "MaxReadSize should be consistent")

	assert.Equal(test, caps1.MaxWriteSize, caps2.MaxWriteSize, "MaxWriteSize should be consistent")
	assert.Equal(test, caps1.MaxWriteSize, caps3.MaxWriteSize, "MaxWriteSize should be consistent")

	assert.Equal(test, caps1.MaxFileSize, caps2.MaxFileSize, "MaxFileSize should be consistent")
	assert.Equal(test, caps1.MaxFileSize, caps3.MaxFileSize, "MaxFileSize should be consistent")

	assert.Equal(test, caps1.SupportsHardLinks, caps2.SupportsHardLinks, "SupportsHardLinks should be consistent")
	assert.Equal(test, caps1.SupportsHardLinks, caps3.SupportsHardLinks, "SupportsHardLinks should be consistent")

	assert.Equal(test, caps1.SupportsSymlinks, caps2.SupportsSymlinks, "SupportsSymlinks should be consistent")
	assert.Equal(test, caps1.SupportsSymlinks, caps3.SupportsSymlinks, "SupportsSymlinks should be consistent")
}

// TestGetFilesystemCapabilities_Values verifies that capabilities have reasonable values.
func (suite *StoreTestSuite) TestGetFilesystemCapabilities_Values(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create a share
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Act
	caps, err := store.GetFilesystemCapabilities(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - Check for reasonable values (not testing exact values as those are implementation-specific)
	assert.Greater(test, caps.MaxReadSize, uint32(0), "MaxReadSize should be positive")
	assert.Greater(test, caps.PreferredReadSize, uint32(0), "PreferredReadSize should be positive")
	assert.LessOrEqual(test, caps.PreferredReadSize, caps.MaxReadSize,
		"PreferredReadSize should not exceed MaxReadSize")

	assert.Greater(test, caps.MaxWriteSize, uint32(0), "MaxWriteSize should be positive")
	assert.Greater(test, caps.PreferredWriteSize, uint32(0), "PreferredWriteSize should be positive")
	assert.LessOrEqual(test, caps.PreferredWriteSize, caps.MaxWriteSize,
		"PreferredWriteSize should not exceed MaxWriteSize")

	assert.Greater(test, caps.MaxFileSize, uint64(0), "MaxFileSize should be positive")
	assert.Greater(test, caps.MaxFilenameLen, uint32(0), "MaxFilenameLen should be positive")
	assert.Greater(test, caps.MaxPathLen, uint32(0), "MaxPathLen should be positive")

	// Case sensitivity checks
	if !caps.CaseSensitive {
		// If case-insensitive, it should still preserve case in most modern systems
		assert.True(test, caps.CasePreserving,
			"Case-insensitive filesystems should typically preserve case")
	}

	assert.Greater(test, int64(caps.TimestampResolution), int64(0),
		"TimestampResolution should be positive")
}

// TestGetFilesystemStatistics_Success verifies that filesystem statistics can be retrieved.
func (suite *StoreTestSuite) TestGetFilesystemStatistics_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create a share to get a valid file handle
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Act
	stats, err := store.GetFilesystemStatistics(ctx, rootHandle)

	// Assert
	require.NoError(test, err, "Getting filesystem statistics should succeed")
	assert.NotNil(test, stats, "Statistics should not be nil")

	// Check basic invariants
	assert.LessOrEqual(test, stats.UsedBytes, stats.TotalBytes,
		"Used bytes should not exceed total bytes")
	assert.LessOrEqual(test, stats.UsedFiles, stats.TotalFiles,
		"Used files should not exceed total files")

	// At least total space and files should be defined
	assert.Greater(test, stats.TotalBytes, uint64(0), "TotalBytes should be positive")
	assert.Greater(test, stats.TotalFiles, uint64(0), "TotalFiles should be positive")
}

// TestGetFilesystemStatistics_Consistency verifies that statistics structure is consistent.
func (suite *StoreTestSuite) TestGetFilesystemStatistics_Consistency(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create a share
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Get statistics
	stats, err := store.GetFilesystemStatistics(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - Check mathematical consistency
	// Note: AvailableBytes might be less than (TotalBytes - UsedBytes) due to reserved space
	assert.LessOrEqual(test, stats.AvailableBytes, stats.TotalBytes-stats.UsedBytes,
		"Available bytes should not exceed (total - used)")

	// Same for files
	assert.LessOrEqual(test, stats.AvailableFiles, stats.TotalFiles-stats.UsedFiles,
		"Available files should not exceed (total - used)")

	// ValidFor should be non-negative
	assert.GreaterOrEqual(test, stats.ValidFor, time.Duration(0),
		"ValidFor should be non-negative")
}

// TestGetFilesystemStatistics_FileCountTracking verifies that file counts are tracked.
func (suite *StoreTestSuite) TestGetFilesystemStatistics_FileCountTracking(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create a share with writable root directory
	shareName := "/export/data"
	rootAttr := DefaultRootDirAttr()
	rootAttr.Mode = 0755
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, rootAttr)
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Get initial statistics
	statsBefore, err := store.GetFilesystemStatistics(ctx, rootHandle)
	require.NoError(test, err)

	// Create auth context with root privileges
	authCtx := metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Create a file
	fileName := "testfile.txt"
	fileAttr := &metadata.FileAttr{
		Type:  metadata.FileTypeRegular,
		Size:  4096,
		Mode:  0644,
		UID:   0,
		GID:   0,
		Mtime: time.Now(),
		Atime: time.Now(),
		Ctime: time.Now(),
	}

	fileHandle, err := store.Create(&authCtx, rootHandle, fileName, fileAttr)
	require.NoError(test, err)

	// Get statistics after file creation
	statsAfterCreate, err := store.GetFilesystemStatistics(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - File count should increase
	assert.Greater(test, statsAfterCreate.UsedFiles, statsBefore.UsedFiles,
		"UsedFiles should increase after creating a file")
	assert.Less(test, statsAfterCreate.AvailableFiles, statsBefore.AvailableFiles,
		"AvailableFiles should decrease after creating a file")

	// Delete the file
	_, err = store.RemoveFile(&authCtx, rootHandle, fileName)
	require.NoError(test, err)

	statsAfterDelete, err := store.GetFilesystemStatistics(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - File count should decrease back
	assert.Less(test, statsAfterDelete.UsedFiles, statsAfterCreate.UsedFiles,
		"UsedFiles should decrease after deletion")
	assert.Greater(test, statsAfterDelete.AvailableFiles, statsAfterCreate.AvailableFiles,
		"AvailableFiles should increase after deletion")

	// Verify the file is actually gone
	_, err = store.GetFile(ctx, fileHandle)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Deleted file should not exist")
}

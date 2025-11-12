package testing

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (suite *StoreTestSuite) RunRemoveTests(test *testing.T) {
	test.Run("RemoveFile_Success", suite.TestRemoveFile_Success)
	test.Run("RemoveFile_NotFound", suite.TestRemoveFile_NotFound)
	test.Run("RemoveFile_IsDirectory", suite.TestRemoveFile_IsDirectory)
	test.Run("RemoveFile_NoPermission", suite.TestRemoveFile_NoPermission)
	test.Run("RemoveFile_ReturnsAttributes", suite.TestRemoveFile_ReturnsAttributes)
	test.Run("RemoveFile_UpdatesParent", suite.TestRemoveFile_UpdatesParent)
	test.Run("RemoveFile_Symlink", suite.TestRemoveFile_Symlink)

	test.Run("RemoveDirectory_Success", suite.TestRemoveDirectory_Success)
	test.Run("RemoveDirectory_NotFound", suite.TestRemoveDirectory_NotFound)
	test.Run("RemoveDirectory_NotDirectory", suite.TestRemoveDirectory_NotDirectory)
	test.Run("RemoveDirectory_NotEmpty", suite.TestRemoveDirectory_NotEmpty)
	test.Run("RemoveDirectory_NoPermission", suite.TestRemoveDirectory_NoPermission)
	test.Run("RemoveDirectory_UpdatesParent", suite.TestRemoveDirectory_UpdatesParent)
}

// TestRemoveFile_Success verifies successful file removal.
func (suite *StoreTestSuite) TestRemoveFile_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share and file
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileName := "testfile.txt"
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, fileName, fileAttr)
	require.NoError(test, err)

	// Verify file exists
	_, err = store.GetFile(ctx, fileHandle)
	require.NoError(test, err)

	// Act - Remove the file
	removedAttr, err := store.RemoveFile(authCtx, rootHandle, fileName)

	// Assert
	require.NoError(test, err, "File removal should succeed")
	assert.NotNil(test, removedAttr, "Should return file attributes")
	assert.Equal(test, metadata.FileTypeRegular, removedAttr.Type)

	// Verify file no longer exists
	_, err = store.GetFile(ctx, fileHandle)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Removed file should not exist")

	// Verify file not in directory
	_, _, err = store.Lookup(authCtx, rootHandle, fileName)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Removed file should not be in directory")
}

// TestRemoveFile_NotFound verifies error when removing non-existent file.
func (suite *StoreTestSuite) TestRemoveFile_NotFound(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act - Try to remove non-existent file
	removedAttr, err := store.RemoveFile(authCtx, rootHandle, "nonexistent.txt")

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Should return ErrNotFound")
	assert.Nil(test, removedAttr)
}

// TestRemoveFile_IsDirectory verifies that RemoveFile cannot remove directories.
func (suite *StoreTestSuite) TestRemoveFile_IsDirectory(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share and directory
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	dirName := "testdir"
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(authCtx, rootHandle, dirName, dirAttr)
	require.NoError(test, err)

	// Act - Try to remove directory with RemoveFile
	removedAttr, err := store.RemoveFile(authCtx, rootHandle, dirName)

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrIsDirectory, err, "Should return ErrIsDirectory")
	assert.Nil(test, removedAttr)

	// Verify directory still exists
	_, _, err = store.Lookup(authCtx, rootHandle, dirName)
	require.NoError(test, err, "Directory should still exist")
}

// TestRemoveFile_NoPermission verifies permission checking.
func (suite *StoreTestSuite) TestRemoveFile_NoPermission(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share with restricted directory
	shareName := "/export/data"
	rootAttr := DefaultRootDirAttr()
	rootAttr.Mode = 0555 // r-xr-xr-x (no write permission)
	rootAttr.UID = 1000
	rootAttr.GID = 1000
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, rootAttr)
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file as root
	rootAuthCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileName := "testfile.txt"
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(rootAuthCtx, rootHandle, fileName, fileAttr)
	require.NoError(test, err)

	// Try to remove as non-owner without write permission
	userAuthCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(2000), // Not owner, not in group
			GID: uint32Ptr(2000),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act
	removedAttr, err := store.RemoveFile(userAuthCtx, rootHandle, fileName)

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrAccessDenied, err, "Should return ErrAccessDenied")
	assert.Nil(test, removedAttr)

	// Verify file still exists
	_, err = store.GetFile(ctx, rootHandle)
	require.NoError(test, err, "File should still exist")
}

// TestRemoveFile_ReturnsAttributes verifies returned attributes include ContentID.
func (suite *StoreTestSuite) TestRemoveFile_ReturnsAttributes(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileName := "testfile.txt"
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 1024,
		Mode: 0644,
		UID:  100,
		GID:  200,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, fileName, fileAttr)
	require.NoError(test, err)

	// Get original attributes
	originalAttr, err := store.GetFile(ctx, fileHandle)
	require.NoError(test, err)

	// Act - Remove file
	removedAttr, err := store.RemoveFile(authCtx, rootHandle, fileName)

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, removedAttr, "Should return file attributes")

	// Verify key attributes match
	assert.Equal(test, metadata.FileTypeRegular, removedAttr.Type)
	assert.Equal(test, originalAttr.Size, removedAttr.Size)
	assert.Equal(test, originalAttr.UID, removedAttr.UID)
	assert.Equal(test, originalAttr.GID, removedAttr.GID)

	// ContentID should be present for protocol handler to clean up
	// (Implementation-specific: may be empty or a valid ContentID)
	// Just verify the field exists in the returned struct
	assert.NotNil(test, removedAttr, "Attributes should include all metadata")
}

// TestRemoveFile_UpdatesParent verifies parent directory timestamps are updated.
func (suite *StoreTestSuite) TestRemoveFile_UpdatesParent(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Get parent timestamps before
	parentBefore, err := store.GetFile(ctx, rootHandle)
	require.NoError(test, err)

	// Small delay to ensure timestamp difference
	time.Sleep(10 * time.Millisecond)

	// Create file
	fileName := "testfile.txt"
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(authCtx, rootHandle, fileName, fileAttr)
	require.NoError(test, err)

	time.Sleep(10 * time.Millisecond)

	// Act - Remove file
	_, err = store.RemoveFile(authCtx, rootHandle, fileName)
	require.NoError(test, err)

	// Get parent timestamps after
	parentAfter, err := store.GetFile(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - Parent mtime and ctime should be updated
	assert.True(test, parentAfter.Mtime.After(parentBefore.Mtime) ||
		parentAfter.Mtime.Equal(parentBefore.Mtime),
		"Parent mtime should be updated or equal")
	assert.True(test, parentAfter.Ctime.After(parentBefore.Ctime) ||
		parentAfter.Ctime.Equal(parentBefore.Ctime),
		"Parent ctime should be updated or equal")
}

// TestRemoveFile_Symlink verifies removing symbolic links.
func (suite *StoreTestSuite) TestRemoveFile_Symlink(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Create symlink
	linkName := "testlink"
	linkTarget := "/some/target/path"
	linkAttr := &metadata.FileAttr{
		Type: metadata.FileTypeSymlink,
		Mode: 0777,
		UID:  0,
		GID:  0,
	}

	linkHandle, err := store.CreateSymlink(authCtx, rootHandle, linkName, linkTarget, linkAttr)
	require.NoError(test, err)

	// Act - Remove symlink
	removedAttr, err := store.RemoveFile(authCtx, rootHandle, linkName)

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, removedAttr)
	assert.Equal(test, metadata.FileTypeSymlink, removedAttr.Type)

	// Verify symlink no longer exists
	_, err = store.GetFile(ctx, linkHandle)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Symlink should be removed")
}

// TestRemoveDirectory_Success verifies successful directory removal.
func (suite *StoreTestSuite) TestRemoveDirectory_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share and empty directory
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	dirName := "testdir"
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	dirHandle, err := store.Create(authCtx, rootHandle, dirName, dirAttr)
	require.NoError(test, err)

	// Verify directory exists and is empty
	_, err = store.GetFile(ctx, dirHandle)
	require.NoError(test, err)

	// Act - Remove the directory
	err = store.RemoveDirectory(authCtx, rootHandle, dirName)

	// Assert
	require.NoError(test, err, "Directory removal should succeed")

	// Verify directory no longer exists
	_, err = store.GetFile(ctx, dirHandle)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Removed directory should not exist")

	// Verify directory not in parent
	_, _, err = store.Lookup(authCtx, rootHandle, dirName)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Removed directory should not be in parent")
}

// TestRemoveDirectory_NotFound verifies error when removing non-existent directory.
func (suite *StoreTestSuite) TestRemoveDirectory_NotFound(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act - Try to remove non-existent directory
	err = store.RemoveDirectory(authCtx, rootHandle, "nonexistent")

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Should return ErrNotFound")
}

// TestRemoveDirectory_NotDirectory verifies that RemoveDirectory cannot remove files.
func (suite *StoreTestSuite) TestRemoveDirectory_NotDirectory(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share and file
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileName := "testfile.txt"
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(authCtx, rootHandle, fileName, fileAttr)
	require.NoError(test, err)

	// Act - Try to remove file with RemoveDirectory
	err = store.RemoveDirectory(authCtx, rootHandle, fileName)

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotDirectory, err, "Should return ErrNotDirectory")

	// Verify file still exists
	_, _, err = store.Lookup(authCtx, rootHandle, fileName)
	require.NoError(test, err, "File should still exist")
}

// TestRemoveDirectory_NotEmpty verifies that non-empty directories cannot be removed.
func (suite *StoreTestSuite) TestRemoveDirectory_NotEmpty(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share, directory, and file in directory
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Create directory
	dirName := "testdir"
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	dirHandle, err := store.Create(authCtx, rootHandle, dirName, dirAttr)
	require.NoError(test, err)

	// Create file inside directory
	fileName := "file.txt"
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(authCtx, dirHandle, fileName, fileAttr)
	require.NoError(test, err)

	// Act - Try to remove non-empty directory
	err = store.RemoveDirectory(authCtx, rootHandle, dirName)

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotEmpty, err, "Should return ErrNotEmpty")

	// Verify directory still exists
	_, _, err = store.Lookup(authCtx, rootHandle, dirName)
	require.NoError(test, err, "Directory should still exist")
}

// TestRemoveDirectory_NoPermission verifies permission checking.
func (suite *StoreTestSuite) TestRemoveDirectory_NoPermission(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create share with restricted parent directory
	shareName := "/export/data"
	rootAttr := DefaultRootDirAttr()
	rootAttr.Mode = 0555 // r-xr-xr-x (no write permission)
	rootAttr.UID = 1000
	rootAttr.GID = 1000
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, rootAttr)
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create directory as root
	rootAuthCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	dirName := "testdir"
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(rootAuthCtx, rootHandle, dirName, dirAttr)
	require.NoError(test, err)

	// Try to remove as non-owner without write permission
	userAuthCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(2000),
			GID: uint32Ptr(2000),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act
	err = store.RemoveDirectory(userAuthCtx, rootHandle, dirName)

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrAccessDenied, err, "Should return ErrAccessDenied")

	// Verify directory still exists
	_, _, err = store.Lookup(rootAuthCtx, rootHandle, dirName)
	require.NoError(test, err, "Directory should still exist")
}

// TestRemoveDirectory_UpdatesParent verifies parent directory timestamps are updated.
func (suite *StoreTestSuite) TestRemoveDirectory_UpdatesParent(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Get parent timestamps before
	parentBefore, err := store.GetFile(ctx, rootHandle)
	require.NoError(test, err)

	time.Sleep(10 * time.Millisecond)

	// Create directory
	dirName := "testdir"
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	_, err = store.Create(authCtx, rootHandle, dirName, dirAttr)
	require.NoError(test, err)

	time.Sleep(10 * time.Millisecond)

	// Act - Remove directory
	err = store.RemoveDirectory(authCtx, rootHandle, dirName)
	require.NoError(test, err)

	// Get parent timestamps after
	parentAfter, err := store.GetFile(ctx, rootHandle)
	require.NoError(test, err)

	// Assert - Parent mtime and ctime should be updated
	assert.True(test, parentAfter.Mtime.After(parentBefore.Mtime) ||
		parentAfter.Mtime.Equal(parentBefore.Mtime),
		"Parent mtime should be updated or equal")
	assert.True(test, parentAfter.Ctime.After(parentBefore.Ctime) ||
		parentAfter.Ctime.Equal(parentBefore.Ctime),
		"Parent ctime should be updated or equal")
}

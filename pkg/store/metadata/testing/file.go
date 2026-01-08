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

// RunFileTests executes all file operation tests in the suite.
func (suite *StoreTestSuite) RunFileTests(t *testing.T) {
	t.Run("Lookup", suite.testLookup)
	t.Run("GetFile", suite.testGetFile)
	t.Run("SetFileAttributes", suite.testSetFileAttributes)
	t.Run("Create", suite.testCreate)
	t.Run("CreateSymlink", suite.testCreateSymlink)
	t.Run("CreateSpecialFile", suite.testCreateSpecialFile)
	t.Run("CreateHardLink", suite.testCreateHardLink)
}

// ============================================================================
// Lookup Tests
// ============================================================================

func (suite *StoreTestSuite) testLookup(test *testing.T) {
	test.Run("LookupRegularFile", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		foundHandle, attr, err := store.Lookup(authCtx, rootHandle, "test.txt")
		require.NoError(t, err)
		assert.Equal(t, fileHandle, foundHandle)
		assert.Equal(t, metadata.FileTypeRegular, attr.Type)
	})

	test.Run("LookupDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "subdir")

		foundHandle, attr, err := store.Lookup(authCtx, rootHandle, "subdir")
		require.NoError(t, err)
		assert.Equal(t, dirHandle, foundHandle)
		assert.Equal(t, metadata.FileTypeDirectory, attr.Type)
	})

	test.Run("LookupCurrentDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		foundHandle, attr, err := store.Lookup(authCtx, rootHandle, ".")
		require.NoError(t, err)
		assert.Equal(t, rootHandle, foundHandle)
		assert.Equal(t, metadata.FileTypeDirectory, attr.Type)
	})

	test.Run("LookupParentDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		subdirHandle := createTestDirectory(t, store, authCtx, rootHandle, "subdir")

		foundHandle, attr, err := store.Lookup(authCtx, subdirHandle, "..")
		require.NoError(t, err)
		assert.Equal(t, rootHandle, foundHandle)
		assert.Equal(t, metadata.FileTypeDirectory, attr.Type)
	})

	test.Run("LookupNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		mustNotLookup(t, store, authCtx, rootHandle, "nonexistent.txt")
	})

	test.Run("LookupNotDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "file.txt")

		_, _, err := store.Lookup(authCtx, fileHandle, "something")
		AssertErrorCode(t, metadata.ErrNotDirectory, err)
	})

	test.Run("LookupPermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create directory with no execute permission for others
		dirHandle := createTestDirectoryWithMode(t, store, rootAuthCtx, rootHandle, "private", 0600)
		createTestFile(t, store, rootAuthCtx, dirHandle, "secret.txt")

		// Try to lookup as different user without execute permission
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		_, _, err := store.Lookup(userAuthCtx, dirHandle, "secret.txt")
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	test.Run("LookupWithExecutePermission", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Directory with world execute permission
		dirHandle := createTestDirectoryWithMode(t, store, rootAuthCtx, rootHandle, "public", 0711)
		fileHandle := createTestFile(t, store, rootAuthCtx, dirHandle, "file.txt")

		// Lookup as different user should succeed (has execute)
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		foundHandle, _, err := store.Lookup(userAuthCtx, dirHandle, "file.txt")
		require.NoError(t, err)
		assert.Equal(t, fileHandle, foundHandle)
	})
}

// ============================================================================
// GetFile Tests
// ============================================================================

func (suite *StoreTestSuite) testGetFile(t *testing.T) {
	t.Run("GetRegularFile", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFileWithMode(t, store, authCtx, rootHandle, "test.txt", 0644)

		attr := mustGetFile(t, store, fileHandle)
		assert.Equal(t, metadata.FileTypeRegular, attr.Type)
		assert.Equal(t, uint32(0644), attr.Mode&0777)
		assert.NotZero(t, attr.Atime)
		assert.NotZero(t, attr.Mtime)
		assert.NotZero(t, attr.Ctime)
	})

	t.Run("GetDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		dirHandle := createTestDirectoryWithMode(t, store, authCtx, rootHandle, "subdir", 0755)

		attr := mustGetFile(t, store, dirHandle)
		assert.Equal(t, metadata.FileTypeDirectory, attr.Type)
		assert.Equal(t, uint32(0755), attr.Mode&0777)
	})

	t.Run("GetFileNotFound", func(t *testing.T) {
		store := suite.NewStore()
		ctx := context.Background()

		invalidHandle := metadata.FileHandle("invalid-handle")
		_, err := store.GetFile(ctx, invalidHandle)
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("GetFileInvalidHandle", func(t *testing.T) {
		store := suite.NewStore()
		ctx := context.Background()

		_, err := store.GetFile(ctx, metadata.FileHandle{})
		AssertErrorCode(t, metadata.ErrInvalidHandle, err)
	})

	t.Run("GetFileNoPermissionCheck", func(t *testing.T) {
		store := suite.NewStore()
		ctx := context.Background()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create private file as root
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "private.txt", 0600)

		// GetFile should succeed without permission check
		attr, err := store.GetFile(ctx, fileHandle)
		require.NoError(t, err)
		assert.Equal(t, metadata.FileTypeRegular, attr.Type)
	})
}

// ============================================================================
// SetFileAttributes Tests
// ============================================================================

func (suite *StoreTestSuite) testSetFileAttributes(t *testing.T) {
	t.Run("SetMode", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFileWithMode(t, store, authCtx, rootHandle, "test.txt", 0644)

		newMode := uint32(0600)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		require.NoError(t, err)

		assertFileMode(t, store, fileHandle, 0600)
	})

	t.Run("SetSize", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		newSize := uint64(1024)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		require.NoError(t, err)

		assertFileSize(t, store, fileHandle, 1024)
	})

	t.Run("SetUID", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, rootAuthCtx, rootHandle, "test.txt")

		newUID := uint32(1000)
		err := store.SetFileAttributes(rootAuthCtx, fileHandle, &metadata.SetAttrs{
			UID: &newUID,
		})
		require.NoError(t, err)

		attr := mustGetFile(t, store, fileHandle)
		assert.Equal(t, uint32(1000), attr.UID)
	})

	t.Run("SetGID", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, rootAuthCtx, rootHandle, "test.txt")

		newGID := uint32(1000)
		err := store.SetFileAttributes(rootAuthCtx, fileHandle, &metadata.SetAttrs{
			GID: &newGID,
		})
		require.NoError(t, err)

		attr := mustGetFile(t, store, fileHandle)
		assert.Equal(t, uint32(1000), attr.GID)
	})

	t.Run("SetTimes", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		newAtime := time.Now().Add(-24 * time.Hour)
		newMtime := time.Now().Add(-48 * time.Hour)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Atime: &newAtime,
			Mtime: &newMtime,
		})
		require.NoError(t, err)

		attr := mustGetFile(t, store, fileHandle)
		assert.True(t, attr.Atime.Equal(newAtime))
		assert.True(t, attr.Mtime.Equal(newMtime))
	})

	t.Run("SetMultipleAttributes", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		newMode := uint32(0600)
		newSize := uint64(2048)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
			Size: &newSize,
		})
		require.NoError(t, err)

		attr := mustGetFile(t, store, fileHandle)
		assert.Equal(t, uint32(0600), attr.Mode&0777)
		assert.Equal(t, uint64(2048), attr.Size)
	})

	t.Run("SetAttributesUpdatesCtimeAutomatically", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attrBefore := mustGetFile(t, store, fileHandle)
		time.Sleep(10 * time.Millisecond)

		newMode := uint32(0600)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		require.NoError(t, err)

		attrAfter := mustGetFile(t, store, fileHandle)
		assertTimestampAfter(t, attrAfter.Ctime, attrBefore.Ctime, "ctime")
	})

	t.Run("SetModePermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		fileHandle := createTestFile(t, store, rootAuthCtx, rootHandle, "test.txt")

		// Try to change mode as non-owner
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		newMode := uint32(0666)
		err := store.SetFileAttributes(userAuthCtx, fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		AssertErrorCode(t, metadata.ErrPermissionDenied, err)
	})

	t.Run("SetUIDRequiresRoot", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")

		// Create file as user
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		fileHandle := createTestFileWithOwner(t, store, userAuthCtx, rootHandle, "test.txt", 1000, 1000)

		// Non-root cannot change UID
		newUID := uint32(2000)
		err := store.SetFileAttributes(userAuthCtx, fileHandle, &metadata.SetAttrs{
			UID: &newUID,
		})
		AssertErrorCode(t, metadata.ErrPermissionDenied, err)
	})

	t.Run("SetSizeRequiresWritePermission", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create read-only file
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "test.txt", 0444)

		// Try to change size without write permission
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		newSize := uint64(1024)
		err := store.SetFileAttributes(userAuthCtx, fileHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	t.Run("SetAttributesFileNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, _ = createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		invalidHandle := metadata.FileHandle("invalid")
		newMode := uint32(0644)
		err := store.SetFileAttributes(authCtx, invalidHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("SetSizeOnDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "subdir")

		newSize := uint64(1024)
		err := store.SetFileAttributes(authCtx, dirHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		AssertErrorCode(t, metadata.ErrInvalidArgument, err)
	})

	t.Run("SetNoAttributesIsNoOp", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")
		attrBefore := mustGetFile(t, store, fileHandle)

		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{})
		require.NoError(t, err)

		attrAfter := mustGetFile(t, store, fileHandle)
		assert.Equal(t, attrBefore.Mode, attrAfter.Mode)
		assert.Equal(t, attrBefore.Size, attrAfter.Size)
		assert.Equal(t, attrBefore.UID, attrAfter.UID)
		assert.Equal(t, attrBefore.GID, attrAfter.GID)
	})
}

// ============================================================================
// Create Tests
// ============================================================================

func (suite *StoreTestSuite) testCreate(t *testing.T) {
	t.Run("CreateRegularFile", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
			UID:  0,
			GID:  0,
		}

		fileHandle, err := store.Create(authCtx, rootHandle, "test.txt", attr)
		require.NoError(t, err)
		assert.NotEmpty(t, fileHandle)

		retrievedAttr := mustGetFile(t, store, fileHandle)
		assert.Equal(t, metadata.FileTypeRegular, retrievedAttr.Type)
		assert.Equal(t, uint32(0644), retrievedAttr.Mode&0777)
		assert.Equal(t, uint64(0), retrievedAttr.Size)
		assert.NotEmpty(t, retrievedAttr.ContentID)
	})

	t.Run("CreateDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		attr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  0,
			GID:  0,
		}

		dirHandle, err := store.Create(authCtx, rootHandle, "subdir", attr)
		require.NoError(t, err)
		assert.NotEmpty(t, dirHandle)

		retrievedAttr := mustGetFile(t, store, dirHandle)
		assert.Equal(t, metadata.FileTypeDirectory, retrievedAttr.Type)
		assert.Equal(t, uint32(0755), retrievedAttr.Mode&0777)
		assert.Empty(t, retrievedAttr.ContentID)
	})

	t.Run("CreateWithDefaultMode", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0, // Default should be applied
			UID:  0,
			GID:  0,
		}

		fileHandle, err := store.Create(authCtx, rootHandle, "test.txt", attr)
		require.NoError(t, err)

		assertFileMode(t, store, fileHandle, 0644)
	})

	t.Run("CreateWithDefaultOwnership", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")

		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
			UID:  0, // Should use authenticated user's UID
			GID:  0, // Should use authenticated user's GID
		}

		fileHandle, err := store.Create(userAuthCtx, rootHandle, "test.txt", attr)
		require.NoError(t, err)

		assertFileOwner(t, store, fileHandle, 1000, 1000)
	})

	t.Run("CreateTimestampsSet", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		before := time.Now()
		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")
		after := time.Now()

		attr := mustGetFile(t, store, fileHandle)
		assert.True(t, attr.Atime.After(before.Add(-time.Second)) && attr.Atime.Before(after.Add(time.Second)))
		assert.True(t, attr.Mtime.After(before.Add(-time.Second)) && attr.Mtime.Before(after.Add(time.Second)))
		assert.True(t, attr.Ctime.After(before.Add(-time.Second)) && attr.Ctime.Before(after.Add(time.Second)))
	})

	t.Run("CreateAlreadyExists", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		}
		_, err := store.Create(authCtx, rootHandle, "test.txt", attr)
		AssertErrorCode(t, metadata.ErrAlreadyExists, err)
	})

	t.Run("CreatePermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create directory without write permission for others
		dirHandle := createTestDirectoryWithMode(t, store, rootAuthCtx, rootHandle, "readonly", 0555)

		// Try to create file as non-owner without write permission
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		}
		_, err := store.Create(userAuthCtx, dirHandle, "test.txt", attr)
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	t.Run("CreateInvalidType", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		// Try to create symlink with Create (should use CreateSymlink)
		attr := &metadata.FileAttr{
			Type: metadata.FileTypeSymlink,
			Mode: 0644,
		}
		_, err := store.Create(authCtx, rootHandle, "test.txt", attr)
		AssertErrorCode(t, metadata.ErrInvalidArgument, err)
	})

	t.Run("CreateParentNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, _ = createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		invalidHandle := metadata.FileHandle("invalid")
		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		}
		_, err := store.Create(authCtx, invalidHandle, "test.txt", attr)
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("CreateParentNotDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "file.txt")

		attr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
		}
		_, err := store.Create(authCtx, fileHandle, "test.txt", attr)
		AssertErrorCode(t, metadata.ErrNotDirectory, err)
	})
}

// ============================================================================
// CreateSymlink Tests
// ============================================================================

func (suite *StoreTestSuite) testCreateSymlink(t *testing.T) {
	t.Run("CreateSymlinkBasic", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		symlinkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link.txt", "/target/path")

		attr := mustGetFile(t, store, symlinkHandle)
		assert.Equal(t, metadata.FileTypeSymlink, attr.Type)
		assert.Equal(t, "/target/path", attr.LinkTarget)
		assert.Empty(t, attr.ContentID)
	})

	t.Run("CreateSymlinkRelativePath", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		symlinkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link.txt", "../relative/path")

		attr := mustGetFile(t, store, symlinkHandle)
		assert.Equal(t, "../relative/path", attr.LinkTarget)
	})

	t.Run("CreateDanglingSymlink", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		symlinkHandle := createTestSymlink(t, store, authCtx, rootHandle, "dangling.txt", "/nonexistent")

		attr := mustGetFile(t, store, symlinkHandle)
		assert.Equal(t, "/nonexistent", attr.LinkTarget)
	})

	t.Run("CreateSymlinkSizeMatchesTarget", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		target := "/some/target/path"
		symlinkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link.txt", target)

		assertFileSize(t, store, symlinkHandle, uint64(len(target)))
	})

	t.Run("CreateSymlinkAlreadyExists", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		createTestFile(t, store, authCtx, rootHandle, "existing.txt")

		attr := &metadata.FileAttr{Mode: 0777}
		_, err := store.CreateSymlink(authCtx, rootHandle, "existing.txt", "/target", attr)
		AssertErrorCode(t, metadata.ErrAlreadyExists, err)
	})

	t.Run("CreateSymlinkPermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		dirHandle := createTestDirectoryWithMode(t, store, rootAuthCtx, rootHandle, "readonly", 0555)

		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		attr := &metadata.FileAttr{Mode: 0777}
		_, err := store.CreateSymlink(userAuthCtx, dirHandle, "link.txt", "/target", attr)
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	t.Run("CreateSymlinkParentNotDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "file.txt")

		attr := &metadata.FileAttr{Mode: 0777}
		_, err := store.CreateSymlink(authCtx, fileHandle, "link.txt", "/target", attr)
		AssertErrorCode(t, metadata.ErrNotDirectory, err)
	})
}

// ============================================================================
// CreateSpecialFile Tests
// ============================================================================

func (suite *StoreTestSuite) testCreateSpecialFile(t *testing.T) {
	t.Run("CreateBlockDevice", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := RootAuthContext()

		deviceHandle := createTestSpecialFile(t, store, rootAuthCtx, rootHandle, "sda1",
			metadata.FileTypeBlockDevice, 8, 1)

		attr := mustGetFile(t, store, deviceHandle)
		assert.Equal(t, metadata.FileTypeBlockDevice, attr.Type)
		assert.Equal(t, uint64(0), attr.Size)
		assert.Empty(t, attr.ContentID)
	})

	t.Run("CreateCharDevice", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := RootAuthContext()

		deviceHandle := createTestSpecialFile(t, store, rootAuthCtx, rootHandle, "tty0",
			metadata.FileTypeCharDevice, 4, 0)

		assertFileType(t, store, deviceHandle, metadata.FileTypeCharDevice)
	})

	t.Run("CreateSocket", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		authCtx := createUserAuthContext(share, 1000, 1000)

		socketHandle := createTestSpecialFile(t, store, authCtx, rootHandle, "server.sock",
			metadata.FileTypeSocket, 0, 0)

		attr := mustGetFile(t, store, socketHandle)
		assert.Equal(t, metadata.FileTypeSocket, attr.Type)
		assert.Equal(t, uint64(0), attr.Size)
	})

	t.Run("CreateFIFO", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		authCtx := createUserAuthContext(share, 1000, 1000)

		fifoHandle := createTestSpecialFile(t, store, authCtx, rootHandle, "mypipe",
			metadata.FileTypeFIFO, 0, 0)

		assertFileType(t, store, fifoHandle, metadata.FileTypeFIFO)
	})

	t.Run("CreateDeviceRequiresRoot", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")

		// Non-root user tries to create device - should return ErrPrivilegeRequired
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		attr := &metadata.FileAttr{Mode: 0600}
		_, err := store.CreateSpecialFile(userAuthCtx, rootHandle, "sda1",
			metadata.FileTypeBlockDevice, attr, 8, 1)
		AssertErrorCode(t, metadata.ErrPrivilegeRequired, err)
	})

	t.Run("CreateSocketDoesNotRequireRoot", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")

		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		socketHandle := createTestSpecialFile(t, store, userAuthCtx, rootHandle, "user.sock",
			metadata.FileTypeSocket, 0, 0)

		assert.NotEmpty(t, socketHandle)
	})

	t.Run("CreateSpecialFileInvalidType", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		attr := &metadata.FileAttr{Mode: 0644}
		_, err := store.CreateSpecialFile(authCtx, rootHandle, "file.txt",
			metadata.FileTypeRegular, attr, 0, 0)
		AssertErrorCode(t, metadata.ErrInvalidArgument, err)
	})

	t.Run("CreateSpecialFileAlreadyExists", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		createTestFile(t, store, authCtx, rootHandle, "existing")

		attr := &metadata.FileAttr{Mode: 0666}
		_, err := store.CreateSpecialFile(authCtx, rootHandle, "existing",
			metadata.FileTypeSocket, attr, 0, 0)
		AssertErrorCode(t, metadata.ErrAlreadyExists, err)
	})
}

// ============================================================================
// CreateHardLink Tests
// ============================================================================

func (suite *StoreTestSuite) testCreateHardLink(t *testing.T) {
	t.Run("CreateHardLinkBasic", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "original.txt")

		err := store.CreateHardLink(authCtx, rootHandle, "link.txt", fileHandle)
		require.NoError(t, err)

		// Lookup the link
		linkHandle, _, err := store.Lookup(authCtx, rootHandle, "link.txt")
		require.NoError(t, err)

		// Both should have same ContentID
		originalAttr := mustGetFile(t, store, fileHandle)
		linkAttr := mustGetFile(t, store, linkHandle)
		assert.Equal(t, originalAttr.ContentID, linkAttr.ContentID)
	})

	t.Run("CreateHardLinkUpdatesTimestamps", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "original.txt")

		originalAttr := mustGetFile(t, store, fileHandle)
		time.Sleep(10 * time.Millisecond)

		err := store.CreateHardLink(authCtx, rootHandle, "link.txt", fileHandle)
		require.NoError(t, err)

		updatedAttr := mustGetFile(t, store, fileHandle)
		assertTimestampAfter(t, updatedAttr.Ctime, originalAttr.Ctime, "ctime")
	})

	t.Run("CreateHardLinkToDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "subdir")

		err := store.CreateHardLink(authCtx, rootHandle, "linkdir", dirHandle)
		AssertErrorCode(t, metadata.ErrIsDirectory, err)
	})

	t.Run("CreateHardLinkAlreadyExists", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "original.txt")
		createTestFile(t, store, authCtx, rootHandle, "existing.txt")

		err := store.CreateHardLink(authCtx, rootHandle, "existing.txt", fileHandle)
		AssertErrorCode(t, metadata.ErrAlreadyExists, err)
	})

	t.Run("CreateHardLinkPermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		fileHandle := createTestFile(t, store, rootAuthCtx, rootHandle, "original.txt")
		dirHandle := createTestDirectoryWithMode(t, store, rootAuthCtx, rootHandle, "readonly", 0555)

		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		err := store.CreateHardLink(userAuthCtx, dirHandle, "link.txt", fileHandle)
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	t.Run("CreateHardLinkTargetNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		invalidHandle := metadata.FileHandle("invalid")
		err := store.CreateHardLink(authCtx, rootHandle, "link.txt", invalidHandle)
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("CreateHardLinkParentNotDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "file.txt")
		targetHandle := createTestFile(t, store, authCtx, rootHandle, "target.txt")

		err := store.CreateHardLink(authCtx, fileHandle, "link.txt", targetHandle)
		AssertErrorCode(t, metadata.ErrNotDirectory, err)
	})
}

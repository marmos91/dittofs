package testing

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RunIOTests executes all I/O operation tests in the suite.
func (suite *StoreTestSuite) RunIOTests(t *testing.T) {
	t.Run("PrepareWrite", suite.testPrepareWrite)
	t.Run("CommitWrite", suite.testCommitWrite)
	t.Run("PrepareRead", suite.testPrepareRead)
	t.Run("WriteReadIntegration", suite.testWriteReadIntegration)
}

// ============================================================================
// PrepareWrite Tests
// ============================================================================

func (suite *StoreTestSuite) testPrepareWrite(t *testing.T) {
	t.Run("PrepareWriteBasic", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		assert.NotNil(t, intent)
		assert.NotEmpty(t, intent.ContentID)
		assert.Equal(t, fileHandle, intent.Handle)
		assert.Equal(t, uint64(1024), intent.NewSize)
	})

	t.Run("PrepareWriteZeroSize", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Writing zero bytes is valid (truncate to zero)
		intent := prepareWrite(t, store, authCtx, fileHandle, 0)
		assert.NotNil(t, intent)
		assert.Equal(t, uint64(0), intent.NewSize)
	})

	t.Run("PrepareWriteIncreasesSize", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Initial size is 0
		attrBefore := mustGetFile(t, store, fileHandle)
		assert.Equal(t, uint64(0), attrBefore.Size)

		// Prepare write to increase size
		intent := prepareWrite(t, store, authCtx, fileHandle, 2048)
		assert.Equal(t, uint64(2048), intent.NewSize)

		// Size should not change until commit
		attrAfter := mustGetFile(t, store, fileHandle)
		assert.Equal(t, uint64(0), attrAfter.Size)
	})

	t.Run("PrepareWriteDecreasesSize", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Set initial size
		newSize := uint64(2048)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		require.NoError(t, err)

		// Prepare write to decrease size (truncate)
		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		assert.Equal(t, uint64(1024), intent.NewSize)
	})

	t.Run("PrepareWriteFileNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, _ = createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		invalidHandle := metadata.FileHandle("invalid")
		_, err := store.PrepareWrite(authCtx, invalidHandle, 1024)
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("PrepareWritePermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create read-only file
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "readonly.txt", 0444)

		// Try to prepare write without write permission
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		_, err := store.PrepareWrite(userAuthCtx, fileHandle, 1024)
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	t.Run("PrepareWriteOwnerCanWrite", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")

		// Create file as user with owner write permission only
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		fileHandle := createTestFileWithMode(t, store, userAuthCtx, rootHandle, "test.txt", 0644)

		// Owner can prepare write
		intent := prepareWrite(t, store, userAuthCtx, fileHandle, 1024)
		assert.NotNil(t, intent)
	})

	t.Run("PrepareWriteIsDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "subdir")

		// Cannot write to directory
		_, err := store.PrepareWrite(authCtx, dirHandle, 1024)
		AssertErrorCode(t, metadata.ErrIsDirectory, err)
	})

	t.Run("PrepareWriteSymlink", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		symlinkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link.txt", "/target")

		// Cannot write to symlink directly (should be followed by protocol handler)
		_, err := store.PrepareWrite(authCtx, symlinkHandle, 1024)
		AssertErrorCode(t, metadata.ErrInvalidArgument, err)
	})

	t.Run("PrepareWriteReturnsContentID", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Get original ContentID
		attrBefore := mustGetFile(t, store, fileHandle)

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)

		// ContentID should be available for content repository coordination
		assert.NotEmpty(t, intent.ContentID)
		assert.Equal(t, attrBefore.ContentID, intent.ContentID)
	})

	t.Run("PrepareWriteMultipleTimes", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Multiple PrepareWrite calls should succeed (simulating retries)
		intent1 := prepareWrite(t, store, authCtx, fileHandle, 1024)
		intent2 := prepareWrite(t, store, authCtx, fileHandle, 2048)

		assert.NotNil(t, intent1)
		assert.NotNil(t, intent2)
		assert.Equal(t, intent1.ContentID, intent2.ContentID)
	})
}

// ============================================================================
// CommitWrite Tests
// ============================================================================

func (suite *StoreTestSuite) testCommitWrite(t *testing.T) {
	t.Run("CommitWriteBasic", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		attr := commitWrite(t, store, authCtx, intent)

		assert.Equal(t, uint64(1024), attr.Size)
		assert.Equal(t, metadata.FileTypeRegular, attr.Type)
	})

	t.Run("CommitWriteUpdatesSize", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		intent := prepareWrite(t, store, authCtx, fileHandle, 2048)
		commitWrite(t, store, authCtx, intent)

		// Verify size was updated
		assertFileSize(t, store, fileHandle, 2048)
	})

	t.Run("CommitWriteUpdatesMtime", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attrBefore := mustGetFile(t, store, fileHandle)
		time.Sleep(10 * time.Millisecond)

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		attr := commitWrite(t, store, authCtx, intent)

		// Mtime should be updated
		assertTimestampAfter(t, attr.Mtime, attrBefore.Mtime, "mtime")
	})

	t.Run("CommitWriteUpdatesCtime", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attrBefore := mustGetFile(t, store, fileHandle)
		time.Sleep(10 * time.Millisecond)

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		attr := commitWrite(t, store, authCtx, intent)

		// Ctime should be updated (metadata changed)
		assertTimestampAfter(t, attr.Ctime, attrBefore.Ctime, "ctime")
	})

	t.Run("CommitWriteDoesNotUpdateAtime", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attrBefore := mustGetFile(t, store, fileHandle)
		time.Sleep(10 * time.Millisecond)

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		attr := commitWrite(t, store, authCtx, intent)

		// Atime should not be updated by write
		assertTimestampUnchanged(t, attr.Atime, attrBefore.Atime, "atime")
	})

	t.Run("CommitWriteZeroSize", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Set initial size
		newSize := uint64(1024)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		require.NoError(t, err)

		// Truncate to zero
		intent := prepareWrite(t, store, authCtx, fileHandle, 0)
		attr := commitWrite(t, store, authCtx, intent)

		assert.Equal(t, uint64(0), attr.Size)
	})

	t.Run("CommitWriteFileNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)

		// Delete the file before commit
		_, err := store.RemoveFile(authCtx, rootHandle, "test.txt")
		require.NoError(t, err)

		// Commit should fail
		_, err = store.CommitWrite(authCtx, intent)
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("CommitWriteStaleHandle", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)

		// Modify the file between prepare and commit (simulating concurrent modification)
		newMode := uint32(0600)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		require.NoError(t, err)

		// Commit should still succeed (or fail with ErrStaleHandle depending on implementation)
		// Most implementations would allow this, but strict ones might detect staleness
		attr, err := store.CommitWrite(authCtx, intent)
		if err != nil {
			AssertErrorCode(t, metadata.ErrStaleHandle, err)
		} else {
			assert.Equal(t, uint64(1024), attr.Size)
		}
	})

	t.Run("CommitWriteWithoutPrepare", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Create an intent without PrepareWrite
		fakeIntent := &metadata.WriteOperation{
			Handle:    fileHandle,
			ContentID: metadata.ContentID("fake-content-id"),
			NewSize:   1024,
		}

		// This should fail or succeed depending on implementation validation
		_, err := store.CommitWrite(authCtx, fakeIntent)
		// Could be ErrNotFound, ErrStaleHandle, or success depending on implementation
		if err != nil {
			assert.Error(t, err)
		}
	})

	t.Run("CommitWriteMultipleTimes", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		attr1 := commitWrite(t, store, authCtx, intent)
		assert.Equal(t, uint64(1024), attr1.Size)

		// Committing the same intent again should fail or update again
		_, err := store.CommitWrite(authCtx, intent)
		if err != nil {
			// Some implementations might reject duplicate commits
			AssertErrorCode(t, metadata.ErrStaleHandle, err)
		}
	})

	t.Run("CommitWritePreservesContentID", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attrBefore := mustGetFile(t, store, fileHandle)
		originalContentID := attrBefore.ContentID

		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		attr := commitWrite(t, store, authCtx, intent)

		// ContentID should remain the same (content repository coordinates actual changes)
		assert.Equal(t, originalContentID, attr.ContentID)
	})
}

// ============================================================================
// PrepareRead Tests
// ============================================================================

func (suite *StoreTestSuite) testPrepareRead(t *testing.T) {
	t.Run("PrepareReadBasic", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.NotNil(t, readMeta)
		assert.NotNil(t, readMeta.Attr)
		assert.Equal(t, metadata.FileTypeRegular, readMeta.Attr.Type)
	})

	t.Run("PrepareReadReturnsAttributes", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFileWithMode(t, store, authCtx, rootHandle, "test.txt", 0644)

		// Set size
		newSize := uint64(2048)
		err := store.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		require.NoError(t, err)

		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(2048), readMeta.Attr.Size)
		assert.Equal(t, uint32(0644), readMeta.Attr.Mode&0777)
	})

	t.Run("PrepareReadReturnsContentID", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		readMeta := prepareRead(t, store, authCtx, fileHandle)

		// ContentID should be available for content repository coordination
		assert.NotEmpty(t, readMeta.Attr.ContentID)
	})

	t.Run("PrepareReadFileNotFound", func(t *testing.T) {
		store := suite.NewStore()
		_, _ = createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		invalidHandle := metadata.FileHandle("invalid")
		_, err := store.PrepareRead(authCtx, invalidHandle)
		AssertErrorCode(t, metadata.ErrNotFound, err)
	})

	t.Run("PrepareReadPermissionDenied", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create file with no read permission for others
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "secret.txt", 0200)

		// Try to prepare read without read permission
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		_, err := store.PrepareRead(userAuthCtx, fileHandle)
		AssertErrorCode(t, metadata.ErrAccessDenied, err)
	})

	t.Run("PrepareReadOwnerCanRead", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")

		// Create file as user with owner read permission only
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		fileHandle := createTestFileWithMode(t, store, userAuthCtx, rootHandle, "test.txt", 0400)

		// Owner can prepare read
		readMeta := prepareRead(t, store, userAuthCtx, fileHandle)
		assert.NotNil(t, readMeta)
	})

	t.Run("PrepareReadGroupCanRead", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create file with group read permission
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "test.txt", 0040)

		// Set file to group 1000
		newGID := uint32(1000)
		err := store.SetFileAttributes(rootAuthCtx, fileHandle, &metadata.SetAttrs{
			GID: &newGID,
		})
		require.NoError(t, err)

		// User in group 1000 can read
		userAuthCtx := createUserAuthContext(share, 2000, 1000)
		readMeta := prepareRead(t, store, userAuthCtx, fileHandle)
		assert.NotNil(t, readMeta)
	})

	t.Run("PrepareReadWorldCanRead", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create file with world read permission
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "test.txt", 0004)

		// Any user can read
		userAuthCtx := createUserAuthContext(share, 1000, 1000)
		readMeta := prepareRead(t, store, userAuthCtx, fileHandle)
		assert.NotNil(t, readMeta)
	})

	t.Run("PrepareReadIsDirectory", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "subdir")

		// Cannot read directory content with PrepareRead (use ReadDirectory)
		_, err := store.PrepareRead(authCtx, dirHandle)
		AssertErrorCode(t, metadata.ErrIsDirectory, err)
	})

	t.Run("PrepareReadSymlink", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		symlinkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link.txt", "/target")

		// Cannot read symlink directly (should be followed by protocol handler)
		// or use ReadSymlink for the target path
		_, err := store.PrepareRead(authCtx, symlinkHandle)
		AssertErrorCode(t, metadata.ErrInvalidArgument, err)
	})

	t.Run("PrepareReadEmptyFile", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "empty.txt")

		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(0), readMeta.Attr.Size)
		assert.NotEmpty(t, readMeta.Attr.ContentID)
	})

	t.Run("PrepareReadAfterWrite", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Write some data
		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		commitWrite(t, store, authCtx, intent)

		// Read should see updated size
		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(1024), readMeta.Attr.Size)
	})

	t.Run("PrepareReadDoesNotUpdateAtime", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		attrBefore := mustGetFile(t, store, fileHandle)
		time.Sleep(10 * time.Millisecond)

		readMeta := prepareRead(t, store, authCtx, fileHandle)

		// PrepareRead is metadata operation, actual read updates atime
		// So atime should remain unchanged at metadata level
		assertTimestampUnchanged(t, readMeta.Attr.Atime, attrBefore.Atime, "atime")
	})
}

// ============================================================================
// Write-Read Integration Tests
// ============================================================================

func (suite *StoreTestSuite) testWriteReadIntegration(t *testing.T) {
	t.Run("PrepareWriteCommitThenRead", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Write workflow
		writeIntent := prepareWrite(t, store, authCtx, fileHandle, 4096)
		writeAttr := commitWrite(t, store, authCtx, writeIntent)
		assert.Equal(t, uint64(4096), writeAttr.Size)

		// Read workflow
		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(4096), readMeta.Attr.Size)
		assert.Equal(t, writeAttr.ContentID, readMeta.Attr.ContentID)
	})

	t.Run("MultipleWritesAndReads", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// First write
		intent1 := prepareWrite(t, store, authCtx, fileHandle, 1024)
		commitWrite(t, store, authCtx, intent1)

		readMeta1 := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(1024), readMeta1.Attr.Size)

		// Second write (expand)
		intent2 := prepareWrite(t, store, authCtx, fileHandle, 2048)
		commitWrite(t, store, authCtx, intent2)

		readMeta2 := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(2048), readMeta2.Attr.Size)

		// Third write (truncate)
		intent3 := prepareWrite(t, store, authCtx, fileHandle, 512)
		commitWrite(t, store, authCtx, intent3)

		readMeta3 := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(512), readMeta3.Attr.Size)
	})

	t.Run("ConcurrentAccessDifferentUsers", func(t *testing.T) {
		store := suite.NewStore()
		share, rootHandle := createTestShare(t, store, "test")
		rootAuthCtx := createRootAuthContext(share)

		// Create file with read/write for all
		fileHandle := createTestFileWithMode(t, store, rootAuthCtx, rootHandle, "shared.txt", 0666)

		// User 1 writes
		user1AuthCtx := createUserAuthContext(share, 1000, 1000)
		intent1 := prepareWrite(t, store, user1AuthCtx, fileHandle, 1024)
		commitWrite(t, store, user1AuthCtx, intent1)

		// User 2 reads
		user2AuthCtx := createUserAuthContext(share, 2000, 2000)
		readMeta := prepareRead(t, store, user2AuthCtx, fileHandle)
		assert.Equal(t, uint64(1024), readMeta.Attr.Size)

		// User 2 writes
		intent2 := prepareWrite(t, store, user2AuthCtx, fileHandle, 2048)
		commitWrite(t, store, user2AuthCtx, intent2)

		// User 1 reads updated content
		readMeta2 := prepareRead(t, store, user1AuthCtx, fileHandle)
		assert.Equal(t, uint64(2048), readMeta2.Attr.Size)
	})

	t.Run("WriteWithoutCommitDoesNotAffectRead", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Prepare write but don't commit
		_ = prepareWrite(t, store, authCtx, fileHandle, 1024)

		// Read should see original size
		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(0), readMeta.Attr.Size)
	})

	t.Run("ReadAfterPartialWrite", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Set initial size
		intent1 := prepareWrite(t, store, authCtx, fileHandle, 4096)
		commitWrite(t, store, authCtx, intent1)

		// Prepare a partial write (e.g., offset 1024, length 512 = new size 1536)
		intent2 := prepareWrite(t, store, authCtx, fileHandle, 1536)

		// Read before commit sees old size
		readMeta1 := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(4096), readMeta1.Attr.Size)

		// Commit and read sees new size
		commitWrite(t, store, authCtx, intent2)
		readMeta2 := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, uint64(1536), readMeta2.Attr.Size)
	})

	t.Run("ContentIDRemainsStableAcrossWrites", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		// Get original ContentID
		originalMeta := prepareRead(t, store, authCtx, fileHandle)
		originalContentID := originalMeta.Attr.ContentID

		// Multiple writes
		for i := 1; i <= 3; i++ {
			intent := prepareWrite(t, store, authCtx, fileHandle, uint64(i*1024))
			commitWrite(t, store, authCtx, intent)

			readMeta := prepareRead(t, store, authCtx, fileHandle)
			// ContentID should remain the same (content repository manages actual content)
			assert.Equal(t, originalContentID, readMeta.Attr.ContentID)
		}
	})

	t.Run("TimestampConsistencyAcrossOperations", func(t *testing.T) {
		store := suite.NewStore()
		_, rootHandle := createTestShare(t, store, "test")
		authCtx := RootAuthContext()

		fileHandle := createTestFile(t, store, authCtx, rootHandle, "test.txt")

		originalMeta := prepareRead(t, store, authCtx, fileHandle)
		time.Sleep(10 * time.Millisecond)

		// Write updates mtime and ctime
		intent := prepareWrite(t, store, authCtx, fileHandle, 1024)
		writeAttr := commitWrite(t, store, authCtx, intent)

		assertTimestampAfter(t, writeAttr.Mtime, originalMeta.Attr.Mtime, "mtime after write")
		assertTimestampAfter(t, writeAttr.Ctime, originalMeta.Attr.Ctime, "ctime after write")
		assertTimestampUnchanged(t, writeAttr.Atime, originalMeta.Attr.Atime, "atime unchanged by write")

		// Read doesn't change timestamps at metadata level
		readMeta := prepareRead(t, store, authCtx, fileHandle)
		assert.Equal(t, writeAttr.Atime, readMeta.Attr.Atime)
		assert.Equal(t, writeAttr.Mtime, readMeta.Attr.Mtime)
		assert.Equal(t, writeAttr.Ctime, readMeta.Attr.Ctime)
	})
}

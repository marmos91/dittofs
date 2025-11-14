package testing

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// RunDirectoryTests executes all directory operation tests
func (suite *StoreTestSuite) RunDirectoryTests(t *testing.T) {
	t.Run("Move", suite.testMove)
	t.Run("ReadSymlink", suite.testReadSymlink)
	t.Run("ReadDirectory", suite.testReadDirectory)
}

// ============================================================================
// Move Tests
// ============================================================================

func (suite *StoreTestSuite) testMove(t *testing.T) {
	t.Run("RenameFileInSameDirectory", suite.testMoveRenameFileInSameDirectory)
	t.Run("MoveFileToAnotherDirectory", suite.testMoveFileToAnotherDirectory)
	t.Run("RenameDirectoryInSameDirectory", suite.testMoveRenameDirectoryInSameDirectory)
	t.Run("MoveDirectoryToAnotherDirectory", suite.testMoveDirectoryToAnotherDirectory)
	t.Run("ReplaceExistingFile", suite.testMoveReplaceExistingFile)
	t.Run("ReplaceEmptyDirectory", suite.testMoveReplaceEmptyDirectory)
	t.Run("NoOpSameNameSameDirectory", suite.testMoveNoOpSameNameSameDirectory)
	t.Run("ErrorNotFound", suite.testMoveErrorNotFound)
	t.Run("ErrorNoWritePermissionSource", suite.testMoveErrorNoWritePermissionSource)
	t.Run("ErrorNoWritePermissionDestination", suite.testMoveErrorNoWritePermissionDestination)
	t.Run("ErrorReplaceNonEmptyDirectory", suite.testMoveErrorReplaceNonEmptyDirectory)
	t.Run("ErrorReplaceFileWithDirectory", suite.testMoveErrorReplaceFileWithDirectory)
	t.Run("ErrorReplaceDirectoryWithFile", suite.testMoveErrorReplaceDirectoryWithFile)
	t.Run("TimestampUpdates", suite.testMoveTimestampUpdates)
}

func (suite *StoreTestSuite) testMoveRenameFileInSameDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create share with directory and file
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	fileHandle := createTestFile(t, store, authCtx, rootHandle, "oldname.txt")

	// Action: Rename file in same directory
	err := store.Move(authCtx, rootHandle, "oldname.txt", rootHandle, "newname.txt")
	require.NoError(t, err)

	// Assert: Old name doesn't exist
	_, _, err = store.Lookup(authCtx, rootHandle, "oldname.txt")
	AssertErrorCode(t, metadata.ErrNotFound, err)

	// Assert: New name exists and points to same file
	newHandle, _, err := store.Lookup(authCtx, rootHandle, "newname.txt")
	require.NoError(t, err)
	require.Equal(t, fileHandle, newHandle)
}

func (suite *StoreTestSuite) testMoveFileToAnotherDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create share with two directories and a file
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dir1Handle := createTestDirectory(t, store, authCtx, rootHandle, "dir1")
	dir2Handle := createTestDirectory(t, store, authCtx, rootHandle, "dir2")
	fileHandle := createTestFile(t, store, authCtx, dir1Handle, "file.txt")

	// Action: Move file from dir1 to dir2
	err := store.Move(authCtx, dir1Handle, "file.txt", dir2Handle, "file.txt")
	require.NoError(t, err)

	// Assert: File no longer in dir1
	_, _, err = store.Lookup(authCtx, dir1Handle, "file.txt")
	AssertErrorCode(t, metadata.ErrNotFound, err)

	// Assert: File now in dir2 with same handle
	newHandle, _, err := store.Lookup(authCtx, dir2Handle, "file.txt")
	require.NoError(t, err)
	require.Equal(t, fileHandle, newHandle)
}

func (suite *StoreTestSuite) testMoveRenameDirectoryInSameDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create share with nested directory
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "olddir")
	// Add a file inside to verify hierarchy is preserved
	createTestFile(t, store, authCtx, dirHandle, "file.txt")

	// Action: Rename directory
	err := store.Move(authCtx, rootHandle, "olddir", rootHandle, "newdir")
	require.NoError(t, err)

	// Assert: Old name doesn't exist
	_, _, err = store.Lookup(authCtx, rootHandle, "olddir")
	AssertErrorCode(t, metadata.ErrNotFound, err)

	// Assert: New name exists and hierarchy preserved
	newHandle, _, err := store.Lookup(authCtx, rootHandle, "newdir")
	require.NoError(t, err)
	require.Equal(t, dirHandle, newHandle)

	// Assert: Contents still accessible
	_, _, err = store.Lookup(authCtx, newHandle, "file.txt")
	require.NoError(t, err)
}

func (suite *StoreTestSuite) testMoveDirectoryToAnotherDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create nested directory structure
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dir1Handle := createTestDirectory(t, store, authCtx, rootHandle, "dir1")
	dir2Handle := createTestDirectory(t, store, authCtx, rootHandle, "dir2")
	subDirHandle := createTestDirectory(t, store, authCtx, dir1Handle, "subdir")
	createTestFile(t, store, authCtx, subDirHandle, "file.txt")

	// Action: Move subdir from dir1 to dir2
	err := store.Move(authCtx, dir1Handle, "subdir", dir2Handle, "subdir")
	require.NoError(t, err)

	// Assert: Moved correctly
	newHandle, _, err := store.Lookup(authCtx, dir2Handle, "subdir")
	require.NoError(t, err)
	require.Equal(t, subDirHandle, newHandle)

	// Assert: Contents preserved
	_, _, err = store.Lookup(authCtx, newHandle, "file.txt")
	require.NoError(t, err)
}

func (suite *StoreTestSuite) testMoveReplaceExistingFile(t *testing.T) {
	ctx := context.Background()
	store := suite.NewStore()

	// Setup: Create two files
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	sourceHandle := createTestFile(t, store, authCtx, rootHandle, "source.txt")
	targetHandle := createTestFile(t, store, authCtx, rootHandle, "target.txt")

	// Action: Move source over target (atomic replacement)
	err := store.Move(authCtx, rootHandle, "source.txt", rootHandle, "target.txt")
	require.NoError(t, err)

	// Assert: Source name gone
	_, _, err = store.Lookup(authCtx, rootHandle, "source.txt")
	AssertErrorCode(t, metadata.ErrNotFound, err)

	// Assert: Target name points to source file
	newHandle, _, err := store.Lookup(authCtx, rootHandle, "target.txt")
	require.NoError(t, err)
	require.Equal(t, sourceHandle, newHandle)

	// Assert: Old target file handle is gone
	_, err = store.GetFile(ctx, targetHandle)
	AssertErrorCode(t, metadata.ErrNotFound, err)
}

func (suite *StoreTestSuite) testMoveReplaceEmptyDirectory(t *testing.T) {
	ctx := context.Background()
	store := suite.NewStore()

	// Setup: Create source dir and empty target dir
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	sourceHandle := createTestDirectory(t, store, authCtx, rootHandle, "source")
	targetHandle := createTestDirectory(t, store, authCtx, rootHandle, "target")

	// Action: Move source over empty target
	err := store.Move(authCtx, rootHandle, "source", rootHandle, "target")
	require.NoError(t, err)

	// Assert: Replacement worked
	newHandle, _, err := store.Lookup(authCtx, rootHandle, "target")
	require.NoError(t, err)
	require.Equal(t, sourceHandle, newHandle)

	// Assert: Old target gone
	_, err = store.GetFile(ctx, targetHandle)
	AssertErrorCode(t, metadata.ErrNotFound, err)
}

func (suite *StoreTestSuite) testMoveNoOpSameNameSameDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)
	createTestFile(t, store, authCtx, rootHandle, "file.txt")

	// Action: Move to same location (no-op)
	err := store.Move(authCtx, rootHandle, "file.txt", rootHandle, "file.txt")
	require.NoError(t, err)

	// Assert: File still exists
	_, _, err = store.Lookup(authCtx, rootHandle, "file.txt")
	require.NoError(t, err)
}

func (suite *StoreTestSuite) testMoveErrorNotFound(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	// Action: Try to move non-existent file
	err := store.Move(authCtx, rootHandle, "nonexistent.txt", rootHandle, "newname.txt")

	// Assert
	AssertErrorCode(t, metadata.ErrNotFound, err)
}

func (suite *StoreTestSuite) testMoveErrorNoWritePermissionSource(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create file in read-only directory
	share, rootHandle := createTestShare(t, store, "test-share")
	rootAuthCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, rootAuthCtx, rootHandle, "readonly")
	createTestFile(t, store, rootAuthCtx, dirHandle, "file.txt")

	// Remove write permission from directory
	setDirectoryPermissions(t, store, rootAuthCtx, dirHandle, 0555) // r-xr-xr-x

	// Use non-root user
	userAuthCtx := createUserAuthContext(share, 1000, 1000)

	// Action: Try to move file from read-only directory
	err := store.Move(userAuthCtx, dirHandle, "file.txt", rootHandle, "file.txt")

	// Assert
	AssertErrorCode(t, metadata.ErrAccessDenied, err)
}

func (suite *StoreTestSuite) testMoveErrorNoWritePermissionDestination(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	rootAuthCtx := createRootAuthContext(share)

	createTestFile(t, store, rootAuthCtx, rootHandle, "file.txt")

	dirHandle := createTestDirectory(t, store, rootAuthCtx, rootHandle, "readonly")
	setDirectoryPermissions(t, store, rootAuthCtx, dirHandle, 0555) // r-xr-xr-x

	userAuthCtx := createUserAuthContext(share, 1000, 1000)

	// Action: Try to move file to read-only directory
	err := store.Move(userAuthCtx, rootHandle, "file.txt", dirHandle, "file.txt")

	// Assert
	AssertErrorCode(t, metadata.ErrAccessDenied, err)
}

func (suite *StoreTestSuite) testMoveErrorReplaceNonEmptyDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Source dir and non-empty target dir
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	createTestDirectory(t, store, authCtx, rootHandle, "source")
	targetHandle := createTestDirectory(t, store, authCtx, rootHandle, "target")
	createTestFile(t, store, authCtx, targetHandle, "file.txt")

	// Action: Try to replace non-empty directory
	err := store.Move(authCtx, rootHandle, "source", rootHandle, "target")

	// Assert
	AssertErrorCode(t, metadata.ErrNotEmpty, err)
}

func (suite *StoreTestSuite) testMoveErrorReplaceFileWithDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Directory source, file target
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	createTestDirectory(t, store, authCtx, rootHandle, "source")
	createTestFile(t, store, authCtx, rootHandle, "target")

	// Action: Try to replace file with directory
	err := store.Move(authCtx, rootHandle, "source", rootHandle, "target")

	// Assert
	AssertErrorCode(t, metadata.ErrNotDirectory, err)
}

func (suite *StoreTestSuite) testMoveErrorReplaceDirectoryWithFile(t *testing.T) {
	store := suite.NewStore()

	// Setup: File source, directory target
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	createTestFile(t, store, authCtx, rootHandle, "source")
	createTestDirectory(t, store, authCtx, rootHandle, "target")

	// Action: Try to replace directory with file
	err := store.Move(authCtx, rootHandle, "source", rootHandle, "target")

	// Assert
	AssertErrorCode(t, metadata.ErrIsDirectory, err)
}

func (suite *StoreTestSuite) testMoveTimestampUpdates(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dir1Handle := createTestDirectory(t, store, authCtx, rootHandle, "dir1")
	dir2Handle := createTestDirectory(t, store, authCtx, rootHandle, "dir2")
	fileHandle := createTestFile(t, store, authCtx, dir1Handle, "file.txt")

	// Get initial timestamps
	fileBefore := mustGetFile(t, store, fileHandle)
	dir1Before := mustGetFile(t, store, dir1Handle)
	dir2Before := mustGetFile(t, store, dir2Handle)

	// Action: Move file between directories
	err := store.Move(authCtx, dir1Handle, "file.txt", dir2Handle, "file.txt")
	require.NoError(t, err)

	// Assert: File ctime updated
	fileAfter := mustGetFile(t, store, fileHandle)
	require.True(t, fileAfter.Ctime.After(fileBefore.Ctime))

	// Assert: Source directory mtime/ctime updated
	dir1After := mustGetFile(t, store, dir1Handle)
	require.True(t, dir1After.Mtime.After(dir1Before.Mtime))
	require.True(t, dir1After.Ctime.After(dir1Before.Ctime))

	// Assert: Destination directory mtime/ctime updated
	dir2After := mustGetFile(t, store, dir2Handle)
	require.True(t, dir2After.Mtime.After(dir2Before.Mtime))
	require.True(t, dir2After.Ctime.After(dir2Before.Ctime))
}

// ============================================================================
// ReadSymlink Tests
// ============================================================================

func (suite *StoreTestSuite) testReadSymlink(t *testing.T) {
	t.Run("ReadAbsolutePath", suite.testReadSymlinkAbsolutePath)
	t.Run("ReadRelativePath", suite.testReadSymlinkRelativePath)
	t.Run("ReadDanglingLink", suite.testReadSymlinkDanglingLink)
	t.Run("ErrorNotSymlink", suite.testReadSymlinkErrorNotSymlink)
	t.Run("ErrorNoReadPermission", suite.testReadSymlinkErrorNoReadPermission)
	t.Run("AttributesReturned", suite.testReadSymlinkAttributesReturned)
}

func (suite *StoreTestSuite) testReadSymlinkAbsolutePath(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create symlink with absolute path
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	targetPath := "/absolute/path/to/target"
	linkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link", targetPath)

	// Action: Read symlink target
	target, attr, err := store.ReadSymlink(authCtx, linkHandle)

	// Assert
	require.NoError(t, err)
	require.Equal(t, targetPath, target)
	require.Equal(t, metadata.FileTypeSymlink, attr.Type)
}

func (suite *StoreTestSuite) testReadSymlinkRelativePath(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create symlink with relative path
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	targetPath := "../relative/target.txt"
	linkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link", targetPath)

	// Action
	target, _, err := store.ReadSymlink(authCtx, linkHandle)

	// Assert
	require.NoError(t, err)
	require.Equal(t, targetPath, target)
}

func (suite *StoreTestSuite) testReadSymlinkDanglingLink(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create symlink pointing to non-existent file
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	targetPath := "/does/not/exist"
	linkHandle := createTestSymlink(t, store, authCtx, rootHandle, "dangling", targetPath)

	// Action: Read dangling symlink (should succeed)
	target, _, err := store.ReadSymlink(authCtx, linkHandle)

	// Assert: Reading the link succeeds even if target doesn't exist
	require.NoError(t, err)
	require.Equal(t, targetPath, target)
}

func (suite *StoreTestSuite) testReadSymlinkErrorNotSymlink(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create regular file
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	fileHandle := createTestFile(t, store, authCtx, rootHandle, "file.txt")

	// Action: Try to read as symlink
	_, _, err := store.ReadSymlink(authCtx, fileHandle)

	// Assert
	AssertErrorCode(t, metadata.ErrInvalidArgument, err)
}

func (suite *StoreTestSuite) testReadSymlinkErrorNoReadPermission(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create symlink with no read permission
	share, rootHandle := createTestShare(t, store, "test-share")
	rootAuthCtx := createRootAuthContext(share)

	linkHandle := createTestSymlink(t, store, rootAuthCtx, rootHandle, "link", "/target")

	// Remove read permission
	setFilePermissions(t, store, rootAuthCtx, linkHandle, 0000)

	// Use non-root user
	userAuthCtx := createUserAuthContext(share, 1000, 1000)

	// Action
	_, _, err := store.ReadSymlink(userAuthCtx, linkHandle)

	// Assert
	AssertErrorCode(t, metadata.ErrAccessDenied, err)
}

func (suite *StoreTestSuite) testReadSymlinkAttributesReturned(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	targetPath := "/some/target"
	linkHandle := createTestSymlink(t, store, authCtx, rootHandle, "link", targetPath)

	// Action
	target, attr, err := store.ReadSymlink(authCtx, linkHandle)

	// Assert: Both target and attributes returned
	require.NoError(t, err)
	require.Equal(t, targetPath, target)
	require.NotNil(t, attr)
	require.Equal(t, metadata.FileTypeSymlink, attr.Type)
	require.Equal(t, uint64(len(targetPath)), attr.Size)
}

// ============================================================================
// ReadDirectory Tests
// ============================================================================

func (suite *StoreTestSuite) testReadDirectory(t *testing.T) {
	t.Run("EmptyDirectory", suite.testReadDirectoryEmpty)
	t.Run("SinglePage", suite.testReadDirectorySinglePage)
	t.Run("MultiplePagesComplete", suite.testReadDirectoryMultiplePagesComplete)
	t.Run("PaginationWithSmallLimit", suite.testReadDirectoryPaginationSmallLimit)
	t.Run("SpecialEntriesDotAndDotDot", suite.testReadDirectorySpecialEntries)
	t.Run("ErrorNotDirectory", suite.testReadDirectoryErrorNotDirectory)
	t.Run("ErrorNoReadPermission", suite.testReadDirectoryErrorNoReadPermission)
	t.Run("ErrorInvalidToken", suite.testReadDirectoryErrorInvalidToken)
	t.Run("ConsistentOrdering", suite.testReadDirectoryConsistentOrdering)
}

func (suite *StoreTestSuite) testReadDirectoryEmpty(t *testing.T) {
	store := suite.NewStore()

	// Setup: Empty directory
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "empty")

	// Action: Read empty directory
	page, err := store.ReadDirectory(authCtx, dirHandle, "", 8192)

	// Assert
	require.NoError(t, err)
	require.False(t, page.HasMore)
	require.Equal(t, "", page.NextToken)
	// May include "." and ".." or be completely empty
	require.LessOrEqual(t, len(page.Entries), 2)
}

func (suite *StoreTestSuite) testReadDirectorySinglePage(t *testing.T) {
	store := suite.NewStore()

	// Setup: Directory with a few files
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "dir")
	createTestFile(t, store, authCtx, dirHandle, "file1.txt")
	createTestFile(t, store, authCtx, dirHandle, "file2.txt")
	createTestFile(t, store, authCtx, dirHandle, "file3.txt")

	// Action: Read directory (should fit in one page)
	page, err := store.ReadDirectory(authCtx, dirHandle, "", 8192)

	// Assert
	require.NoError(t, err)
	require.False(t, page.HasMore)
	require.Equal(t, "", page.NextToken)

	// Should have 3 files (plus possibly "." and "..")
	fileCount := countNonSpecialEntries(page.Entries)
	require.Equal(t, 3, fileCount)

	// Verify all files present
	names := extractEntryNames(page.Entries)
	require.Contains(t, names, "file1.txt")
	require.Contains(t, names, "file2.txt")
	require.Contains(t, names, "file3.txt")
}

func (suite *StoreTestSuite) testReadDirectoryMultiplePagesComplete(t *testing.T) {
	store := suite.NewStore()

	// Setup: Directory with many files
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "dir")
	expectedFiles := make([]string, 50)
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("file%03d.txt", i)
		expectedFiles[i] = name
		createTestFile(t, store, authCtx, dirHandle, name)
	}

	// Action: Read all pages with small page size
	var allEntries []metadata.DirEntry
	token := ""
	pageCount := 0

	for {
		page, err := store.ReadDirectory(authCtx, dirHandle, token, 512) // Small page
		require.NoError(t, err)

		allEntries = append(allEntries, page.Entries...)
		pageCount++

		if !page.HasMore {
			require.Equal(t, "", page.NextToken)
			break
		}

		require.NotEqual(t, "", page.NextToken)
		token = page.NextToken

		// Safety: prevent infinite loop
		require.Less(t, pageCount, 100, "Too many pages, possible infinite loop")
	}

	// Assert: All files retrieved
	names := extractEntryNames(allEntries)
	for _, expectedFile := range expectedFiles {
		require.Contains(t, names, expectedFile)
	}

	// Assert: Multiple pages were used
	require.Greater(t, pageCount, 1, "Expected multiple pages with small page size")
}

func (suite *StoreTestSuite) testReadDirectoryPaginationSmallLimit(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "dir")
	for i := 0; i < 10; i++ {
		createTestFile(t, store, authCtx, dirHandle, fmt.Sprintf("file%d.txt", i))
	}

	// Action: Read first page with very small limit
	page1, err := store.ReadDirectory(authCtx, dirHandle, "", 100)
	require.NoError(t, err)

	// Assert: Should have more pages
	require.True(t, page1.HasMore)
	require.NotEqual(t, "", page1.NextToken)
	require.Greater(t, len(page1.Entries), 0)

	// Action: Read second page
	page2, err := store.ReadDirectory(authCtx, dirHandle, page1.NextToken, 100)
	require.NoError(t, err)

	// Assert: Second page is different from first
	firstNames := extractEntryNames(page1.Entries)
	secondNames := extractEntryNames(page2.Entries)

	// No overlap between pages
	for _, name := range secondNames {
		require.NotContains(t, firstNames, name, "Entry appeared in multiple pages")
	}
}

func (suite *StoreTestSuite) testReadDirectorySpecialEntries(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "dir")
	createTestFile(t, store, authCtx, dirHandle, "file.txt")

	// Action: Read from beginning
	page, err := store.ReadDirectory(authCtx, dirHandle, "", 8192)
	require.NoError(t, err)

	// Assert: May include "." and ".." at the beginning
	// This is implementation-specific, so we just verify they're valid if present
	for i, entry := range page.Entries {
		if entry.Name == "." || entry.Name == ".." {
			// Special entries should be at the beginning
			require.Less(t, i, 2, "Special entries should be first")
		}
	}
}

func (suite *StoreTestSuite) testReadDirectoryErrorNotDirectory(t *testing.T) {
	store := suite.NewStore()

	// Setup: Create file, not directory
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	fileHandle := createTestFile(t, store, authCtx, rootHandle, "file.txt")

	// Action: Try to read file as directory
	_, err := store.ReadDirectory(authCtx, fileHandle, "", 8192)

	// Assert
	AssertErrorCode(t, metadata.ErrNotDirectory, err)
}

func (suite *StoreTestSuite) testReadDirectoryErrorNoReadPermission(t *testing.T) {
	store := suite.NewStore()

	// Setup: Directory with no read permission
	share, rootHandle := createTestShare(t, store, "test-share")
	rootAuthCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, rootAuthCtx, rootHandle, "dir")
	setDirectoryPermissions(t, store, rootAuthCtx, dirHandle, 0000) // No permissions

	// Use non-root user
	userAuthCtx := createUserAuthContext(share, 1000, 1000)

	// Action
	_, err := store.ReadDirectory(userAuthCtx, dirHandle, "", 8192)

	// Assert
	AssertErrorCode(t, metadata.ErrAccessDenied, err)
}

func (suite *StoreTestSuite) testReadDirectoryErrorInvalidToken(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "dir")

	// Action: Try with invalid token
	_, err := store.ReadDirectory(authCtx, dirHandle, "invalid-token-xyz", 8192)

	// Assert
	AssertErrorCode(t, metadata.ErrInvalidArgument, err)
}

func (suite *StoreTestSuite) testReadDirectoryConsistentOrdering(t *testing.T) {
	store := suite.NewStore()

	// Setup
	share, rootHandle := createTestShare(t, store, "test-share")
	authCtx := createRootAuthContext(share)

	dirHandle := createTestDirectory(t, store, authCtx, rootHandle, "dir")
	for i := 0; i < 5; i++ {
		createTestFile(t, store, authCtx, dirHandle, fmt.Sprintf("file%d.txt", i))
	}

	// Action: Read directory twice
	page1, err := store.ReadDirectory(authCtx, dirHandle, "", 8192)
	require.NoError(t, err)

	page2, err := store.ReadDirectory(authCtx, dirHandle, "", 8192)
	require.NoError(t, err)

	// Assert: Same order both times
	names1 := extractEntryNames(page1.Entries)
	names2 := extractEntryNames(page2.Entries)
	require.Equal(t, names1, names2, "Directory listing order should be consistent")
}

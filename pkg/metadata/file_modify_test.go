package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Move Path Propagation Tests
// ============================================================================

func TestMove_UpdatesFilePath(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	// Create a file in root
	_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "myfile.txt", &metadata.FileAttr{
		Mode: 0644,
	})
	require.NoError(t, err)

	// Create destination directory
	_, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "dest", &metadata.FileAttr{
		Mode: 0755,
	})
	require.NoError(t, err)

	destHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "dest")
	require.NoError(t, err)

	// Get handle before move
	fileHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "myfile.txt")
	require.NoError(t, err)

	// Verify original path
	file, err := fx.store.GetFile(ctx, fileHandle)
	require.NoError(t, err)
	assert.Equal(t, "/myfile.txt", file.Path)

	// Move file to dest directory
	err = fx.service.Move(fx.rootContext(), fx.rootHandle, "myfile.txt", destHandle, "moved.txt")
	require.NoError(t, err)

	// Verify path updated in store
	file, err = fx.store.GetFile(ctx, fileHandle)
	require.NoError(t, err)
	assert.Equal(t, "/dest/moved.txt", file.Path)
}

func TestMove_UpdatesDirectoryPath(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	// Create a directory
	_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "olddir", &metadata.FileAttr{
		Mode: 0755,
	})
	require.NoError(t, err)

	dirHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "olddir")
	require.NoError(t, err)

	// Verify original path
	dir, err := fx.store.GetFile(ctx, dirHandle)
	require.NoError(t, err)
	assert.Equal(t, "/olddir", dir.Path)

	// Rename directory
	err = fx.service.Move(fx.rootContext(), fx.rootHandle, "olddir", fx.rootHandle, "newdir")
	require.NoError(t, err)

	// Verify path updated in store
	dir, err = fx.store.GetFile(ctx, dirHandle)
	require.NoError(t, err)
	assert.Equal(t, "/newdir", dir.Path)
}

func TestMove_UpdatesDescendantPaths(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	// Build tree: /a/b/c/file.txt
	_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "a", &metadata.FileAttr{
		Mode: 0755,
	})
	require.NoError(t, err)

	aHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "a")
	require.NoError(t, err)

	_, err = fx.service.CreateDirectory(fx.rootContext(), aHandle, "b", &metadata.FileAttr{
		Mode: 0755,
	})
	require.NoError(t, err)

	bHandle, err := fx.store.GetChild(ctx, aHandle, "b")
	require.NoError(t, err)

	_, err = fx.service.CreateDirectory(fx.rootContext(), bHandle, "c", &metadata.FileAttr{
		Mode: 0755,
	})
	require.NoError(t, err)

	cHandle, err := fx.store.GetChild(ctx, bHandle, "c")
	require.NoError(t, err)

	_, err = fx.service.CreateFile(fx.rootContext(), cHandle, "file.txt", &metadata.FileAttr{
		Mode: 0644,
	})
	require.NoError(t, err)

	fileHandle, err := fx.store.GetChild(ctx, cHandle, "file.txt")
	require.NoError(t, err)

	// Verify original paths
	aFile, err := fx.store.GetFile(ctx, aHandle)
	require.NoError(t, err)
	assert.Equal(t, "/a", aFile.Path)

	bFile, err := fx.store.GetFile(ctx, bHandle)
	require.NoError(t, err)
	assert.Equal(t, "/a/b", bFile.Path)

	cFile, err := fx.store.GetFile(ctx, cHandle)
	require.NoError(t, err)
	assert.Equal(t, "/a/b/c", cFile.Path)

	leafFile, err := fx.store.GetFile(ctx, fileHandle)
	require.NoError(t, err)
	assert.Equal(t, "/a/b/c/file.txt", leafFile.Path)

	// Rename /a to /x
	err = fx.service.Move(fx.rootContext(), fx.rootHandle, "a", fx.rootHandle, "x")
	require.NoError(t, err)

	// Verify all paths updated recursively
	aFile, err = fx.store.GetFile(ctx, aHandle)
	require.NoError(t, err)
	assert.Equal(t, "/x", aFile.Path, "renamed directory's own path should be /x")

	bFile, err = fx.store.GetFile(ctx, bHandle)
	require.NoError(t, err)
	assert.Equal(t, "/x/b", bFile.Path, "child directory path should be /x/b")

	cFile, err = fx.store.GetFile(ctx, cHandle)
	require.NoError(t, err)
	assert.Equal(t, "/x/b/c", cFile.Path, "nested directory path should be /x/b/c")

	leafFile, err = fx.store.GetFile(ctx, fileHandle)
	require.NoError(t, err)
	assert.Equal(t, "/x/b/c/file.txt", leafFile.Path, "nested file path should be /x/b/c/file.txt")
}

func TestMove_SameDirectoryRename(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	// Create a file
	_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "old.txt", &metadata.FileAttr{
		Mode: 0644,
	})
	require.NoError(t, err)

	fileHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "old.txt")
	require.NoError(t, err)

	// Verify original path
	file, err := fx.store.GetFile(ctx, fileHandle)
	require.NoError(t, err)
	assert.Equal(t, "/old.txt", file.Path)

	// Rename within same directory
	err = fx.service.Move(fx.rootContext(), fx.rootHandle, "old.txt", fx.rootHandle, "new.txt")
	require.NoError(t, err)

	// Verify path updated
	file, err = fx.store.GetFile(ctx, fileHandle)
	require.NoError(t, err)
	assert.Equal(t, "/new.txt", file.Path)
}

func TestMove_EmptyDirectoryRename(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	// Create an empty directory
	_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "empty", &metadata.FileAttr{
		Mode: 0755,
	})
	require.NoError(t, err)

	dirHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "empty")
	require.NoError(t, err)

	// Verify original path
	dir, err := fx.store.GetFile(ctx, dirHandle)
	require.NoError(t, err)
	assert.Equal(t, "/empty", dir.Path)

	// Rename empty directory
	err = fx.service.Move(fx.rootContext(), fx.rootHandle, "empty", fx.rootHandle, "renamed")
	require.NoError(t, err)

	// Verify path updated (no children to traverse)
	dir, err = fx.store.GetFile(ctx, dirHandle)
	require.NoError(t, err)
	assert.Equal(t, "/renamed", dir.Path)
}

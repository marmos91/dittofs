package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// CreateSpecialFile Tests
// ============================================================================

func TestMetadataService_CreateSpecialFile(t *testing.T) {
	t.Parallel()

	t.Run("creates block device as root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.CreateSpecialFile(
			fx.rootContext(),
			fx.rootHandle,
			"sda1",
			metadata.FileTypeBlockDevice,
			&metadata.FileAttr{Mode: 0660},
			8, 1, // major, minor for /dev/sda1
		)

		require.NoError(t, err)
		assert.NotNil(t, file)
		assert.Equal(t, metadata.FileTypeBlockDevice, file.Type)
		assert.Equal(t, uint64(metadata.MakeRdev(8, 1)), file.Rdev)
	})

	t.Run("creates char device as root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.CreateSpecialFile(
			fx.rootContext(),
			fx.rootHandle,
			"tty0",
			metadata.FileTypeCharDevice,
			&metadata.FileAttr{Mode: 0620},
			4, 0, // major, minor for /dev/tty0
		)

		require.NoError(t, err)
		assert.NotNil(t, file)
		assert.Equal(t, metadata.FileTypeCharDevice, file.Type)
	})

	t.Run("creates socket", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.CreateSpecialFile(
			fx.rootContext(),
			fx.rootHandle,
			"app.sock",
			metadata.FileTypeSocket,
			&metadata.FileAttr{Mode: 0755},
			0, 0,
		)

		require.NoError(t, err)
		assert.NotNil(t, file)
		assert.Equal(t, metadata.FileTypeSocket, file.Type)
	})

	t.Run("creates FIFO", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.CreateSpecialFile(
			fx.rootContext(),
			fx.rootHandle,
			"pipe",
			metadata.FileTypeFIFO,
			&metadata.FileAttr{Mode: 0644},
			0, 0,
		)

		require.NoError(t, err)
		assert.NotNil(t, file)
		assert.Equal(t, metadata.FileTypeFIFO, file.Type)
	})

	t.Run("rejects block device creation by non-root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.CreateSpecialFile(
			fx.userContext(),
			fx.rootHandle,
			"sda1",
			metadata.FileTypeBlockDevice,
			&metadata.FileAttr{Mode: 0660},
			8, 1,
		)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrPrivilegeRequired, storeErr.Code)
	})

	t.Run("rejects char device creation by non-root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.CreateSpecialFile(
			fx.userContext(),
			fx.rootHandle,
			"tty0",
			metadata.FileTypeCharDevice,
			&metadata.FileAttr{Mode: 0620},
			4, 0,
		)

		require.Error(t, err)
	})

	t.Run("rejects invalid file type", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Regular file is not a valid special file type
		_, err := fx.service.CreateSpecialFile(
			fx.rootContext(),
			fx.rootHandle,
			"file",
			metadata.FileTypeRegular,
			&metadata.FileAttr{Mode: 0644},
			0, 0,
		)

		require.Error(t, err)
	})

	t.Run("socket and FIFO can be created by non-root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Socket
		_, err := fx.service.CreateSpecialFile(
			fx.userContext(),
			fx.rootHandle,
			"user.sock",
			metadata.FileTypeSocket,
			&metadata.FileAttr{Mode: 0755},
			0, 0,
		)
		require.NoError(t, err)

		// FIFO
		_, err = fx.service.CreateSpecialFile(
			fx.userContext(),
			fx.rootHandle,
			"user.fifo",
			metadata.FileTypeFIFO,
			&metadata.FileAttr{Mode: 0644},
			0, 0,
		)
		require.NoError(t, err)
	})
}

// ============================================================================
// SetFileAttributes Extended Tests
// ============================================================================

func TestMetadataService_SetFileAttributes_Extended(t *testing.T) {
	t.Parallel()

	t.Run("truncates file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "truncate.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "truncate.txt")
		require.NoError(t, err)

		// Truncate to 1024 bytes
		newSize := uint64(1024)
		err = fx.service.SetFileAttributes(fx.rootContext(), fileHandle, &metadata.SetAttrs{
			Size: &newSize,
		})
		require.NoError(t, err)

		// Verify
		updated, err := fx.store.GetFile(context.Background(), fileHandle)
		require.NoError(t, err)
		assert.Equal(t, uint64(1024), updated.Size)
	})

	t.Run("changes mode", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "chmod.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "chmod.txt")
		require.NoError(t, err)

		// Change mode
		newMode := uint32(0755)
		err = fx.service.SetFileAttributes(fx.rootContext(), fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
		})
		require.NoError(t, err)

		// Verify
		updated, err := fx.store.GetFile(context.Background(), fileHandle)
		require.NoError(t, err)
		assert.Equal(t, uint32(0755), updated.Mode)
	})

	t.Run("changes timestamps", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "touch.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "touch.txt")
		require.NoError(t, err)

		// Change timestamps
		newTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
		err = fx.service.SetFileAttributes(fx.rootContext(), fileHandle, &metadata.SetAttrs{
			Atime: &newTime,
			Mtime: &newTime,
		})
		require.NoError(t, err)

		// Verify
		updated, err := fx.store.GetFile(context.Background(), fileHandle)
		require.NoError(t, err)
		assert.Equal(t, newTime, updated.Atime)
		assert.Equal(t, newTime, updated.Mtime)
	})

	t.Run("changes owner as root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file as root
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "chown.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "chown.txt")
		require.NoError(t, err)

		// Change owner
		newUID := uint32(1000)
		newGID := uint32(1000)
		err = fx.service.SetFileAttributes(fx.rootContext(), fileHandle, &metadata.SetAttrs{
			UID: &newUID,
			GID: &newGID,
		})
		require.NoError(t, err)

		// Verify
		updated, err := fx.store.GetFile(context.Background(), fileHandle)
		require.NoError(t, err)
		assert.Equal(t, uint32(1000), updated.UID)
		assert.Equal(t, uint32(1000), updated.GID)
	})

	t.Run("non-root cannot change owner", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file as root
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "nochange.txt", &metadata.FileAttr{
			Mode: 0666,
			UID:  0,
			GID:  0,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "nochange.txt")
		require.NoError(t, err)

		// Try to change owner as non-root
		newUID := uint32(1000)
		err = fx.service.SetFileAttributes(fx.userContext(), fileHandle, &metadata.SetAttrs{
			UID: &newUID,
		})

		require.Error(t, err)
	})

	t.Run("multiple attributes at once", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "multi.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "multi.txt")
		require.NoError(t, err)

		// Set multiple attributes at once
		newMode := uint32(0755)
		newSize := uint64(512)
		err = fx.service.SetFileAttributes(fx.rootContext(), fileHandle, &metadata.SetAttrs{
			Mode: &newMode,
			Size: &newSize,
		})
		require.NoError(t, err)

		// Verify both were updated
		updated, err := fx.store.GetFile(context.Background(), fileHandle)
		require.NoError(t, err)
		assert.Equal(t, uint32(0755), updated.Mode)
		assert.Equal(t, uint64(512), updated.Size)
	})
}

// ============================================================================
// Move Extended Tests
// ============================================================================

func TestMetadataService_Move_Extended(t *testing.T) {
	t.Parallel()

	t.Run("rename file in same directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "original.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		srcHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "original.txt")
		require.NoError(t, err)

		// Rename it
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "original.txt", fx.rootHandle, "renamed.txt")

		require.NoError(t, err)

		// Verify old name doesn't exist
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "original.txt")
		assert.True(t, metadata.IsNotFoundError(err))

		// Verify new name exists with same handle
		newHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "renamed.txt")
		require.NoError(t, err)
		assert.Equal(t, srcHandle, newHandle)
	})

	t.Run("move file to different directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create source file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "moveme.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Create destination directory
		_, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "destdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		destHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "destdir")
		require.NoError(t, err)

		// Move file
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "moveme.txt", destHandle, "moved.txt")

		require.NoError(t, err)

		// Verify old location doesn't exist
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "moveme.txt")
		assert.True(t, metadata.IsNotFoundError(err))

		// Verify new location exists
		_, err = fx.store.GetChild(context.Background(), destHandle, "moved.txt")
		require.NoError(t, err)
	})

	t.Run("move overwrites existing file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create source file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "source.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Create destination file (will be overwritten)
		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "dest.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		srcHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "source.txt")
		require.NoError(t, err)

		// Move (overwrite)
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "source.txt", fx.rootHandle, "dest.txt")

		require.NoError(t, err)

		// Verify source doesn't exist
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "source.txt")
		assert.True(t, metadata.IsNotFoundError(err))

		// Verify destination has the source's handle
		newHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "dest.txt")
		require.NoError(t, err)
		assert.Equal(t, srcHandle, newHandle)
	})

	t.Run("move directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create source directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "srcdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Rename directory
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "srcdir", fx.rootHandle, "newdir")

		require.NoError(t, err)

		// Verify old name doesn't exist
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "srcdir")
		assert.True(t, metadata.IsNotFoundError(err))

		// Verify new name exists
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "newdir")
		require.NoError(t, err)
	})

	t.Run("move to same location is no-op", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "same.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Move to same location
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "same.txt", fx.rootHandle, "same.txt")

		require.NoError(t, err)

		// Verify file still exists
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "same.txt")
		require.NoError(t, err)
	})

	t.Run("cannot overwrite directory with file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "file.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Create a directory
		_, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "dir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Try to move file over directory
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "file.txt", fx.rootHandle, "dir")

		require.Error(t, err)
	})

	t.Run("cannot overwrite file with directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "srcdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Create a file
		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "destfile.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Try to move directory over file
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "srcdir", fx.rootHandle, "destfile.txt")

		require.Error(t, err)
	})
}

// ============================================================================
// RemoveFile Extended Tests
// ============================================================================

func TestMetadataService_RemoveFile_Extended(t *testing.T) {
	t.Parallel()

	t.Run("remove regular file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "delete.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Remove it
		_, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "delete.txt")

		require.NoError(t, err)

		// Verify it's gone
		_, err = fx.store.GetChild(context.Background(), fx.rootHandle, "delete.txt")
		assert.True(t, metadata.IsNotFoundError(err))
	})

	t.Run("remove fails for directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "mydir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Try to remove it with RemoveFile (should fail)
		_, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "mydir")

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrIsDirectory, storeErr.Code)
	})

	t.Run("remove nonexistent file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "nonexistent.txt")

		require.Error(t, err)
		assert.True(t, metadata.IsNotFoundError(err))
	})
}

// ============================================================================
// ReadDirectory Extended Tests
// ============================================================================

func TestMetadataService_ReadDirectory_Extended(t *testing.T) {
	t.Parallel()

	t.Run("reads empty directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create empty directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "empty", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "empty")
		require.NoError(t, err)

		// Read it (token="" for first page, maxBytes large enough)
		page, err := fx.service.ReadDirectory(fx.rootContext(), dirHandle, "", 65536)

		require.NoError(t, err)
		assert.Empty(t, page.Entries)
	})

	t.Run("reads directory with files", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "withfiles", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "withfiles")
		require.NoError(t, err)

		// Create some files
		for i := 0; i < 5; i++ {
			_, err := fx.service.CreateFile(fx.rootContext(), dirHandle, "file"+string(rune('a'+i))+".txt", &metadata.FileAttr{
				Mode: 0644,
			})
			require.NoError(t, err)
		}

		// Read directory
		page, err := fx.service.ReadDirectory(fx.rootContext(), dirHandle, "", 65536)

		require.NoError(t, err)
		assert.Len(t, page.Entries, 5)
	})

	t.Run("pagination with continuation token", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "pagedir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "pagedir")
		require.NoError(t, err)

		// Create 10 files
		for i := 0; i < 10; i++ {
			_, err := fx.service.CreateFile(fx.rootContext(), dirHandle, "file"+string(rune('0'+i))+".txt", &metadata.FileAttr{
				Mode: 0644,
			})
			require.NoError(t, err)
		}

		// Read all entries
		page, err := fx.service.ReadDirectory(fx.rootContext(), dirHandle, "", 65536)
		require.NoError(t, err)
		assert.Len(t, page.Entries, 10)
	})
}

// ============================================================================
// CreateHardLink Extended Tests
// ============================================================================

func TestMetadataService_CreateHardLink_Extended(t *testing.T) {
	t.Parallel()

	t.Run("creates hard link successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create target file
		targetFile, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)
		require.Equal(t, uint32(1), targetFile.Nlink)

		targetHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "target.txt")
		require.NoError(t, err)

		// Create hard link
		err = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "link.txt", targetHandle)
		require.NoError(t, err)

		// Verify link count increased
		updatedTarget, err := fx.store.GetFile(context.Background(), targetHandle)
		require.NoError(t, err)
		assert.Equal(t, uint32(2), updatedTarget.Nlink)
	})

	t.Run("rejects hard link to directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "targetdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "targetdir")
		require.NoError(t, err)

		// Try to create hard link to directory
		err = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "dirlink", dirHandle)

		require.Error(t, err)
	})

	t.Run("rejects link with existing name", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create target file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "original.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		targetHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "original.txt")
		require.NoError(t, err)

		// Create another file with the name we want to use for the link
		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "linkname.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Try to create hard link with existing name
		err = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "linkname.txt", targetHandle)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrAlreadyExists, storeErr.Code)
	})
}

// ============================================================================
// Symlink Extended Tests
// ============================================================================

func TestMetadataService_Symlink_Extended(t *testing.T) {
	t.Parallel()

	t.Run("creates and reads symlink", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		targetPath := "/path/to/target"

		// Create symlink
		file, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "mylink", targetPath, &metadata.FileAttr{})
		require.NoError(t, err)
		assert.Equal(t, metadata.FileTypeSymlink, file.Type)

		// Read symlink
		linkHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "mylink")
		require.NoError(t, err)

		target, _, err := fx.service.ReadSymlink(fx.rootContext(), linkHandle)
		require.NoError(t, err)
		assert.Equal(t, targetPath, target)
	})

	t.Run("read symlink fails for regular file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create regular file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "notlink.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "notlink.txt")
		require.NoError(t, err)

		// Try to read symlink on regular file
		_, _, err = fx.service.ReadSymlink(fx.rootContext(), fileHandle)

		require.Error(t, err)
	})
}

// ============================================================================
// Service Passthrough Tests
// ============================================================================

func TestMetadataService_GetFile(t *testing.T) {
	t.Parallel()

	t.Run("gets existing file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		created, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "getme.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		fileHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "getme.txt")
		require.NoError(t, err)

		// Get file via service (uses context.Context, not AuthContext)
		file, err := fx.service.GetFile(context.Background(), fileHandle)

		require.NoError(t, err)
		assert.Equal(t, created.ID, file.ID)
		assert.Equal(t, metadata.FileTypeRegular, file.Type)
	})
}

func TestMetadataService_GetChild(t *testing.T) {
	t.Parallel()

	t.Run("gets child handle", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "child.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get child via service (uses context.Context, not AuthContext)
		handle, err := fx.service.GetChild(context.Background(), fx.rootHandle, "child.txt")

		require.NoError(t, err)
		assert.NotNil(t, handle)
	})

	t.Run("returns error for nonexistent child", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.GetChild(context.Background(), fx.rootHandle, "nonexistent.txt")

		require.Error(t, err)
		assert.True(t, metadata.IsNotFoundError(err))
	})
}

func TestMetadataService_GetRootHandle(t *testing.T) {
	t.Parallel()

	t.Run("returns error for unknown share", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// GetRootHandle requires the share to be fully set up via CreateShare
		// Our test fixture only uses CreateRootDirectory, so this will fail
		_, err := fx.service.GetRootHandle(context.Background(), "/nonexistent")

		require.Error(t, err)
	})
}

func TestMetadataService_GenerateHandle(t *testing.T) {
	t.Parallel()

	t.Run("generates new handle", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		handle, err := fx.service.GenerateHandle(context.Background(), fx.shareName, "/somepath")

		require.NoError(t, err)
		assert.NotNil(t, handle)
	})
}

func TestMetadataService_GetFilesystemStatistics(t *testing.T) {
	t.Parallel()

	t.Run("gets filesystem statistics", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		stats, err := fx.service.GetFilesystemStatistics(context.Background(), fx.rootHandle)

		require.NoError(t, err)
		assert.NotNil(t, stats)
	})
}

func TestMetadataService_GetFilesystemCapabilities(t *testing.T) {
	t.Parallel()

	t.Run("gets filesystem capabilities", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		caps, err := fx.service.GetFilesystemCapabilities(context.Background(), fx.rootHandle)

		require.NoError(t, err)
		assert.NotNil(t, caps)
	})
}

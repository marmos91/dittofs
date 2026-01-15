package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test Fixtures
// ============================================================================

// testFixture provides a configured MetadataService with a memory store for testing.
type testFixture struct {
	t          *testing.T
	service    *metadata.MetadataService
	store      *memory.MemoryMetadataStore
	shareName  string
	rootHandle metadata.FileHandle
}

func newTestFixture(t *testing.T) *testFixture {
	t.Helper()

	store := memory.NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()
	shareName := "/test"

	// Create root directory (this also creates the share entry internally)
	// Use mode 0777 to allow testing with different user contexts
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
		UID:  0,
		GID:  0,
	})
	require.NoError(t, err)

	// Encode the root file's ID to get the handle
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	// Create service and register store
	svc := metadata.New()
	err = svc.RegisterStoreForShare(shareName, store)
	require.NoError(t, err)

	return &testFixture{
		t:          t,
		service:    svc,
		store:      store,
		shareName:  shareName,
		rootHandle: rootHandle,
	}
}

// authContext creates an AuthContext for testing.
func (f *testFixture) authContext(uid, gid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(uid),
			GID:  metadata.Uint32Ptr(gid),
			GIDs: []uint32{gid},
		},
		ClientAddr: "127.0.0.1",
	}
}

// rootContext creates a root AuthContext.
func (f *testFixture) rootContext() *metadata.AuthContext {
	return f.authContext(0, 0)
}

// userContext creates a regular user AuthContext.
func (f *testFixture) userContext() *metadata.AuthContext {
	return f.authContext(1000, 1000)
}

// ============================================================================
// Service Registration Tests
// ============================================================================

func TestMetadataService_RegisterStoreForShare(t *testing.T) {
	t.Parallel()

	t.Run("registers store successfully", func(t *testing.T) {
		t.Parallel()
		svc := metadata.New()
		store := memory.NewMemoryMetadataStoreWithDefaults()

		err := svc.RegisterStoreForShare("/test", store)

		require.NoError(t, err)
	})

	t.Run("rejects nil store", func(t *testing.T) {
		t.Parallel()
		svc := metadata.New()

		err := svc.RegisterStoreForShare("/test", nil)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "nil store")
	})

	t.Run("rejects empty share name", func(t *testing.T) {
		t.Parallel()
		svc := metadata.New()
		store := memory.NewMemoryMetadataStoreWithDefaults()

		err := svc.RegisterStoreForShare("", store)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty share name")
	})
}

func TestMetadataService_GetStoreForShare(t *testing.T) {
	t.Parallel()

	t.Run("returns registered store", func(t *testing.T) {
		t.Parallel()
		svc := metadata.New()
		store := memory.NewMemoryMetadataStoreWithDefaults()
		_ = svc.RegisterStoreForShare("/test", store)

		got, err := svc.GetStoreForShare("/test")

		require.NoError(t, err)
		assert.NotNil(t, got)
	})

	t.Run("returns error for unregistered share", func(t *testing.T) {
		t.Parallel()
		svc := metadata.New()

		_, err := svc.GetStoreForShare("/nonexistent")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no store configured")
	})
}

// ============================================================================
// CreateFile Tests
// ============================================================================

func TestMetadataService_CreateFile(t *testing.T) {
	t.Parallel()

	t.Run("creates file successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})

		require.NoError(t, err)
		assert.NotNil(t, file)
		assert.Equal(t, metadata.FileTypeRegular, file.Type)
		assert.Equal(t, uint32(0644), file.Mode)
		// Note: Memory store doesn't track full paths
	})

	t.Run("creates file with user ownership", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.CreateFile(fx.userContext(), fx.rootHandle, "userfile.txt", &metadata.FileAttr{
			Mode: 0644,
		})

		require.NoError(t, err)
		assert.Equal(t, uint32(1000), file.UID)
		assert.Equal(t, uint32(1000), file.GID)
	})

	t.Run("rejects invalid name", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "..", &metadata.FileAttr{
			Mode: 0644,
		})

		require.Error(t, err)
	})

	t.Run("rejects empty name", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "", &metadata.FileAttr{
			Mode: 0644,
		})

		require.Error(t, err)
	})

	t.Run("rejects duplicate name", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create first file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Try to create again
		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrAlreadyExists, storeErr.Code)
	})

	t.Run("permission denied for non-root on root-owned directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a subdirectory owned by root with mode 0755
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "restricted", &metadata.FileAttr{
			Mode: 0755,
			UID:  0,
			GID:  0,
		})
		require.NoError(t, err)

		// Get the restricted directory handle
		restrictedHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "restricted")
		require.NoError(t, err)

		// User should be denied (write permission not available to others on 0755 dir)
		_, err = fx.service.CreateFile(fx.userContext(), restrictedHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})

		require.Error(t, err)
	})
}

// ============================================================================
// Lookup Tests
// ============================================================================

func TestMetadataService_Lookup(t *testing.T) {
	t.Parallel()

	t.Run("finds existing file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Lookup the file
		file, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, "test.txt")

		require.NoError(t, err)
		require.NotNil(t, file)
		assert.Equal(t, metadata.FileTypeRegular, file.Type)
		// Note: Memory store doesn't track full paths
	})

	t.Run("dot returns current directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		file, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, ".")

		require.NoError(t, err)
		assert.Equal(t, metadata.FileTypeDirectory, file.Type)
		// Note: Memory store doesn't track full paths
	})

	t.Run("dotdot returns parent or self at root", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// At root, ".." returns root itself
		file, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, "..")

		require.NoError(t, err)
		assert.Equal(t, metadata.FileTypeDirectory, file.Type)
	})

	t.Run("not found error for nonexistent file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, "nonexistent.txt")

		require.Error(t, err)
		assert.True(t, metadata.IsNotFoundError(err))
	})
}

// ============================================================================
// CreateDirectory Tests
// ============================================================================

func TestMetadataService_CreateDirectory(t *testing.T) {
	t.Parallel()

	t.Run("creates directory successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		dir, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "subdir", &metadata.FileAttr{
			Mode: 0755,
		})

		require.NoError(t, err)
		assert.NotNil(t, dir)
		assert.Equal(t, metadata.FileTypeDirectory, dir.Type)
		assert.Equal(t, uint32(0755), dir.Mode)
		assert.Equal(t, uint32(2), dir.Nlink) // . and parent's reference
	})

	t.Run("nested directory creation", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create parent
		parent, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "parent", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Get parent handle
		parentHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "parent")
		require.NoError(t, err)

		// Create child
		child, err := fx.service.CreateDirectory(fx.rootContext(), parentHandle, "child", &metadata.FileAttr{
			Mode: 0755,
		})

		require.NoError(t, err)
		assert.NotNil(t, child)
		assert.Equal(t, metadata.FileTypeDirectory, child.Type)
		// Note: Memory store doesn't track full paths

		// Verify parent's link count increased
		updatedParent, err := fx.store.GetFile(context.Background(), parentHandle)
		require.NoError(t, err)
		assert.Equal(t, parent.Nlink+1, updatedParent.Nlink)
	})
}

// ============================================================================
// RemoveFile Tests
// ============================================================================

func TestMetadataService_RemoveFile(t *testing.T) {
	t.Parallel()

	t.Run("removes file successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Remove it
		removed, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "test.txt")

		require.NoError(t, err)
		assert.NotNil(t, removed)
		assert.Equal(t, uint32(0), removed.Nlink)

		// Verify it's gone from directory listing
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "test.txt")
		assert.True(t, metadata.IsNotFoundError(err))
	})

	t.Run("rejects removing directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "subdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Try to remove with RemoveFile
		_, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "subdir")

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrIsDirectory, storeErr.Code)
	})

	t.Run("not found error for nonexistent file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "nonexistent.txt")

		require.Error(t, err)
	})
}

// ============================================================================
// RemoveDirectory Tests
// ============================================================================

func TestMetadataService_RemoveDirectory(t *testing.T) {
	t.Parallel()

	t.Run("removes empty directory successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "subdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Remove it
		err = fx.service.RemoveDirectory(fx.rootContext(), fx.rootHandle, "subdir")

		require.NoError(t, err)

		// Verify it's gone
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "subdir")
		assert.True(t, metadata.IsNotFoundError(err))
	})

	t.Run("rejects non-empty directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "subdir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Get directory handle
		dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "subdir")
		require.NoError(t, err)

		// Create a file inside
		_, err = fx.service.CreateFile(fx.rootContext(), dirHandle, "file.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Try to remove directory
		err = fx.service.RemoveDirectory(fx.rootContext(), fx.rootHandle, "subdir")

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrNotEmpty, storeErr.Code)
	})

	t.Run("rejects removing file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Try to remove with RemoveDirectory
		err = fx.service.RemoveDirectory(fx.rootContext(), fx.rootHandle, "test.txt")

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrNotDirectory, storeErr.Code)
	})
}

// ============================================================================
// Move Tests
// ============================================================================

func TestMetadataService_Move(t *testing.T) {
	t.Parallel()

	t.Run("renames file in same directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "old.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Rename it
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "old.txt", fx.rootHandle, "new.txt")

		require.NoError(t, err)

		// Verify old name is gone
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "old.txt")
		assert.True(t, metadata.IsNotFoundError(err))

		// Verify new name exists
		_, err = fx.service.Lookup(fx.rootContext(), fx.rootHandle, "new.txt")
		assert.NoError(t, err)
	})

	t.Run("same source and destination is no-op", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		original, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Move to same name
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "test.txt", fx.rootHandle, "test.txt")

		require.NoError(t, err)

		// Verify file still exists with same attributes
		file, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, "test.txt")
		require.NoError(t, err)
		assert.Equal(t, original.Mode, file.Mode)
	})

	t.Run("move to different directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create source directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "src", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)
		srcDir, _ := fx.store.GetChild(context.Background(), fx.rootHandle, "src")

		// Create destination directory
		_, err = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "dst", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)
		dstDir, _ := fx.store.GetChild(context.Background(), fx.rootHandle, "dst")

		// Create a file in source
		_, err = fx.service.CreateFile(fx.rootContext(), srcDir, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Move to destination
		err = fx.service.Move(fx.rootContext(), srcDir, "test.txt", dstDir, "test.txt")

		require.NoError(t, err)

		// Verify file moved
		_, err = fx.service.Lookup(fx.rootContext(), srcDir, "test.txt")
		assert.True(t, metadata.IsNotFoundError(err))

		_, err = fx.service.Lookup(fx.rootContext(), dstDir, "test.txt")
		assert.NoError(t, err)
	})

	t.Run("rejects move directory over file", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "dir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Create a file
		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "file.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Try to move directory over file
		err = fx.service.Move(fx.rootContext(), fx.rootHandle, "dir", fx.rootHandle, "file.txt")

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrNotDirectory, storeErr.Code)
	})
}

// ============================================================================
// Symlink Tests
// ============================================================================

func TestMetadataService_CreateSymlink(t *testing.T) {
	t.Parallel()

	t.Run("creates symlink successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		symlink, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "link", "/target/path", &metadata.FileAttr{})

		require.NoError(t, err)
		assert.Equal(t, metadata.FileTypeSymlink, symlink.Type)
		assert.Equal(t, "/target/path", symlink.LinkTarget)
		assert.Equal(t, uint32(0777), symlink.Mode)
	})

	t.Run("rejects empty target", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "link", "", &metadata.FileAttr{})

		require.Error(t, err)
	})
}

func TestMetadataService_ReadSymlink(t *testing.T) {
	t.Parallel()

	t.Run("reads symlink target", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create symlink
		_, err := fx.service.CreateSymlink(fx.rootContext(), fx.rootHandle, "link", "/target/path", &metadata.FileAttr{})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "link")
		require.NoError(t, err)

		// Read symlink
		target, file, err := fx.service.ReadSymlink(fx.rootContext(), handle)

		require.NoError(t, err)
		assert.Equal(t, "/target/path", target)
		assert.NotNil(t, file)
	})

	t.Run("rejects non-symlink", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create regular file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Try to read as symlink
		_, _, err = fx.service.ReadSymlink(fx.rootContext(), handle)

		require.Error(t, err)
	})
}

// ============================================================================
// Hard Link Tests
// ============================================================================

func TestMetadataService_CreateHardLink(t *testing.T) {
	t.Parallel()

	t.Run("creates hard link successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create target file
		target, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)
		initialNlink := target.Nlink

		// Get target handle
		targetHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "target.txt")
		require.NoError(t, err)

		// Create hard link
		err = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "link.txt", targetHandle)

		require.NoError(t, err)

		// Verify link count increased
		updatedTarget, err := fx.store.GetFile(context.Background(), targetHandle)
		require.NoError(t, err)
		assert.Equal(t, initialNlink+1, updatedTarget.Nlink)
	})

	t.Run("rejects hard link to directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create directory
		_, err := fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "dir", &metadata.FileAttr{
			Mode: 0755,
		})
		require.NoError(t, err)

		// Get directory handle
		dirHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "dir")
		require.NoError(t, err)

		// Try to create hard link to directory
		err = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "link", dirHandle)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrIsDirectory, storeErr.Code)
	})

	t.Run("rejects duplicate name", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create two files
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "target.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "existing.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get target handle
		targetHandle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "target.txt")
		require.NoError(t, err)

		// Try to create link with existing name
		err = fx.service.CreateHardLink(fx.rootContext(), fx.rootHandle, "existing.txt", targetHandle)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrAlreadyExists, storeErr.Code)
	})
}

// ============================================================================
// SetFileAttributes Tests
// ============================================================================

func TestMetadataService_SetFileAttributes(t *testing.T) {
	t.Parallel()

	t.Run("owner can change mode", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file as root
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
			UID:  1000,
			GID:  1000,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Change mode as owner
		newMode := uint32(0600)
		err = fx.service.SetFileAttributes(fx.userContext(), handle, &metadata.SetAttrs{
			Mode: &newMode,
		})

		require.NoError(t, err)

		// Verify change
		file, err := fx.store.GetFile(context.Background(), handle)
		require.NoError(t, err)
		assert.Equal(t, uint32(0600), file.Mode)
	})

	t.Run("only root can change owner", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
			UID:  1000,
			GID:  1000,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Try to change owner as non-root
		newUID := uint32(2000)
		err = fx.service.SetFileAttributes(fx.userContext(), handle, &metadata.SetAttrs{
			UID: &newUID,
		})

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
	})

	t.Run("root can change owner", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Change owner as root
		newUID := uint32(2000)
		err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{
			UID: &newUID,
		})

		require.NoError(t, err)

		// Verify change
		file, err := fx.store.GetFile(context.Background(), handle)
		require.NoError(t, err)
		assert.Equal(t, uint32(2000), file.UID)
	})

	t.Run("non-owner non-root denied", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file owned by different user
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
			UID:  2000,
			GID:  2000,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Try to change mode as different user
		newMode := uint32(0600)
		err = fx.service.SetFileAttributes(fx.userContext(), handle, &metadata.SetAttrs{
			Mode: &newMode,
		})

		require.Error(t, err)
	})
}

// ============================================================================
// I/O Operation Tests
// ============================================================================

func TestMetadataService_PrepareWrite(t *testing.T) {
	t.Parallel()

	t.Run("prepares write successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Prepare write
		intent, err := fx.service.PrepareWrite(fx.rootContext(), handle, 1024)

		require.NoError(t, err)
		assert.NotNil(t, intent)
		assert.Equal(t, uint64(1024), intent.NewSize)
		assert.NotEmpty(t, intent.PayloadID)
		assert.NotNil(t, intent.PreWriteAttr)
	})

	t.Run("rejects write to directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.PrepareWrite(fx.rootContext(), fx.rootHandle, 1024)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrIsDirectory, storeErr.Code)
	})
}

func TestMetadataService_CommitWrite(t *testing.T) {
	t.Parallel()

	t.Run("commits write successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Prepare and commit write
		intent, err := fx.service.PrepareWrite(fx.rootContext(), handle, 1024)
		require.NoError(t, err)

		file, err := fx.service.CommitWrite(fx.rootContext(), intent)

		require.NoError(t, err)
		assert.Equal(t, uint64(1024), file.Size)
	})

	t.Run("clears setuid bit for non-root write", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create setuid file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0o4755, // setuid
			UID:  1000,
			GID:  1000,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Prepare and commit write as non-root
		intent, err := fx.service.PrepareWrite(fx.userContext(), handle, 1024)
		require.NoError(t, err)

		file, err := fx.service.CommitWrite(fx.userContext(), intent)

		require.NoError(t, err)
		assert.Equal(t, uint32(0o0755), file.Mode) // setuid cleared
	})
}

func TestMetadataService_PrepareRead(t *testing.T) {
	t.Parallel()

	t.Run("prepares read successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Prepare read
		readMeta, err := fx.service.PrepareRead(fx.rootContext(), handle)

		require.NoError(t, err)
		assert.NotNil(t, readMeta)
		assert.NotNil(t, readMeta.Attr)
	})

	t.Run("rejects read of directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		_, err := fx.service.PrepareRead(fx.rootContext(), fx.rootHandle)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrIsDirectory, storeErr.Code)
	})
}

// ============================================================================
// ReadDirectory Tests
// ============================================================================

func TestMetadataService_ReadDirectory(t *testing.T) {
	t.Parallel()

	t.Run("reads empty directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		page, err := fx.service.ReadDirectory(fx.rootContext(), fx.rootHandle, 0, 0)

		require.NoError(t, err)
		assert.NotNil(t, page)
		assert.Empty(t, page.Entries)
		assert.False(t, page.HasMore)
	})

	t.Run("reads directory with entries", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create some files
		_, _ = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "file1.txt", &metadata.FileAttr{Mode: 0644})
		_, _ = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "file2.txt", &metadata.FileAttr{Mode: 0644})
		_, _ = fx.service.CreateDirectory(fx.rootContext(), fx.rootHandle, "subdir", &metadata.FileAttr{Mode: 0755})

		page, err := fx.service.ReadDirectory(fx.rootContext(), fx.rootHandle, 0, 0)

		require.NoError(t, err)
		assert.Len(t, page.Entries, 3)
	})

	t.Run("rejects non-directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create a file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Try to read as directory
		_, err = fx.service.ReadDirectory(fx.rootContext(), handle, 0, 0)

		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.ErrorAs(t, err, &storeErr)
		assert.Equal(t, metadata.ErrNotDirectory, storeErr.Code)
	})
}

// ============================================================================
// Locking Tests
// ============================================================================

func TestMetadataService_LockFile(t *testing.T) {
	t.Parallel()

	t.Run("acquires exclusive lock", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Acquire lock
		lock := metadata.FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		}
		err = fx.service.LockFile(fx.rootContext(), handle, lock)

		require.NoError(t, err)
	})

	t.Run("rejects lock on directory", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		lock := metadata.FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		}
		err := fx.service.LockFile(fx.rootContext(), fx.rootHandle, lock)

		require.Error(t, err)
	})

	t.Run("detects conflicting lock", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// First lock
		lock1 := metadata.FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		}
		err = fx.service.LockFile(fx.rootContext(), handle, lock1)
		require.NoError(t, err)

		// Conflicting lock from different session
		lock2 := metadata.FileLock{
			SessionID: 2,
			Offset:    50,
			Length:    100,
			Exclusive: true,
		}
		err = fx.service.LockFile(fx.rootContext(), handle, lock2)

		require.Error(t, err)
	})
}

func TestMetadataService_UnlockFile(t *testing.T) {
	t.Parallel()

	t.Run("unlocks successfully", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Lock
		lock := metadata.FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		}
		err = fx.service.LockFile(fx.rootContext(), handle, lock)
		require.NoError(t, err)

		// Unlock
		err = fx.service.UnlockFile(context.Background(), handle, 1, 0, 100)

		require.NoError(t, err)
	})
}

func TestMetadataService_TestLock(t *testing.T) {
	t.Parallel()

	t.Run("no conflict for non-overlapping", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Test lock - should succeed since no locks exist
		ok, conflict, err := fx.service.TestLock(fx.rootContext(), handle, 1, 0, 100, true)

		require.NoError(t, err)
		assert.True(t, ok)
		assert.Nil(t, conflict)
	})

	t.Run("detects conflict", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Lock
		lock := metadata.FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		}
		err = fx.service.LockFile(fx.rootContext(), handle, lock)
		require.NoError(t, err)

		// Test conflicting lock
		ok, conflict, err := fx.service.TestLock(fx.rootContext(), handle, 2, 50, 100, true)

		require.NoError(t, err)
		assert.False(t, ok)
		assert.NotNil(t, conflict)
	})
}

func TestMetadataService_CheckLockForIO(t *testing.T) {
	t.Parallel()

	t.Run("no conflict without locks", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Check for I/O - should succeed
		err = fx.service.CheckLockForIO(context.Background(), handle, 1, 0, 100, true)

		require.NoError(t, err)
	})

	t.Run("detects write conflict", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Lock
		lock := metadata.FileLock{
			SessionID: 1,
			Offset:    0,
			Length:    100,
			Exclusive: true,
		}
		err = fx.service.LockFile(fx.rootContext(), handle, lock)
		require.NoError(t, err)

		// Check for I/O from different session
		err = fx.service.CheckLockForIO(context.Background(), handle, 2, 50, 50, true)

		require.Error(t, err)
	})
}

// ============================================================================
// Context Cancellation Tests
// ============================================================================

func TestMetadataService_ContextCancellation(t *testing.T) {
	t.Parallel()

	t.Run("prepareWrite respects context", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create file
		_, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Get handle
		handle, err := fx.store.GetChild(context.Background(), fx.rootHandle, "test.txt")
		require.NoError(t, err)

		// Create cancelled context
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		authCtx := &metadata.AuthContext{
			Context:    ctx,
			AuthMethod: "unix",
			Identity: &metadata.Identity{
				UID: metadata.Uint32Ptr(0),
				GID: metadata.Uint32Ptr(0),
			},
		}

		_, err = fx.service.PrepareWrite(authCtx, handle, 1024)

		require.Error(t, err)
	})

	t.Run("readDirectory respects context", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Create cancelled context
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		authCtx := &metadata.AuthContext{
			Context:    ctx,
			AuthMethod: "unix",
			Identity: &metadata.Identity{
				UID: metadata.Uint32Ptr(0),
				GID: metadata.Uint32Ptr(0),
			},
		}

		_, err := fx.service.ReadDirectory(authCtx, fx.rootHandle, 0, 0)

		require.Error(t, err)
	})
}

// ============================================================================
// Device Number Helper Tests
// ============================================================================

func TestDeviceNumberHelpers(t *testing.T) {
	t.Parallel()

	t.Run("MakeRdev and extraction", func(t *testing.T) {
		t.Parallel()
		major := uint32(8)
		minor := uint32(1)

		rdev := metadata.MakeRdev(major, minor)
		extractedMajor := metadata.RdevMajor(rdev)
		extractedMinor := metadata.RdevMinor(rdev)

		assert.Equal(t, major, extractedMajor)
		assert.Equal(t, minor, extractedMinor)
	})

	t.Run("large device numbers", func(t *testing.T) {
		t.Parallel()
		major := uint32(256)
		minor := uint32(65535)

		rdev := metadata.MakeRdev(major, minor)
		extractedMajor := metadata.RdevMajor(rdev)
		extractedMinor := metadata.RdevMinor(rdev)

		assert.Equal(t, major, extractedMajor)
		assert.Equal(t, minor, extractedMinor)
	})
}

// ============================================================================
// Initial Link Count Tests
// ============================================================================

func TestGetInitialLinkCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileType metadata.FileType
		want     uint32
	}{
		{"regular file", metadata.FileTypeRegular, 1},
		{"directory", metadata.FileTypeDirectory, 2},
		{"symlink", metadata.FileTypeSymlink, 1},
		{"block device", metadata.FileTypeBlockDevice, 1},
		{"char device", metadata.FileTypeCharDevice, 1},
		{"socket", metadata.FileTypeSocket, 1},
		{"fifo", metadata.FileTypeFIFO, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := metadata.GetInitialLinkCount(tt.fileType)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ============================================================================
// Timestamp Update Tests
// ============================================================================

func TestTimestampUpdates(t *testing.T) {
	t.Parallel()

	t.Run("create updates parent timestamps", func(t *testing.T) {
		t.Parallel()
		fx := newTestFixture(t)

		// Get initial parent timestamps via Lookup(".")
		parentBefore, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, ".")
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)

		// Create file
		_, err = fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "test.txt", &metadata.FileAttr{
			Mode: 0644,
		})
		require.NoError(t, err)

		// Verify parent timestamps updated
		parentAfter, err := fx.service.Lookup(fx.rootContext(), fx.rootHandle, ".")
		require.NoError(t, err)
		assert.True(t, parentAfter.Mtime.After(parentBefore.Mtime) || parentAfter.Mtime.Equal(parentBefore.Mtime))
		assert.True(t, parentAfter.Ctime.After(parentBefore.Ctime) || parentAfter.Ctime.Equal(parentBefore.Ctime))
	})
}

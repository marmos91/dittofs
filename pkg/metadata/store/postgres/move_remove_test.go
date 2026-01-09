package postgres

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestMove_SameDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "oldname.txt")

	// Move/rename in same directory
	err := store.Move(ctx, rootHandle, "oldname.txt", rootHandle, "newname.txt")
	if err != nil {
		t.Fatalf("Move failed: %v", err)
	}

	// Old name should not exist
	_, err = store.Lookup(ctx, rootHandle, "oldname.txt")
	assertError(t, err, metadata.ErrNotFound, "old name should not exist")

	// New name should exist
	moved, err := store.Lookup(ctx, rootHandle, "newname.txt")
	if err != nil {
		t.Fatalf("Lookup new name failed: %v", err)
	}

	// File ID should be the same
	if moved.ID != file.ID {
		t.Errorf("expected same file ID, got different IDs")
	}

	// Path should be updated
	if moved.Path != "/newname.txt" {
		t.Errorf("expected path '/newname.txt', got '%s'", moved.Path)
	}
}

func TestMove_CrossDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create source and destination directories
	srcDir := createTestDirectory(t, store, ctx, rootHandle, "src")
	srcHandle := getFileHandle(srcDir)

	dstDir := createTestDirectory(t, store, ctx, rootHandle, "dst")
	dstHandle := getFileHandle(dstDir)

	// Create file in source
	file := createTestFile(t, store, ctx, srcHandle, "file.txt")

	// Move to destination
	err := store.Move(ctx, srcHandle, "file.txt", dstHandle, "file.txt")
	if err != nil {
		t.Fatalf("Move failed: %v", err)
	}

	// Should not exist in source
	_, err = store.Lookup(ctx, srcHandle, "file.txt")
	assertError(t, err, metadata.ErrNotFound, "file in source")

	// Should exist in destination
	moved, err := store.Lookup(ctx, dstHandle, "file.txt")
	if err != nil {
		t.Fatalf("Lookup in destination failed: %v", err)
	}

	// File ID should be the same
	if moved.ID != file.ID {
		t.Errorf("expected same file ID")
	}

	// Path should be updated
	if moved.Path != "/dst/file.txt" {
		t.Errorf("expected path '/dst/file.txt', got '%s'", moved.Path)
	}
}

func TestMove_DirectoryWithContents(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create directory with nested structure
	// /src/subdir/file.txt
	srcDir := createTestDirectory(t, store, ctx, rootHandle, "src")
	srcHandle := getFileHandle(srcDir)

	subdir := createTestDirectory(t, store, ctx, srcHandle, "subdir")
	subdirHandle := getFileHandle(subdir)

	_ = createTestFile(t, store, ctx, subdirHandle, "file.txt")

	// Move /src to /dst
	err := store.Move(ctx, rootHandle, "src", rootHandle, "dst")
	if err != nil {
		t.Fatalf("Move failed: %v", err)
	}

	// Verify structure moved
	movedDir, err := store.Lookup(ctx, rootHandle, "dst")
	if err != nil {
		t.Fatalf("Lookup dst failed: %v", err)
	}
	if movedDir.Path != "/dst" {
		t.Errorf("expected path '/dst', got '%s'", movedDir.Path)
	}

	movedDirHandle := getFileHandle(movedDir)
	movedSubdir, err := store.Lookup(ctx, movedDirHandle, "subdir")
	if err != nil {
		t.Fatalf("Lookup subdir failed: %v", err)
	}
	if movedSubdir.Path != "/dst/subdir" {
		t.Errorf("expected path '/dst/subdir', got '%s'", movedSubdir.Path)
	}

	movedSubdirHandle := getFileHandle(movedSubdir)
	movedFile, err := store.Lookup(ctx, movedSubdirHandle, "file.txt")
	if err != nil {
		t.Fatalf("Lookup file.txt failed: %v", err)
	}
	if movedFile.Path != "/dst/subdir/file.txt" {
		t.Errorf("expected path '/dst/subdir/file.txt', got '%s'", movedFile.Path)
	}
}

func TestMove_ReplaceFile(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create two files
	file1 := createTestFile(t, store, ctx, rootHandle, "file1.txt")
	file2 := createTestFile(t, store, ctx, rootHandle, "file2.txt")

	// Move file1 to file2 (destination should be replaced)
	err := store.Move(ctx, rootHandle, "file1.txt", rootHandle, "file2.txt")
	if err != nil {
		t.Fatalf("Move failed: %v", err)
	}

	// Verify file1 no longer exists
	_, err = store.Lookup(ctx, rootHandle, "file1.txt")
	assertError(t, err, metadata.ErrNotFound, "source should not exist")

	// Verify file2 now points to file1's content
	newFile2, err := store.Lookup(ctx, rootHandle, "file2.txt")
	if err != nil {
		t.Fatalf("Lookup file2 failed: %v", err)
	}

	// Should have file1's ID (same file, new name)
	if newFile2.ID != file1.ID {
		t.Errorf("expected file2 to have file1's ID %v, got %v", file1.ID, newFile2.ID)
	}

	// Old file2's ID should be gone
	if newFile2.ID == file2.ID {
		t.Error("file2 should have been replaced, but has same ID")
	}
}

func TestMove_ReplaceDirectoryNotAllowed(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create a file and a directory
	_ = createTestFile(t, store, ctx, rootHandle, "file.txt")
	_ = createTestDirectory(t, store, ctx, rootHandle, "dir")

	// Try to replace directory with file - should fail
	err := store.Move(ctx, rootHandle, "file.txt", rootHandle, "dir")
	assertError(t, err, metadata.ErrIsDirectory, "cannot replace directory with file")
}

func TestMove_ReplaceNonEmptyDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create two directories
	dir1 := createTestDirectory(t, store, ctx, rootHandle, "dir1")
	dir2 := createTestDirectory(t, store, ctx, rootHandle, "dir2")
	dir2Handle := getFileHandle(dir2)

	// Add a file to dir2 (make it non-empty)
	_ = createTestFile(t, store, ctx, dir2Handle, "child.txt")

	// Try to replace non-empty dir2 with dir1 - should fail
	dir1Handle := getFileHandle(dir1)
	err := store.Move(ctx, rootHandle, "dir1", rootHandle, "dir2")
	assertError(t, err, metadata.ErrNotEmpty, "cannot replace non-empty directory")

	// dir1 should still exist
	_, err = store.Lookup(ctx, rootHandle, "dir1")
	if err != nil {
		t.Errorf("dir1 should still exist: %v", err)
	}

	// dir2 should still exist with its child
	_, err = store.Lookup(ctx, dir1Handle, "child.txt")
	// Since dir1 wasn't deleted, this lookup should fail (child is in dir2)
	_ = err // expected
}

func TestMove_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create directory with root ownership
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0700, // Only owner can write
		UID:  0,
		GID:  0,
	}
	restrictedDir, _ := store.Create(rootCtx, rootHandle, "restricted", attr)
	restrictedHandle := getFileHandle(restrictedDir)

	// Create file in restricted directory as root
	_ = createTestFile(t, store, rootCtx, restrictedHandle, "file.txt")

	// Try to move as non-root user
	userCtx := createTestAuthContext()
	err := store.Move(userCtx, restrictedHandle, "file.txt", rootHandle, "moved.txt")
	assertError(t, err, metadata.ErrPermissionDenied, "move without write permission")
}

func TestRemoveFile(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")

	// Remove the file
	removed, err := store.RemoveFile(ctx, rootHandle, "testfile.txt")
	if err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}

	// Verify removed file metadata
	if removed.ID != file.ID {
		t.Errorf("expected same file ID")
	}

	// File should not exist in directory anymore (Lookup should fail)
	_, err = store.Lookup(ctx, rootHandle, "testfile.txt")
	assertError(t, err, metadata.ErrNotFound, "file should be removed from directory")

	// GetFile should still succeed with nlink=0 (POSIX compliance:
	// fstat() on an open fd after unlink() should return nlink=0, not ESTALE)
	fileHandle := getFileHandle(file)
	orphanedFile, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		t.Errorf("GetFile after removal should succeed: %v", err)
	}
	if orphanedFile.Nlink != 0 {
		t.Errorf("expected nlink=0 for orphaned file, got %d", orphanedFile.Nlink)
	}
}

func TestRemoveFile_NotFile(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	_ = createTestDirectory(t, store, ctx, rootHandle, "testdir")

	// Try to remove directory with RemoveFile
	_, err := store.RemoveFile(ctx, rootHandle, "testdir")
	assertError(t, err, metadata.ErrIsDirectory, "remove directory as file")
}

func TestRemoveFile_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create directory with root ownership
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0700, // Only owner can write
		UID:  0,
		GID:  0,
	}
	restrictedDir, _ := store.Create(rootCtx, rootHandle, "restricted", attr)
	restrictedHandle := getFileHandle(restrictedDir)

	// Create file as root
	_ = createTestFile(t, store, rootCtx, restrictedHandle, "file.txt")

	// Try to remove as non-root user
	userCtx := createTestAuthContext()
	_, err := store.RemoveFile(userCtx, restrictedHandle, "file.txt")
	assertError(t, err, metadata.ErrPermissionDenied, "remove without write permission")
}

func TestRemoveDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	dir := createTestDirectory(t, store, ctx, rootHandle, "testdir")

	// Remove the directory
	err := store.RemoveDirectory(ctx, rootHandle, "testdir")
	if err != nil {
		t.Fatalf("RemoveDirectory failed: %v", err)
	}

	// Directory should not exist anymore
	_, err = store.Lookup(ctx, rootHandle, "testdir")
	assertError(t, err, metadata.ErrNotFound, "directory should be removed")

	// GetFile should also fail
	dirHandle := getFileHandle(dir)
	_, err = store.GetFile(ctx.Context, dirHandle)
	assertError(t, err, metadata.ErrNotFound, "directory should not be retrievable")
}

func TestRemoveDirectory_NotEmpty(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	dir := createTestDirectory(t, store, ctx, rootHandle, "testdir")
	dirHandle := getFileHandle(dir)

	// Add file to directory
	_ = createTestFile(t, store, ctx, dirHandle, "file.txt")

	// Try to remove non-empty directory
	err := store.RemoveDirectory(ctx, rootHandle, "testdir")
	assertError(t, err, metadata.ErrNotEmpty, "remove non-empty directory")
}

func TestRemoveDirectory_NotDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	_ = createTestFile(t, store, ctx, rootHandle, "file.txt")

	// Try to remove file with RemoveDirectory
	err := store.RemoveDirectory(ctx, rootHandle, "file.txt")
	assertError(t, err, metadata.ErrNotDirectory, "remove file as directory")
}

func TestRemoveDirectory_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create parent directory with root ownership
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0700, // Only owner can write
		UID:  0,
		GID:  0,
	}
	restrictedDir, _ := store.Create(rootCtx, rootHandle, "restricted", attr)
	restrictedHandle := getFileHandle(restrictedDir)

	// Create subdirectory as root
	_ = createTestDirectory(t, store, rootCtx, restrictedHandle, "subdir")

	// Try to remove as non-root user
	userCtx := createTestAuthContext()
	err := store.RemoveDirectory(userCtx, restrictedHandle, "subdir")
	assertError(t, err, metadata.ErrPermissionDenied, "remove without write permission")
}

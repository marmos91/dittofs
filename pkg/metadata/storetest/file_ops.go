package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runFileOpsTests runs all file operation conformance tests.
func runFileOpsTests(t *testing.T, factory StoreFactory) {
	t.Run("CreateFile", func(t *testing.T) { testCreateFile(t, factory) })
	t.Run("GetFile", func(t *testing.T) { testGetFile(t, factory) })
	t.Run("DeleteFile", func(t *testing.T) { testDeleteFile(t, factory) })
	t.Run("CreateHardLink", func(t *testing.T) { testCreateHardLink(t, factory) })
	t.Run("SetFileAttributes", func(t *testing.T) { testSetFileAttributes(t, factory) })
	t.Run("Rename", func(t *testing.T) { testRename(t, factory) })
	t.Run("GetFileNotFound", func(t *testing.T) { testGetFileNotFound(t, factory) })
	t.Run("GetChildNotFound", func(t *testing.T) { testGetChildNotFound(t, factory) })
}

// testCreateFile verifies that creating a file results in a retrievable entry with correct attributes.
func testCreateFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "hello.txt", 0644)

	ctx := t.Context()

	// Verify the file exists via GetFile
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	if file.Type != metadata.FileTypeRegular {
		t.Errorf("Type = %v, want FileTypeRegular", file.Type)
	}
	if file.Mode != 0644 {
		t.Errorf("Mode = %o, want 0644", file.Mode)
	}
	if file.UID != 1000 {
		t.Errorf("UID = %d, want 1000", file.UID)
	}
	if file.GID != 1000 {
		t.Errorf("GID = %d, want 1000", file.GID)
	}

	// Verify handle is non-nil
	if handle == nil {
		t.Error("handle should not be nil")
	}
}

// testGetFile verifies that creating then getting a file returns consistent data.
func testGetFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "test.txt", 0600)

	ctx := t.Context()

	// Get the file
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	// Verify roundtrip
	if file.Type != metadata.FileTypeRegular {
		t.Errorf("Type = %v, want FileTypeRegular", file.Type)
	}
	if file.Mode != 0600 {
		t.Errorf("Mode = %o, want 0600", file.Mode)
	}

	// Verify child lookup works
	childHandle, err := store.GetChild(ctx, rootHandle, "test.txt")
	if err != nil {
		t.Fatalf("GetChild() failed: %v", err)
	}
	if string(childHandle) != string(handle) {
		t.Error("GetChild() returned different handle than expected")
	}
}

// testDeleteFile verifies that deleting a file removes it from the store.
func testDeleteFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "todelete.txt", 0644)

	ctx := t.Context()

	// Delete the file
	if err := store.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile() failed: %v", err)
	}

	// Remove child entry
	if err := store.DeleteChild(ctx, rootHandle, "todelete.txt"); err != nil {
		t.Fatalf("DeleteChild() failed: %v", err)
	}

	// Verify file is gone
	_, err := store.GetFile(ctx, handle)
	if err == nil {
		t.Error("GetFile() should fail after deletion")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}

	// Verify child is gone
	_, err = store.GetChild(ctx, rootHandle, "todelete.txt")
	if err == nil {
		t.Error("GetChild() should fail after deletion")
	}
}

// testCreateHardLink verifies hard link creation and link count tracking.
func testCreateHardLink(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "original.txt", 0644)

	ctx := t.Context()

	// Add a hard link (new name pointing to same handle)
	if err := store.SetChild(ctx, rootHandle, "link.txt", handle); err != nil {
		t.Fatalf("SetChild() for hard link failed: %v", err)
	}

	// Increment link count
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	// Verify link count
	count, err := store.GetLinkCount(ctx, handle)
	if err != nil {
		t.Fatalf("GetLinkCount() failed: %v", err)
	}
	if count != 2 {
		t.Errorf("link count = %d, want 2", count)
	}

	// Verify both names resolve to same handle
	h1, err := store.GetChild(ctx, rootHandle, "original.txt")
	if err != nil {
		t.Fatalf("GetChild(original.txt) failed: %v", err)
	}
	h2, err := store.GetChild(ctx, rootHandle, "link.txt")
	if err != nil {
		t.Fatalf("GetChild(link.txt) failed: %v", err)
	}
	if string(h1) != string(h2) {
		t.Error("hard link handles should be identical")
	}
}

// testSetFileAttributes verifies that file attributes can be updated.
func testSetFileAttributes(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "attrs.txt", 0644)

	ctx := t.Context()

	// Get the current file
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	// Modify attributes
	file.Mode = 0755
	file.UID = 2000
	file.Size = 1024

	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() with updated attrs failed: %v", err)
	}

	// Verify changes
	updated, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after update failed: %v", err)
	}
	if updated.Mode != 0755 {
		t.Errorf("Mode = %o, want 0755", updated.Mode)
	}
	if updated.UID != 2000 {
		t.Errorf("UID = %d, want 2000", updated.UID)
	}
	if updated.Size != 1024 {
		t.Errorf("Size = %d, want 1024", updated.Size)
	}
}

// testRename verifies that renaming a file removes the old name and creates the new name.
func testRename(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "old.txt", 0644)

	ctx := t.Context()

	// Rename: remove old child, add new child
	if err := store.DeleteChild(ctx, rootHandle, "old.txt"); err != nil {
		t.Fatalf("DeleteChild(old.txt) failed: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "new.txt", handle); err != nil {
		t.Fatalf("SetChild(new.txt) failed: %v", err)
	}

	// Verify old name is gone
	_, err := store.GetChild(ctx, rootHandle, "old.txt")
	if err == nil {
		t.Error("GetChild(old.txt) should fail after rename")
	}

	// Verify new name exists
	newHandle, err := store.GetChild(ctx, rootHandle, "new.txt")
	if err != nil {
		t.Fatalf("GetChild(new.txt) failed: %v", err)
	}
	if string(newHandle) != string(handle) {
		t.Error("renamed handle should be the same")
	}
}

// testGetFileNotFound verifies that GetFile returns an appropriate error for non-existent handles.
func testGetFileNotFound(t *testing.T, factory StoreFactory) {
	store := factory(t)
	_ = createTestShare(t, store, "/test")

	ctx := t.Context()

	// Generate a handle that doesn't exist in the store
	fakeHandle, err := metadata.GenerateNewHandle("/test")
	if err != nil {
		t.Fatalf("GenerateNewHandle() failed: %v", err)
	}

	_, err = store.GetFile(ctx, fakeHandle)
	if err == nil {
		t.Error("GetFile() should fail for non-existent handle")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

// testGetChildNotFound verifies that GetChild returns an appropriate error for non-existent names.
func testGetChildNotFound(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	ctx := t.Context()

	_, err := store.GetChild(ctx, rootHandle, "nonexistent.txt")
	if err == nil {
		t.Error("GetChild() should fail for non-existent name")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

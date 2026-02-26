package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runPermissionsTests runs all permission-related conformance tests.
//
// These tests verify store-level operations that support permission checking,
// such as file attributes used for access control. Business-level permission
// checks (CheckAccess) live in the MetadataService layer, not in individual stores.
func runPermissionsTests(t *testing.T, factory StoreFactory) {
	t.Run("FilePermissionAttributes", func(t *testing.T) { testFilePermissionAttributes(t, factory) })
	t.Run("DirectoryPermissionAttributes", func(t *testing.T) { testDirectoryPermissionAttributes(t, factory) })
	t.Run("RootOwnership", func(t *testing.T) { testRootOwnership(t, factory) })
}

// testFilePermissionAttributes verifies that permission bits are correctly stored and retrieved.
func testFilePermissionAttributes(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	ctx := t.Context()

	// Create files with different permission modes
	testCases := []struct {
		name string
		mode uint32
	}{
		{"readonly.txt", 0444},
		{"readwrite.txt", 0644},
		{"executable.sh", 0755},
		{"private.txt", 0600},
		{"allperms.txt", 0777},
	}

	for _, tc := range testCases {
		handle := createTestFile(t, store, "/test", rootHandle, tc.name, tc.mode)

		file, err := store.GetFile(ctx, handle)
		if err != nil {
			t.Fatalf("GetFile(%s) failed: %v", tc.name, err)
		}

		if file.Mode != tc.mode {
			t.Errorf("Mode for %s = %o, want %o", tc.name, file.Mode, tc.mode)
		}
	}
}

// testDirectoryPermissionAttributes verifies that directory permissions are stored and retrieved.
func testDirectoryPermissionAttributes(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	ctx := t.Context()

	// Create a directory with restricted permissions
	handle, err := store.GenerateHandle(ctx, "/test", "/restricted")
	if err != nil {
		t.Fatalf("GenerateHandle() failed: %v", err)
	}

	dir := &metadata.File{
		ShareName: "/test",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0700,
			UID:  1000,
			GID:  1000,
		},
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle() failed: %v", err)
	}
	dir.ID = id

	if err := store.PutFile(ctx, dir); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent() failed: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "restricted", handle); err != nil {
		t.Fatalf("SetChild() failed: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	// Verify directory attributes
	result, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	if result.Mode != 0700 {
		t.Errorf("Mode = %o, want 0700", result.Mode)
	}
	if result.UID != 1000 {
		t.Errorf("UID = %d, want 1000", result.UID)
	}
}

// testRootOwnership verifies that root directory ownership is stored correctly.
func testRootOwnership(t *testing.T, factory StoreFactory) {
	store := factory(t)

	ctx := t.Context()

	// Create share
	share := &metadata.Share{Name: "/owntest"}
	if err := store.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare() failed: %v", err)
	}

	// Create root with specific ownership
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}
	rootFile, err := store.CreateRootDirectory(ctx, "/owntest", rootAttr)
	if err != nil {
		t.Fatalf("CreateRootDirectory() failed: %v", err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle() failed: %v", err)
	}

	// Verify root ownership
	file, err := store.GetFile(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	if file.UID != 0 {
		t.Errorf("root UID = %d, want 0", file.UID)
	}
	if file.GID != 0 {
		t.Errorf("root GID = %d, want 0", file.GID)
	}
	if file.Mode != 0755 {
		t.Errorf("root Mode = %o, want 0755", file.Mode)
	}
}

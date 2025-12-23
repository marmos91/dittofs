//go:build integration

package badger_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/marmos91/dittofs/pkg/store/metadata/badger"
)

// TestGuestPermissionWithMode511 tests that a guest user (UID 65534)
// can create files in a root directory with mode 511 (0777 octal).
//
// This test reproduces the issue where SMB guest users are denied write
// permission despite the root directory having world-writable permissions.
func TestGuestPermissionWithMode511(t *testing.T) {
	ctx := context.Background()

	// Create temporary directory for test database
	tempDir, err := os.MkdirTemp("", "dittofs-badger-perm-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	// Create store
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
	}
	defer store.Close()

	// Create root directory with mode 511 (0777 in octal), owned by root
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 511, // 0777 in octal = rwxrwxrwx
		UID:  0,   // root
		GID:  0,   // root
	}

	rootFile, err := store.CreateRootDirectory(ctx, "/s3", rootAttr)
	if err != nil {
		t.Fatalf("Failed to create root directory: %v", err)
	}

	// Verify the root directory has the correct mode
	t.Logf("Root directory created with mode: %o (decimal: %d)", rootFile.Mode, rootFile.Mode)
	if rootFile.Mode != 511 {
		t.Errorf("Expected mode 511, got %d", rootFile.Mode)
	}

	// Encode root handle for operations
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("Failed to encode root handle: %v", err)
	}

	// Test Case 1: Guest user (UID 65534, GID 65534)
	t.Run("GuestCanCreateFile", func(t *testing.T) {
		guestUID := uint32(65534)
		guestGID := uint32(65534)
		guestAuthCtx := &metadata.AuthContext{
			Context: ctx,
			Identity: &metadata.Identity{
				UID: &guestUID,
				GID: &guestGID,
			},
			ClientAddr: "127.0.0.1",
		}

		// Check if guest has write permission on root directory
		perms, err := store.CheckPermissions(guestAuthCtx, rootHandle, metadata.PermissionWrite)
		if err != nil {
			t.Fatalf("CheckPermissions failed: %v", err)
		}

		if perms&metadata.PermissionWrite == 0 {
			t.Errorf("Guest should have write permission on mode 511 directory, but got permissions: %v", perms)
		} else {
			t.Logf("Guest has write permission: %v", perms)
		}

		// Try to actually create a file
		fileAttr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
			UID:  guestUID,
			GID:  guestGID,
		}

		file, err := store.Create(guestAuthCtx, rootHandle, "guest_test.txt", fileAttr)
		if err != nil {
			t.Errorf("Guest failed to create file: %v", err)
		} else {
			t.Logf("Guest successfully created file: %s", file.Path)
		}
	})

	// Test Case 2: Authenticated user (UID 1000, GID 1000)
	t.Run("AuthenticatedUserCanCreateFile", func(t *testing.T) {
		userUID := uint32(1000)
		userGID := uint32(1000)
		userAuthCtx := &metadata.AuthContext{
			Context: ctx,
			Identity: &metadata.Identity{
				UID: &userUID,
				GID: &userGID,
			},
			ClientAddr: "127.0.0.1",
		}

		// Check if user has write permission on root directory
		perms, err := store.CheckPermissions(userAuthCtx, rootHandle, metadata.PermissionWrite)
		if err != nil {
			t.Fatalf("CheckPermissions failed: %v", err)
		}

		if perms&metadata.PermissionWrite == 0 {
			t.Errorf("User should have write permission on mode 511 directory, but got permissions: %v", perms)
		} else {
			t.Logf("User has write permission: %v", perms)
		}

		// Try to actually create a file
		fileAttr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
			UID:  userUID,
			GID:  userGID,
		}

		file, err := store.Create(userAuthCtx, rootHandle, "user_test.txt", fileAttr)
		if err != nil {
			t.Errorf("User failed to create file: %v", err)
		} else {
			t.Logf("User successfully created file: %s", file.Path)
		}
	})
}

// TestRootDirectoryModeUpdate tests that root directory mode is updated when config changes.
// This ensures that persisted BadgerDB data is updated when the config specifies different
// root directory attributes than what was previously stored.
func TestRootDirectoryModeUpdate(t *testing.T) {
	ctx := context.Background()

	// Create temporary directory for test database
	tempDir, err := os.MkdirTemp("", "dittofs-badger-update-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	// Step 1: Create store and root directory with mode 0755
	t.Run("CreateWithMode755ThenUpdateTo777", func(t *testing.T) {
		// First, create the share with mode 0755
		store1, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
		}

		rootAttr1 := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755, // rwxr-xr-x (only owner can write)
			UID:  0,
			GID:  0,
		}

		rootFile1, err := store1.CreateRootDirectory(ctx, "/testshare", rootAttr1)
		if err != nil {
			t.Fatalf("Failed to create root directory: %v", err)
		}

		t.Logf("Initial root directory mode: %o (decimal: %d)", rootFile1.Mode, rootFile1.Mode)
		if rootFile1.Mode != 0755 {
			t.Errorf("Expected initial mode 0755 (493), got %d", rootFile1.Mode)
		}

		// Close the store (simulating server restart)
		store1.Close()

		// Step 2: Reopen store and request mode 511 (0777)
		store2, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to reopen BadgerMetadataStore: %v", err)
		}
		defer store2.Close()

		rootAttr2 := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 511, // 0777 - rwxrwxrwx (everyone can write)
			UID:  0,
			GID:  0,
		}

		rootFile2, err := store2.CreateRootDirectory(ctx, "/testshare", rootAttr2)
		if err != nil {
			t.Fatalf("Failed to get/update root directory: %v", err)
		}

		t.Logf("Updated root directory mode: %o (decimal: %d)", rootFile2.Mode, rootFile2.Mode)
		if rootFile2.Mode != 511 {
			t.Errorf("Expected updated mode 511 (0777), got %d", rootFile2.Mode)
		}

		// Step 3: Verify guest can now write
		rootHandle, err := metadata.EncodeFileHandle(rootFile2)
		if err != nil {
			t.Fatalf("Failed to encode root handle: %v", err)
		}

		guestUID := uint32(65534)
		guestGID := uint32(65534)
		guestAuthCtx := &metadata.AuthContext{
			Context: ctx,
			Identity: &metadata.Identity{
				UID: &guestUID,
				GID: &guestGID,
			},
			ClientAddr: "127.0.0.1",
		}

		perms, err := store2.CheckPermissions(guestAuthCtx, rootHandle, metadata.PermissionWrite)
		if err != nil {
			t.Fatalf("CheckPermissions failed: %v", err)
		}

		if perms&metadata.PermissionWrite == 0 {
			t.Errorf("Guest should have write permission after mode update, but got permissions: %v", perms)
		} else {
			t.Logf("Guest correctly has write permission after mode update: %v", perms)
		}
	})
}

// TestPermissionWithMode755 tests that only owner can write to a mode 755 directory.
// This is a sanity check to ensure permission checking is working correctly.
func TestPermissionWithMode755(t *testing.T) {
	ctx := context.Background()

	// Create temporary directory for test database
	tempDir, err := os.MkdirTemp("", "dittofs-badger-perm755-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	// Create store
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
	}
	defer store.Close()

	// Create root directory with mode 0755, owned by root
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755, // rwxr-xr-x
		UID:  0,    // root
		GID:  0,    // root
	}

	rootFile, err := store.CreateRootDirectory(ctx, "/testshare", rootAttr)
	if err != nil {
		t.Fatalf("Failed to create root directory: %v", err)
	}

	t.Logf("Root directory created with mode: %o (decimal: %d)", rootFile.Mode, rootFile.Mode)

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("Failed to encode root handle: %v", err)
	}

	// Guest should NOT have write permission with mode 755
	t.Run("GuestCannotWriteMode755", func(t *testing.T) {
		guestUID := uint32(65534)
		guestGID := uint32(65534)
		guestAuthCtx := &metadata.AuthContext{
			Context: ctx,
			Identity: &metadata.Identity{
				UID: &guestUID,
				GID: &guestGID,
			},
			ClientAddr: "127.0.0.1",
		}

		perms, err := store.CheckPermissions(guestAuthCtx, rootHandle, metadata.PermissionWrite)
		if err != nil {
			t.Fatalf("CheckPermissions failed: %v", err)
		}

		if perms&metadata.PermissionWrite != 0 {
			t.Errorf("Guest should NOT have write permission on mode 755 directory, but got permissions: %v", perms)
		} else {
			t.Logf("Guest correctly denied write permission on mode 755: %v", perms)
		}
	})

	// Root (UID 0) should have write permission
	t.Run("RootCanWriteMode755", func(t *testing.T) {
		rootUID := uint32(0)
		rootGID := uint32(0)
		rootAuthCtx := &metadata.AuthContext{
			Context: ctx,
			Identity: &metadata.Identity{
				UID: &rootUID,
				GID: &rootGID,
			},
			ClientAddr: "127.0.0.1",
		}

		perms, err := store.CheckPermissions(rootAuthCtx, rootHandle, metadata.PermissionWrite)
		if err != nil {
			t.Fatalf("CheckPermissions failed: %v", err)
		}

		if perms&metadata.PermissionWrite == 0 {
			t.Errorf("Root should have write permission, but got permissions: %v", perms)
		} else {
			t.Logf("Root correctly has write permission: %v", perms)
		}
	})
}

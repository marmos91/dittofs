//go:build integration

package badger_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestBadgerStore_RootDirectoryPermissions tests that root directory permissions
// are correctly persisted and can be updated.
func TestBadgerStore_RootDirectoryPermissions(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "dittofs-badger-perm-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
	}
	defer store.Close()

	t.Run("CreateRootWithMode777", func(t *testing.T) {
		rootAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0777,
			UID:  0,
			GID:  0,
		}

		rootFile, err := store.CreateRootDirectory(ctx, "/share777", rootAttr)
		if err != nil {
			t.Fatalf("Failed to create root directory: %v", err)
		}

		if rootFile.Mode != 0777 {
			t.Errorf("Expected mode 0777, got %o", rootFile.Mode)
		}

		// Verify persistence
		rootHandle, _ := metadata.EncodeFileHandle(rootFile)
		retrieved, err := store.GetFile(ctx, rootHandle)
		if err != nil {
			t.Fatalf("Failed to get file: %v", err)
		}

		if retrieved.Mode != 0777 {
			t.Errorf("Retrieved mode %o, expected 0777", retrieved.Mode)
		}
	})

	t.Run("CreateRootWithMode755", func(t *testing.T) {
		rootAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  1000,
			GID:  1000,
		}

		rootFile, err := store.CreateRootDirectory(ctx, "/share755", rootAttr)
		if err != nil {
			t.Fatalf("Failed to create root directory: %v", err)
		}

		if rootFile.Mode != 0755 {
			t.Errorf("Expected mode 0755, got %o", rootFile.Mode)
		}
	})
}

// TestBadgerStore_PermissionPersistence tests that file permissions persist across restarts.
func TestBadgerStore_PermissionPersistence(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "dittofs-badger-persist-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	var rootHandle metadata.FileHandle

	// Phase 1: Create with mode 0755
	{
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create store: %v", err)
		}

		rootAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  0,
			GID:  0,
		}

		rootFile, err := store.CreateRootDirectory(ctx, "/persist", rootAttr)
		if err != nil {
			t.Fatalf("Failed to create root: %v", err)
		}

		rootHandle, _ = metadata.EncodeFileHandle(rootFile)
		store.Close()
	}

	// Phase 2: Reopen and verify persistence
	{
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to reopen store: %v", err)
		}
		defer store.Close()

		retrieved, err := store.GetFile(ctx, rootHandle)
		if err != nil {
			t.Fatalf("Failed to get persisted file: %v", err)
		}

		if retrieved.Mode != 0755 {
			t.Errorf("Expected persisted mode 0755, got %o", retrieved.Mode)
		}
		if retrieved.UID != 0 {
			t.Errorf("Expected UID 0, got %d", retrieved.UID)
		}
	}
}

// TestBadgerStore_FileWithDifferentOwners tests files with different ownership.
func TestBadgerStore_FileWithDifferentOwners(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "dittofs-badger-owners-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer store.Close()

	shareName := "/owners"

	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("Failed to create root: %v", err)
	}

	rootHandle, _ := metadata.EncodeFileHandle(rootFile)

	testCases := []struct {
		name string
		uid  uint32
		gid  uint32
		mode uint32
	}{
		{"root_owned", 0, 0, 0644},
		{"user_owned", 1000, 1000, 0644},
		{"guest_owned", 65534, 65534, 0600},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handle, err := store.GenerateHandle(ctx, shareName, "/"+tc.name+".txt")
			if err != nil {
				t.Fatalf("Failed to generate handle: %v", err)
			}

			// Decode handle to get the UUID
			_, id, err := metadata.DecodeFileHandle(handle)
			if err != nil {
				t.Fatalf("Failed to decode handle: %v", err)
			}

			file := &metadata.File{
				ID:        id,
				ShareName: shareName,
				FileAttr: metadata.FileAttr{
					Type: metadata.FileTypeRegular,
					Mode: tc.mode,
					UID:  tc.uid,
					GID:  tc.gid,
				},
			}

			err = store.PutFile(ctx, file)
			if err != nil {
				t.Fatalf("Failed to put file: %v", err)
			}

			err = store.SetChild(ctx, rootHandle, tc.name+".txt", handle)
			if err != nil {
				t.Fatalf("Failed to set child: %v", err)
			}

			// Verify
			retrieved, err := store.GetFile(ctx, handle)
			if err != nil {
				t.Fatalf("Failed to get file: %v", err)
			}

			if retrieved.UID != tc.uid {
				t.Errorf("Expected UID %d, got %d", tc.uid, retrieved.UID)
			}
			if retrieved.GID != tc.gid {
				t.Errorf("Expected GID %d, got %d", tc.gid, retrieved.GID)
			}
			if retrieved.Mode != tc.mode {
				t.Errorf("Expected mode %o, got %o", tc.mode, retrieved.Mode)
			}
		})
	}
}

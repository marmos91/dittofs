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

// TestBadgerMetadataStore_Integration runs integration tests for BadgerDB metadata store.
func TestBadgerMetadataStore_Integration(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "dittofs-badger-meta-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	t.Run("CreateStoreAndHealthcheck", func(t *testing.T) {
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
		}
		defer store.Close()

		err = store.Healthcheck(ctx)
		if err != nil {
			t.Fatalf("Healthcheck failed: %v", err)
		}
	})

	t.Run("CreateRootDirectory", func(t *testing.T) {
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
		}
		defer store.Close()

		rootAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  1000,
			GID:  1000,
		}

		rootFile, err := store.CreateRootDirectory(ctx, "testshare", rootAttr)
		if err != nil {
			t.Fatalf("Failed to create root directory: %v", err)
		}

		if rootFile == nil {
			t.Fatal("Root file should not be nil")
		}

		rootHandle, err := metadata.EncodeFileHandle(rootFile)
		if err != nil {
			t.Fatalf("Failed to encode file handle: %v", err)
		}

		fileAttr, err := store.GetFile(ctx, rootHandle)
		if err != nil {
			t.Fatalf("Failed to get file: %v", err)
		}

		if fileAttr.Type != metadata.FileTypeDirectory {
			t.Errorf("Expected directory type, got %v", fileAttr.Type)
		}
		if fileAttr.Mode != 0755 {
			t.Errorf("Expected mode 0755, got %o", fileAttr.Mode)
		}
	})

	t.Run("Persistence", func(t *testing.T) {
		var rootHandle metadata.FileHandle

		// Phase 1: Create store, add data, close
		{
			store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
			if err != nil {
				t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
			}

			rootAttr := &metadata.FileAttr{
				Type: metadata.FileTypeDirectory,
				Mode: 0755,
				UID:  1000,
				GID:  1000,
			}

			rootFile, err := store.CreateRootDirectory(ctx, "persistshare", rootAttr)
			if err != nil {
				t.Fatalf("Failed to create root directory: %v", err)
			}

			rootHandle, err = metadata.EncodeFileHandle(rootFile)
			if err != nil {
				t.Fatalf("Failed to encode file handle: %v", err)
			}

			if err := store.Close(); err != nil {
				t.Fatalf("Failed to close store: %v", err)
			}
		}

		// Phase 2: Reopen store and verify data persisted
		{
			store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
			if err != nil {
				t.Fatalf("Failed to reopen BadgerMetadataStore: %v", err)
			}
			defer store.Close()

			fileAttr, err := store.GetFile(ctx, rootHandle)
			if err != nil {
				t.Fatalf("Failed to get persisted file: %v", err)
			}

			if fileAttr.Type != metadata.FileTypeDirectory {
				t.Errorf("Expected directory type, got %v", fileAttr.Type)
			}

			shareName, _, err := metadata.DecodeFileHandle(rootHandle)
			if err != nil {
				t.Fatalf("Failed to decode share name from handle: %v", err)
			}
			if shareName != "persistshare" {
				t.Errorf("Expected share name 'persistshare', got '%s'", shareName)
			}
		}
	})
}

// TestBadgerMetadataStore_CRUD tests basic CRUD operations on the store.
func TestBadgerMetadataStore_CRUD(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "dittofs-badger-crud-*")
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

	shareName := "/files"

	// Create root directory
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  1000,
		GID:  1000,
	}

	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("Failed to create root directory: %v", err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("Failed to encode root file handle: %v", err)
	}

	t.Run("PutFile", func(t *testing.T) {
		handle, err := store.GenerateHandle(ctx, shareName, "/testfile.txt")
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
				Mode: 0644,
				UID:  1000,
				GID:  1000,
			},
		}

		err = store.PutFile(ctx, file)
		if err != nil {
			t.Fatalf("Failed to put file: %v", err)
		}

		// Verify file exists
		retrieved, err := store.GetFile(ctx, handle)
		if err != nil {
			t.Fatalf("Failed to get file: %v", err)
		}

		if retrieved.Type != metadata.FileTypeRegular {
			t.Errorf("Expected regular file type, got %v", retrieved.Type)
		}
	})

	t.Run("SetChild_GetChild", func(t *testing.T) {
		// Create a child file
		childHandle, err := store.GenerateHandle(ctx, shareName, "/child.txt")
		if err != nil {
			t.Fatalf("Failed to generate handle: %v", err)
		}

		// Decode handle to get the UUID
		_, childID, err := metadata.DecodeFileHandle(childHandle)
		if err != nil {
			t.Fatalf("Failed to decode handle: %v", err)
		}

		childFile := &metadata.File{
			ID:        childID,
			ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Mode: 0644,
				UID:  1000,
				GID:  1000,
			},
		}

		err = store.PutFile(ctx, childFile)
		if err != nil {
			t.Fatalf("Failed to put child file: %v", err)
		}

		// Set child relationship
		err = store.SetChild(ctx, rootHandle, "child.txt", childHandle)
		if err != nil {
			t.Fatalf("Failed to set child: %v", err)
		}

		// Get child back
		retrievedHandle, err := store.GetChild(ctx, rootHandle, "child.txt")
		if err != nil {
			t.Fatalf("Failed to get child: %v", err)
		}

		if string(retrievedHandle) != string(childHandle) {
			t.Errorf("Handle mismatch")
		}
	})

	t.Run("ListChildren", func(t *testing.T) {
		entries, _, err := store.ListChildren(ctx, rootHandle, "", 100)
		if err != nil {
			t.Fatalf("Failed to list children: %v", err)
		}

		if len(entries) == 0 {
			t.Error("Expected at least one child entry")
		}

		found := false
		for _, entry := range entries {
			if entry.Name == "child.txt" {
				found = true
				break
			}
		}

		if !found {
			t.Error("child.txt not found in listing")
		}
	})

	t.Run("DeleteChild", func(t *testing.T) {
		err := store.DeleteChild(ctx, rootHandle, "child.txt")
		if err != nil {
			t.Fatalf("Failed to delete child: %v", err)
		}

		_, err = store.GetChild(ctx, rootHandle, "child.txt")
		if err == nil {
			t.Error("Expected error getting deleted child")
		}
	})
}

// TestBadgerMetadataStore_Healthcheck tests healthcheck functionality.
func TestBadgerMetadataStore_Healthcheck(t *testing.T) {
	ctx := context.Background()

	tempDir, err := os.MkdirTemp("", "dittofs-badger-health-*")
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

	err = store.Healthcheck(ctx)
	if err != nil {
		t.Fatalf("Healthcheck should succeed: %v", err)
	}

	store.Close()

	err = store.Healthcheck(ctx)
	if err == nil {
		t.Error("Healthcheck should fail after close")
	}
}

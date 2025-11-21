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

// TestBadgerMetadataStore_Integration runs integration tests for BadgerDB metadata store.
//
// Prerequisites:
//   - None (BadgerDB is embedded, no external services needed)
//   - Run with: go test -tags=integration ./test/integration/badger/...
//
// These tests verify that BadgerDB metadata store:
//   - Can be created and initialized
//   - Persists data across restarts
//   - Handles basic file/directory operations
func TestBadgerMetadataStore_Integration(t *testing.T) {
	ctx := context.Background()

	// ========================================================================
	// Setup: Create temporary directory for test database
	// ========================================================================

	tempDir, err := os.MkdirTemp("", "dittofs-badger-meta-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "metadata.db")

	// ========================================================================
	// Test: Create store and verify healthcheck
	// ========================================================================

	t.Run("CreateStoreAndHealthcheck", func(t *testing.T) {
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
		}
		defer store.Close()

		// Verify store can perform healthcheck
		err = store.Healthcheck(ctx)
		if err != nil {
			t.Fatalf("Healthcheck failed: %v", err)
		}
	})

	// ========================================================================
	// Test: Create root directory
	// ========================================================================

	t.Run("CreateRootDirectory", func(t *testing.T) {
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
		}
		defer store.Close()

		// Create a root directory for a share
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

		// Verify root file was created
		if rootFile == nil {
			t.Fatal("Root file should not be nil")
		}

		// Encode the file handle
		rootHandle, err := metadata.EncodeFileHandle(rootFile)
		if err != nil {
			t.Fatalf("Failed to encode file handle: %v", err)
		}

		// Get the file attributes back
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

	// ========================================================================
	// Test: Persistence across restarts
	// ========================================================================

	t.Run("Persistence", func(t *testing.T) {
		var rootHandle metadata.FileHandle

		// Phase 1: Create store, add data, close
		{
			store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
			if err != nil {
				t.Fatalf("Failed to create BadgerMetadataStore: %v", err)
			}

			// Create root directory
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

			// Encode the file handle for persistence check
			rootHandle, err = metadata.EncodeFileHandle(rootFile)
			if err != nil {
				t.Fatalf("Failed to encode file handle: %v", err)
			}

			// Close store
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

			// Try to get the file we created
			fileAttr, err := store.GetFile(ctx, rootHandle)
			if err != nil {
				t.Fatalf("Failed to get persisted file: %v", err)
			}

			if fileAttr.Type != metadata.FileTypeDirectory {
				t.Errorf("Expected directory type, got %v", fileAttr.Type)
			}

			// Verify share name
			shareName, err := store.GetShareNameForHandle(ctx, rootHandle)
			if err != nil {
				t.Fatalf("Failed to get share name: %v", err)
			}
			if shareName != "persistshare" {
				t.Errorf("Expected share name 'persistshare', got '%s'", shareName)
			}
		}
	})
}

// TestBadgerMetadataStore_FileOperations tests basic file and directory operations.
func TestBadgerMetadataStore_FileOperations(t *testing.T) {
	ctx := context.Background()

	// ========================================================================
	// Setup
	// ========================================================================

	tempDir, err := os.MkdirTemp("", "dittofs-badger-files-*")
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

	// Create root directory
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  1000,
		GID:  1000,
	}

	rootFile, err := store.CreateRootDirectory(ctx, "files", rootAttr)
	if err != nil {
		t.Fatalf("Failed to create root directory: %v", err)
	}

	// Encode the file handle
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("Failed to encode root file handle: %v", err)
	}

	// Create auth context
	uid := uint32(1000)
	gid := uint32(1000)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  &uid,
			GID:  &gid,
			GIDs: []uint32{1000},
		},
	}

	// ========================================================================
	// Test: Create directory
	// ========================================================================

	t.Run("CreateDirectory", func(t *testing.T) {
		dirAttr := &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  1000,
			GID:  1000,
		}

		dirFile, err := store.Create(authCtx, rootHandle, "testdir", dirAttr)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		// Encode the directory handle
		dirHandle, err := metadata.EncodeFileHandle(dirFile)
		if err != nil {
			t.Fatalf("Failed to encode directory handle: %v", err)
		}

		// Verify directory exists
		attr, err := store.GetFile(ctx, dirHandle)
		if err != nil {
			t.Fatalf("Failed to get directory attributes: %v", err)
		}
		if attr.Type != metadata.FileTypeDirectory {
			t.Errorf("Expected directory type, got %v", attr.Type)
		}
	})

	// ========================================================================
	// Test: Create file
	// ========================================================================

	t.Run("CreateFile", func(t *testing.T) {
		fileAttr := &metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0644,
			UID:  1000,
			GID:  1000,
		}

		createdFile, err := store.Create(authCtx, rootHandle, "testfile.txt", fileAttr)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Encode the file handle
		fileHandle, err := metadata.EncodeFileHandle(createdFile)
		if err != nil {
			t.Fatalf("Failed to encode file handle: %v", err)
		}

		// Verify file exists
		attr, err := store.GetFile(ctx, fileHandle)
		if err != nil {
			t.Fatalf("Failed to get file attributes: %v", err)
		}
		if attr.Type != metadata.FileTypeRegular {
			t.Errorf("Expected regular file type, got %v", attr.Type)
		}
	})

	// ========================================================================
	// Test: Lookup
	// ========================================================================

	t.Run("Lookup", func(t *testing.T) {
		// Lookup the file we just created
		lookedUpFile, err := store.Lookup(authCtx, rootHandle, "testfile.txt")
		if err != nil {
			t.Fatalf("Failed to lookup file: %v", err)
		}

		if lookedUpFile == nil {
			t.Fatal("Looked up file should not be nil")
		}

		if lookedUpFile.Type != metadata.FileTypeRegular {
			t.Errorf("Expected regular file type, got %v", lookedUpFile.Type)
		}
	})

	// ========================================================================
	// Test: List directory
	// ========================================================================

	t.Run("ListDirectory", func(t *testing.T) {
		result, err := store.ReadDirectory(authCtx, rootHandle, "", 4096)
		if err != nil {
			t.Fatalf("Failed to read directory: %v", err)
		}

		entries := result.Entries

		// Should have at least 2 entries (testdir and testfile.txt)
		if len(entries) < 2 {
			t.Errorf("Expected at least 2 entries, got %d", len(entries))
		}

		// Verify entries
		foundDir := false
		foundFile := false
		for _, entry := range entries {
			if entry.Name == "testdir" {
				foundDir = true
			}
			if entry.Name == "testfile.txt" {
				foundFile = true
			}
		}

		if !foundDir {
			t.Error("Directory 'testdir' not found in listing")
		}
		if !foundFile {
			t.Error("File 'testfile.txt' not found in listing")
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

	// Test healthcheck
	err = store.Healthcheck(ctx)
	if err != nil {
		t.Fatalf("Healthcheck should succeed: %v", err)
	}

	// Close store
	store.Close()

	// Healthcheck after close should fail
	err = store.Healthcheck(ctx)
	if err == nil {
		t.Error("Healthcheck should fail after close")
	}
}

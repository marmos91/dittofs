package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// StoreFactory creates a fresh MetadataStore instance for each test.
// The factory receives *testing.T so it can use t.TempDir() for stores
// that need filesystem paths and t.Cleanup() for teardown.
type StoreFactory func(t *testing.T) metadata.MetadataStore

// RunConformanceSuite runs the full conformance test suite against the provided
// store factory. Each test gets a fresh store instance to ensure isolation.
//
// The suite covers three categories:
//   - FileOps: file CRUD, hard links, attributes, read/write, rename, truncate
//   - DirOps: directory CRUD, listing, nesting, non-empty removal
//   - Permissions: access checking (requires auth context support)
func RunConformanceSuite(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("FileOps", func(t *testing.T) {
		runFileOpsTests(t, factory)
	})

	t.Run("DirOps", func(t *testing.T) {
		runDirOpsTests(t, factory)
	})

	t.Run("Permissions", func(t *testing.T) {
		runPermissionsTests(t, factory)
	})
}

// createTestShare is a helper that creates a share and root directory for testing.
// Returns the root handle.
func createTestShare(t *testing.T, store metadata.MetadataStore, shareName string) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Create share
	share := &metadata.Share{
		Name: shareName,
	}
	if err := store.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare(%q) failed: %v", shareName, err)
	}

	// Create root directory
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  0,
		GID:  0,
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	if err != nil {
		t.Fatalf("CreateRootDirectory(%q) failed: %v", shareName, err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle() failed: %v", err)
	}

	return rootHandle
}

// createTestFile is a helper that creates a regular file in a directory.
// Returns the file handle.
func createTestFile(t *testing.T, store metadata.MetadataStore, shareName string, dirHandle metadata.FileHandle, name string, mode uint32) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Generate handle
	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		t.Fatalf("GenerateHandle() failed: %v", err)
	}

	// Create file entry
	file := &metadata.File{
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: mode,
			UID:  1000,
			GID:  1000,
		},
	}
	// Decode handle to set ID
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle() failed: %v", err)
	}
	file.ID = id

	// Put file
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}

	// Set parent
	if err := store.SetParent(ctx, handle, dirHandle); err != nil {
		t.Fatalf("SetParent() failed: %v", err)
	}

	// Set child
	if err := store.SetChild(ctx, dirHandle, name, handle); err != nil {
		t.Fatalf("SetChild() failed: %v", err)
	}

	// Set link count
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	return handle
}

// createTestDir is a helper that creates a directory within a parent directory.
// Returns the directory handle.
func createTestDir(t *testing.T, store metadata.MetadataStore, shareName string, parentHandle metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()

	ctx := t.Context()

	// Generate handle
	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		t.Fatalf("GenerateHandle() failed: %v", err)
	}

	// Create dir entry
	dir := &metadata.File{
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0755,
			UID:  1000,
			GID:  1000,
		},
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle() failed: %v", err)
	}
	dir.ID = id

	// Put directory
	if err := store.PutFile(ctx, dir); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}

	// Set parent
	if err := store.SetParent(ctx, handle, parentHandle); err != nil {
		t.Fatalf("SetParent() failed: %v", err)
	}

	// Set child in parent
	if err := store.SetChild(ctx, parentHandle, name, handle); err != nil {
		t.Fatalf("SetChild() failed: %v", err)
	}

	// Set link count (2 for directories: . and parent entry)
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	return handle
}

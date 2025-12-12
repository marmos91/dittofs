package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

func TestNewPostgresMetadataStore(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	// Verify store was created
	if store == nil {
		t.Fatal("expected non-nil store")
	}

	// Verify pool is healthy
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := store.Healthcheck(ctx); err != nil {
		t.Fatalf("healthcheck failed: %v", err)
	}
}

func TestCreateRootDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	shareName := "test-share"
	attr := &metadata.FileAttr{
		Mode: 0755,
		UID:  1000,
		GID:  1000,
	}

	root, err := store.CreateRootDirectory(context.Background(), shareName, attr)
	if err != nil {
		t.Fatalf("CreateRootDirectory failed: %v", err)
	}

	// Verify root directory attributes
	if root.Path != "/" {
		t.Errorf("expected path '/', got '%s'", root.Path)
	}
	if root.Type != metadata.FileTypeDirectory {
		t.Errorf("expected directory type, got %v", root.Type)
	}
	if root.Mode != 0755 {
		t.Errorf("expected mode 0755, got %o", root.Mode)
	}
	if root.UID != 1000 {
		t.Errorf("expected UID 1000, got %d", root.UID)
	}
	if root.GID != 1000 {
		t.Errorf("expected GID 1000, got %d", root.GID)
	}
	if root.ShareName != shareName {
		t.Errorf("expected share name %s, got %s", shareName, root.ShareName)
	}
}

func TestCreateRootDirectory_EmptyShareName(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	attr := &metadata.FileAttr{
		Mode: 0755,
		UID:  0,
		GID:  0,
	}

	_, err := store.CreateRootDirectory(context.Background(), "", attr)
	assertError(t, err, metadata.ErrInvalidArgument, "empty share name")
}

func TestGetFile(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")

	// Get the file
	fileHandle := getFileHandle(file)
	retrieved, err := store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}

	// Verify file attributes
	assertFileEqual(t, file, retrieved, "GetFile")
}

func TestGetFile_InvalidHandle(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	// Create invalid handle
	invalidHandle := metadata.FileHandle([]byte("invalid:not-a-uuid"))

	_, err := store.GetFile(context.Background(), invalidHandle)
	assertError(t, err, metadata.ErrInvalidHandle, "invalid handle")
}

func TestGetFile_NotFound(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	_, _ = mustGetRootHandle(t, store)

	// Create handle for non-existent file
	nonExistentID := mustParseUUID("00000000-0000-0000-0000-000000000000")
	handle, _ := metadata.EncodeShareHandle("test-share", nonExistentID)

	_, err := store.GetFile(context.Background(), handle)
	assertError(t, err, metadata.ErrNotFound, "non-existent file")
}

func TestLookup(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")

	// Lookup the file
	found, err := store.Lookup(ctx, rootHandle, "testfile.txt")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	// Verify found file matches created file
	assertFileEqual(t, file, found, "Lookup")
}

func TestLookup_NotFound(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	_, err := store.Lookup(ctx, rootHandle, "nonexistent.txt")
	assertError(t, err, metadata.ErrNotFound, "non-existent file")
}

func TestLookup_EmptyName(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	_, err := store.Lookup(ctx, rootHandle, "")
	assertError(t, err, metadata.ErrInvalidArgument, "empty name")
}

func TestLookup_DotFiles(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Test "." lookup
	_, err := store.Lookup(ctx, rootHandle, ".")
	assertError(t, err, metadata.ErrInvalidArgument, "dot lookup")

	// Test ".." lookup
	_, err = store.Lookup(ctx, rootHandle, "..")
	assertError(t, err, metadata.ErrInvalidArgument, "dotdot lookup")
}

func TestLookup_ParentNotDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "regularfile.txt")
	fileHandle := getFileHandle(file)

	// Try to lookup in a regular file (not a directory)
	_, err := store.Lookup(ctx, fileHandle, "child.txt")
	assertError(t, err, metadata.ErrNotDirectory, "parent not directory")
}

func TestCreate_File(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	file, err := store.Create(ctx, rootHandle, "newfile.txt", attr)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify file attributes
	if file.Type != metadata.FileTypeRegular {
		t.Errorf("expected regular file type, got %v", file.Type)
	}
	if file.Mode != 0644 {
		t.Errorf("expected mode 0644, got %o", file.Mode)
	}
	if file.Path != "/newfile.txt" {
		t.Errorf("expected path '/newfile.txt', got '%s'", file.Path)
	}
	if file.Size != 0 {
		t.Errorf("expected size 0, got %d", file.Size)
	}
	if file.ContentID == "" {
		t.Error("expected non-empty ContentID")
	}
}

func TestCreate_Directory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	dir, err := store.Create(ctx, rootHandle, "newdir", attr)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify directory attributes
	if dir.Type != metadata.FileTypeDirectory {
		t.Errorf("expected directory type, got %v", dir.Type)
	}
	if dir.Mode != 0755 {
		t.Errorf("expected mode 0755, got %o", dir.Mode)
	}
	if dir.Path != "/newdir" {
		t.Errorf("expected path '/newdir', got '%s'", dir.Path)
	}
}

func TestCreate_AlreadyExists(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Create file
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	_, err := store.Create(ctx, rootHandle, "duplicate.txt", attr)
	if err != nil {
		t.Fatalf("first Create failed: %v", err)
	}

	// Try to create again
	_, err = store.Create(ctx, rootHandle, "duplicate.txt", attr)
	assertError(t, err, metadata.ErrAlreadyExists, "duplicate create")
}

func TestCreate_InvalidType(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Type: metadata.FileTypeSymlink, // Symlink should use CreateSymlink
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	_, err := store.Create(ctx, rootHandle, "invalid.txt", attr)
	assertError(t, err, metadata.ErrInvalidArgument, "invalid type")
}

// Helper function to parse UUID (for testing)
func mustParseUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

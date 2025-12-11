package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

func TestCreateSymlink(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0777,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	symlink, err := store.CreateSymlink(ctx, rootHandle, "testlink", "/target/path", attr)
	if err != nil {
		t.Fatalf("CreateSymlink failed: %v", err)
	}

	// Verify symlink attributes
	if symlink.Type != metadata.FileTypeSymlink {
		t.Errorf("expected symlink type, got %v", symlink.Type)
	}
	if symlink.LinkTarget != "/target/path" {
		t.Errorf("expected target '/target/path', got '%s'", symlink.LinkTarget)
	}
	if symlink.Path != "/testlink" {
		t.Errorf("expected path '/testlink', got '%s'", symlink.Path)
	}
	if symlink.Size != uint64(len("/target/path")) {
		t.Errorf("expected size %d, got %d", len("/target/path"), symlink.Size)
	}
}

func TestReadSymlink(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0777,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	symlink, err := store.CreateSymlink(ctx, rootHandle, "testlink", "/target/path", attr)
	if err != nil {
		t.Fatalf("CreateSymlink failed: %v", err)
	}
	symlinkHandle := getFileHandle(symlink)

	// Read symlink
	target, file, err := store.ReadSymlink(ctx, symlinkHandle)
	if err != nil {
		t.Fatalf("ReadSymlink failed: %v", err)
	}

	if target != "/target/path" {
		t.Errorf("expected target '/target/path', got '%s'", target)
	}
	if file.ID != symlink.ID {
		t.Error("expected same file ID")
	}
}

func TestReadSymlink_NotSymlink(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "regularfile.txt")
	fileHandle := getFileHandle(file)

	// Try to read symlink on regular file
	_, _, err := store.ReadSymlink(ctx, fileHandle)
	assertError(t, err, metadata.ErrInvalidArgument, "read symlink on regular file")
}

func TestCreateSymlink_AlreadyExists(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0777,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	// Create first symlink
	_, err := store.CreateSymlink(ctx, rootHandle, "testlink", "/target1", attr)
	if err != nil {
		t.Fatalf("first CreateSymlink failed: %v", err)
	}

	// Try to create another with same name
	_, err = store.CreateSymlink(ctx, rootHandle, "testlink", "/target2", attr)
	assertError(t, err, metadata.ErrAlreadyExists, "duplicate symlink")
}

func TestGetFilesystemCapabilities(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	caps, err := store.GetFilesystemCapabilities(context.Background(), rootHandle)
	if err != nil {
		t.Fatalf("GetFilesystemCapabilities failed: %v", err)
	}

	// Verify capabilities are reasonable
	if caps.MaxReadSize == 0 {
		t.Error("expected non-zero MaxReadSize")
	}
	if caps.MaxWriteSize == 0 {
		t.Error("expected non-zero MaxWriteSize")
	}
	if caps.MaxFileSize == 0 {
		t.Error("expected non-zero MaxFileSize")
	}
	if caps.MaxFilenameLen == 0 {
		t.Error("expected non-zero MaxFilenameLen")
	}
}

func TestGetFilesystemStatistics(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	// Get initial statistics
	stats1, err := store.GetFilesystemStatistics(context.Background(), rootHandle)
	if err != nil {
		t.Fatalf("GetFilesystemStatistics failed: %v", err)
	}

	initialUsedBytes := stats1.UsedBytes
	initialUsedFiles := stats1.UsedFiles

	// Create a file
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Write some data
	writeOp, _ := store.PrepareWrite(ctx, fileHandle, 1024)
	_, _ = store.CommitWrite(ctx, writeOp)

	// Get statistics again
	stats2, err := store.GetFilesystemStatistics(context.Background(), rootHandle)
	if err != nil {
		t.Fatalf("second GetFilesystemStatistics failed: %v", err)
	}

	// Used bytes should increase
	if stats2.UsedBytes <= initialUsedBytes {
		t.Errorf("expected UsedBytes to increase from %d to at least %d",
			initialUsedBytes, initialUsedBytes+1024)
	}

	// Used files should increase by at least 1 (the file we created)
	if stats2.UsedFiles <= initialUsedFiles {
		t.Errorf("expected UsedFiles to increase from %d", initialUsedFiles)
	}

	// Verify available space decreases
	if stats2.AvailableBytes >= stats1.AvailableBytes {
		t.Error("expected AvailableBytes to decrease")
	}
}

func TestGetFilesystemStatistics_Caching(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Get statistics
	stats1, err := store.GetFilesystemStatistics(context.Background(), rootHandle)
	if err != nil {
		t.Fatalf("GetFilesystemStatistics failed: %v", err)
	}

	// Get statistics again immediately (should use cache)
	stats2, err := store.GetFilesystemStatistics(context.Background(), rootHandle)
	if err != nil {
		t.Fatalf("second GetFilesystemStatistics failed: %v", err)
	}

	// Should return same values (from cache)
	if stats1.UsedBytes != stats2.UsedBytes {
		t.Error("expected cached statistics to match")
	}
	if stats1.UsedFiles != stats2.UsedFiles {
		t.Error("expected cached file count to match")
	}
}

func TestHealthcheck(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := store.Healthcheck(ctx)
	if err != nil {
		t.Fatalf("Healthcheck failed: %v", err)
	}
}

func TestHealthcheck_AfterClose(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)

	// Close the store
	store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Healthcheck should fail
	err := store.Healthcheck(ctx)
	if err == nil {
		t.Fatal("expected healthcheck to fail after Close")
	}
}

func TestGetServerConfig(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	// Initially should be empty or default
	_, err := store.GetServerConfig(context.Background())
	if err != nil && err.(*metadata.StoreError).Code != metadata.ErrNotFound {
		t.Fatalf("GetServerConfig failed: %v", err)
	}

	// Set config
	newConfig := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"setting1": "value1",
			"setting2": 42,
		},
	}

	err = store.SetServerConfig(context.Background(), newConfig)
	if err != nil {
		t.Fatalf("SetServerConfig failed: %v", err)
	}

	// Get config again
	retrieved, err := store.GetServerConfig(context.Background())
	if err != nil {
		t.Fatalf("GetServerConfig after set failed: %v", err)
	}

	// Verify settings
	if len(retrieved.CustomSettings) != 2 {
		t.Errorf("expected 2 settings, got %d", len(retrieved.CustomSettings))
	}
	if retrieved.CustomSettings["setting1"] != "value1" {
		t.Errorf("expected setting1='value1', got '%v'", retrieved.CustomSettings["setting1"])
	}
}

func TestSetServerConfig_Update(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	// Set initial config
	config1 := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"key1": "value1",
		},
	}
	_ = store.SetServerConfig(context.Background(), config1)

	// Update config
	config2 := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"key2": "value2",
		},
	}
	err := store.SetServerConfig(context.Background(), config2)
	if err != nil {
		t.Fatalf("SetServerConfig update failed: %v", err)
	}

	// Verify updated config
	retrieved, _ := store.GetServerConfig(context.Background())
	if len(retrieved.CustomSettings) != 1 {
		t.Errorf("expected 1 setting after update, got %d", len(retrieved.CustomSettings))
	}
	if retrieved.CustomSettings["key2"] != "value2" {
		t.Error("expected updated config")
	}
}

func TestGetShareNameForHandle(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, shareName := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Get share name from handle
	retrievedShareName, err := store.GetShareNameForHandle(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("GetShareNameForHandle failed: %v", err)
	}

	if retrievedShareName != shareName {
		t.Errorf("expected share name '%s', got '%s'", shareName, retrievedShareName)
	}
}

func TestGetShareNameForHandle_InvalidHandle(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	// Invalid handle
	invalidHandle := metadata.FileHandle([]byte("invalid:not-a-uuid"))

	_, err := store.GetShareNameForHandle(context.Background(), invalidHandle)
	assertError(t, err, metadata.ErrInvalidHandle, "invalid handle")
}

func TestCreateSpecialFile_NotSupported(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	// Try to create device file
	_, err := store.CreateSpecialFile(ctx, rootHandle, "device", metadata.FileTypeCharDevice, attr, 0, 0)
	assertError(t, err, metadata.ErrNotSupported, "create special file")
}

func TestCreateHardLink_NotSupported(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer store.Close()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)

	// Try to create hard link
	err := store.CreateHardLink(ctx, rootHandle, "link.txt", fileHandle)
	assertError(t, err, metadata.ErrNotSupported, "create hard link")
}

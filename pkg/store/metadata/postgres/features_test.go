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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	_ = store.Close()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

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
	defer func() { _ = store.Close() }()

	// Invalid handle
	invalidHandle := metadata.FileHandle([]byte("invalid:not-a-uuid"))

	_, err := store.GetShareNameForHandle(context.Background(), invalidHandle)
	assertError(t, err, metadata.ErrInvalidHandle, "invalid handle")
}

func TestCreateSpecialFile_CharDevice(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Use root context for device creation
	rootUID := uint32(0)
	rootGID := uint32(0)
	rootCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID:  &rootUID,
			GID:  &rootGID,
			GIDs: []uint32{0},
		},
	}

	attr := &metadata.FileAttr{
		Mode: 0644,
	}

	// Create character device (like /dev/null - major 1, minor 3)
	file, err := store.CreateSpecialFile(rootCtx, rootHandle, "null", metadata.FileTypeCharDevice, attr, 1, 3)
	if err != nil {
		t.Fatalf("CreateSpecialFile (char device) failed: %v", err)
	}

	if file.Type != metadata.FileTypeCharDevice {
		t.Errorf("expected char device type, got %v", file.Type)
	}
	if file.Path != "/null" {
		t.Errorf("expected path '/null', got '%s'", file.Path)
	}
	if file.Size != 0 {
		t.Errorf("expected size 0, got %d", file.Size)
	}
}

func TestCreateSpecialFile_BlockDevice(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Use root context for device creation
	rootUID := uint32(0)
	rootGID := uint32(0)
	rootCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID:  &rootUID,
			GID:  &rootGID,
			GIDs: []uint32{0},
		},
	}

	attr := &metadata.FileAttr{
		Mode: 0660,
	}

	// Create block device (like /dev/sda - major 8, minor 0)
	file, err := store.CreateSpecialFile(rootCtx, rootHandle, "sda", metadata.FileTypeBlockDevice, attr, 8, 0)
	if err != nil {
		t.Fatalf("CreateSpecialFile (block device) failed: %v", err)
	}

	if file.Type != metadata.FileTypeBlockDevice {
		t.Errorf("expected block device type, got %v", file.Type)
	}
}

func TestCreateSpecialFile_FIFO(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	// Create FIFO (named pipe) - non-root users can create FIFOs
	file, err := store.CreateSpecialFile(ctx, rootHandle, "mypipe", metadata.FileTypeFIFO, attr, 0, 0)
	if err != nil {
		t.Fatalf("CreateSpecialFile (FIFO) failed: %v", err)
	}

	if file.Type != metadata.FileTypeFIFO {
		t.Errorf("expected FIFO type, got %v", file.Type)
	}
	if file.Path != "/mypipe" {
		t.Errorf("expected path '/mypipe', got '%s'", file.Path)
	}
}

func TestCreateSpecialFile_Socket(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0755,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	// Create Unix socket - non-root users can create sockets
	file, err := store.CreateSpecialFile(ctx, rootHandle, "mysock", metadata.FileTypeSocket, attr, 0, 0)
	if err != nil {
		t.Fatalf("CreateSpecialFile (socket) failed: %v", err)
	}

	if file.Type != metadata.FileTypeSocket {
		t.Errorf("expected socket type, got %v", file.Type)
	}
}

func TestCreateSpecialFile_DeviceRequiresRoot(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Use non-root context
	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	// Non-root user trying to create device should fail
	_, err := store.CreateSpecialFile(ctx, rootHandle, "device", metadata.FileTypeCharDevice, attr, 1, 3)
	assertError(t, err, metadata.ErrAccessDenied, "create device as non-root")
}

func TestCreateSpecialFile_InvalidType(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0644,
	}

	// Regular file type is not valid for CreateSpecialFile
	_, err := store.CreateSpecialFile(ctx, rootHandle, "test", metadata.FileTypeRegular, attr, 0, 0)
	assertError(t, err, metadata.ErrInvalidArgument, "create special file with regular type")
}

func TestCreateSpecialFile_AlreadyExists(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()

	attr := &metadata.FileAttr{
		Mode: 0644,
		UID:  *ctx.Identity.UID,
		GID:  *ctx.Identity.GID,
	}

	// Create FIFO
	_, err := store.CreateSpecialFile(ctx, rootHandle, "mypipe", metadata.FileTypeFIFO, attr, 0, 0)
	if err != nil {
		t.Fatalf("First CreateSpecialFile failed: %v", err)
	}

	// Try to create again with same name
	_, err = store.CreateSpecialFile(ctx, rootHandle, "mypipe", metadata.FileTypeFIFO, attr, 0, 0)
	assertError(t, err, metadata.ErrAlreadyExists, "create duplicate special file")
}

func TestCreateHardLink(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)

	// Create hard link
	err := store.CreateHardLink(ctx, rootHandle, "link.txt", fileHandle)
	if err != nil {
		t.Fatalf("CreateHardLink failed: %v", err)
	}

	// Lookup the link and verify it points to the same file
	link, err := store.Lookup(ctx, rootHandle, "link.txt")
	if err != nil {
		t.Fatalf("Lookup link failed: %v", err)
	}

	// Should have the same ID as the original
	if link.ID != file.ID {
		t.Errorf("expected link to have same ID as original, got %v vs %v", link.ID, file.ID)
	}

	// Should have the same content
	if link.ContentID != file.ContentID {
		t.Errorf("expected link to have same ContentID as original")
	}
}

func TestCreateHardLink_VerifyLinkCountIncrement(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)

	// Create first hard link
	err := store.CreateHardLink(ctx, rootHandle, "link1.txt", fileHandle)
	if err != nil {
		t.Fatalf("CreateHardLink 1 failed: %v", err)
	}

	// Create second hard link
	err = store.CreateHardLink(ctx, rootHandle, "link2.txt", fileHandle)
	if err != nil {
		t.Fatalf("CreateHardLink 2 failed: %v", err)
	}

	// Verify all three names resolve to the same file
	original, _ := store.Lookup(ctx, rootHandle, "original.txt")
	link1, _ := store.Lookup(ctx, rootHandle, "link1.txt")
	link2, _ := store.Lookup(ctx, rootHandle, "link2.txt")

	if original.ID != link1.ID || original.ID != link2.ID {
		t.Error("all links should reference the same file ID")
	}
}

func TestCreateHardLink_RemoveOriginal(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)
	originalID := file.ID

	// Create hard link
	err := store.CreateHardLink(ctx, rootHandle, "hardlink.txt", fileHandle)
	if err != nil {
		t.Fatalf("CreateHardLink failed: %v", err)
	}

	// Remove the original file
	_, err = store.RemoveFile(ctx, rootHandle, "original.txt")
	if err != nil {
		t.Fatalf("RemoveFile original failed: %v", err)
	}

	// Verify original is gone
	_, err = store.Lookup(ctx, rootHandle, "original.txt")
	assertError(t, err, metadata.ErrNotFound, "original should be gone")

	// Verify hard link still exists and points to the same file
	link, err := store.Lookup(ctx, rootHandle, "hardlink.txt")
	if err != nil {
		t.Fatalf("Lookup hardlink failed after original deleted: %v", err)
	}

	// Should still have the same file ID
	if link.ID != originalID {
		t.Errorf("hard link should still reference same file ID, got %v vs %v", link.ID, originalID)
	}

	// Should still be retrievable via GetFile
	linkHandle := getFileHandle(link)
	_, err = store.GetFile(ctx.Context, linkHandle)
	if err != nil {
		t.Errorf("GetFile on hard link failed after original deleted: %v", err)
	}
}

// TestCreateHardLink_RemoveOriginal_ContentIDClearing verifies that when a file with
// hard links is removed, the returned File has an empty ContentID to signal that the
// caller should NOT delete the content (other hard links still reference it).
func TestCreateHardLink_RemoveOriginal_ContentIDClearing(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)
	originalContentID := file.ContentID

	// Verify original file has a ContentID
	if originalContentID == "" {
		t.Fatal("expected file to have a ContentID")
	}

	// Create hard link
	err := store.CreateHardLink(ctx, rootHandle, "hardlink.txt", fileHandle)
	if err != nil {
		t.Fatalf("CreateHardLink failed: %v", err)
	}

	// Remove the original file - ContentID should be cleared since hard link exists
	removed, err := store.RemoveFile(ctx, rootHandle, "original.txt")
	if err != nil {
		t.Fatalf("RemoveFile original failed: %v", err)
	}

	// ContentID should be empty because link count > 0
	if removed.ContentID != "" {
		t.Errorf("expected empty ContentID when other hard links exist, got '%s'", removed.ContentID)
	}

	// Now remove the hard link - this should return the ContentID
	removedLast, err := store.RemoveFile(ctx, rootHandle, "hardlink.txt")
	if err != nil {
		t.Fatalf("RemoveFile hardlink failed: %v", err)
	}

	// ContentID should be present because this was the last link
	if removedLast.ContentID != originalContentID {
		t.Errorf("expected ContentID '%s' when last link removed, got '%s'",
			originalContentID, removedLast.ContentID)
	}
}

func TestCreateHardLink_AlreadyExists(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)

	// Create first hard link
	err := store.CreateHardLink(ctx, rootHandle, "link.txt", fileHandle)
	if err != nil {
		t.Fatalf("CreateHardLink failed: %v", err)
	}

	// Try to create another link with the same name
	err = store.CreateHardLink(ctx, rootHandle, "link.txt", fileHandle)
	assertError(t, err, metadata.ErrAlreadyExists, "duplicate hard link")
}

func TestCreateHardLink_ToDirectory(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	dir := createTestDirectory(t, store, ctx, rootHandle, "testdir")
	dirHandle := getFileHandle(dir)

	// Try to create hard link to directory
	err := store.CreateHardLink(ctx, rootHandle, "link", dirHandle)
	assertError(t, err, metadata.ErrIsDirectory, "hard link to directory")
}

func TestCreateHardLink_InvalidName(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)

	// Try invalid names
	testCases := []string{"", ".", ".."}
	for _, name := range testCases {
		err := store.CreateHardLink(ctx, rootHandle, name, fileHandle)
		assertError(t, err, metadata.ErrInvalidArgument, "invalid name: "+name)
	}
}

func TestCreateHardLink_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create a restricted directory as root
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0500, // Only owner can read/execute, no write
		UID:  0,
		GID:  0,
	}
	dir, err := store.Create(rootCtx, rootHandle, "restricted", attr)
	if err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}
	dirHandle := getFileHandle(dir)

	// Create a file as root in root directory
	file := createTestFile(t, store, rootCtx, rootHandle, "original.txt")
	fileHandle := getFileHandle(file)

	// Try to create hard link as non-root user in restricted directory
	userCtx := createTestAuthContext()
	err = store.CreateHardLink(userCtx, dirHandle, "link.txt", fileHandle)
	assertError(t, err, metadata.ErrPermissionDenied, "write permission denied")
}

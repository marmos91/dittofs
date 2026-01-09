package postgres

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestPrepareWrite(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Prepare write
	writeOp, err := store.PrepareWrite(ctx, fileHandle, 1024)
	if err != nil {
		t.Fatalf("PrepareWrite failed: %v", err)
	}

	// Verify write operation
	if !bytes.Equal(writeOp.Handle, fileHandle) {
		t.Errorf("expected handle %v, got %v", fileHandle, writeOp.Handle)
	}
	if writeOp.NewSize != 1024 {
		t.Errorf("expected new size 1024, got %d", writeOp.NewSize)
	}
	if writeOp.ContentID == "" {
		t.Error("expected non-empty ContentID")
	}
	if writeOp.PreWriteAttr == nil {
		t.Fatal("expected non-nil PreWriteAttr")
	}
	if writeOp.PreWriteAttr.Size != 0 {
		t.Errorf("expected pre-write size 0, got %d", writeOp.PreWriteAttr.Size)
	}
}

func TestPrepareWrite_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create file with root ownership and mode 0600
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0600, // Only owner can write
		UID:  0,
		GID:  0,
	}
	file, err := store.Create(rootCtx, rootHandle, "restricted.txt", attr)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	fileHandle := getFileHandle(file)

	// Try to prepare write as non-root user
	userCtx := createTestAuthContext()
	_, err = store.PrepareWrite(userCtx, fileHandle, 1024)
	assertError(t, err, metadata.ErrPermissionDenied, "write permission denied")
}

func TestPrepareWrite_NotRegularFile(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	dir := createTestDirectory(t, store, ctx, rootHandle, "testdir")
	dirHandle := getFileHandle(dir)

	// Try to prepare write on directory
	_, err := store.PrepareWrite(ctx, dirHandle, 1024)
	assertError(t, err, metadata.ErrIsDirectory, "write to directory")
}

func TestCommitWrite(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Prepare write
	writeOp, err := store.PrepareWrite(ctx, fileHandle, 1024)
	if err != nil {
		t.Fatalf("PrepareWrite failed: %v", err)
	}

	// Commit write
	updatedFile, err := store.CommitWrite(ctx, writeOp)
	if err != nil {
		t.Fatalf("CommitWrite failed: %v", err)
	}

	// Verify updated file
	if updatedFile.Size != 1024 {
		t.Errorf("expected size 1024, got %d", updatedFile.Size)
	}

	// Mtime should be updated
	if !updatedFile.Mtime.After(file.Mtime) && !updatedFile.Mtime.Equal(file.Mtime) {
		t.Error("expected mtime to be updated")
	}

	// Ctime should be updated
	if !updatedFile.Ctime.After(file.Ctime) {
		t.Error("expected ctime to be updated")
	}

	// Verify via GetFile
	retrieved, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}

	if retrieved.Size != 1024 {
		t.Errorf("expected retrieved size 1024, got %d", retrieved.Size)
	}
}

func TestCommitWrite_MultipleWrites(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// First write (0 -> 1024)
	writeOp1, _ := store.PrepareWrite(ctx, fileHandle, 1024)
	updatedFile1, err := store.CommitWrite(ctx, writeOp1)
	if err != nil {
		t.Fatalf("CommitWrite 1 failed: %v", err)
	}
	if updatedFile1.Size != 1024 {
		t.Errorf("write 1: expected size 1024, got %d", updatedFile1.Size)
	}

	// Second write (1024 -> 2048)
	writeOp2, _ := store.PrepareWrite(ctx, fileHandle, 2048)
	updatedFile2, err := store.CommitWrite(ctx, writeOp2)
	if err != nil {
		t.Fatalf("CommitWrite 2 failed: %v", err)
	}
	if updatedFile2.Size != 2048 {
		t.Errorf("write 2: expected size 2048, got %d", updatedFile2.Size)
	}

	// Third write at earlier offset (simulates out-of-order writes)
	// CommitWrite should NOT shrink the file - only SETATTR can truncate
	// This tests that concurrent writes completing out of order don't corrupt size
	writeOp3, _ := store.PrepareWrite(ctx, fileHandle, 512)
	updatedFile3, err := store.CommitWrite(ctx, writeOp3)
	if err != nil {
		t.Fatalf("CommitWrite 3 failed: %v", err)
	}
	// Size should remain at 2048 (the largest size seen), not shrink to 512
	if updatedFile3.Size != 2048 {
		t.Errorf("write 3: expected size to remain at 2048 (no shrink via CommitWrite), got %d", updatedFile3.Size)
	}
}

func TestPrepareRead(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Write some data first
	writeOp, _ := store.PrepareWrite(ctx, fileHandle, 1024)
	_, _ = store.CommitWrite(ctx, writeOp)

	// Prepare read
	readMeta, err := store.PrepareRead(ctx, fileHandle)
	if err != nil {
		t.Fatalf("PrepareRead failed: %v", err)
	}

	// Verify read metadata
	if readMeta.Attr == nil {
		t.Fatal("expected non-nil Attr")
	}
	if readMeta.Attr.ContentID == "" {
		t.Error("expected non-empty ContentID")
	}
	if readMeta.Attr.Size != 1024 {
		t.Errorf("expected attr size 1024, got %d", readMeta.Attr.Size)
	}
}

func TestPrepareRead_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create file with root ownership and mode 0600
	rootCtx := createRootAuthContext()
	attr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0600, // Only owner can read
		UID:  0,
		GID:  0,
	}
	file, err := store.Create(rootCtx, rootHandle, "restricted.txt", attr)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	fileHandle := getFileHandle(file)

	// Try to prepare read as non-root user
	userCtx := createTestAuthContext()
	_, err = store.PrepareRead(userCtx, fileHandle)
	assertError(t, err, metadata.ErrPermissionDenied, "read permission denied")
}

func TestPrepareRead_NotRegularFile(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	dir := createTestDirectory(t, store, ctx, rootHandle, "testdir")
	dirHandle := getFileHandle(dir)

	// Try to prepare read on directory
	_, err := store.PrepareRead(ctx, dirHandle)
	assertError(t, err, metadata.ErrIsDirectory, "read from directory")
}

func TestSetFileAttributes(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	ctx := createTestAuthContext()
	file := createTestFile(t, store, ctx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Update attributes
	newMode := uint32(0755)
	newSize := uint64(2048)
	attrs := &metadata.SetAttrs{
		Mode: &newMode,
		Size: &newSize,
	}

	err := store.SetFileAttributes(ctx, fileHandle, attrs)
	if err != nil {
		t.Fatalf("SetFileAttributes failed: %v", err)
	}

	// Verify updated attributes
	updated, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}

	if updated.Mode != 0755 {
		t.Errorf("expected mode 0755, got %o", updated.Mode)
	}
	if updated.Size != 2048 {
		t.Errorf("expected size 2048, got %d", updated.Size)
	}

	// Ctime should be updated
	if !updated.Ctime.After(file.Ctime) {
		t.Error("expected ctime to be updated")
	}
}

func TestSetFileAttributes_Ownership(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create file as root
	rootCtx := createRootAuthContext()
	file := createTestFile(t, store, rootCtx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Change ownership as root
	newUID := uint32(1001)
	newGID := uint32(1001)
	attrs := &metadata.SetAttrs{
		UID: &newUID,
		GID: &newGID,
	}

	err := store.SetFileAttributes(rootCtx, fileHandle, attrs)
	if err != nil {
		t.Fatalf("SetFileAttributes failed: %v", err)
	}

	// Verify ownership changed
	updated, _ := store.GetFile(rootCtx.Context, fileHandle)
	if updated.UID != 1001 {
		t.Errorf("expected UID 1001, got %d", updated.UID)
	}
	if updated.GID != 1001 {
		t.Errorf("expected GID 1001, got %d", updated.GID)
	}
}

func TestSetFileAttributes_PermissionDenied(t *testing.T) {
	store, tc := setupTestStore(t)
	defer tc.cleanup(t)
	defer func() { _ = store.Close() }()

	rootHandle, _ := mustGetRootHandle(t, store)

	// Create file as root
	rootCtx := createRootAuthContext()
	file := createTestFile(t, store, rootCtx, rootHandle, "testfile.txt")
	fileHandle := getFileHandle(file)

	// Try to change ownership as non-root user
	userCtx := createTestAuthContext()
	newMode := uint32(0777)
	attrs := &metadata.SetAttrs{
		Mode: &newMode,
	}

	err := store.SetFileAttributes(userCtx, fileHandle, attrs)
	assertError(t, err, metadata.ErrPrivilegeRequired, "change attributes without ownership")
}

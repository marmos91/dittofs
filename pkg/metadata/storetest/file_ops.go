package storetest

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runFileOpsTests runs all file operation conformance tests.
func runFileOpsTests(t *testing.T, factory StoreFactory) {
	t.Run("CreateFile", func(t *testing.T) { testCreateFile(t, factory) })
	t.Run("GetFile", func(t *testing.T) { testGetFile(t, factory) })
	t.Run("DeleteFile", func(t *testing.T) { testDeleteFile(t, factory) })
	t.Run("CreateHardLink", func(t *testing.T) { testCreateHardLink(t, factory) })
	t.Run("ContentIdStableAcrossRename", func(t *testing.T) { testContentIdStableAcrossRename(t, factory) })
	t.Run("HardLinkRenameKeepsOtherName", func(t *testing.T) { testHardLinkRenameKeepsOtherName(t, factory) })
	t.Run("DerivedPathReflectsParentRename", func(t *testing.T) { testDerivedPathReflectsParentRename(t, factory) })
	t.Run("SetFileAttributes", func(t *testing.T) { testSetFileAttributes(t, factory) })
	t.Run("Rename", func(t *testing.T) { testRename(t, factory) })
	t.Run("GetFileNotFound", func(t *testing.T) { testGetFileNotFound(t, factory) })
	t.Run("GetChildNotFound", func(t *testing.T) { testGetChildNotFound(t, factory) })
	t.Run("TimestampPrecision", func(t *testing.T) { testTimestampPrecision(t, factory) })
	t.Run("HighModeBits", func(t *testing.T) { testHighModeBits(t, factory) })
}

// testTimestampPrecision verifies file timestamps round-trip with full
// nanosecond (sub-microsecond) fidelity through PutFile/GetFile on every
// backend. SMB FILETIME carries 100ns granularity; a backend that truncates to
// microseconds (the postgres TIMESTAMPTZ default) returns a different FILETIME
// on QUERY than was set, failing WPTS BVT_SMB2Basic_QueryAndSet_FileInfo
// while memory/badger pass (#882). This is the deterministic CI replacement for
// that WPTS assertion's precision class (#869).
func testTimestampPrecision(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", rootHandle, "ts.txt", 0644)
	ctx := t.Context()

	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	// 100ns-granular, sub-microsecond components (123456700ns has a 700ns
	// remainder a microsecond column would truncate). Use UTC so the
	// comparison is location-independent.
	mtime := time.Unix(1700000000, 123456700).UTC()
	atime := time.Unix(1699999999, 987654300).UTC()
	ctime := time.Unix(1700000001, 100).UTC()
	creation := time.Unix(1699999998, 999999900).UTC()

	file.Mtime = mtime
	file.Atime = atime
	file.Ctime = ctime
	file.CreationTime = creation

	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}

	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after put failed: %v", err)
	}

	check := func(name string, want, have time.Time) {
		if !have.Equal(want) {
			t.Errorf("%s = %d ns, want %d ns (delta %d ns) — backend truncates sub-microsecond precision",
				name, have.UnixNano(), want.UnixNano(), want.UnixNano()-have.UnixNano())
		}
	}
	check("Mtime", mtime, got.Mtime)
	check("Atime", atime, got.Atime)
	check("Ctime", ctime, got.Ctime)
	check("CreationTime", creation, got.CreationTime)
}

// testHighModeBits verifies a file mode carrying high bits above the POSIX
// permission range round-trips through PutFile/GetFile on every backend. The
// SMB adapter stores DOS attributes (e.g. modeDOSExplicit = 0x10000) in high
// mode bits; a backend that range-checks mode to <= 0o7777 rejects a SET_INFO
// FILE_BASIC_INFORMATION with attributes as STATUS_INVALID_PARAMETER, failing
// the WPTS BVT ChangeNotify tests on postgres while memory/badger pass (#882).
func testHighModeBits(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	handle := createTestFile(t, store, "/test", rootHandle, "mode.txt", 0644)
	ctx := t.Context()

	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	// 0x10000 | 0o644: modeDOSExplicit set plus POSIX rw-r--r--, the shape
	// SMBModeFromAttrs produces for a SET_INFO with FileAttributes.
	const highMode = uint32(0x10000 | 0o644)
	file.Mode = highMode
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() with high mode bits 0x%X failed: %v", highMode, err)
	}

	got, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after put failed: %v", err)
	}
	if got.Mode != highMode {
		t.Errorf("Mode = 0x%X, want 0x%X — backend dropped high mode bits", got.Mode, highMode)
	}
}

// testCreateFile verifies that creating a file results in a retrievable entry with correct attributes.
func testCreateFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "hello.txt", 0644)

	ctx := t.Context()

	// Verify the file exists via GetFile
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	if file.Type != metadata.FileTypeRegular {
		t.Errorf("Type = %v, want FileTypeRegular", file.Type)
	}
	if file.Mode != 0644 {
		t.Errorf("Mode = %o, want 0644", file.Mode)
	}
	if file.UID != 1000 {
		t.Errorf("UID = %d, want 1000", file.UID)
	}
	if file.GID != 1000 {
		t.Errorf("GID = %d, want 1000", file.GID)
	}

	// Verify handle is non-nil
	if handle == nil {
		t.Error("handle should not be nil")
	}
}

// testGetFile verifies that creating then getting a file returns consistent data.
func testGetFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "test.txt", 0600)

	ctx := t.Context()

	// Get the file
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	// Verify roundtrip
	if file.Type != metadata.FileTypeRegular {
		t.Errorf("Type = %v, want FileTypeRegular", file.Type)
	}
	if file.Mode != 0600 {
		t.Errorf("Mode = %o, want 0600", file.Mode)
	}

	// Verify child lookup works
	childHandle, err := store.GetChild(ctx, rootHandle, "test.txt")
	if err != nil {
		t.Fatalf("GetChild() failed: %v", err)
	}
	if string(childHandle) != string(handle) {
		t.Error("GetChild() returned different handle than expected")
	}
}

// testDeleteFile verifies that deleting a file removes it from the store.
func testDeleteFile(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "todelete.txt", 0644)

	ctx := t.Context()

	// Delete the file
	if err := store.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile() failed: %v", err)
	}

	// Remove child entry
	if err := store.DeleteChild(ctx, rootHandle, "todelete.txt"); err != nil {
		t.Fatalf("DeleteChild() failed: %v", err)
	}

	// Verify file is gone
	_, err := store.GetFile(ctx, handle)
	if err == nil {
		t.Error("GetFile() should fail after deletion")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}

	// Verify child is gone
	_, err = store.GetChild(ctx, rootHandle, "todelete.txt")
	if err == nil {
		t.Error("GetChild() should fail after deletion")
	}
}

// testCreateHardLink verifies hard link creation and link count tracking.
func testCreateHardLink(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "original.txt", 0644)

	ctx := t.Context()

	// Add a hard link (new name pointing to same handle)
	if err := store.SetChild(ctx, rootHandle, "link.txt", handle); err != nil {
		t.Fatalf("SetChild() for hard link failed: %v", err)
	}

	// Increment link count
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}

	// Verify link count
	count, err := store.GetLinkCount(ctx, handle)
	if err != nil {
		t.Fatalf("GetLinkCount() failed: %v", err)
	}
	if count != 2 {
		t.Errorf("link count = %d, want 2", count)
	}

	// Verify both names resolve to same handle
	h1, err := store.GetChild(ctx, rootHandle, "original.txt")
	if err != nil {
		t.Fatalf("GetChild(original.txt) failed: %v", err)
	}
	h2, err := store.GetChild(ctx, rootHandle, "link.txt")
	if err != nil {
		t.Fatalf("GetChild(link.txt) failed: %v", err)
	}
	if string(h1) != string(h2) {
		t.Error("hard link handles should be identical")
	}
}

// testHardLinkRenameKeepsOtherName is the core regression #1166 enables:
// renaming one name of a hard-linked inode must not break, stale, or detach the
// inode's other names. This was the postgres-only failure class (the single
// canonical files.path went dead when the matching name was renamed away). With
// File.Path derived from the namespace on every backend, the survivor stays
// fully reachable.
//
// Steps: create A, hard-link it as B, rename A->C; assert (a) B still resolves
// to the same inode, (b) GetFile on the inode succeeds with a valid derived
// path that is one of the live names (B or C), (c) link count unchanged at 2.
// Then unlink C and assert B still resolves and nlink decremented to 1.
func testHardLinkRenameKeepsOtherName(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	ctx := t.Context()

	// Create A and a second hard link B to the same inode.
	handle := createTestFile(t, store, "/test", rootHandle, "A", 0644)
	if err := store.SetChild(ctx, rootHandle, "B", handle); err != nil {
		t.Fatalf("SetChild(B) failed: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 2); err != nil {
		t.Fatalf("SetLinkCount(2) failed: %v", err)
	}

	// Rename A -> C (drop the old edge, add the new one). The inode keeps its
	// other name B; only the A edge is replaced by C.
	if err := store.DeleteChild(ctx, rootHandle, "A"); err != nil {
		t.Fatalf("DeleteChild(A) failed: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "C", handle); err != nil {
		t.Fatalf("SetChild(C) failed: %v", err)
	}

	// (a) B still resolves to the same inode.
	bHandle, err := store.GetChild(ctx, rootHandle, "B")
	if err != nil {
		t.Fatalf("GetChild(B) after rename failed: %v", err)
	}
	if string(bHandle) != string(handle) {
		t.Errorf("B resolves to a different inode after renaming A->C")
	}
	// A is gone; C resolves to the inode.
	if _, err := store.GetChild(ctx, rootHandle, "A"); err == nil {
		t.Error("GetChild(A) should fail after rename A->C")
	}
	cHandle, err := store.GetChild(ctx, rootHandle, "C")
	if err != nil {
		t.Fatalf("GetChild(C) failed: %v", err)
	}
	if string(cHandle) != string(handle) {
		t.Errorf("C resolves to a different inode")
	}

	// (b) GetFile succeeds and returns a valid derived path that is one of the
	// inode's live names (B or C) — never the renamed-away A.
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile after rename failed: %v", err)
	}
	if file.Path != "/B" && file.Path != "/C" {
		t.Errorf("derived path = %q, want a live name (/B or /C)", file.Path)
	}

	// (c) link count unchanged at 2.
	count, err := store.GetLinkCount(ctx, handle)
	if err != nil {
		t.Fatalf("GetLinkCount failed: %v", err)
	}
	if count != 2 {
		t.Errorf("link count after rename = %d, want 2", count)
	}

	// Unlink C: drop the edge and decrement nlink. B must remain reachable.
	if err := store.DeleteChild(ctx, rootHandle, "C"); err != nil {
		t.Fatalf("DeleteChild(C) failed: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount(1) failed: %v", err)
	}

	bHandle, err = store.GetChild(ctx, rootHandle, "B")
	if err != nil {
		t.Fatalf("GetChild(B) after unlink(C) failed: %v", err)
	}
	if string(bHandle) != string(handle) {
		t.Errorf("B resolves to a different inode after unlink(C)")
	}

	file, err = store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile after unlink(C) failed: %v", err)
	}
	if file.Path != "/B" {
		t.Errorf("derived path after unlink(C) = %q, want /B", file.Path)
	}
	if file.Nlink != 1 {
		t.Errorf("Nlink after unlink(C) = %d, want 1", file.Nlink)
	}
}

// testDerivedPathReflectsParentRename asserts File.Path is read-derived: after
// renaming a parent directory, a child's GetFile returns the new path with no
// explicit per-descendant path-update pass (#1166 deleted updateDescendantPaths).
func testDerivedPathReflectsParentRename(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	ctx := t.Context()

	dirHandle := createTestDir(t, store, "/test", rootHandle, "olddir")
	childHandle := createTestFile(t, store, "/test", dirHandle, "child.txt", 0644)

	// Sanity: original derived path.
	child, err := store.GetFile(ctx, childHandle)
	if err != nil {
		t.Fatalf("GetFile(child) failed: %v", err)
	}
	if child.Path != "/olddir/child.txt" {
		t.Fatalf("initial child path = %q, want /olddir/child.txt", child.Path)
	}

	// Rename the parent directory edge only — no descendant path writes.
	if err := store.DeleteChild(ctx, rootHandle, "olddir"); err != nil {
		t.Fatalf("DeleteChild(olddir) failed: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "newdir", dirHandle); err != nil {
		t.Fatalf("SetChild(newdir) failed: %v", err)
	}

	// The child's path is reconstructed fresh from the namespace.
	child, err = store.GetFile(ctx, childHandle)
	if err != nil {
		t.Fatalf("GetFile(child) after parent rename failed: %v", err)
	}
	if child.Path != "/newdir/child.txt" {
		t.Errorf("derived child path = %q, want /newdir/child.txt", child.Path)
	}
}

// testContentIdStableAcrossRename verifies that a regular file's PayloadID
// (block-store content_id) is independent of its path: it survives a rename to
// a different directory unchanged, and GetFileByPayloadID still resolves to the
// same inode afterwards. UUID-based PayloadIDs (#1166 PR-3) make content_id
// stable across rename — a path-derived content_id would have gone stale and
// broken the flusher's GetFileByPayloadID lookup after the move.
func testContentIdStableAcrossRename(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")
	ctx := t.Context()

	dirHandle := createTestDir(t, store, "/test", rootHandle, "src")
	handle := createTestFile(t, store, "/test", dirHandle, "blob.dat", 0o644)

	// Stamp a PayloadID the way the service does at create time, then capture
	// it. The store-level harness bypasses the service, so set it explicitly.
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	const payloadID = metadata.PayloadID("test/blob-content-id")
	file.PayloadID = payloadID
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() with PayloadID failed: %v", err)
	}

	// Rename across directories: drop the old edge, add a new one under root.
	if err := store.DeleteChild(ctx, dirHandle, "blob.dat"); err != nil {
		t.Fatalf("DeleteChild(src/blob.dat) failed: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "renamed.dat", handle); err != nil {
		t.Fatalf("SetChild(renamed.dat) failed: %v", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent(root) failed: %v", err)
	}

	// The derived path changed, but the PayloadID must not.
	moved, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after rename failed: %v", err)
	}
	if moved.Path != "/renamed.dat" {
		t.Errorf("derived path after rename = %q, want /renamed.dat", moved.Path)
	}
	if moved.PayloadID != payloadID {
		t.Errorf("PayloadID changed across rename: got %q, want %q", moved.PayloadID, payloadID)
	}

	// And the content-id lookup must still resolve after the move, returning
	// the same inode's content_id and its new derived path.
	got, err := store.GetFileByPayloadID(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetFileByPayloadID() after rename failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetFileByPayloadID() after rename returned nil file")
	}
	if got.PayloadID != payloadID {
		t.Errorf("GetFileByPayloadID returned PayloadID %q, want %q", got.PayloadID, payloadID)
	}
	if got.Path != "/renamed.dat" {
		t.Errorf("GetFileByPayloadID returned path %q, want /renamed.dat", got.Path)
	}
}

// testSetFileAttributes verifies that file attributes can be updated.
func testSetFileAttributes(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "attrs.txt", 0644)

	ctx := t.Context()

	// Get the current file
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}

	// Modify attributes
	file.Mode = 0755
	file.UID = 2000
	file.Size = 1024

	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() with updated attrs failed: %v", err)
	}

	// Verify changes
	updated, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() after update failed: %v", err)
	}
	if updated.Mode != 0755 {
		t.Errorf("Mode = %o, want 0755", updated.Mode)
	}
	if updated.UID != 2000 {
		t.Errorf("UID = %d, want 2000", updated.UID)
	}
	if updated.Size != 1024 {
		t.Errorf("Size = %d, want 1024", updated.Size)
	}
}

// testRename verifies that renaming a file removes the old name and creates the new name.
func testRename(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	handle := createTestFile(t, store, "/test", rootHandle, "old.txt", 0644)

	ctx := t.Context()

	// Rename: remove old child, add new child
	if err := store.DeleteChild(ctx, rootHandle, "old.txt"); err != nil {
		t.Fatalf("DeleteChild(old.txt) failed: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "new.txt", handle); err != nil {
		t.Fatalf("SetChild(new.txt) failed: %v", err)
	}

	// Verify old name is gone
	_, err := store.GetChild(ctx, rootHandle, "old.txt")
	if err == nil {
		t.Error("GetChild(old.txt) should fail after rename")
	}

	// Verify new name exists
	newHandle, err := store.GetChild(ctx, rootHandle, "new.txt")
	if err != nil {
		t.Fatalf("GetChild(new.txt) failed: %v", err)
	}
	if string(newHandle) != string(handle) {
		t.Error("renamed handle should be the same")
	}
}

// testGetFileNotFound verifies that GetFile returns an appropriate error for non-existent handles.
func testGetFileNotFound(t *testing.T, factory StoreFactory) {
	store := factory(t)
	_ = createTestShare(t, store, "/test")

	ctx := t.Context()

	// Generate a handle that doesn't exist in the store
	fakeHandle, err := metadata.GenerateNewHandle("/test")
	if err != nil {
		t.Fatalf("GenerateNewHandle() failed: %v", err)
	}

	_, err = store.GetFile(ctx, fakeHandle)
	if err == nil {
		t.Error("GetFile() should fail for non-existent handle")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

// testGetChildNotFound verifies that GetChild returns an appropriate error for non-existent names.
func testGetChildNotFound(t *testing.T, factory StoreFactory) {
	store := factory(t)
	rootHandle := createTestShare(t, store, "/test")

	ctx := t.Context()

	_, err := store.GetChild(ctx, rootHandle, "nonexistent.txt")
	if err == nil {
		t.Error("GetChild() should fail for non-existent name")
	}
	if !metadata.IsNotFoundError(err) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

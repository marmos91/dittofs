package storetest

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// runStoreSurfaceTests covers MetadataStore interface methods that previously
// had ZERO cross-backend conformance coverage (area-6 audit H1): DeleteShare,
// GetUsedBytes, GetFileByPayloadID, filesystem meta/stats/caps, server config,
// and Healthcheck. These surfaces are production-consumed (share removal,
// statfs/quota, the background flusher) yet each backend implemented them
// independently, so divergence or regression could pass the full suite green
// — the exact CI-blind class that hid the postgres #853 bugs. Each scenario
// runs identically against memory, badger, and postgres via the factory.
func runStoreSurfaceTests(t *testing.T, factory StoreFactory) {
	t.Run("DeleteShare", func(t *testing.T) { testDeleteShare(t, factory) })
	t.Run("DeleteShareViaTransaction", func(t *testing.T) { testDeleteShareViaTransaction(t, factory) })
	t.Run("DuplicateCreateShare", func(t *testing.T) { testDuplicateCreateShare(t, factory) })
	t.Run("GetUsedBytes", func(t *testing.T) { testGetUsedBytes(t, factory) })
	t.Run("GetQuotaUsage", func(t *testing.T) { testGetQuotaUsage(t, factory) })
	t.Run("GetQuotaUsageChown", func(t *testing.T) { testGetQuotaUsageChown(t, factory) })
	t.Run("GetFileByPayloadID", func(t *testing.T) { testGetFileByPayloadID(t, factory) })
	t.Run("FilesystemMetaStatsCaps", func(t *testing.T) { testFilesystemMetaStatsCaps(t, factory) })
	t.Run("ServerConfigRoundTrip", func(t *testing.T) { testServerConfigRoundTrip(t, factory) })
	t.Run("Healthcheck", func(t *testing.T) { testHealthcheck(t, factory) })
	t.Run("Pagination", func(t *testing.T) { testListChildrenPagination(t, factory) })
	t.Run("DeleteSharePurgesUsedBytesAndObjectIndex", func(t *testing.T) { testDeleteSharePurgesCounters(t, factory) })
	t.Run("ListChildrenCursorAfterDeletedEntry", func(t *testing.T) { testListChildrenCursorAfterDelete(t, factory) })
}

func testDeleteSharePurgesCounters(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/purge-test"
	rootHandle := createTestShare(t, store, shareName)
	handle := createTestFile(t, store, shareName, rootHandle, "big.bin", 0o644)
	setFileSize(t, store, handle, 8192)

	// Set an ObjectID so the objectIndex secondary mapping is exercised.
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	for i := range file.ObjectID {
		file.ObjectID[i] = byte(i + 1)
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile with ObjectID: %v", err)
	}

	if got := store.GetUsedBytes(); got != 8192 {
		t.Fatalf("pre-delete GetUsedBytes() = %d, want 8192", got)
	}

	if err := store.DeleteShare(ctx, shareName); err != nil {
		t.Fatalf("DeleteShare() failed: %v", err)
	}

	// usedBytes must return to 0 after the share is deleted.
	if got := store.GetUsedBytes(); got != 0 {
		t.Fatalf("post-delete GetUsedBytes() = %d, want 0 — DeleteShare did not decrement usedBytes", got)
	}

	// Recreating the share and a file with the same ObjectID must not produce
	// ErrConflict — the objectIndex entry must have been purged.
	root2 := createTestShare(t, store, shareName)
	handle2 := createTestFile(t, store, shareName, root2, "big.bin", 0o644)
	file2, err := store.GetFile(ctx, handle2)
	if err != nil {
		t.Fatalf("GetFile after recreate: %v", err)
	}
	for i := range file2.ObjectID {
		file2.ObjectID[i] = byte(i + 1) // same ObjectID as before
	}
	if err := store.PutFile(ctx, file2); err != nil {
		t.Fatalf("PutFile same ObjectID after DeleteShare: got ErrConflict or unexpected error: %v — objectIndex was not purged by DeleteShare", err)
	}
}

func testListChildrenCursorAfterDelete(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/cursor-del"
	rootHandle := createTestShare(t, store, shareName)

	// Create entries a, b, c, d, e in sorted order.
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		createTestFile(t, store, shareName, rootHandle, name, 0o644)
	}

	// Page 1: limit=2, cursor="". Returns ["a","b"], nextCursor="b".
	page1, cur1, err := store.ListChildren(ctx, rootHandle, "", 2)
	if err != nil {
		t.Fatalf("ListChildren page1: %v", err)
	}
	if len(page1) != 2 || page1[0].Name != "a" || page1[1].Name != "b" {
		t.Fatalf("page1 = %v, want [a b]", namesOf(page1))
	}
	if cur1 != "b" {
		t.Fatalf("cursor after page1 = %q, want 'b'", cur1)
	}

	// Delete the cursor entry "b" to simulate a file deleted between READDIR calls.
	bHandle, err := store.GetChild(ctx, rootHandle, "b")
	if err != nil {
		t.Fatalf("GetChild(b): %v", err)
	}
	if err := store.DeleteChild(ctx, rootHandle, "b"); err != nil {
		t.Fatalf("DeleteChild(b): %v", err)
	}
	if err := store.DeleteFile(ctx, bHandle); err != nil {
		t.Fatalf("DeleteFile(b): %v", err)
	}

	// Page 2: cursor="b" but "b" no longer exists. Must return ["c","d"], not restart from "a".
	page2, _, err := store.ListChildren(ctx, rootHandle, cur1, 2)
	if err != nil {
		t.Fatalf("ListChildren page2: %v", err)
	}
	for _, e := range page2 {
		if e.Name == "a" || e.Name == "b" {
			t.Errorf("page2 contains %q — cursor did not advance past deleted entry, READDIR restarted", e.Name)
		}
	}
	if len(page2) == 0 {
		t.Fatal("page2 is empty — no entries after deleted cursor")
	}
	if page2[0].Name != "c" {
		t.Errorf("page2[0] = %q, want 'c' — cursor must resume after the deleted entry's sorted position", page2[0].Name)
	}
}

func namesOf(entries []metadata.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// setFileSize reads a file, sets its logical Size, and writes it back. Used to
// drive the per-backend usedBytes counter (it tracks size deltas on PutFile).
func setFileSize(t *testing.T, store metadata.Store, handle metadata.FileHandle, size uint64) {
	t.Helper()
	ctx := t.Context()

	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	file.Size = size
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}
}

// testDeleteShare verifies that DeleteShare removes the share AND its file
// metadata: ListShares must exclude it and the share-root must no longer
// resolve. The store.go:161 contract is "removes a share and all its
// metadata"; a backend that drops only the share row orphans every file and
// leaves the root inode occupying the unique root-path index, breaking
// same-name recreation.
func testDeleteShare(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/del-share"
	rootHandle := createTestShare(t, store, shareName)

	// Populate the share with a file and a subdirectory so DeleteShare has
	// metadata to reclaim, not just the bare root.
	createTestFile(t, store, shareName, rootHandle, "keep.txt", 0o644)
	subdir := createTestDir(t, store, shareName, rootHandle, "sub")
	createTestFile(t, store, shareName, subdir, "nested.txt", 0o644)

	if err := store.DeleteShare(ctx, shareName); err != nil {
		t.Fatalf("DeleteShare() failed: %v", err)
	}

	// ListShares must no longer report the deleted share.
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares() failed: %v", err)
	}
	for _, s := range shares {
		if s == shareName {
			t.Fatalf("ListShares() still includes deleted share %q: %v", shareName, shares)
		}
	}

	// The share root must no longer resolve.
	if _, err := store.GetRootHandle(ctx, shareName); err == nil {
		t.Error("GetRootHandle() should fail after DeleteShare")
	} else if !metadata.IsNotFoundError(err) {
		t.Errorf("GetRootHandle() after delete: got %v, want not-found", err)
	}

	// The file rows must be gone too — a backend that only drops the share
	// row leaves these resolvable (orphaned metadata).
	if _, err := store.GetFile(ctx, rootHandle); err == nil {
		t.Error("GetFile(root) should fail after DeleteShare — root inode orphaned")
	}

	// Recreating a share with the same name must succeed: the orphaned root
	// inode must not keep the unique root-path index occupied.
	if root2 := createTestShare(t, store, shareName); root2 == nil {
		t.Fatal("re-CreateShare after DeleteShare returned nil root handle")
	}

	// Deleting a share that does not exist returns not-found.
	if err := store.DeleteShare(ctx, "/never-existed"); err == nil {
		t.Error("DeleteShare(missing) should fail")
	} else if !metadata.IsNotFoundError(err) {
		t.Errorf("DeleteShare(missing): got %v, want not-found", err)
	}
}

// testDeleteShareViaTransaction exercises DeleteShare through WithTransaction.
// The transaction-path and pool-path are independent implementations, so the
// tx path must tear down all file metadata too — dropping only the share row
// orphans every file inode (the bug this pins for the badger/postgres tx path).
func testDeleteShareViaTransaction(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/del-share-tx"
	rootHandle := createTestShare(t, store, shareName)
	createTestFile(t, store, shareName, rootHandle, "keep.txt", 0o644)
	subdir := createTestDir(t, store, shareName, rootHandle, "sub")
	createTestFile(t, store, shareName, subdir, "nested.txt", 0o644)

	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteShare(ctx, shareName)
	}); err != nil {
		t.Fatalf("WithTransaction(DeleteShare) failed: %v", err)
	}

	// The share root must no longer resolve.
	if _, err := store.GetRootHandle(ctx, shareName); err == nil {
		t.Error("GetRootHandle() should fail after tx DeleteShare")
	}

	// File rows must be gone — a tx path that only drops the share row leaves
	// the root inode resolvable (orphaned metadata).
	if _, err := store.GetFile(ctx, rootHandle); err == nil {
		t.Error("GetFile(root) should fail after tx DeleteShare — root inode orphaned")
	}

	// Same-name recreation must succeed (no stale root inode in the index).
	if root2 := createTestShare(t, store, shareName); root2 == nil {
		t.Fatal("re-CreateShare after tx DeleteShare returned nil root handle")
	}
}

// testDuplicateCreateShare verifies the store.go:154 clause "Returns
// ErrAlreadyExists if share already exists" across backends. All three guard
// this individually; this is the missing shared-suite assertion.
func testDuplicateCreateShare(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/dup-share"
	if err := store.CreateShare(ctx, &metadata.Share{Name: shareName}); err != nil {
		t.Fatalf("first CreateShare() failed: %v", err)
	}

	err := store.CreateShare(ctx, &metadata.Share{Name: shareName})
	if err == nil {
		t.Fatal("duplicate CreateShare() should fail")
	}
	var se *metadata.StoreError
	if !errors.As(err, &se) || se.Code != metadata.ErrAlreadyExists {
		t.Fatalf("duplicate CreateShare: got %v, want StoreError{Code: ErrAlreadyExists}", err)
	}
}

// testGetUsedBytes verifies the incremental usedBytes counter (an O(1) atomic
// per backend, store.go:376) tracks a deterministic create→grow→truncate-down
// →delete sequence identically on every backend. Directories must never count.
func testGetUsedBytes(t *testing.T, factory StoreFactory) {
	store := factory(t)

	const shareName = "/used-bytes"
	rootHandle := createTestShare(t, store, shareName)

	if got := store.GetUsedBytes(); got != 0 {
		t.Fatalf("GetUsedBytes() = %d, want 0 on a fresh share", got)
	}

	// Create a regular file and grow it to 1000 bytes.
	fileHandle := createTestFile(t, store, shareName, rootHandle, "data.bin", 0o644)
	setFileSize(t, store, fileHandle, 1000)
	if got := store.GetUsedBytes(); got != 1000 {
		t.Fatalf("GetUsedBytes() = %d, want 1000 after a 1000-byte write", got)
	}

	// A directory must not move the counter.
	createTestDir(t, store, shareName, rootHandle, "subdir")
	if got := store.GetUsedBytes(); got != 1000 {
		t.Fatalf("GetUsedBytes() = %d, want 1000 — directories must not count", got)
	}

	// Truncate the file down to 250 bytes.
	setFileSize(t, store, fileHandle, 250)
	if got := store.GetUsedBytes(); got != 250 {
		t.Fatalf("GetUsedBytes() = %d, want 250 after truncate-down", got)
	}

	// Delete the file: the counter must return to 0.
	ctx := t.Context()
	if err := store.DeleteChild(ctx, rootHandle, "data.bin"); err != nil {
		t.Fatalf("DeleteChild() failed: %v", err)
	}
	if err := store.DeleteFile(ctx, fileHandle); err != nil {
		t.Fatalf("DeleteFile() failed: %v", err)
	}
	if got := store.GetUsedBytes(); got != 0 {
		t.Fatalf("GetUsedBytes() = %d, want 0 after deleting the only file", got)
	}
}

// createTestFileOwned creates a regular file owned by a specific uid/gid and a
// given size, wiring parent/child/link-count like createTestFile. Used by the
// per-identity quota usage conformance tests.
func createTestFileOwned(t *testing.T, store metadata.Store, shareName string, dirHandle metadata.FileHandle, name string, uid, gid uint32, size uint64) metadata.FileHandle {
	t.Helper()
	ctx := t.Context()

	fullPath := childFullPath(t, store, dirHandle, name)
	handle, err := store.GenerateHandle(ctx, shareName, fullPath)
	if err != nil {
		t.Fatalf("GenerateHandle() failed: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle() failed: %v", err)
	}
	file := &metadata.File{
		ShareName: shareName,
		Path:      fullPath,
		ID:        id,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  uid,
			GID:  gid,
			Size: size,
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() failed: %v", err)
	}
	if err := store.SetParent(ctx, handle, dirHandle); err != nil {
		t.Fatalf("SetParent() failed: %v", err)
	}
	if err := store.SetChild(ctx, dirHandle, name, handle); err != nil {
		t.Fatalf("SetChild() failed: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount() failed: %v", err)
	}
	return handle
}

func wantUsage(t *testing.T, store metadata.Store, scope metadata.QuotaScope, id uint32, wantBytes, wantFiles int64) {
	t.Helper()
	u, err := store.GetQuotaUsage(scope, id)
	if err != nil {
		t.Fatalf("GetQuotaUsage(%v, %d) failed: %v", scope, id, err)
	}
	if u.Bytes != wantBytes || u.Files != wantFiles {
		t.Fatalf("GetQuotaUsage(%v, %d) = {Bytes:%d Files:%d}, want {Bytes:%d Files:%d}",
			scope, id, u.Bytes, u.Files, wantBytes, wantFiles)
	}
}

// testGetQuotaUsage verifies per-identity usage accounting (bytes + file count)
// for both user and group scopes across create / grow / truncate / delete, and
// that distinct owners are tracked independently. Directories must never count.
func testGetQuotaUsage(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/quota-usage"
	rootHandle := createTestShare(t, store, shareName)

	// Fresh share: every identity reports zero.
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 0, 0)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 0, 0)

	// uid=1000/gid=2000 creates a 500-byte file.
	hA := createTestFileOwned(t, store, shareName, rootHandle, "a.bin", 1000, 2000, 500)
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 500, 1)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 500, 1)

	// uid=1000/gid=2000 creates a second 300-byte file: bytes+count roll up.
	createTestFileOwned(t, store, shareName, rootHandle, "b.bin", 1000, 2000, 300)
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 800, 2)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 800, 2)

	// A different owner (uid=1001/gid=2000) is tracked independently for the
	// user scope; the shared gid rolls them together.
	createTestFileOwned(t, store, shareName, rootHandle, "c.bin", 1001, 2000, 100)
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 800, 2)
	wantUsage(t, store, metadata.QuotaScopeUser, 1001, 100, 1)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 900, 3)

	// A directory must not count toward any identity.
	createTestDir(t, store, shareName, rootHandle, "subdir")
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 800, 2)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 900, 3)

	// Grow a.bin from 500 to 900: only the byte counter for its owner moves.
	setFileSize(t, store, hA, 900)
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 1200, 2)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 1300, 3)

	// Truncate a.bin down to 100: byte counter shrinks, count unchanged.
	setFileSize(t, store, hA, 100)
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 400, 2)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 500, 3)

	// Delete a.bin: bytes+count removed from its owner.
	if err := store.DeleteChild(ctx, rootHandle, "a.bin"); err != nil {
		t.Fatalf("DeleteChild() failed: %v", err)
	}
	if err := store.DeleteFile(ctx, hA); err != nil {
		t.Fatalf("DeleteFile() failed: %v", err)
	}
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 300, 1)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 400, 2)
}

// testGetQuotaUsageChown verifies that changing a regular file's UID/GID via
// PutFile moves its bytes+count from the old identity to the new — the
// easy-to-miss accounting event called out by #1151.
func testGetQuotaUsageChown(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/quota-chown"
	rootHandle := createTestShare(t, store, shareName)

	handle := createTestFileOwned(t, store, shareName, rootHandle, "f.bin", 1000, 2000, 400)
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 400, 1)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 400, 1)
	wantUsage(t, store, metadata.QuotaScopeUser, 1001, 0, 0)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2001, 0, 0)

	// Chown to uid=1001/gid=2001: usage moves wholesale.
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	file.UID = 1001
	file.GID = 2001
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() chown failed: %v", err)
	}
	wantUsage(t, store, metadata.QuotaScopeUser, 1000, 0, 0)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2000, 0, 0)
	wantUsage(t, store, metadata.QuotaScopeUser, 1001, 400, 1)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2001, 400, 1)

	// Chown only the gid (uid stays 1001): user scope unchanged, group moves.
	file, err = store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	file.GID = 2002
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() gid-only chown failed: %v", err)
	}
	wantUsage(t, store, metadata.QuotaScopeUser, 1001, 400, 1)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2001, 0, 0)
	wantUsage(t, store, metadata.QuotaScopeGroup, 2002, 400, 1)
}

// testGetFileByPayloadID verifies the flusher's content-id lookup: a file
// stored with a known PayloadID round-trips, and an unknown id returns
// not-found.
func testGetFileByPayloadID(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/payload"
	rootHandle := createTestShare(t, store, shareName)
	handle := createTestFile(t, store, shareName, rootHandle, "blob.dat", 0o644)

	const payloadID = metadata.PayloadID("payload-roundtrip-id")
	file, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile() failed: %v", err)
	}
	file.PayloadID = payloadID
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile() with PayloadID failed: %v", err)
	}

	got, err := store.GetFileByPayloadID(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetFileByPayloadID(known) failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetFileByPayloadID(known) returned nil file")
	}
	if got.PayloadID != payloadID {
		t.Errorf("GetFileByPayloadID returned PayloadID %q, want %q", got.PayloadID, payloadID)
	}

	// Miss: an unknown PayloadID returns not-found.
	if _, err := store.GetFileByPayloadID(ctx, metadata.PayloadID("does-not-exist")); err == nil {
		t.Error("GetFileByPayloadID(unknown) should fail")
	} else if !metadata.IsNotFoundError(err) {
		t.Errorf("GetFileByPayloadID(unknown): got %v, want not-found", err)
	}
}

// testFilesystemMetaStatsCaps verifies the filesystem metadata / statistics /
// capabilities surfaces.
//
// Note: GetFilesystemMeta does NOT round-trip a prior PutFilesystemMeta on the
// memory backend (memory always recomputes from store.capabilities + live
// statistics rather than reading back a persisted blob), so the cross-backend
// contract asserted here is the one every backend honors: GetFilesystemMeta
// returns a populated struct, and SetFilesystemCapabilities is observable via
// GetFilesystemCapabilities. Both capabilities and statistics resolve against
// a live root handle.
func testFilesystemMetaStatsCaps(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/fsmeta"
	rootHandle := createTestShare(t, store, shareName)

	// GetFilesystemMeta returns a non-nil struct with sane capabilities on
	// every backend.
	meta, err := store.GetFilesystemMeta(ctx, shareName)
	if err != nil {
		t.Fatalf("GetFilesystemMeta() failed: %v", err)
	}
	if meta == nil {
		t.Fatal("GetFilesystemMeta() returned nil")
	}
	if meta.Capabilities.MaxFilenameLen == 0 {
		t.Error("GetFilesystemMeta() Capabilities.MaxFilenameLen = 0, want a sane non-zero limit")
	}

	// Statistics resolve against the root handle and report a non-zero total.
	stats, err := store.GetFilesystemStatistics(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFilesystemStatistics() failed: %v", err)
	}
	if stats == nil {
		t.Fatal("GetFilesystemStatistics() returned nil")
	}
	if stats.TotalBytes == 0 {
		t.Error("GetFilesystemStatistics() TotalBytes = 0, want a non-zero total")
	}

	// SetFilesystemCapabilities must be observable through
	// GetFilesystemCapabilities (resolved against the root handle).
	caps, err := store.GetFilesystemCapabilities(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFilesystemCapabilities() failed: %v", err)
	}
	if caps == nil {
		t.Fatal("GetFilesystemCapabilities() returned nil")
	}
	updated := *caps
	updated.MaxFilenameLen = caps.MaxFilenameLen + 7
	store.SetFilesystemCapabilities(updated)

	after, err := store.GetFilesystemCapabilities(ctx, rootHandle)
	if err != nil {
		t.Fatalf("GetFilesystemCapabilities() after Set failed: %v", err)
	}
	if after.MaxFilenameLen != updated.MaxFilenameLen {
		t.Errorf("GetFilesystemCapabilities after Set: MaxFilenameLen = %d, want %d",
			after.MaxFilenameLen, updated.MaxFilenameLen)
	}
}

// testServerConfigRoundTrip verifies SetServerConfig is observable through
// GetServerConfig across backends.
func testServerConfigRoundTrip(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	// A fresh store reports an empty (or at least readable) config.
	if _, err := store.GetServerConfig(ctx); err != nil {
		t.Fatalf("GetServerConfig() on fresh store failed: %v", err)
	}

	want := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"smb.signing_required": true,
			"nfs.mount.allowed":    "192.168.1.0/24",
		},
	}
	if err := store.SetServerConfig(ctx, want); err != nil {
		t.Fatalf("SetServerConfig() failed: %v", err)
	}

	got, err := store.GetServerConfig(ctx)
	if err != nil {
		t.Fatalf("GetServerConfig() failed: %v", err)
	}
	if len(got.CustomSettings) != len(want.CustomSettings) {
		t.Fatalf("GetServerConfig CustomSettings len = %d, want %d (%v)",
			len(got.CustomSettings), len(want.CustomSettings), got.CustomSettings)
	}
	if got.CustomSettings["nfs.mount.allowed"] != "192.168.1.0/24" {
		t.Errorf("GetServerConfig nfs.mount.allowed = %v, want 192.168.1.0/24",
			got.CustomSettings["nfs.mount.allowed"])
	}
}

// testHealthcheck verifies a live store reports StatusHealthy.
func testHealthcheck(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	rep := store.Healthcheck(ctx)
	if rep.Status != health.StatusHealthy {
		t.Fatalf("Healthcheck() Status = %q (message %q), want %q",
			rep.Status, rep.Message, health.StatusHealthy)
	}
}

// testListChildrenPagination exercises the backend-divergent pagination path
// (postgres keyset vs badger iterator-prefix vs memory map). It creates more
// children than the page limit, threads nextCursor with a small limit until
// exhausted, and asserts the union equals the full set with no duplicates and
// no missing entries. It also asserts limit==0 selects the default page size
// (large enough to return everything in one page).
func testListChildrenPagination(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()

	const shareName = "/paged"
	rootHandle := createTestShare(t, store, shareName)

	// Create N children, N > the small page size K used below.
	const total = 25
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("entry-%02d.txt", i)
		createTestFile(t, store, shareName, rootHandle, name, 0o644)
		want[name] = true
	}

	// Page with a small limit, threading nextCursor until empty. Assert the
	// union equals the full set with no dup and no missing entry.
	const pageSize = 4
	seen := make(map[string]int, total)
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > total+5 {
			t.Fatalf("pagination did not terminate after %d pages — cursor likely not advancing", pages)
		}
		entries, next, err := store.ListChildren(ctx, rootHandle, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListChildren(cursor=%q) failed: %v", cursor, err)
		}
		if len(entries) > pageSize {
			t.Fatalf("page returned %d entries, exceeds limit %d", len(entries), pageSize)
		}
		for _, e := range entries {
			seen[e.Name]++
		}
		if next == "" {
			break
		}
		cursor = next
	}

	// No duplicates.
	for name, count := range seen {
		if count != 1 {
			t.Errorf("entry %q returned %d times across pages, want exactly 1", name, count)
		}
	}
	// No missing entries / no extras.
	if len(seen) != total {
		missing := make([]string, 0)
		for name := range want {
			if seen[name] == 0 {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		t.Fatalf("paged union has %d distinct entries, want %d (missing: %v)", len(seen), total, missing)
	}
	for name := range seen {
		if !want[name] {
			t.Errorf("paged union contains unexpected entry %q", name)
		}
	}

	// limit==0 selects the default page size, which is large enough to return
	// every child in a single page (no continuation cursor).
	entries, next, err := store.ListChildren(ctx, rootHandle, "", 0)
	if err != nil {
		t.Fatalf("ListChildren(limit=0) failed: %v", err)
	}
	if len(entries) != total {
		t.Fatalf("ListChildren(limit=0) returned %d entries, want %d (default page size)", len(entries), total)
	}
	if next != "" {
		t.Errorf("ListChildren(limit=0) nextCursor = %q, want empty (default page fits all)", next)
	}
}

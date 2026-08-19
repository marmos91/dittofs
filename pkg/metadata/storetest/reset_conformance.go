package storetest

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// fileForReadStore and createCacheStore mirror the optional fast-path
// interfaces the metadata service probes for. A backend that implements them
// answers from its own derived caches, which is exactly the state this suite
// has to see torn down by Reset and RestoreSnapshot; a backend that does not
// falls back to the plain Store methods below.
type fileForReadStore interface {
	GetFileForRead(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error)
}

type createCacheStore interface {
	GetFileForCreate(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error)
	GetChildForCreate(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error)
}

// readFile loads handle through the backend's read fast path when it has one,
// so any file-read cache behind it is both warmed and consulted.
func readFile(ctx context.Context, store metadata.Store, handle metadata.FileHandle) (*metadata.File, error) {
	if r, ok := store.(fileForReadStore); ok {
		return r.GetFileForRead(ctx, handle)
	}
	return store.GetFile(ctx, handle)
}

// readParent loads a directory through the backend's create fast path when it
// has one, so any parent-directory cache behind it is warmed and consulted.
func readParent(ctx context.Context, store metadata.Store, handle metadata.FileHandle) (*metadata.File, error) {
	if c, ok := store.(createCacheStore); ok {
		return c.GetFileForCreate(ctx, handle)
	}
	return store.GetFile(ctx, handle)
}

// lookupChild resolves name through the backend's create fast path when it has
// one, so any directory-entry cache behind it is warmed and consulted. That
// cache also holds negative entries, so calling this against an empty store
// arms a cached ABSENT that a later restore has to clear.
func lookupChild(ctx context.Context, store metadata.Store, dir metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if c, ok := store.(createCacheStore); ok {
		return c.GetChildForCreate(ctx, dir, name)
	}
	return store.GetChild(ctx, dir, name)
}

// asResetable is a helper that type-asserts a MetadataStore to Resetable,
// calling t.Fatal if the assertion fails. Mirrors asSnapshotable.
func asResetable(t *testing.T, store metadata.Store) metadata.Resetable {
	t.Helper()
	r, ok := store.(metadata.Resetable)
	if !ok {
		t.Fatal("store does not implement metadata.Resetable")
	}
	return r
}

// ResetThenRestoreConformance verifies that a store implementing both
// Snapshotable and Resetable satisfies the restore-flow contract: a
// populated store can be backed up, Reset to empty, then restored from
// the same backup to its original content. Reset must leave the store
// empty enough for Restore to proceed past its
// ErrRestoreDestinationNotEmpty precondition, and the restored state
// must equal the pre-Reset state.
func ResetThenRestoreConformance(t *testing.T, factory SnapshotableStoreFactory) {
	t.Helper()

	store := factory(t)
	b := asSnapshotable(t, store)
	r := asResetable(t, store)

	ctx := t.Context()

	// Populate: unique prefix "rst" to avoid name collisions if a factory
	// reuses the same backing DB across the Snapshot suite and this suite.
	shareName, uniqueHashes := populateTestData(t, store, "rst")

	// Block records live outside the file tree, so Reset and Restore have to
	// carry them explicitly — nothing rebuilds a block's sync state or
	// live-chunk count from the manifest.
	blockRec := block.BlockRecord{
		BlockID:        "rst-blk-001",
		BlockHash:      block.ContentHash{5, 5, 5},
		Length:         8192,
		LiveChunkCount: 2,
		SyncState:      block.BlockStateRemote,
	}
	if err := store.PutBlockRecord(ctx, blockRec); err != nil {
		t.Fatalf("PutBlockRecord: %v", err)
	}

	// 1. Back up the populated store into a buffer.
	var dumpBuf bytes.Buffer
	hs, err := b.WriteSnapshot(ctx, &dumpBuf)
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if hs.Len() != len(uniqueHashes) {
		t.Fatalf("WriteSnapshot HashSet.Len() = %d, want %d", hs.Len(), len(uniqueHashes))
	}

	// Warm every cache the backend derives from the records Reset is about to
	// drop, so the post-Reset checks below exercise the caches rather than the
	// backing store: share options, the file read path, the parent-directory
	// read path, and the directory-entry lookup.
	if _, err := store.GetShareOptions(ctx, shareName); err != nil {
		t.Fatalf("GetShareOptions(%q) pre-Reset: %v", shareName, err)
	}
	rootHandle, err := store.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("GetRootHandle(%q) pre-Reset: %v", shareName, err)
	}
	alphaHandle, err := lookupChild(ctx, store, rootHandle, "alpha.bin")
	if err != nil {
		t.Fatalf("lookup alpha.bin pre-Reset: %v", err)
	}
	if _, err := readFile(ctx, store, alphaHandle); err != nil {
		t.Fatalf("read alpha.bin pre-Reset: %v", err)
	}
	if _, err := readParent(ctx, store, rootHandle); err != nil {
		t.Fatalf("read root pre-Reset: %v", err)
	}

	// 2. Reset the SAME store in place — no close/reopen.
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// Reset must re-derive every in-memory structure built from the dropped
	// records, not just the durable ones. A surviving share-options entry is a
	// permission decision made against a share that no longer exists.
	if _, err := store.GetShareOptions(ctx, shareName); err == nil {
		t.Fatalf("post-Reset GetShareOptions(%q) succeeded, want an error", shareName)
	}
	if _, found, err := store.GetBlockRecord(ctx, blockRec.BlockID); err != nil {
		t.Fatalf("post-Reset GetBlockRecord: %v", err)
	} else if found {
		t.Errorf("block record %q survived Reset", blockRec.BlockID)
	}

	// The same rule applies to every other derived cache: a file whose record
	// was dropped must not still be readable, a dropped directory must not
	// still resolve, and a dropped directory entry must not still be found.
	// This lookup also arms a negative dirent entry for the restore below.
	if _, err := readFile(ctx, store, alphaHandle); err == nil {
		t.Errorf("post-Reset read of alpha.bin succeeded, want an error")
	}
	if _, err := readParent(ctx, store, rootHandle); err == nil {
		t.Errorf("post-Reset read of the root directory succeeded, want an error")
	}
	if _, err := lookupChild(ctx, store, rootHandle, "alpha.bin"); err == nil {
		t.Errorf("post-Reset lookup of alpha.bin succeeded, want an error")
	}

	// Reset must clear the per-identity quota cache too, not just the
	// share/file maps: a stale cache keeps enforcing limits against data
	// that no longer exists.
	if u, err := store.GetQuotaUsage(metadata.QuotaScopeUser, 1000); err != nil || u.Bytes != 0 || u.Files != 0 {
		t.Fatalf("post-Reset GetQuotaUsage(user, 1000) = %+v, err=%v, want zero", u, err)
	}

	// 3. Empty assertion: ListShares returns zero entries post-Reset.
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares post-Reset: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("post-Reset ListShares = %v, want empty", shares)
	}

	// 4. Restore from the same dump into the (now-empty) same store
	//    instance. The Reset above satisfied the
	//    ErrRestoreDestinationNotEmpty precondition so Restore must succeed.
	if err := b.RestoreSnapshot(ctx, &dumpBuf); err != nil {
		t.Fatalf("Restore post-Reset: %v", err)
	}

	if got, found, err := store.GetBlockRecord(ctx, blockRec.BlockID); err != nil {
		t.Fatalf("post-Restore GetBlockRecord: %v", err)
	} else if !found {
		t.Errorf("block record %q missing after Restore", blockRec.BlockID)
	} else if got != blockRec {
		t.Errorf("restored block record = %+v, want %+v", got, blockRec)
	}

	// Restore must reseed the quota cache from the restored files, not leave
	// it at the post-Reset zero state.
	const wantBytes = populateTestDataUsedBytes
	if u, err := store.GetQuotaUsage(metadata.QuotaScopeUser, 1000); err != nil || u.Bytes != wantBytes || u.Files != 2 {
		t.Fatalf("post-Restore GetQuotaUsage(user, 1000) = %+v, err=%v, want {Bytes:%d Files:2}", u, err, wantBytes)
	}

	// 5. Verify shares + representative file survived round-trip.
	restored, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares post-Restore: %v", err)
	}
	found := false
	for _, s := range restored {
		if s == shareName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("share %q not found post-Restore (shares: %v)", shareName, restored)
	}

	rootHandle, err = store.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("GetRootHandle(%q): %v", shareName, err)
	}

	// Resolved through the caching lookup so the ABSENT entry armed while the
	// store was empty has to have been cleared by the restore.
	alphaHandle, err = lookupChild(ctx, store, rootHandle, "alpha.bin")
	if err != nil {
		t.Fatalf("lookup alpha.bin post-Restore: %v", err)
	}
	if _, err := readParent(ctx, store, rootHandle); err != nil {
		t.Fatalf("read root post-Restore: %v", err)
	}
	alphaFile, err := readFile(ctx, store, alphaHandle)
	if err != nil {
		t.Fatalf("read alpha post-Restore: %v", err)
	}
	if alphaFile.Size != 8<<20 {
		t.Errorf("alpha.Size = %d, want %d", alphaFile.Size, 8<<20)
	}
	if alphaFile.Mode != 0o644 {
		t.Errorf("alpha.Mode = %o, want %o", alphaFile.Mode, 0o644)
	}
	if len(alphaFile.Blocks) != 2 {
		t.Fatalf("alpha.Blocks len = %d, want 2", len(alphaFile.Blocks))
	}

	betaHandle, err := lookupChild(ctx, store, rootHandle, "beta.bin")
	if err != nil {
		t.Fatalf("lookup beta.bin post-Restore: %v", err)
	}
	betaFile, err := readFile(ctx, store, betaHandle)
	if err != nil {
		t.Fatalf("read beta post-Restore: %v", err)
	}
	if betaFile.Size != 6<<20 {
		t.Errorf("beta.Size = %d, want %d", betaFile.Size, 6<<20)
	}
	if len(betaFile.Blocks) != 2 {
		t.Fatalf("beta.Blocks len = %d, want 2", len(betaFile.Blocks))
	}
}

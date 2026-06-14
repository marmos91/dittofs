//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// createShareRoot creates a fresh share and returns its root directory handle.
func createShareRoot(t *testing.T, store metadata.Store, shareName string) metadata.FileHandle {
	t.Helper()
	rootFile, err := store.CreateRootDirectory(t.Context(), shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory(%q): %v", shareName, err)
	}
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle(root): %v", err)
	}
	return rootHandle
}

// putSizedFile creates a regular file of the given size under shareName/name
// and returns its handle.
func putSizedFile(t *testing.T, store metadata.Store, shareName, rootName string, rootHandle metadata.FileHandle, name string, size uint64) metadata.FileHandle {
	t.Helper()
	ctx := t.Context()
	handle, err := store.GenerateHandle(ctx, shareName, "/"+name)
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/" + name,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  1000,
			GID:  1000,
			Size: size,
		},
	}
	if err := store.PutFile(ctx, file); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := store.SetParent(ctx, handle, rootHandle); err != nil {
		t.Fatalf("SetParent: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, name, handle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}
	if err := store.SetLinkCount(ctx, handle, 1); err != nil {
		t.Fatalf("SetLinkCount: %v", err)
	}
	return handle
}

// setFileSize updates the size of an existing file (GetFile -> mutate -> PutFile).
func setFileSize(t *testing.T, store metadata.Store, handle metadata.FileHandle, size uint64) {
	t.Helper()
	ctx := t.Context()
	f, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	f.FileAttr.Size = size
	if err := store.PutFile(ctx, f); err != nil {
		t.Fatalf("PutFile (resize): %v", err)
	}
}

// TestPostgres_ListShares_CompleteList exercises the ListShares pool path and,
// together with the rows.Err() check added by the fix, guards against silent
// truncation of the share list.
func TestPostgres_ListShares_CompleteList(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	const N = 5
	names := make([]string, N)
	for i := range names {
		names[i] = fmt.Sprintf("/ls-test-%02d", i)
		createShareRoot(t, store, names[i])
	}

	got, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares() error: %v", err)
	}
	gotSet := make(map[string]bool, len(got))
	for _, g := range got {
		gotSet[g] = true
	}
	for _, n := range names {
		if !gotSet[n] {
			t.Errorf("ListShares() missing %q (got %v)", n, got)
		}
	}
}

// TestPostgres_PutFile_UsedBytesAccurate proves the old-size scan is honoured:
// a shrink must subtract the prior size. If the old-size scan error were
// silently swallowed and oldSize stayed 0, the delta would over-credit
// usedBytes.
func TestPostgres_PutFile_UsedBytesAccurate(t *testing.T) {
	store := newTestStore(t)

	rootHandle := createShareRoot(t, store, "/used-bytes-pg")
	fh := putSizedFile(t, store, "/used-bytes-pg", "/used-bytes-pg", rootHandle, "data.bin", 0)

	base := store.GetUsedBytes()

	// Grow to 1000.
	setFileSize(t, store, fh, 1000)
	if got := store.GetUsedBytes(); got != base+1000 {
		t.Fatalf("after grow: GetUsedBytes() = %d, want %d", got, base+1000)
	}

	// Shrink to 400. With the old-size scan honoured the delta is 400-1000=-600,
	// leaving usedBytes at base+400. A silenced scan error would yield base+1400.
	setFileSize(t, store, fh, 400)
	if got := store.GetUsedBytes(); got != base+400 {
		t.Fatalf("after shrink: GetUsedBytes() = %d, want %d (delta bug would give %d)",
			got, base+400, base+1400)
	}
}

// TestPostgres_GetFilesystemStatistics_PerShare proves the pool-path stats are
// scoped to the share encoded in the handle, not aggregated across all shares.
func TestPostgres_GetFilesystemStatistics_PerShare(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	rootA := createShareRoot(t, store, "/stats-a")
	putSizedFile(t, store, "/stats-a", "/stats-a", rootA, "a.bin", 1000)

	rootB := createShareRoot(t, store, "/stats-b")
	putSizedFile(t, store, "/stats-b", "/stats-b", rootB, "b.bin", 3000)

	statsA, err := store.GetFilesystemStatistics(ctx, rootA)
	if err != nil {
		t.Fatalf("GetFilesystemStatistics(share A): %v", err)
	}
	if statsA.UsedBytes != 1000 {
		t.Errorf("share A: UsedBytes = %d, want 1000 (bug aggregates all shares)", statsA.UsedBytes)
	}
	if statsA.UsedFiles != 1 {
		t.Errorf("share A: UsedFiles = %d, want 1", statsA.UsedFiles)
	}

	statsB, err := store.GetFilesystemStatistics(ctx, rootB)
	if err != nil {
		t.Fatalf("GetFilesystemStatistics(share B): %v", err)
	}
	if statsB.UsedBytes != 3000 {
		t.Errorf("share B: UsedBytes = %d, want 3000", statsB.UsedBytes)
	}
	if statsB.UsedFiles != 1 {
		t.Errorf("share B: UsedFiles = %d, want 1", statsB.UsedFiles)
	}
}

// TestPostgres_TxGetFilesystemStatistics_PerShare is the transaction-path
// variant of the per-share scoping assertion.
func TestPostgres_TxGetFilesystemStatistics_PerShare(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()

	rootA := createShareRoot(t, store, "/tx-stats-a")
	putSizedFile(t, store, "/tx-stats-a", "/tx-stats-a", rootA, "a.bin", 1000)

	rootB := createShareRoot(t, store, "/tx-stats-b")
	putSizedFile(t, store, "/tx-stats-b", "/tx-stats-b", rootB, "b.bin", 3000)

	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		statsA, err := tx.GetFilesystemStatistics(ctx, rootA)
		if err != nil {
			return err
		}
		if statsA.UsedBytes != 1000 {
			t.Errorf("tx share A: UsedBytes = %d, want 1000", statsA.UsedBytes)
		}
		statsB, err := tx.GetFilesystemStatistics(ctx, rootB)
		if err != nil {
			return err
		}
		if statsB.UsedBytes != 3000 {
			t.Errorf("tx share B: UsedBytes = %d, want 3000", statsB.UsedBytes)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}
}

// TestPostgres_WithTransaction_RetriesOnSerializationFailure forces a
// serialization conflict between two transactions touching the same row and
// asserts that WithTransaction transparently retries (returns nil) rather than
// surfacing the 40001 as a hard error. Before the Cause/Unwrap fix,
// isRetryableError could not see the PgError through the mapped StoreError and
// the racing transaction would fail.
func TestPostgres_WithTransaction_RetriesOnSerializationFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	rootHandle := createShareRoot(t, store, "/retry-test")
	fileHandle := putSizedFile(t, store, "/retry-test", "/retry-test", rootHandle, "f.bin", 0)

	ready := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- store.WithTransaction(ctx, func(tx metadata.Transaction) error {
			f, err := tx.GetFile(ctx, fileHandle)
			if err != nil {
				return err
			}
			close(ready) // let the racing tx proceed
			f.FileAttr.Size = 1000
			return tx.PutFile(ctx, f)
		})
	}()

	<-ready
	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		f, err := tx.GetFile(ctx, fileHandle)
		if err != nil {
			return err
		}
		f.FileAttr.Size = 2000
		return tx.PutFile(ctx, f)
	}); err != nil {
		t.Fatalf("racing tx failed (retry not transparent?): %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("first tx failed: %v", err)
	}

	final, err := store.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetFile final: %v", err)
	}
	if final.FileAttr.Size != 1000 && final.FileAttr.Size != 2000 {
		t.Fatalf("final size = %d, want 1000 or 2000", final.FileAttr.Size)
	}
}

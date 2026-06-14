//go:build integration

package badger_test

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestBadger_UsedBytes_RetryNoDoubleCount exercises the conflict-retry path:
// many goroutines repeatedly UPDATE the SAME file's size, which provokes
// BadgerDB ErrConflict and forces WithTransaction to re-run the closure. With
// the usedBytes delta accumulated on the tx (pendingDelta) and applied once
// after a successful commit, the counter must end exactly equal to the final
// file size — never inflated by a retried closure re-applying its delta.
func TestBadger_UsedBytes_RetryNoDoubleCount(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	defer store.Close()

	const shareName = "testshare"
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}

	// Create one regular file.
	handle, err := store.GenerateHandle(ctx, shareName, "/f.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	mkFile := func(size uint64) *metadata.File {
		now := time.Now()
		return &metadata.File{
			ID: fileID, ShareName: shareName, Path: "/f.bin",
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular, Mode: 0o644, Size: size,
				Atime: now, Mtime: now, Ctime: now,
			},
		}
	}
	if err := store.PutFile(ctx, mkFile(0)); err != nil {
		t.Fatalf("initial PutFile: %v", err)
	}
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "f.bin", handle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}

	// Hammer the same file's size from many goroutines to provoke conflicts
	// and retries. The last writer (by value) is racy, but whatever the final
	// committed size is, usedBytes MUST equal it exactly.
	const workers = 16
	const itersPerWorker = 40
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(base uint64) {
			defer wg.Done()
			for i := 0; i < itersPerWorker; i++ {
				size := base*1000 + uint64(i)*7
				_ = store.PutFile(ctx, mkFile(size))
			}
		}(uint64(w + 1))
	}
	wg.Wait()

	// The counter must equal the final stored file size, with no double-count
	// from any conflict retry.
	final, err := store.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got := store.GetUsedBytes(); got != int64(final.Size) {
		t.Fatalf("usedBytes=%d but final file size=%d (conflict-retry double-count)", got, final.Size)
	}
}

// TestBadger_Restore_ReinitsUsedBytes verifies that Restore reinitializes the
// in-memory usedBytes counter from the restored file rows. Before the fix,
// GetUsedBytes returned a stale value after Restore until server restart
// because initUsedBytesCounter was only called at store-open time.
func TestBadger_Restore_ReinitsUsedBytes(t *testing.T) {
	ctx := context.Background()

	// --- SOURCE STORE: one regular file (1 MiB) under the share root. ---
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, srcPath)
	if err != nil {
		t.Fatalf("src store: %v", err)
	}
	defer src.Close()

	const shareName = "restore-usedbytes"
	const wantSize = uint64(1 << 20) // 1 MiB

	rootFile, err := src.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}

	fileHandle, err := src.GenerateHandle(ctx, shareName, "/data.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(fileHandle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	now := time.Now()
	if err := src.PutFile(ctx, &metadata.File{
		ID: fileID, ShareName: shareName, Path: "/data.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, Size: wantSize,
			Atime: now, Mtime: now, Ctime: now,
		},
	}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := src.SetChild(ctx, rootHandle, "data.bin", fileHandle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}

	// --- BACKUP ---
	var buf bytes.Buffer
	if _, err := src.Backup(ctx, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// --- DESTINATION STORE: fresh, empty ---
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dstPath)
	if err != nil {
		t.Fatalf("dst store: %v", err)
	}
	defer dst.Close()

	if got := dst.GetUsedBytes(); got != 0 {
		t.Fatalf("pre-Restore GetUsedBytes = %d, want 0", got)
	}

	// --- RESTORE ---
	if err := dst.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The counter must reflect the restored file's size immediately, without a
	// server restart (i.e. without re-opening the store).
	if got := dst.GetUsedBytes(); got != int64(wantSize) {
		t.Fatalf("post-Restore GetUsedBytes = %d, want %d (usedBytes counter not reinitialized after Restore)", got, wantSize)
	}
}

// TestBadger_GetFilesystemStatistics_IgnoresNonRegular verifies that
// GetFilesystemStatistics.UsedBytes counts only regular files, mirroring
// initUsedBytesCounter and GetUsedBytes. Before the fix, the transaction-path
// iteration accumulated file.Size for ALL types (directories, symlinks, etc.),
// inflating the reported used-bytes figure.
func TestBadger_GetFilesystemStatistics_IgnoresNonRegular(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	defer store.Close()

	const shareName = "stats-type-guard"
	const regularSize = uint64(4096)
	const dirSize = uint64(512) // non-zero so the bug is detectable

	// Root directory carries a non-zero size so a buggy accumulator counts it.
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755, Size: dirSize,
	})
	if err != nil {
		t.Fatalf("CreateRootDirectory: %v", err)
	}
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	if err != nil {
		t.Fatalf("EncodeShareHandle: %v", err)
	}

	// Regular file with a known size.
	fileHandle, err := store.GenerateHandle(ctx, shareName, "/f.bin")
	if err != nil {
		t.Fatalf("GenerateHandle: %v", err)
	}
	_, fileID, err := metadata.DecodeFileHandle(fileHandle)
	if err != nil {
		t.Fatalf("DecodeFileHandle: %v", err)
	}
	now := time.Now()
	if err := store.PutFile(ctx, &metadata.File{
		ID: fileID, ShareName: shareName, Path: "/f.bin",
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644, Size: regularSize,
			Atime: now, Mtime: now, Ctime: now,
		},
	}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if err := store.SetChild(ctx, rootHandle, "f.bin", fileHandle); err != nil {
		t.Fatalf("SetChild: %v", err)
	}

	// The store-level GetFilesystemStatistics (server.go) serves UsedBytes from
	// the atomic counter, so it cannot exercise the transaction-path iteration
	// where the type-guard bug lived. Reach the transaction path directly via
	// WithTransaction + an interface assertion on the concrete tx.
	type fsStater interface {
		GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error)
	}
	var stats *metadata.FilesystemStatistics
	if err := store.WithTransaction(ctx, func(tx metadata.Transaction) error {
		st, ok := tx.(fsStater)
		if !ok {
			t.Fatalf("badger transaction does not expose GetFilesystemStatistics")
		}
		s, err := st.GetFilesystemStatistics(ctx, rootHandle)
		if err != nil {
			return err
		}
		stats = s
		return nil
	}); err != nil {
		t.Fatalf("WithTransaction(GetFilesystemStatistics): %v", err)
	}

	// UsedBytes must equal only the regular-file contribution. Before the fix
	// the iteration added the directory's size too, so this would be
	// regularSize+dirSize.
	if stats.UsedBytes != regularSize {
		t.Fatalf("tx GetFilesystemStatistics.UsedBytes = %d, want %d (regular only); "+
			"directory size %d must not be counted", stats.UsedBytes, regularSize, dirSize)
	}

	// fileCount still counts every inode (root dir + regular file = 2).
	if stats.UsedFiles != 2 {
		t.Fatalf("tx GetFilesystemStatistics.UsedFiles = %d, want 2", stats.UsedFiles)
	}

	// Consistency cross-check: the atomic counter (GetUsedBytes) must agree.
	if got := store.GetUsedBytes(); got != int64(regularSize) {
		t.Fatalf("GetUsedBytes = %d, want %d", got, regularSize)
	}
}

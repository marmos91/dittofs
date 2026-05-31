//go:build integration

package badger_test

import (
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

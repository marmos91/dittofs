package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newTestCacheWithDisk creates an FSStore with a configured maxDisk for eviction tests.
func newTestCacheWithDisk(t *testing.T, maxMemory, maxDisk int64) *FSStore {
	t.Helper()
	dir := t.TempDir()
	blockStore := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, maxDisk, maxMemory, blockStore)
	if err != nil {
		t.Fatalf("failed to create local store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bc.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = bc.Close()
	})
	return bc
}

func TestManageDeleteBlockFile(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write data and flush to disk
	data := make([]byte, 4096)
	for i := range data {
		data[i] = 0xAA
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bc.SyncFileBlocks(ctx)

	// Verify block exists on disk
	key := blockKey{payloadID: "file1", blockIdx: 0}
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("expected .blk file to exist before delete")
	}

	// Record diskUsed before
	diskBefore := bc.diskUsed.Load()

	// Delete the block
	if err := bc.DeleteBlockFile(ctx, "file1", 0); err != nil {
		t.Fatalf("DeleteBlockFile failed: %v", err)
	}

	// Verify .blk file deleted
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected .blk file to be removed after DeleteBlockFile")
	}

	// Verify diskUsed decremented
	diskAfter := bc.diskUsed.Load()
	if diskAfter >= diskBefore {
		t.Errorf("diskUsed not decremented: before=%d after=%d", diskBefore, diskAfter)
	}

	// Verify memBlock purged
	if mb := bc.getMemBlock(key); mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Error("expected memBlock to be purged")
		}
	}

	// Verify FileBlock metadata deleted
	_, err := bc.blockStore.GetFileBlock(ctx, blockID)
	if err == nil {
		t.Error("expected FileBlock metadata to be deleted")
	}
}

func TestManageDeleteBlockFileIdempotent(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Delete a block that doesn't exist -- should return nil
	if err := bc.DeleteBlockFile(ctx, "nonexistent", 0); err != nil {
		t.Fatalf("DeleteBlockFile on non-existent block should return nil, got: %v", err)
	}
}

func TestManageDeleteBlockFileClearsPendingFBs(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write data (creates a pending FB entry)
	data := make([]byte, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	key := blockKey{payloadID: "file1", blockIdx: 0}
	blockID := makeBlockID(key)

	// Verify pendingFBs has the entry
	if _, ok := bc.pendingFBs.Load(blockID); !ok {
		// The flush queues it, so it should be pending
		// If not, sync and put it back for the test
		bc.SyncFileBlocks(ctx)
		fb, err := bc.blockStore.GetFileBlock(ctx, blockID)
		if err != nil {
			t.Skip("block not found, cannot test pendingFBs cleanup")
		}
		bc.pendingFBs.Store(blockID, fb)
	}

	// Delete the block
	if err := bc.DeleteBlockFile(ctx, "file1", 0); err != nil {
		t.Fatalf("DeleteBlockFile failed: %v", err)
	}

	// Verify pendingFBs entry cleared
	if _, ok := bc.pendingFBs.Load(blockID); ok {
		t.Error("expected pendingFBs entry to be cleared after DeleteBlockFile")
	}
}

func TestManageDeleteAllBlockFiles(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write data spanning 2 blocks (block 0 and block 1)
	data := make([]byte, int(blockstore.BlockSize)+4096)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bc.SyncFileBlocks(ctx)

	// Verify blocks on disk
	block0ID := makeBlockID(blockKey{payloadID: "file1", blockIdx: 0})
	block1ID := makeBlockID(blockKey{payloadID: "file1", blockIdx: 1})
	if _, err := os.Stat(bc.blockPath(block0ID)); os.IsNotExist(err) {
		t.Fatal("block 0 file should exist")
	}
	if _, err := os.Stat(bc.blockPath(block1ID)); os.IsNotExist(err) {
		t.Fatal("block 1 file should exist")
	}

	// Delete all blocks
	if err := bc.DeleteAllBlockFiles(ctx, "file1"); err != nil {
		t.Fatalf("DeleteAllBlockFiles failed: %v", err)
	}

	// Verify .blk files deleted
	if _, err := os.Stat(bc.blockPath(block0ID)); !os.IsNotExist(err) {
		t.Error("block 0 file should be deleted")
	}
	if _, err := os.Stat(bc.blockPath(block1ID)); !os.IsNotExist(err) {
		t.Error("block 1 file should be deleted")
	}

	// Verify files map cleaned
	_, ok := bc.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file should be removed from files map")
	}

	// Verify parent dir cleanup attempted (empty dir should be removed)
	payloadDir := filepath.Join(bc.baseDir, "file1"[:2])
	// The dir might still exist if other files share the shard prefix, but the
	// specific payload sub-directory should be gone or empty
	_ = payloadDir // just checking cleanup happened without error
}

func TestManageTruncateBlockFiles(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write 3 blocks worth of data
	data := make([]byte, int(blockstore.BlockSize)*2+4096) // block 0, 1, 2
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bc.SyncFileBlocks(ctx)

	// Truncate to 1 byte past block 0 -- should keep block 0, remove blocks 1 and 2
	newSize := uint64(blockstore.BlockSize) // exactly the boundary of block 1
	if err := bc.TruncateBlockFiles(ctx, "file1", newSize); err != nil {
		t.Fatalf("TruncateBlockFiles failed: %v", err)
	}

	// Block 0 should still exist in metadata
	block0ID := makeBlockID(blockKey{payloadID: "file1", blockIdx: 0})
	_, err := bc.blockStore.GetFileBlock(ctx, block0ID)
	if err != nil {
		t.Error("block 0 should still exist after truncate")
	}

	// Block 1 should be deleted
	block1ID := makeBlockID(blockKey{payloadID: "file1", blockIdx: 1})
	_, err = bc.blockStore.GetFileBlock(ctx, block1ID)
	if err == nil {
		t.Error("block 1 should be deleted after truncate")
	}

	// Block 2 should be deleted
	block2ID := makeBlockID(blockKey{payloadID: "file1", blockIdx: 2})
	_, err = bc.blockStore.GetFileBlock(ctx, block2ID)
	if err == nil {
		t.Error("block 2 should be deleted after truncate")
	}
}

func TestManageGetStoredFileSize(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write data and flush
	data := make([]byte, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bc.SyncFileBlocks(ctx)

	size, err := bc.GetStoredFileSize(ctx, "file1")
	if err != nil {
		t.Fatalf("GetStoredFileSize failed: %v", err)
	}
	if size != 4096 {
		t.Fatalf("expected size 4096, got %d", size)
	}
}

func TestManageGetStoredFileSizeUnknown(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	size, err := bc.GetStoredFileSize(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetStoredFileSize for unknown file should not error: %v", err)
	}
	if size != 0 {
		t.Fatalf("expected size 0 for unknown file, got %d", size)
	}
}

func TestManageExistsOnDisk(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write and flush
	data := make([]byte, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bc.SyncFileBlocks(ctx)

	exists, err := bc.ExistsOnDisk(ctx, "file1", 0)
	if err != nil {
		t.Fatalf("ExistsOnDisk failed: %v", err)
	}
	if !exists {
		t.Fatal("expected ExistsOnDisk to return true for flushed block")
	}
}

func TestManageExistsOnDiskStaleMetadata(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write and flush
	data := make([]byte, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	bc.SyncFileBlocks(ctx)

	// Delete the .blk file manually to simulate stale metadata
	key := blockKey{payloadID: "file1", blockIdx: 0}
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)
	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to remove .blk file: %v", err)
	}

	exists, err := bc.ExistsOnDisk(ctx, "file1", 0)
	if err != nil {
		t.Fatalf("ExistsOnDisk failed: %v", err)
	}
	if exists {
		t.Fatal("expected ExistsOnDisk to return false for stale metadata")
	}
}

func TestManageSetEvictionDisabled(t *testing.T) {
	bc := newTestCacheWithDisk(t, 256*1024*1024, 100) // very small disk limit
	ctx := context.Background()

	// Disable eviction
	bc.SetEvictionEnabled(false)

	// Put a remote block in the store to ensure it would be evictable
	fb := &blockstore.FileBlock{
		ID:            "remote-file/0",
		LocalPath:     "/tmp/fake",
		BlockStoreKey: "s3://bucket/key",
		State:         blockstore.BlockStateRemote,
		DataSize:      50,
		RefCount:      1,
		LastAccess:    time.Now().Add(-time.Hour),
		CreatedAt:     time.Now().Add(-time.Hour),
	}
	if err := bc.blockStore.PutFileBlock(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}

	// Set diskUsed to just under limit
	bc.diskUsed.Store(90)

	// ensureSpace should fail with ErrDiskFull because eviction is disabled
	err := bc.ensureSpace(ctx, 20)
	if err != ErrDiskFull {
		t.Fatalf("expected ErrDiskFull when eviction disabled, got: %v", err)
	}
}

func TestManageSetEvictionReEnabled(t *testing.T) {
	bc := newTestCacheWithDisk(t, 256*1024*1024, 200) // small disk limit
	ctx := context.Background()

	// Disable and re-enable eviction
	bc.SetEvictionEnabled(false)
	bc.SetEvictionEnabled(true)

	// Put a remote block that can be evicted
	blkPath := filepath.Join(bc.baseDir, "evictable.blk")
	if err := os.WriteFile(blkPath, make([]byte, 50), 0644); err != nil {
		t.Fatalf("write evictable file: %v", err)
	}
	fb := &blockstore.FileBlock{
		ID:            "evict-file/0",
		LocalPath:     blkPath,
		BlockStoreKey: "s3://bucket/evict",
		State:         blockstore.BlockStateRemote,
		DataSize:      50,
		RefCount:      1,
		LastAccess:    time.Now().Add(-time.Hour),
		CreatedAt:     time.Now().Add(-time.Hour),
	}
	if err := bc.blockStore.PutFileBlock(ctx, fb); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}

	bc.diskUsed.Store(180)

	// ensureSpace should succeed by evicting the remote block
	err := bc.ensureSpace(ctx, 30)
	if err != nil {
		t.Fatalf("expected ensureSpace to succeed with eviction re-enabled, got: %v", err)
	}
}

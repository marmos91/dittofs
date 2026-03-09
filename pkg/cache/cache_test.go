package cache

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newTestCache creates a BlockCache with a temporary directory and in-memory block store.
func newTestCache(t *testing.T, maxMemory int64) *BlockCache {
	t.Helper()
	dir := t.TempDir()
	blockStore := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, 0, maxMemory, blockStore)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bc.Start(ctx)
	t.Cleanup(func() {
		cancel()
		bc.Close()
	})
	return bc
}

func TestWriteAndReadSimple(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte("hello"), 100)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt returned cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt returned wrong data")
	}
}

func TestWriteFullBlock(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write exactly one full 8MB block
	data := bytes.Repeat([]byte{0xAB}, int(BlockSize))
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Block should have been flushed to disk (memBlock stays but data=nil)
	key := blockKey{payloadID: "file1", blockIdx: 0}
	mb := bc.getMemBlock(key)
	if mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Error("expected memBlock data to be nil after full block flush")
		}
	}

	// Should still be readable from disk
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt from disk failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt from disk returned cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt from disk returned wrong data")
	}
}

func TestMultiBlockWrite(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write 20MB spanning 3 blocks (8MB each)
	size := 20 * 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read back and verify
	dest := make([]byte, size)
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt returned cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt returned wrong data")
	}

	// Check file size
	fileSize, ok := bc.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found")
	}
	if fileSize != uint64(size) {
		t.Fatalf("expected file size %d, got %d", size, fileSize)
	}
}

func TestFlushCallsFsync(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write partial block (won't auto-flush)
	data := bytes.Repeat([]byte{0xCD}, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// memBlock should exist before Flush
	key := blockKey{payloadID: "file1", blockIdx: 0}
	if mb := bc.getMemBlock(key); mb == nil {
		t.Error("expected memBlock to exist before Flush")
	}

	// Flush (NFS COMMIT path)
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// After flush, memBlock stays but data should be nil
	if mb := bc.getMemBlock(key); mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Error("expected memBlock data to be nil after Flush")
		}
	}

	// Data should still be readable from disk
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt after Flush failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after Flush returned cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt after Flush returned wrong data")
	}
}

func TestConcurrentWritesDifferentFiles(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	const numFiles = 10
	const writeSize = 1024 * 1024 // 1MB per file

	var wg sync.WaitGroup
	errs := make([]error, numFiles)

	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payloadID := "file" + string(rune('A'+idx))
			data := bytes.Repeat([]byte{byte(idx)}, writeSize)
			errs[idx] = bc.WriteAt(ctx, payloadID, data, 0)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent write %d failed: %v", i, err)
		}
	}

	// Verify all files
	for i := 0; i < numFiles; i++ {
		payloadID := "file" + string(rune('A'+idx(i)))
		expected := bytes.Repeat([]byte{byte(i)}, writeSize)
		dest := make([]byte, writeSize)
		found, err := bc.ReadAt(ctx, payloadID, dest, 0)
		if err != nil {
			t.Fatalf("ReadAt file %d failed: %v", i, err)
		}
		if !found {
			t.Fatalf("ReadAt file %d cache miss", i)
		}
		if !bytes.Equal(dest, expected) {
			t.Fatalf("ReadAt file %d wrong data", i)
		}
	}
}

// idx is a helper to generate payload IDs.
func idx(i int) int { return i }

func TestConcurrentWritesSameFile(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	const numWriters = 8
	const writeSize = 4096 // 4KB each

	var wg sync.WaitGroup

	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			offset := uint64(idx) * writeSize
			data := bytes.Repeat([]byte{byte(idx)}, writeSize)
			if err := bc.WriteAt(ctx, "file1", data, offset); err != nil {
				t.Errorf("concurrent write %d failed: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Each 4KB region should have the corresponding byte value
	for i := 0; i < numWriters; i++ {
		offset := uint64(i) * writeSize
		dest := make([]byte, writeSize)
		found, err := bc.ReadAt(ctx, "file1", dest, offset)
		if err != nil {
			t.Fatalf("ReadAt region %d failed: %v", i, err)
		}
		if !found {
			t.Fatalf("ReadAt region %d cache miss", i)
		}
		expected := bytes.Repeat([]byte{byte(i)}, writeSize)
		if !bytes.Equal(dest, expected) {
			t.Fatalf("ReadAt region %d wrong data (got %d, expected %d)", i, dest[0], byte(i))
		}
	}
}

func TestBackpressure(t *testing.T) {
	// Set very low memory budget (32MB = 4 blocks)
	bc := newTestCache(t, 32*1024*1024)
	ctx := context.Background()

	// Write 80MB (10 blocks) to trigger backpressure
	const totalSize = 80 * 1024 * 1024
	data := make([]byte, totalSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt with backpressure failed: %v", err)
	}

	// Memory should not exceed 2x budget (hard backpressure limit)
	if bc.memUsed.Load() > bc.maxMemory*2 {
		t.Fatalf("memory usage %d exceeds 2x budget %d", bc.memUsed.Load(), bc.maxMemory*2)
	}

	// Data should be fully readable
	dest := make([]byte, totalSize)
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt after backpressure failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after backpressure cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt after backpressure wrong data")
	}
}

func TestRemove(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xFF}, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := bc.Remove(ctx, "file1"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	_, ok := bc.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file still tracked after Remove")
	}
}

func TestTruncate(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write 16MB (2 full blocks)
	data := bytes.Repeat([]byte{0xAA}, 16*1024*1024)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Truncate to 4MB (block 1 should be purged)
	if err := bc.Truncate(ctx, "file1", 4*1024*1024); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	fileSize, ok := bc.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found after Truncate")
	}
	if fileSize != 4*1024*1024 {
		t.Fatalf("expected file size %d, got %d", 4*1024*1024, fileSize)
	}
}

func TestDirectDiskWrite(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write a full block to get it flushed to disk
	data := bytes.Repeat([]byte{0xAB}, int(BlockSize))
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Verify the block was flushed (memBlock stays but data=nil)
	key := blockKey{payloadID: "file1", blockIdx: 0}
	mb := bc.getMemBlock(key)
	if mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Fatal("expected memBlock data to be nil after full block write")
		}
	}

	// Now write a small partial update to the same block -- should go direct to disk
	patch := []byte("patched!")
	if err := bc.WriteAt(ctx, "file1", patch, 100); err != nil {
		t.Fatalf("direct disk write failed: %v", err)
	}

	// Verify the patch was written correctly
	dest := make([]byte, len(patch))
	found, err := bc.ReadAt(ctx, "file1", dest, 100)
	if err != nil {
		t.Fatalf("ReadAt after direct disk write failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after direct disk write returned cache miss")
	}
	if !bytes.Equal(dest, patch) {
		t.Fatalf("direct disk write wrong data: got %q, want %q", dest, patch)
	}
}

func TestListFiles(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := bc.WriteAt(ctx, id, []byte("data"), 0); err != nil {
			t.Fatalf("WriteAt %s failed: %v", id, err)
		}
	}

	files := bc.ListFiles()
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	got := make(map[string]bool)
	for _, f := range files {
		got[f] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !got[id] {
			t.Errorf("missing file %s", id)
		}
	}
}

func TestStats(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	if err := bc.WriteAt(ctx, "f1", bytes.Repeat([]byte{1}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if err := bc.WriteAt(ctx, "f2", bytes.Repeat([]byte{2}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats := bc.Stats()
	if stats.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.MemBlockCount != 2 {
		t.Errorf("expected 2 memBlocks, got %d", stats.MemBlockCount)
	}
	if stats.MemUsed != 2*int64(BlockSize) {
		t.Errorf("expected memUsed %d, got %d", 2*int64(BlockSize), stats.MemUsed)
	}

	// After flushing, memBlocks should be 0
	if _, err := bc.Flush(ctx, "f1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "f2"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	stats = bc.Stats()
	if stats.MemBlockCount != 0 {
		t.Errorf("expected 0 memBlocks after flush, got %d", stats.MemBlockCount)
	}
}

func TestConcurrentFlushAndWrite(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Start writing in background
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			data := bytes.Repeat([]byte{byte(i)}, 4096)
			if err := bc.WriteAt(ctx, "file1", data, uint64(i)*4096); err != nil {
				t.Errorf("write %d failed: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := bc.Flush(ctx, "file1"); err != nil {
				t.Errorf("flush %d failed: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()
}

func TestNoFsyncOnBlockFill(t *testing.T) {
	// This test verifies that flushBlock (called when a block fills up during
	// writes) does NOT call fsync. The .blk file should exist but without
	// the durability guarantee of fsync (which is deferred to Flush/COMMIT).
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write exactly one full block to trigger flushBlock
	data := bytes.Repeat([]byte{0xBB}, int(BlockSize))
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// The .blk file should exist (written but not fsynced)
	key := blockKey{payloadID: "file1", blockIdx: 0}
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("expected .blk file to exist after block fill")
	}

	// Data should be correct
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt wrong data")
	}
}

func TestWriteDownloaded(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xCC}, 4096)
	if err := bc.WriteDownloaded(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteDownloaded failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt cache miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt wrong data")
	}
}

func TestBlockPathSharding(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)

	// A blockID like "abc/0" should be sharded as "<baseDir>/ab/abc/0.blk"
	path := bc.blockPath("abc/0")
	expected := filepath.Join(bc.baseDir, "ab", "abc/0.blk")
	if path != expected {
		t.Errorf("blockPath wrong: got %s, want %s", path, expected)
	}
}

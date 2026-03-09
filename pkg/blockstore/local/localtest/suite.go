package localtest

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// Factory creates a new LocalStore instance for testing.
// Each test calls Factory to get a fresh, independent store.
type Factory func(t *testing.T) local.LocalStore

// RunSuite runs the full conformance test suite against a LocalStore implementation.
func RunSuite(t *testing.T, factory Factory) {
	t.Run("WriteAndRead", func(t *testing.T) { testWriteAndRead(t, factory) })
	t.Run("ReadCacheMiss", func(t *testing.T) { testReadCacheMiss(t, factory) })
	t.Run("WriteMultiBlock", func(t *testing.T) { testWriteMultiBlock(t, factory) })
	t.Run("Flush", func(t *testing.T) { testFlush(t, factory) })
	t.Run("GetDirtyBlocks", func(t *testing.T) { testGetDirtyBlocks(t, factory) })
	t.Run("Truncate", func(t *testing.T) { testTruncate(t, factory) })
	t.Run("EvictMemory", func(t *testing.T) { testEvictMemory(t, factory) })
	t.Run("DeleteBlockFile", func(t *testing.T) { testDeleteBlockFile(t, factory) })
	t.Run("DeleteAllBlockFiles", func(t *testing.T) { testDeleteAllBlockFiles(t, factory) })
	t.Run("GetFileSize", func(t *testing.T) { testGetFileSize(t, factory) })
	t.Run("ListFiles", func(t *testing.T) { testListFiles(t, factory) })
	t.Run("Stats", func(t *testing.T) { testStats(t, factory) })
	t.Run("WriteFromRemote", func(t *testing.T) { testWriteFromRemote(t, factory) })
	t.Run("MarkBlockState", func(t *testing.T) { testMarkBlockState(t, factory) })
	t.Run("GetBlockData", func(t *testing.T) { testGetBlockData(t, factory) })
	t.Run("IsBlockCached", func(t *testing.T) { testIsBlockCached(t, factory) })
	t.Run("CloseRejectsOps", func(t *testing.T) { testCloseRejectsOps(t, factory) })
}

func testWriteAndRead(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte("hello"), 100)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := store.ReadAt(ctx, "file1", dest, 0)
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

func testReadCacheMiss(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	dest := make([]byte, 4096)
	found, err := store.ReadAt(ctx, "nonexistent", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt on missing file should not error: %v", err)
	}
	if found {
		t.Fatal("expected cache miss for nonexistent file")
	}
}

func testWriteMultiBlock(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Write data spanning 2+ blocks
	size := int(blockstore.BlockSize) + 4096
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	dest := make([]byte, size)
	found, err := store.ReadAt(ctx, "file1", dest, 0)
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

func testFlush(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	flushed, err := store.Flush(ctx, "file1")
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if len(flushed) == 0 {
		t.Fatal("expected at least one flushed block")
	}

	// Data should still be readable after flush
	dest := make([]byte, len(data))
	found, err := store.ReadAt(ctx, "file1", dest, 0)
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

func testGetDirtyBlocks(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xCD}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	pending, err := store.GetDirtyBlocks(ctx, "file1")
	if err != nil {
		t.Fatalf("GetDirtyBlocks failed: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected at least one pending block")
	}
	if pending[0].DataSize != 4096 {
		t.Fatalf("expected DataSize 4096, got %d", pending[0].DataSize)
	}
}

func testTruncate(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xAA}, 8192)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.Truncate(ctx, "file1", 4096); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	fileSize, ok := store.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found after Truncate")
	}
	if fileSize != 4096 {
		t.Fatalf("expected file size 4096, got %d", fileSize)
	}
}

func testEvictMemory(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xFF}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.EvictMemory(ctx, "file1"); err != nil {
		t.Fatalf("EvictMemory failed: %v", err)
	}

	_, ok := store.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file still tracked after EvictMemory")
	}
}

func testDeleteBlockFile(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xBB}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.DeleteBlockFile(ctx, "file1", 0); err != nil {
		t.Fatalf("DeleteBlockFile failed: %v", err)
	}

	// Block should no longer be cached
	if store.IsBlockCached(ctx, "file1", 0) {
		t.Error("block should not be cached after DeleteBlockFile")
	}
}

func testDeleteAllBlockFiles(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Write to two blocks
	data := make([]byte, int(blockstore.BlockSize)+4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.DeleteAllBlockFiles(ctx, "file1"); err != nil {
		t.Fatalf("DeleteAllBlockFiles failed: %v", err)
	}

	_, ok := store.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file still tracked after DeleteAllBlockFiles")
	}
}

func testGetFileSize(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Not found case
	_, ok := store.GetFileSize(ctx, "nonexistent")
	if ok {
		t.Fatal("expected GetFileSize to return false for nonexistent file")
	}

	// Write and check
	data := bytes.Repeat([]byte{0xCC}, 8192)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	size, ok := store.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found")
	}
	if size != 8192 {
		t.Fatalf("expected file size 8192, got %d", size)
	}
}

func testListFiles(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := store.WriteAt(ctx, id, []byte("data"), 0); err != nil {
			t.Fatalf("WriteAt %s failed: %v", id, err)
		}
	}

	files := store.ListFiles()
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

func testStats(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.WriteAt(ctx, "f1", bytes.Repeat([]byte{1}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if err := store.WriteAt(ctx, "f2", bytes.Repeat([]byte{2}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats := store.Stats()
	if stats.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.MemBlockCount < 2 {
		t.Errorf("expected at least 2 memBlocks, got %d", stats.MemBlockCount)
	}
}

func testWriteFromRemote(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xDD}, 4096)
	if err := store.WriteFromRemote(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteFromRemote failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := store.ReadAt(ctx, "file1", dest, 0)
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

func testMarkBlockState(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xEE}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Flush to make it Local
	if _, err := store.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Mark as syncing
	if !store.MarkBlockSyncing(ctx, "file1", 0) {
		t.Error("MarkBlockSyncing should succeed after flush")
	}

	// Can't mark syncing again (not Local anymore)
	if store.MarkBlockSyncing(ctx, "file1", 0) {
		t.Error("MarkBlockSyncing should fail when already Syncing")
	}

	// Mark remote
	if !store.MarkBlockRemote(ctx, "file1", 0) {
		t.Error("MarkBlockRemote should succeed")
	}

	// Revert to local
	if !store.MarkBlockLocal(ctx, "file1", 0) {
		t.Error("MarkBlockLocal should succeed")
	}
}

func testGetBlockData(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x42}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	blockData, dataSize, err := store.GetBlockData(ctx, "file1", 0)
	if err != nil {
		t.Fatalf("GetBlockData failed: %v", err)
	}
	if dataSize != 4096 {
		t.Fatalf("expected dataSize 4096, got %d", dataSize)
	}
	if !bytes.Equal(blockData[:4096], data) {
		t.Fatal("GetBlockData returned wrong data")
	}
}

func testIsBlockCached(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Not cached initially
	if store.IsBlockCached(ctx, "file1", 0) {
		t.Fatal("block should not be cached before write")
	}

	// Write and check
	if err := store.WriteAt(ctx, "file1", []byte("data"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if !store.IsBlockCached(ctx, "file1", 0) {
		t.Fatal("block should be cached after write")
	}
}

func testCloseRejectsOps(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Operations after Close should fail
	if err := store.WriteAt(ctx, "file1", []byte("data"), 0); err == nil {
		t.Error("WriteAt should fail after Close")
	}

	_, err := store.ReadAt(ctx, "file1", make([]byte, 4), 0)
	if err == nil {
		t.Error("ReadAt should fail after Close")
	}
}

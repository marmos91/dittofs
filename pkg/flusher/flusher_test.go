package flusher

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	blockmemory "github.com/marmos91/dittofs/pkg/store/block/memory"
)

func TestFlusher_FlushRemaining_EmptyCache(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Flush empty cache should not error
	err := f.FlushRemaining(ctx, "share1", []byte("file1"), "share1/content123")
	if err != nil {
		t.Errorf("FlushRemaining on empty cache returned error: %v", err)
	}
}

func TestFlusher_FlushRemaining_SmallFile(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Write some data to cache
	fileHandle := []byte("file1")
	data := []byte("hello world")
	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Flush the file
	// Note: contentID already includes share name (e.g., "share1/path/to/file")
	err := f.FlushRemaining(ctx, "share1", fileHandle, "share1/content123")
	if err != nil {
		t.Fatalf("FlushRemaining failed: %v", err)
	}

	// Verify data was uploaded to block store
	keys, err := store.ListByPrefix(ctx, "share1/content123/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 1 {
		t.Errorf("Expected 1 block, got %d. Keys: %v", len(keys), keys)
	}

	// Verify block content
	blockData, err := store.ReadBlock(ctx, keys[0])
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(blockData) != string(data) {
		t.Errorf("Block data mismatch: got %q, want %q", blockData, data)
	}
}

func TestFlusher_FlushRemaining_LargeFile(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Write 10MB of data (will create 3 blocks: 4MB + 4MB + 2MB)
	fileHandle := []byte("file1")
	data := make([]byte, 10*1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Flush the file
	err := f.FlushRemaining(ctx, "share1", fileHandle, "share1/content123")
	if err != nil {
		t.Fatalf("FlushRemaining failed: %v", err)
	}

	// Verify blocks were created
	keys, err := store.ListByPrefix(ctx, "share1/content123/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("Expected 3 blocks, got %d. Keys: %v", len(keys), keys)
	}

	// Verify total size
	totalSize := store.TotalSize()
	if totalSize != int64(len(data)) {
		t.Errorf("Total size mismatch: got %d, want %d", totalSize, len(data))
	}
}

func TestFlusher_WaitForUploads_NoUploads(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Wait for uploads on file with no uploads should not error
	err := f.WaitForUploads(ctx, "share1/content123")
	if err != nil {
		t.Errorf("WaitForUploads with no uploads returned error: %v", err)
	}
}

func TestFlusher_ReadBlocks_SingleBlock(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Pre-populate block store
	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world from s3")
	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read through flusher
	result, err := f.ReadBlocks(ctx, "share1", []byte("file1"), "share1/content123", 0, 0, uint32(len(data)))
	if err != nil {
		t.Fatalf("ReadBlocks failed: %v", err)
	}

	if string(result) != string(data) {
		t.Errorf("ReadBlocks returned %q, want %q", result, data)
	}
}

func TestFlusher_ReadBlocks_PartialBlock(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Pre-populate block store with 4MB block
	blockKey := "share1/content123/chunk-0/block-0"
	data := make([]byte, BlockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read partial range
	offset := uint32(100)
	length := uint32(1000)
	result, err := f.ReadBlocks(ctx, "share1", []byte("file1"), "share1/content123", 0, offset, length)
	if err != nil {
		t.Fatalf("ReadBlocks failed: %v", err)
	}

	if uint32(len(result)) != length {
		t.Errorf("ReadBlocks returned %d bytes, want %d", len(result), length)
	}

	// Verify content
	for i := uint32(0); i < length; i++ {
		expected := byte((offset + i) % 256)
		if result[i] != expected {
			t.Errorf("ReadBlocks[%d] = %d, want %d", i, result[i], expected)
			break
		}
	}
}

func TestFlusher_ReadBlocks_MultipleBlocks(t *testing.T) {
	ctx := context.Background()
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())
	defer f.Close()

	// Pre-populate block store with multiple blocks
	totalSize := 10 * 1024 * 1024 // 10MB
	data := make([]byte, totalSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Split into 4MB blocks and upload
	for blockIdx := 0; blockIdx*BlockSize < totalSize; blockIdx++ {
		start := blockIdx * BlockSize
		end := min(start+BlockSize, totalSize)
		blockKey := "share1/content123/chunk-0/block-" + string(rune('0'+blockIdx))
		if err := store.WriteBlock(ctx, blockKey, data[start:end]); err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}
	}

	// Read entire range
	result, err := f.ReadBlocks(ctx, "share1", []byte("file1"), "share1/content123", 0, 0, uint32(totalSize))
	if err != nil {
		t.Fatalf("ReadBlocks failed: %v", err)
	}

	if len(result) != totalSize {
		t.Errorf("ReadBlocks returned %d bytes, want %d", len(result), totalSize)
	}

	// Verify content
	for i := 0; i < totalSize; i++ {
		expected := byte(i % 256)
		if result[i] != expected {
			t.Errorf("ReadBlocks[%d] = %d, want %d", i, result[i], expected)
			break
		}
	}
}

func TestFlusher_Close(t *testing.T) {
	c := cache.New(0)
	defer c.Close()

	store := blockmemory.New()
	defer store.Close()

	f := New(c, store, DefaultConfig())

	// Close should not error
	if err := f.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}

	// Double close should be idempotent
	if err := f.Close(); err != nil {
		t.Errorf("Second Close returned error: %v", err)
	}

	// Operations after close should fail
	ctx := context.Background()
	err := f.FlushRemaining(ctx, "share1", []byte("file1"), "share1/content123")
	if err == nil {
		t.Error("FlushRemaining after Close should return error")
	}
}

func TestAssembleBlocks(t *testing.T) {
	tests := []struct {
		name          string
		blocks        [][]byte
		offset        uint32
		length        uint32
		startBlockIdx uint32
		want          string
	}{
		{
			name:          "single block full",
			blocks:        [][]byte{[]byte("hello")},
			offset:        0,
			length:        5,
			startBlockIdx: 0,
			want:          "hello",
		},
		{
			name:          "single block partial",
			blocks:        [][]byte{[]byte("hello world")},
			offset:        6,
			length:        5,
			startBlockIdx: 0,
			want:          "world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assembleBlocks(tt.blocks, tt.offset, tt.length, tt.startBlockIdx)
			if string(got) != tt.want {
				t.Errorf("assembleBlocks() = %q, want %q", got, tt.want)
			}
		})
	}
}

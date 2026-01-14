//go:build integration

package fs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/payload/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "blockstore-fs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	s, err := NewWithPath(tmpDir)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("NewWithPath failed: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		os.RemoveAll(tmpDir)
	})

	return s
}

func TestStore_WriteAndRead(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	// Write block
	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read block
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("ReadBlock returned %q, want %q", read, data)
	}

	// Verify file exists on disk
	path := filepath.Join(s.BasePath(), "share1", "content123", "chunk-0", "block-0")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("Block file not found at %s", path)
	}
}

func TestStore_ReadBlockNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.ReadBlock(ctx, "nonexistent")
	if err != store.ErrBlockNotFound {
		t.Errorf("ReadBlock returned error %v, want %v", err, store.ErrBlockNotFound)
	}
}

func TestStore_ReadBlockRange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read range from start
	read, err := s.ReadBlockRange(ctx, blockKey, 0, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "hello" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "hello")
	}

	// Read range from middle
	read, err = s.ReadBlockRange(ctx, blockKey, 6, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "world" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "world")
	}

	// Read range that exceeds length (should truncate)
	read, err = s.ReadBlockRange(ctx, blockKey, 6, 100)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "world" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "world")
	}
}

func TestStore_DeleteBlock(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Delete block
	if err := s.DeleteBlock(ctx, blockKey); err != nil {
		t.Fatalf("DeleteBlock failed: %v", err)
	}

	// Verify block is deleted
	_, err := s.ReadBlock(ctx, blockKey)
	if err != store.ErrBlockNotFound {
		t.Errorf("ReadBlock after delete returned error %v, want %v", err, store.ErrBlockNotFound)
	}

	// Verify empty directories were cleaned up
	chunkDir := filepath.Join(s.BasePath(), "share1", "content123", "chunk-0")
	if _, err := os.Stat(chunkDir); !os.IsNotExist(err) {
		t.Errorf("Empty chunk directory should be removed: %s", chunkDir)
	}
}

func TestStore_DeleteByPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Write multiple blocks
	blocks := map[string][]byte{
		"share1/content123/chunk-0/block-0": []byte("data0"),
		"share1/content123/chunk-0/block-1": []byte("data1"),
		"share1/content123/chunk-1/block-0": []byte("data2"),
		"share2/content456/chunk-0/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := s.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	// Delete all blocks for share1/content123
	if err := s.DeleteByPrefix(ctx, "share1/content123"); err != nil {
		t.Fatalf("DeleteByPrefix failed: %v", err)
	}

	// Verify share1/content123 blocks are deleted
	for key := range blocks {
		_, err := s.ReadBlock(ctx, key)
		if key[:17] == "share1/content123" {
			if err != store.ErrBlockNotFound {
				t.Errorf("ReadBlock(%s) after delete returned error %v, want %v", key, err, store.ErrBlockNotFound)
			}
		} else {
			if err != nil {
				t.Errorf("ReadBlock(%s) after delete returned unexpected error: %v", key, err)
			}
		}
	}

	// Verify share2 is untouched
	read, err := s.ReadBlock(ctx, "share2/content456/chunk-0/block-0")
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if string(read) != "data3" {
		t.Errorf("ReadBlock returned %q, want %q", read, "data3")
	}
}

func TestStore_ListByPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Write multiple blocks
	blocks := map[string][]byte{
		"share1/content123/chunk-0/block-0": []byte("data0"),
		"share1/content123/chunk-0/block-1": []byte("data1"),
		"share1/content123/chunk-1/block-0": []byte("data2"),
		"share2/content456/chunk-0/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := s.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	// List all blocks for share1/content123
	keys, err := s.ListByPrefix(ctx, "share1/content123")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3: %v", len(keys), keys)
	}

	// List all blocks for share1
	keys, err = s.ListByPrefix(ctx, "share1")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3: %v", len(keys), keys)
	}

	// List all blocks
	keys, err = s.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 4 {
		t.Errorf("ListByPrefix returned %d keys, want 4: %v", len(keys), keys)
	}
}

func TestStore_ListByPrefix_Empty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// List non-existent prefix
	keys, err := s.ListByPrefix(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 0 {
		t.Errorf("ListByPrefix returned %d keys, want 0", len(keys))
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Close the store
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// All operations should return ErrStoreClosed
	if _, err := s.ReadBlock(ctx, "key"); err != store.ErrStoreClosed {
		t.Errorf("ReadBlock on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if err := s.WriteBlock(ctx, "key", []byte("data")); err != store.ErrStoreClosed {
		t.Errorf("WriteBlock on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if err := s.DeleteBlock(ctx, "key"); err != store.ErrStoreClosed {
		t.Errorf("DeleteBlock on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if _, err := s.ListByPrefix(ctx, ""); err != store.ErrStoreClosed {
		t.Errorf("ListByPrefix on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if err := s.HealthCheck(ctx); err != store.ErrStoreClosed {
		t.Errorf("HealthCheck on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}
}

func TestStore_HealthCheck(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Should be healthy
	if err := s.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}

func TestStore_OverwriteBlock(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	blockKey := "share1/content123/chunk-0/block-0"

	// Write initial data
	if err := s.WriteBlock(ctx, blockKey, []byte("initial")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Overwrite with new data
	if err := s.WriteBlock(ctx, blockKey, []byte("updated")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read and verify
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != "updated" {
		t.Errorf("ReadBlock returned %q, want %q", read, "updated")
	}
}

func TestStore_LargeBlock(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	blockKey := "share1/content123/chunk-0/block-0"

	// Write 4MB block (BlockSize)
	data := make([]byte, store.BlockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read full block
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if len(read) != store.BlockSize {
		t.Errorf("ReadBlock returned %d bytes, want %d", len(read), store.BlockSize)
	}

	// Verify some bytes
	for i := 0; i < 100; i++ {
		if read[i] != byte(i%256) {
			t.Errorf("ReadBlock[%d] = %d, want %d", i, read[i], byte(i%256))
		}
	}

	// Read range from middle
	offset := int64(store.BlockSize / 2)
	length := int64(1024)
	rangeData, err := s.ReadBlockRange(ctx, blockKey, offset, length)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if len(rangeData) != int(length) {
		t.Errorf("ReadBlockRange returned %d bytes, want %d", len(rangeData), length)
	}
}

func TestStore_InvalidBasePath(t *testing.T) {
	// Empty base path
	_, err := New(Config{BasePath: ""})
	if err == nil {
		t.Error("New with empty base path should fail")
	}

	// Non-existent path without CreateDir
	_, err = New(Config{
		BasePath:  "/nonexistent/path/that/does/not/exist",
		CreateDir: false,
	})
	if err == nil {
		t.Error("New with non-existent path should fail")
	}
}

func TestStore_DeleteNonExistent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Delete non-existent block should not error
	if err := s.DeleteBlock(ctx, "nonexistent/block"); err != nil {
		t.Errorf("DeleteBlock on non-existent block returned error: %v", err)
	}

	// DeleteByPrefix on non-existent prefix should not error
	if err := s.DeleteByPrefix(ctx, "nonexistent/prefix"); err != nil {
		t.Errorf("DeleteByPrefix on non-existent prefix returned error: %v", err)
	}
}

// ============================================================================
// Benchmarks
// ============================================================================

func newBenchStore(b *testing.B) *Store {
	b.Helper()

	tmpDir, err := os.MkdirTemp("", "blockstore-fs-bench-*")
	if err != nil {
		b.Fatalf("failed to create temp dir: %v", err)
	}

	s, err := NewWithPath(tmpDir)
	if err != nil {
		os.RemoveAll(tmpDir)
		b.Fatalf("NewWithPath failed: %v", err)
	}

	b.Cleanup(func() {
		s.Close()
		os.RemoveAll(tmpDir)
	})

	return s
}

func BenchmarkWriteBlock(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"4MB", 4 * 1024 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			ctx := context.Background()
			s := newBenchStore(b)
			data := make([]byte, sz.size)

			b.SetBytes(int64(sz.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				blockKey := filepath.Join("bench", "chunk-0", "block-"+string(rune('0'+i%10)))
				if err := s.WriteBlock(ctx, blockKey, data); err != nil {
					b.Fatalf("WriteBlock failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkReadBlock(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"4MB", 4 * 1024 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			ctx := context.Background()
			s := newBenchStore(b)
			data := make([]byte, sz.size)
			blockKey := "bench/chunk-0/block-0"

			// Pre-write the block
			if err := s.WriteBlock(ctx, blockKey, data); err != nil {
				b.Fatalf("WriteBlock failed: %v", err)
			}

			b.SetBytes(int64(sz.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := s.ReadBlock(ctx, blockKey); err != nil {
					b.Fatalf("ReadBlock failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkReadBlockRange(b *testing.B) {
	ctx := context.Background()
	s := newBenchStore(b)

	// Write a 4MB block
	blockKey := "bench/chunk-0/block-0"
	data := make([]byte, 4*1024*1024)
	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		b.Fatalf("WriteBlock failed: %v", err)
	}

	ranges := []struct {
		name   string
		offset int64
		length int64
	}{
		{"1KB_start", 0, 1024},
		{"1KB_middle", 2 * 1024 * 1024, 1024},
		{"64KB_start", 0, 64 * 1024},
		{"64KB_middle", 2 * 1024 * 1024, 64 * 1024},
	}

	for _, r := range ranges {
		b.Run(r.name, func(b *testing.B) {
			b.SetBytes(r.length)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := s.ReadBlockRange(ctx, blockKey, r.offset, r.length); err != nil {
					b.Fatalf("ReadBlockRange failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkWriteBlock_Parallel(b *testing.B) {
	ctx := context.Background()
	s := newBenchStore(b)
	data := make([]byte, 64*1024) // 64KB blocks

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			blockKey := filepath.Join("bench", "parallel", "block-"+string(rune('0'+i%100)))
			if err := s.WriteBlock(ctx, blockKey, data); err != nil {
				b.Fatalf("WriteBlock failed: %v", err)
			}
			i++
		}
	})
}

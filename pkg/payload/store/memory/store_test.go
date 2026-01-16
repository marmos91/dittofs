package memory

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/payload/store"
)

func TestStore_WriteAndRead(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

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
}

func TestStore_ReadBlockNotFound(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	_, err := s.ReadBlock(ctx, "nonexistent")
	if err != store.ErrBlockNotFound {
		t.Errorf("ReadBlock returned error %v, want %v", err, store.ErrBlockNotFound)
	}
}

func TestStore_ReadBlockRange(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read range
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
	s := New()
	defer s.Close()

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
}

func TestStore_DeleteByPrefix(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

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
	if err := s.DeleteByPrefix(ctx, "share1/content123/"); err != nil {
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
}

func TestStore_ListByPrefix(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

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
	keys, err := s.ListByPrefix(ctx, "share1/content123/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3", len(keys))
	}

	// List all blocks for share1
	keys, err = s.ListByPrefix(ctx, "share1/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3", len(keys))
	}

	// List all blocks
	keys, err = s.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 4 {
		t.Errorf("ListByPrefix returned %d keys, want 4", len(keys))
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	ctx := context.Background()
	s := New()

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
}

func TestStore_DataIsolation(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	// Write block
	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Modify original data
	data[0] = 'X'

	// Read block - should not be affected by modification
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if read[0] != 'h' {
		t.Errorf("WriteBlock did not copy data: got %c, want 'h'", read[0])
	}

	// Modify read data
	read[0] = 'Y'

	// Read again - should not be affected
	read2, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if read2[0] != 'h' {
		t.Errorf("ReadBlock did not copy data: got %c, want 'h'", read2[0])
	}
}

func TestStore_BlockCount(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	if s.BlockCount() != 0 {
		t.Errorf("BlockCount on empty store returned %d, want 0", s.BlockCount())
	}

	// Write blocks
	if err := s.WriteBlock(ctx, "key1", []byte("data1")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}
	if err := s.WriteBlock(ctx, "key2", []byte("data2")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if s.BlockCount() != 2 {
		t.Errorf("BlockCount returned %d, want 2", s.BlockCount())
	}
}

func TestStore_TotalSize(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	if s.TotalSize() != 0 {
		t.Errorf("TotalSize on empty store returned %d, want 0", s.TotalSize())
	}

	// Write blocks
	if err := s.WriteBlock(ctx, "key1", []byte("hello")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}
	if err := s.WriteBlock(ctx, "key2", []byte("world")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if s.TotalSize() != 10 {
		t.Errorf("TotalSize returned %d, want 10", s.TotalSize())
	}
}

// ============================================================================
// Benchmarks
// ============================================================================

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
			s := New()
			defer s.Close()
			data := make([]byte, sz.size)

			b.SetBytes(int64(sz.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				blockKey := "bench/chunk-0/block-0"
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
			s := New()
			defer s.Close()
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
	s := New()
	defer s.Close()

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
	s := New()
	defer s.Close()
	data := make([]byte, 64*1024) // 64KB blocks

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			blockKey := "bench/parallel/block-" + string(rune('0'+i%100))
			if err := s.WriteBlock(ctx, blockKey, data); err != nil {
				b.Fatalf("WriteBlock failed: %v", err)
			}
			i++
		}
	})
}

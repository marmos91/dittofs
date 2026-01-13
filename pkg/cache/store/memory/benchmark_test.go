package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
)

// ============================================================================
// Write Benchmarks
// ============================================================================

// BenchmarkWriteSlice_Sequential measures sequential write performance.
// This is the hot path for NFS file copies.
func BenchmarkWriteSlice_Sequential(b *testing.B) {
	sizes := []int{4 * 1024, 32 * 1024, 64 * 1024, 128 * 1024}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%dKB", size/1024), func(b *testing.B) {
			c := NewCache(0)
			defer c.Close()

			ctx := context.Background()
			fileHandle := []byte("benchmark-file")
			data := make([]byte, size)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32(i * size)
				chunkIdx := offset / cache.ChunkSize
				offsetInChunk := offset % cache.ChunkSize

				if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWriteSlice_Random measures random write performance.
// This simulates database-like workloads with scattered writes.
func BenchmarkWriteSlice_Random(b *testing.B) {
	c := NewCache(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := []byte("benchmark-file")
	data := make([]byte, 4*1024) // 4KB writes

	// Pre-populate with some data to create multiple slices
	for i := 0; i < 100; i++ {
		offset := uint32(i * 64 * 1024) // 64KB apart
		chunkIdx := offset / cache.ChunkSize
		offsetInChunk := offset % cache.ChunkSize
		_ = c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk)
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Random offset within first 10MB
		offset := uint32((i * 7919) % (10 * 1024 * 1024)) // Prime for distribution
		chunkIdx := offset / cache.ChunkSize
		offsetInChunk := offset % cache.ChunkSize

		if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteSlice_Concurrent measures concurrent write performance.
// This simulates multiple NFS clients writing to different files.
func BenchmarkWriteSlice_Concurrent(b *testing.B) {
	c := NewCache(0)
	defer c.Close()

	ctx := context.Background()
	data := make([]byte, 32*1024) // 32KB writes

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			fileHandle := []byte(fmt.Sprintf("file-%d", i%100)) // 100 different files
			offset := uint32((i * 32 * 1024) % (64 * 1024 * 1024))
			chunkIdx := offset / cache.ChunkSize
			offsetInChunk := offset % cache.ChunkSize

			if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// ============================================================================
// Read Benchmarks
// ============================================================================

// BenchmarkReadSlice measures read performance with slice merging.
func BenchmarkReadSlice(b *testing.B) {
	sizes := []int{4 * 1024, 32 * 1024, 64 * 1024, 128 * 1024}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%dKB", size/1024), func(b *testing.B) {
			c := NewCache(0)
			defer c.Close()

			ctx := context.Background()
			fileHandle := []byte("benchmark-file")

			// Pre-populate with 10MB of data
			data := make([]byte, size)
			for i := 0; i < 10*1024*1024/size; i++ {
				offset := uint32(i * size)
				chunkIdx := offset / cache.ChunkSize
				offsetInChunk := offset % cache.ChunkSize
				_ = c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk)
			}

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32((i * size) % (10 * 1024 * 1024))
				chunkIdx := offset / cache.ChunkSize
				offsetInChunk := offset % cache.ChunkSize

				_, _, err := c.ReadSlice(ctx, fileHandle, chunkIdx, offsetInChunk, uint32(size))
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReadSlice_ManySlices measures read performance when many slices need merging.
func BenchmarkReadSlice_ManySlices(b *testing.B) {
	sliceCounts := []int{1, 5, 10, 20, 50}

	for _, count := range sliceCounts {
		b.Run(fmt.Sprintf("slices=%d", count), func(b *testing.B) {
			c := NewCache(0)
			defer c.Close()

			ctx := context.Background()
			fileHandle := []byte("benchmark-file")

			// Create overlapping slices
			for i := 0; i < count; i++ {
				data := make([]byte, 4*1024)
				offset := uint32(i * 2 * 1024) // 2KB offset increments, so slices overlap
				_ = c.WriteSlice(ctx, fileHandle, 0, data, offset)
			}

			readSize := uint32(32 * 1024)
			b.SetBytes(int64(readSize))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, _, err := c.ReadSlice(ctx, fileHandle, 0, 0, readSize)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ============================================================================
// Coalesce Benchmarks
// ============================================================================

// BenchmarkCoalesceChunk measures coalescing performance.
func BenchmarkCoalesceChunk(b *testing.B) {
	sliceCounts := []int{2, 5, 10, 20, 50, 100}

	for _, count := range sliceCounts {
		b.Run(fmt.Sprintf("slices=%d", count), func(b *testing.B) {
			store := New()
			ctx := context.Background()
			fileHandle := []byte("benchmark-file")

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// Setup: create many non-adjacent slices
				for j := 0; j < count; j++ {
					slice := cache.Slice{
						ID:     fmt.Sprintf("slice-%d", j),
						Offset: uint32(j * 4 * 1024), // 4KB each, non-adjacent
						Length: uint32(2 * 1024),    // 2KB data (2KB gap between)
						Data:   make([]byte, 2*1024),
						State:  cache.SliceStatePending,
					}
					_ = store.AddSlice(ctx, fileHandle, 0, slice)
				}
				b.StartTimer()

				if err := store.CoalesceChunk(ctx, fileHandle, 0); err != nil {
					b.Fatal(err)
				}

				b.StopTimer()
				_ = store.RemoveFile(ctx, fileHandle)
			}
		})
	}
}

// BenchmarkCoalesceChunk_Adjacent measures coalescing with adjacent slices.
func BenchmarkCoalesceChunk_Adjacent(b *testing.B) {
	sliceCounts := []int{2, 5, 10, 20, 50, 100}

	for _, count := range sliceCounts {
		b.Run(fmt.Sprintf("slices=%d", count), func(b *testing.B) {
			store := New()
			ctx := context.Background()
			fileHandle := []byte("benchmark-file")

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// Setup: create adjacent slices (will merge into one)
				for j := 0; j < count; j++ {
					slice := cache.Slice{
						ID:     fmt.Sprintf("slice-%d", j),
						Offset: uint32(j * 4 * 1024), // Adjacent 4KB slices
						Length: uint32(4 * 1024),
						Data:   make([]byte, 4*1024),
						State:  cache.SliceStatePending,
					}
					_ = store.AddSlice(ctx, fileHandle, 0, slice)
				}
				b.StartTimer()

				if err := store.CoalesceChunk(ctx, fileHandle, 0); err != nil {
					b.Fatal(err)
				}

				b.StopTimer()
				_ = store.RemoveFile(ctx, fileHandle)
			}
		})
	}
}

// ============================================================================
// ExtendAdjacentSlice Benchmarks
// ============================================================================

// BenchmarkExtendAdjacentSlice measures the sequential write optimization.
func BenchmarkExtendAdjacentSlice(b *testing.B) {
	store := New()
	defer store.Close()

	ctx := context.Background()
	fileHandle := []byte("benchmark-file")
	data := make([]byte, 32*1024) // 32KB writes

	// Create initial slice
	initialSlice := cache.Slice{
		ID:     "initial",
		Offset: 0,
		Length: 32 * 1024,
		Data:   make([]byte, 32*1024),
		State:  cache.SliceStatePending,
	}
	_ = store.AddSlice(ctx, fileHandle, 0, initialSlice)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	currentOffset := uint32(32 * 1024) // Start after initial slice
	for i := 0; i < b.N; i++ {
		if currentOffset+uint32(len(data)) > cache.ChunkSize {
			// Reset for next chunk cycle
			b.StopTimer()
			_ = store.RemoveFile(ctx, fileHandle)
			_ = store.AddSlice(ctx, fileHandle, 0, initialSlice)
			currentOffset = 32 * 1024
			b.StartTimer()
		}

		if !store.ExtendAdjacentSlice(ctx, fileHandle, 0, currentOffset, data) {
			b.Fatalf("ExtendAdjacentSlice should succeed at offset %d", currentOffset)
		}
		currentOffset += uint32(len(data))
	}
}

// BenchmarkExtendAdjacentSlice_Miss measures performance when no adjacent slice exists.
func BenchmarkExtendAdjacentSlice_Miss(b *testing.B) {
	store := New()
	defer store.Close()

	ctx := context.Background()
	fileHandle := []byte("benchmark-file")
	data := make([]byte, 4*1024)

	// Create a slice that won't be adjacent to our writes
	slice := cache.Slice{
		ID:     "far-away",
		Offset: 1024 * 1024, // 1MB offset
		Length: 4 * 1024,
		Data:   data,
		State:  cache.SliceStatePending,
	}
	_ = store.AddSlice(ctx, fileHandle, 0, slice)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Try to extend at offset 0 - should always miss
		_ = store.ExtendAdjacentSlice(ctx, fileHandle, 0, 0, data)
	}
}

// ============================================================================
// End-to-End Benchmarks
// ============================================================================

// BenchmarkE2E_SequentialWrite simulates a typical NFS file copy.
func BenchmarkE2E_SequentialWrite(b *testing.B) {
	fileSizes := []int{1, 10, 100} // MB

	for _, sizeMB := range fileSizes {
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			c := NewCache(0)
			ctx := context.Background()

			writeSize := 32 * 1024 // 32KB per write (typical NFS)
			totalWrites := (sizeMB * 1024 * 1024) / writeSize
			data := make([]byte, writeSize)

			b.SetBytes(int64(sizeMB * 1024 * 1024))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				fileHandle := []byte(fmt.Sprintf("file-%d", i))

				// Simulate sequential writes
				for j := 0; j < totalWrites; j++ {
					offset := uint32(j * writeSize)
					chunkIdx := offset / cache.ChunkSize
					offsetInChunk := offset % cache.ChunkSize

					if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
						b.Fatal(err)
					}
				}

				// Simulate COMMIT
				chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
				for _, chunkIdx := range chunkIndices {
					if err := c.store.CoalesceChunk(ctx, fileHandle, chunkIdx); err != nil {
						b.Fatal(err)
					}
				}
			}

			c.Close()
		})
	}
}

// BenchmarkE2E_ReadAfterWrite simulates write then read pattern.
func BenchmarkE2E_ReadAfterWrite(b *testing.B) {
	c := NewCache(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := []byte("benchmark-file")
	writeSize := 32 * 1024
	data := make([]byte, writeSize)

	// Write 10MB
	for i := 0; i < 320; i++ {
		offset := uint32(i * writeSize)
		chunkIdx := offset / cache.ChunkSize
		offsetInChunk := offset % cache.ChunkSize
		_ = c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk)
	}

	// Coalesce
	chunkIndices := c.store.GetChunkIndices(ctx, fileHandle)
	for _, chunkIdx := range chunkIndices {
		_ = c.store.CoalesceChunk(ctx, fileHandle, chunkIdx)
	}

	readSize := uint32(64 * 1024) // 64KB reads
	b.SetBytes(int64(readSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32((i * int(readSize)) % (10 * 1024 * 1024))
		chunkIdx := offset / cache.ChunkSize
		offsetInChunk := offset % cache.ChunkSize

		_, _, err := c.ReadSlice(ctx, fileHandle, chunkIdx, offsetInChunk, readSize)
		if err != nil {
			b.Fatal(err)
		}
	}
}

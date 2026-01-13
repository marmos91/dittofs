package cache

import (
	"context"
	"fmt"
	"testing"
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
			c := New(0)
			defer c.Close()

			ctx := context.Background()
			fileHandle := []byte("benchmark-file")
			data := make([]byte, size)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32(i * size)
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

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
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := []byte("benchmark-file")
	data := make([]byte, 4*1024) // 4KB writes

	// Pre-populate with some data to create multiple slices
	for i := 0; i < 100; i++ {
		offset := uint32(i * 64 * 1024) // 64KB apart
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		_ = c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk)
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Random offset within first 10MB
		offset := uint32((i * 7919) % (10 * 1024 * 1024)) // Prime for distribution
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize

		if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteSlice_Concurrent measures concurrent write performance.
// This simulates multiple NFS clients writing to different files.
func BenchmarkWriteSlice_Concurrent(b *testing.B) {
	c := New(0)
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
			chunkIdx := offset / ChunkSize
			offsetInChunk := offset % ChunkSize

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
			c := New(0)
			defer c.Close()

			ctx := context.Background()
			fileHandle := []byte("benchmark-file")

			// Pre-populate with 10MB of data
			data := make([]byte, size)
			for i := 0; i < 10*1024*1024/size; i++ {
				offset := uint32(i * size)
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize
				_ = c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk)
			}

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				offset := uint32((i * size) % (10 * 1024 * 1024))
				chunkIdx := offset / ChunkSize
				offsetInChunk := offset % ChunkSize

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
			c := New(0)
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
// End-to-End Benchmarks
// ============================================================================

// BenchmarkE2E_SequentialWrite simulates a typical NFS file copy.
func BenchmarkE2E_SequentialWrite(b *testing.B) {
	fileSizes := []int{1, 10, 100} // MB

	for _, sizeMB := range fileSizes {
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			c := New(0)
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
					chunkIdx := offset / ChunkSize
					offsetInChunk := offset % ChunkSize

					if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
						b.Fatal(err)
					}
				}

				// Simulate COMMIT (coalesce)
				if err := c.CoalesceWrites(ctx, fileHandle); err != nil {
					b.Fatal(err)
				}
			}

			c.Close()
		})
	}
}

// BenchmarkE2E_ReadAfterWrite simulates write then read pattern.
func BenchmarkE2E_ReadAfterWrite(b *testing.B) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := []byte("benchmark-file")
	writeSize := 32 * 1024
	data := make([]byte, writeSize)

	// Write 10MB
	for i := 0; i < 320; i++ {
		offset := uint32(i * writeSize)
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		_ = c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk)
	}

	// Coalesce
	_ = c.CoalesceWrites(ctx, fileHandle)

	readSize := uint32(64 * 1024) // 64KB reads
	b.SetBytes(int64(readSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32((i * int(readSize)) % (10 * 1024 * 1024))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize

		_, _, err := c.ReadSlice(ctx, fileHandle, chunkIdx, offsetInChunk, readSize)
		if err != nil {
			b.Fatal(err)
		}
	}
}

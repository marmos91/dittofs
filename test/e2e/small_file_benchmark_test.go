package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// BenchmarkSmallFileWorkloads tests performance for typical sync&share workloads
// with small files (documents, configs, etc.)
//
// Three realistic scenarios:
// 1. Sequential writes: Simulates incremental file uploads (Dropbox-style sync)
// 2. Multi-file updates: Simulates document editing with autosave
// 3. Interleaved read-write: Simulates sync operations with verification
//
// Each benchmark is run against three backend configurations:
// - memory: Standard in-memory store (full-file copy-on-write)
// - memory-chunked: Page-based memory store (64KB pages)
// - filesystem: Actual filesystem storage
//
// Usage:
//   go test -bench=BenchmarkSmallFileWorkloads -benchtime=10s -benchmem ./test/e2e/

func BenchmarkSmallFileWorkloads(b *testing.B) {
	// Check if NFS mounting is available
	if !framework.CanMount(&testing.T{}) {
		b.Skip("NFS mounting not available on this system")
	}

	storeTypes := []framework.StoreType{
		framework.StoreTypeMemory,
		framework.StoreTypeMemoryChunked,
		framework.StoreTypeFilesystem,
	}

	for _, storeType := range storeTypes {
		storeType := storeType // capture for parallel benchmarks
		b.Run(string(storeType), func(b *testing.B) {
			srv, mount := setupSmallFileServer(b, storeType)
			defer mount.Cleanup()
			defer srv.Stop()

			// Run sub-benchmarks
			b.Run("SequentialWrites", func(b *testing.B) {
				benchmarkSequentialWrites(b, mount.MountPoint())
			})

			b.Run("MultiFileUpdates", func(b *testing.B) {
				benchmarkMultiFileUpdates(b, mount.MountPoint())
			})

			b.Run("InterleavedReadWrite", func(b *testing.B) {
				benchmarkInterleavedReadWrite(b, mount.MountPoint())
			})
		})
	}
}

// setupSmallFileServer creates and mounts a test server for small file benchmarks
func setupSmallFileServer(b *testing.B, storeType framework.StoreType) (*framework.TestServer, *framework.NFSMount) {
	b.Helper()

	// Create and start server
	srv := framework.NewTestServer(&testing.T{}, framework.TestServerConfig{
		ContentStore: storeType,
		LogLevel:     "ERROR",
	})

	if err := srv.Start(); err != nil {
		b.Fatalf("Failed to start server: %v", err)
	}

	// Mount the filesystem
	mount := framework.NewNFSMount(&testing.T{}, framework.MountConfig{
		ServerPort: srv.Port(),
		ExportPath: srv.ShareName(),
	})
	if err := mount.Mount(); err != nil {
		srv.Stop()
		b.Fatalf("Failed to mount: %v", err)
	}

	return srv, mount
}

// benchmarkSequentialWrites simulates incremental file uploads
// Scenario: User uploads 10 small files sequentially (e.g., syncing a documents folder)
func benchmarkSequentialWrites(b *testing.B, mountPoint string) {
	const (
		numFiles = 10
		fileSize = 4096 // 4KB files
	)

	// Prepare test data
	data := bytes.Repeat([]byte("test data for sequential write benchmark\n"), fileSize/42)

	b.ResetTimer()
	b.SetBytes(int64(numFiles * fileSize))

	for i := 0; i < b.N; i++ {
		// Write 10 files sequentially
		for fileIdx := 0; fileIdx < numFiles; fileIdx++ {
			filename := filepath.Join(mountPoint, fmt.Sprintf("seq_file_%d_%d.txt", i, fileIdx))
			if err := os.WriteFile(filename, data, 0644); err != nil {
				b.Fatalf("Failed to write file: %v", err)
			}
		}

		// Cleanup
		for fileIdx := 0; fileIdx < numFiles; fileIdx++ {
			filename := filepath.Join(mountPoint, fmt.Sprintf("seq_file_%d_%d.txt", i, fileIdx))
			os.Remove(filename)
		}
	}
}

// benchmarkMultiFileUpdates simulates document editing with autosave
// Scenario: User edits 5 documents, each file is updated 10 times (auto-save every few seconds)
func benchmarkMultiFileUpdates(b *testing.B, mountPoint string) {
	const (
		numFiles       = 5
		updatesPerFile = 10
		fileSize       = 8192 // 8KB documents
	)

	// Create initial files
	initialData := bytes.Repeat([]byte("initial content\n"), fileSize/16)
	updateData := bytes.Repeat([]byte("updated content\n"), fileSize/16)

	for fileIdx := 0; fileIdx < numFiles; fileIdx++ {
		filename := filepath.Join(mountPoint, fmt.Sprintf("doc_%d.txt", fileIdx))
		if err := os.WriteFile(filename, initialData, 0644); err != nil {
			b.Fatalf("Failed to create initial file: %v", err)
		}
	}

	defer func() {
		for fileIdx := 0; fileIdx < numFiles; fileIdx++ {
			filename := filepath.Join(mountPoint, fmt.Sprintf("doc_%d.txt", fileIdx))
			os.Remove(filename)
		}
	}()

	b.ResetTimer()
	b.SetBytes(int64(numFiles * updatesPerFile * fileSize))

	for i := 0; i < b.N; i++ {
		// Update each file multiple times
		for updateIdx := 0; updateIdx < updatesPerFile; updateIdx++ {
			for fileIdx := 0; fileIdx < numFiles; fileIdx++ {
				filename := filepath.Join(mountPoint, fmt.Sprintf("doc_%d.txt", fileIdx))
				if err := os.WriteFile(filename, updateData, 0644); err != nil {
					b.Fatalf("Failed to update file: %v", err)
				}
			}
		}
	}
}

// benchmarkInterleavedReadWrite simulates sync operations with verification
// Scenario: Sync client writes a file, reads it back for verification, repeats
func benchmarkInterleavedReadWrite(b *testing.B, mountPoint string) {
	const (
		numOperations = 20
		fileSize      = 4096 // 4KB files
	)

	data := bytes.Repeat([]byte("test data for interleaved benchmark\n"), fileSize/37)

	b.ResetTimer()
	b.SetBytes(int64(numOperations * fileSize * 2)) // *2 for read+write

	for i := 0; i < b.N; i++ {
		for opIdx := 0; opIdx < numOperations; opIdx++ {
			filename := filepath.Join(mountPoint, fmt.Sprintf("sync_file_%d.txt", opIdx))

			// Write
			if err := os.WriteFile(filename, data, 0644); err != nil {
				b.Fatalf("Failed to write file: %v", err)
			}

			// Read back for verification
			readData, err := os.ReadFile(filename)
			if err != nil {
				b.Fatalf("Failed to read file: %v", err)
			}

			if !bytes.Equal(data, readData) {
				b.Fatalf("Data mismatch: expected %d bytes, got %d bytes", len(data), len(readData))
			}

			// Cleanup
			os.Remove(filename)
		}
	}
}

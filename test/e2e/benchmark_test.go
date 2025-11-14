package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// BenchmarkE2E is the main entry point for end-to-end benchmarks
// It benchmarks all operations against all store type combinations
func BenchmarkE2E(b *testing.B) {
	// Check if NFS mounting is available
	if !framework.CanMount(&testing.T{}) {
		b.Skip("NFS mounting not available on this system")
	}

	// Define store types to benchmark
	storeTypes := []framework.StoreType{
		framework.StoreTypeMemory,
		framework.StoreTypeFilesystem,
	}

	// Run all benchmarks for each store type
	for _, storeType := range storeTypes {
		storeType := storeType // capture for parallel benchmarks
		b.Run(string(storeType), func(b *testing.B) {
			// File operations
			b.Run("FileOperations", func(b *testing.B) {
				benchmarkFileOperations(b, storeType)
			})

			// Directory operations
			b.Run("DirectoryOperations", func(b *testing.B) {
				benchmarkDirectoryOperations(b, storeType)
			})

			// Read throughput
			b.Run("ReadThroughput", func(b *testing.B) {
				benchmarkReadThroughput(b, storeType)
			})

			// Write throughput
			b.Run("WriteThroughput", func(b *testing.B) {
				benchmarkWriteThroughput(b, storeType)
			})

			// Mixed workload
			b.Run("MixedWorkload", func(b *testing.B) {
				benchmarkMixedWorkload(b, storeType)
			})

			// Metadata operations
			b.Run("MetadataOperations", func(b *testing.B) {
				benchmarkMetadataOperations(b, storeType)
			})
		})
	}
}

// benchmarkFileOperations measures file creation, read, write, and deletion
func benchmarkFileOperations(b *testing.B, storeType framework.StoreType) {
	srv, mount := setupBenchmarkServer(b, storeType)
	defer mount.Cleanup()
	defer srv.Stop()

	b.Run("Create", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			filename := filepath.Join(mount.MountPoint(), fmt.Sprintf("file_%d", i))
			f, err := os.Create(filename)
			if err != nil {
				b.Fatalf("Failed to create file: %v", err)
			}
			f.Close()
		}
	})

	b.Run("Stat", func(b *testing.B) {
		// Create test file
		testFile := filepath.Join(mount.MountPoint(), "stat_test")
		f, _ := os.Create(testFile)
		f.Close()

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_, err := os.Stat(testFile)
			if err != nil {
				b.Fatalf("Failed to stat file: %v", err)
			}
		}
	})

	b.Run("Delete", func(b *testing.B) {
		// Pre-create files
		files := make([]string, b.N)
		for i := 0; i < b.N; i++ {
			filename := filepath.Join(mount.MountPoint(), fmt.Sprintf("del_%d", i))
			f, _ := os.Create(filename)
			f.Close()
			files[i] = filename
		}

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			err := os.Remove(files[i])
			if err != nil {
				b.Fatalf("Failed to delete file: %v", err)
			}
		}
	})

	b.Run("Rename", func(b *testing.B) {
		// Pre-create files
		files := make([]string, b.N)
		for i := 0; i < b.N; i++ {
			filename := filepath.Join(mount.MountPoint(), fmt.Sprintf("ren_%d", i))
			f, _ := os.Create(filename)
			f.Close()
			files[i] = filename
		}

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			newName := filepath.Join(mount.MountPoint(), fmt.Sprintf("renamed_%d", i))
			err := os.Rename(files[i], newName)
			if err != nil {
				b.Fatalf("Failed to rename file: %v", err)
			}
		}
	})
}

// benchmarkDirectoryOperations measures directory operations
func benchmarkDirectoryOperations(b *testing.B, storeType framework.StoreType) {
	srv, mount := setupBenchmarkServer(b, storeType)
	defer mount.Cleanup()
	defer srv.Stop()

	b.Run("Mkdir", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			dirname := filepath.Join(mount.MountPoint(), fmt.Sprintf("dir_%d", i))
			err := os.Mkdir(dirname, 0755)
			if err != nil {
				b.Fatalf("Failed to create directory: %v", err)
			}
		}
	})

	b.Run("Readdir", func(b *testing.B) {
		// Create directory with files
		testDir := filepath.Join(mount.MountPoint(), "readdir_test")
		os.Mkdir(testDir, 0755)
		for i := 0; i < 100; i++ {
			f, _ := os.Create(filepath.Join(testDir, fmt.Sprintf("file%d", i)))
			f.Close()
		}

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			entries, err := os.ReadDir(testDir)
			if err != nil {
				b.Fatalf("Failed to read directory: %v", err)
			}
			if len(entries) != 100 {
				b.Fatalf("Expected 100 entries, got %d", len(entries))
			}
		}
	})

	b.Run("ReaddirLarge", func(b *testing.B) {
		// Create directory with many files
		testDir := filepath.Join(mount.MountPoint(), "readdir_large")
		os.Mkdir(testDir, 0755)
		for i := 0; i < 1000; i++ {
			f, _ := os.Create(filepath.Join(testDir, fmt.Sprintf("file%d", i)))
			f.Close()
		}

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			entries, err := os.ReadDir(testDir)
			if err != nil {
				b.Fatalf("Failed to read directory: %v", err)
			}
			if len(entries) != 1000 {
				b.Fatalf("Expected 1000 entries, got %d", len(entries))
			}
		}
	})
}

// benchmarkReadThroughput measures read throughput with different file sizes
func benchmarkReadThroughput(b *testing.B, storeType framework.StoreType) {
	srv, mount := setupBenchmarkServer(b, storeType)
	defer mount.Cleanup()
	defer srv.Stop()

	sizes := []struct {
		name string
		size int64
	}{
		{"4KB", 4 * 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			// Create test file
			testFile := filepath.Join(mount.MountPoint(), fmt.Sprintf("read_%s", size.name))
			f, err := os.Create(testFile)
			if err != nil {
				b.Fatalf("Failed to create test file: %v", err)
			}

			// Write data
			data := make([]byte, size.size)
			for i := range data {
				data[i] = byte(i % 256)
			}
			if _, err := f.Write(data); err != nil {
				f.Close()
				b.Fatalf("Failed to write test data: %v", err)
			}
			f.Close()

			// Benchmark reads
			buf := make([]byte, size.size)
			b.SetBytes(size.size)
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				f, err := os.Open(testFile)
				if err != nil {
					b.Fatalf("Failed to open file: %v", err)
				}

				n, err := f.Read(buf)
				if err != nil {
					f.Close()
					b.Fatalf("Failed to read file: %v", err)
				}
				if int64(n) != size.size {
					f.Close()
					b.Fatalf("Expected to read %d bytes, got %d", size.size, n)
				}

				f.Close()
			}
		})
	}
}

// benchmarkWriteThroughput measures write throughput with different file sizes
func benchmarkWriteThroughput(b *testing.B, storeType framework.StoreType) {
	srv, mount := setupBenchmarkServer(b, storeType)
	defer mount.Cleanup()
	defer srv.Stop()

	sizes := []struct {
		name string
		size int64
	}{
		{"4KB", 4 * 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			// Prepare data
			data := make([]byte, size.size)
			for i := range data {
				data[i] = byte(i % 256)
			}

			b.SetBytes(size.size)
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				testFile := filepath.Join(mount.MountPoint(), fmt.Sprintf("write_%s_%d", size.name, i))
				f, err := os.Create(testFile)
				if err != nil {
					b.Fatalf("Failed to create file: %v", err)
				}

				n, err := f.Write(data)
				if err != nil {
					f.Close()
					b.Fatalf("Failed to write file: %v", err)
				}
				if int64(n) != size.size {
					f.Close()
					b.Fatalf("Expected to write %d bytes, got %d", size.size, n)
				}

				f.Close()
			}
		})
	}
}

// benchmarkMixedWorkload simulates a realistic mixed workload
func benchmarkMixedWorkload(b *testing.B, storeType framework.StoreType) {
	srv, mount := setupBenchmarkServer(b, storeType)
	defer mount.Cleanup()
	defer srv.Stop()

	// Pre-create some files
	testDir := filepath.Join(mount.MountPoint(), "mixed")
	os.Mkdir(testDir, 0755)
	for i := 0; i < 50; i++ {
		f, _ := os.Create(filepath.Join(testDir, fmt.Sprintf("existing_%d", i)))
		f.Write(make([]byte, 4096))
		f.Close()
	}

	data := make([]byte, 4096)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		op := i % 5
		switch op {
		case 0: // Create and write
			f, _ := os.Create(filepath.Join(testDir, fmt.Sprintf("new_%d", i)))
			f.Write(data)
			f.Close()

		case 1: // Read existing
			idx := i % 50
			f, _ := os.Open(filepath.Join(testDir, fmt.Sprintf("existing_%d", idx)))
			f.Read(data)
			f.Close()

		case 2: // Stat
			idx := i % 50
			os.Stat(filepath.Join(testDir, fmt.Sprintf("existing_%d", idx)))

		case 3: // Readdir
			os.ReadDir(testDir)

		case 4: // Rename
			if i > 100 {
				oldIdx := (i - 100) % 50
				oldFile := filepath.Join(testDir, fmt.Sprintf("new_%d", oldIdx))
				newFile := filepath.Join(testDir, fmt.Sprintf("renamed_%d", oldIdx))
				os.Rename(oldFile, newFile)
			}
		}
	}
}

// benchmarkMetadataOperations measures metadata-heavy operations
func benchmarkMetadataOperations(b *testing.B, storeType framework.StoreType) {
	srv, mount := setupBenchmarkServer(b, storeType)
	defer mount.Cleanup()
	defer srv.Stop()

	b.Run("Chmod", func(b *testing.B) {
		// Create test file
		testFile := filepath.Join(mount.MountPoint(), "chmod_test")
		f, _ := os.Create(testFile)
		f.Close()

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			mode := os.FileMode(0644)
			if i%2 == 0 {
				mode = 0755
			}
			err := os.Chmod(testFile, mode)
			if err != nil {
				b.Fatalf("Failed to chmod: %v", err)
			}
		}
	})

	b.Run("Chtimes", func(b *testing.B) {
		// Create test file
		testFile := filepath.Join(mount.MountPoint(), "chtimes_test")
		f, _ := os.Create(testFile)
		f.Close()

		now := time.Now()

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			err := os.Chtimes(testFile, now, now)
			if err != nil {
				b.Fatalf("Failed to chtimes: %v", err)
			}
		}
	})
}

// Helper function to setup benchmark server and mount
func setupBenchmarkServer(b *testing.B, storeType framework.StoreType) (*framework.TestServer, *framework.NFSMount) {
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

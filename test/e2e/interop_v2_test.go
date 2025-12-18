//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
)

// TestProtocolInteropV2 tests NFS and SMB protocol interoperability.
// Files created via one protocol should be accessible via the other.
func TestProtocolInteropV2(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		if !tc.HasNFS() || !tc.HasSMB() {
			t.Skip("Both NFS and SMB required for interop tests")
		}

		// Test NFS write, SMB read
		t.Run("NFSWriteSMBRead", func(t *testing.T) {
			filePath := "nfs_to_smb_v2.txt"
			content := []byte("Written via NFS, read via SMB")

			// Write via NFS
			framework.WriteFile(t, tc.NFSPath(filePath), content)

			// Small delay to ensure metadata is synchronized
			time.Sleep(100 * time.Millisecond)

			// Read via SMB
			readContent := framework.ReadFile(t, tc.SMBPath(filePath))
			if !bytes.Equal(readContent, content) {
				t.Error("Content written via NFS not readable via SMB")
			}
		})

		// Test SMB write, NFS read
		t.Run("SMBWriteNFSRead", func(t *testing.T) {
			filePath := "smb_to_nfs_v2.txt"
			content := []byte("Written via SMB, read via NFS")

			// Write via SMB
			framework.WriteFile(t, tc.SMBPath(filePath), content)

			// Small delay to ensure metadata is synchronized
			time.Sleep(100 * time.Millisecond)

			// Read via NFS
			readContent := framework.ReadFile(t, tc.NFSPath(filePath))
			if !bytes.Equal(readContent, content) {
				t.Error("Content written via SMB not readable via NFS")
			}
		})

		// Test cross-protocol folder operations
		t.Run("CrossProtocolFolders", func(t *testing.T) {
			// Create folder via NFS (framework.CreateDir handles chmod for cross-protocol access)
			folderPath := "cross_protocol_folder_v2"
			framework.CreateDir(t, tc.NFSPath(folderPath))

			time.Sleep(100 * time.Millisecond)

			// Verify visible via SMB
			if !framework.DirExists(tc.SMBPath(folderPath)) {
				t.Error("Folder created via NFS not visible via SMB")
			}

			// Create file via SMB inside the folder
			filePath := filepath.Join(folderPath, "smb_file.txt")
			framework.WriteFile(t, tc.SMBPath(filePath), []byte("SMB content"))

			time.Sleep(100 * time.Millisecond)

			// Read file via NFS
			content := framework.ReadFile(t, tc.NFSPath(filePath))
			if !bytes.Equal(content, []byte("SMB content")) {
				t.Error("File created via SMB not readable via NFS")
			}
		})

		// Test metadata consistency across protocols
		t.Run("MetadataConsistency", func(t *testing.T) {
			filePath := "metadata_test_v2.txt"
			content := []byte("Testing metadata consistency")

			// Create via NFS
			framework.WriteFile(t, tc.NFSPath(filePath), content)

			time.Sleep(100 * time.Millisecond)

			// Check size via SMB
			nfsInfo := framework.GetFileInfo(t, tc.NFSPath(filePath))
			smbInfo := framework.GetFileInfo(t, tc.SMBPath(filePath))

			if nfsInfo.Size != smbInfo.Size {
				t.Errorf("Size mismatch: NFS=%d, SMB=%d", nfsInfo.Size, smbInfo.Size)
			}
		})

		// Test delete operations across protocols
		t.Run("CrossProtocolDelete", func(t *testing.T) {
			// Create via NFS
			filePath := "delete_cross_v2.txt"
			framework.WriteFile(t, tc.NFSPath(filePath), []byte("to be deleted"))

			time.Sleep(200 * time.Millisecond)

			// Delete via SMB
			if err := os.Remove(tc.SMBPath(filePath)); err != nil {
				t.Fatalf("Failed to delete via SMB: %v", err)
			}

			// Allow more time for cross-protocol cache invalidation
			time.Sleep(500 * time.Millisecond)

			// Verify deleted via NFS
			if framework.FileExists(tc.NFSPath(filePath)) {
				t.Error("File deleted via SMB still visible via NFS")
			}
		})
	})
}

// TestSimultaneousProtocolAccessV2 tests concurrent access from both protocols.
func TestSimultaneousProtocolAccessV2(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		if !tc.HasNFS() || !tc.HasSMB() {
			t.Skip("Both NFS and SMB required for simultaneous access tests")
		}

		// Create directory (framework.CreateDir handles chmod for cross-protocol access)
		basePath := "simultaneous_v2"
		framework.CreateDir(t, tc.NFSPath(basePath))

		var wg sync.WaitGroup
		errors := make(chan error, 20)

		// NFS writes
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				filePath := filepath.Join(basePath, fmt.Sprintf("nfs_%d.txt", idx))
				content := []byte(fmt.Sprintf("NFS content %d", idx))
				if err := os.WriteFile(tc.NFSPath(filePath), content, 0644); err != nil {
					errors <- fmt.Errorf("NFS write %d: %w", idx, err)
				}
			}(i)
		}

		// SMB writes
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				filePath := filepath.Join(basePath, fmt.Sprintf("smb_%d.txt", idx))
				content := []byte(fmt.Sprintf("SMB content %d", idx))
				if err := os.WriteFile(tc.SMBPath(filePath), content, 0644); err != nil {
					errors <- fmt.Errorf("SMB write %d: %w", idx, err)
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		for err := range errors {
			t.Errorf("Simultaneous access error: %v", err)
		}

		// Allow metadata to settle
		time.Sleep(200 * time.Millisecond)

		// Verify all files exist via both protocols
		for i := 0; i < 10; i++ {
			nfsFile := filepath.Join(basePath, fmt.Sprintf("nfs_%d.txt", i))
			smbFile := filepath.Join(basePath, fmt.Sprintf("smb_%d.txt", i))

			// Check NFS files via SMB
			if !framework.FileExists(tc.SMBPath(nfsFile)) {
				t.Errorf("NFS file %d not visible via SMB", i)
			}

			// Check SMB files via NFS
			if !framework.FileExists(tc.NFSPath(smbFile)) {
				t.Errorf("SMB file %d not visible via NFS", i)
			}
		}
	})
}

// TestLargeFileInteropV2 tests large file operations across protocols.
func TestLargeFileInteropV2(t *testing.T) {
	framework.SkipIfShort(t, "large file interop test")

	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		if !tc.HasNFS() || !tc.HasSMB() {
			t.Skip("Both NFS and SMB required for large file interop tests")
		}

		// Test 1MB file
		t.Run("1MB_NFSWriteSMBRead", func(t *testing.T) {
			filePath := "large_nfs_to_smb_v2.bin"
			checksum := framework.WriteRandomFile(t, tc.NFSPath(filePath), 1*1024*1024)

			time.Sleep(200 * time.Millisecond)

			framework.VerifyFileChecksum(t, tc.SMBPath(filePath), checksum)
		})

		t.Run("1MB_SMBWriteNFSRead", func(t *testing.T) {
			filePath := "large_smb_to_nfs_v2.bin"
			checksum := framework.WriteRandomFile(t, tc.SMBPath(filePath), 1*1024*1024)

			time.Sleep(200 * time.Millisecond)

			framework.VerifyFileChecksum(t, tc.NFSPath(filePath), checksum)
		})
	})
}

// TestStoreInteropV2 verifies that all store combinations work correctly.
func TestStoreInteropV2(t *testing.T) {
	framework.RunOnAllConfigs(t, func(t *testing.T, tc *framework.TestContext) {
		// Basic operations that exercise both metadata and content stores
		t.Run("MetadataContentIntegration", func(t *testing.T) {
			// Create a file (tests metadata create + content write)
			filePath := tc.Path("integration_test_v2.txt")
			content := []byte("Testing metadata and content store integration")
			framework.WriteFile(t, filePath, content)

			// Read file (tests metadata lookup + content read)
			readContent := framework.ReadFile(t, filePath)
			if !bytes.Equal(readContent, content) {
				t.Error("Content mismatch - store integration issue")
			}

			// Stat file (tests metadata read)
			info := framework.GetFileInfo(t, filePath)
			if info.Size != int64(len(content)) {
				t.Errorf("Size mismatch: expected %d, got %d", len(content), info.Size)
			}

			// Delete file (tests metadata delete + content cleanup)
			framework.RemoveAll(t, filePath)
			if framework.FileExists(filePath) {
				t.Error("File should be deleted")
			}
		})

		// Directory operations (metadata only)
		t.Run("MetadataDirectoryOps", func(t *testing.T) {
			dirPath := tc.Path("metadata_dir_v2")
			framework.CreateDir(t, dirPath)

			if !framework.DirExists(dirPath) {
				t.Error("Directory should exist")
			}

			// Create subdirectory
			subDir := filepath.Join(dirPath, "subdir")
			framework.CreateDir(t, subDir)

			// List directory
			entries := framework.ListDir(t, dirPath)
			if len(entries) != 1 {
				t.Errorf("Expected 1 entry, got %d", len(entries))
			}
		})

		// Large content (content store stress)
		t.Run("ContentStoreStress", func(t *testing.T) {
			if testing.Short() {
				t.Skip("Skipping content store stress in short mode")
			}

			filePath := tc.Path("stress_test_v2.bin")
			checksum := framework.WriteRandomFile(t, filePath, 5*1024*1024) // 5MB

			framework.VerifyFileChecksum(t, filePath, checksum)
		})
	})
}

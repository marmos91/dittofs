//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestInteropNFSWriteSMBRead tests writing a file via NFS and reading it via SMB
func TestInteropNFSWriteSMBRead(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Write file via NFS
			fileName := "nfs_to_smb.txt"
			content := []byte("Written via NFS, read via SMB!")
			nfsPath := tc.NFSPath(fileName)

			err := os.WriteFile(nfsPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to write via NFS: %v", err)
			}

			// Read via SMB
			smbPath := tc.SMBPath(fileName)
			readContent, err := os.ReadFile(smbPath)
			if err != nil {
				t.Fatalf("Failed to read via SMB: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch: NFS wrote %q, SMB read %q", content, readContent)
			}
		})
	}
}

// TestInteropSMBWriteNFSRead tests writing a file via SMB and reading it via NFS
func TestInteropSMBWriteNFSRead(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Write file via SMB
			fileName := "smb_to_nfs.txt"
			content := []byte("Written via SMB, read via NFS!")
			smbPath := tc.SMBPath(fileName)

			err := os.WriteFile(smbPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to write via SMB: %v", err)
			}

			// Read via NFS
			nfsPath := tc.NFSPath(fileName)
			readContent, err := os.ReadFile(nfsPath)
			if err != nil {
				t.Fatalf("Failed to read via NFS: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch: SMB wrote %q, NFS read %q", content, readContent)
			}
		})
	}
}

// TestInteropNFSCreateFolderSMBList tests creating folders via NFS and listing via SMB
func TestInteropNFSCreateFolderSMBList(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create folders via NFS
			for i := 0; i < 5; i++ {
				folderName := fmt.Sprintf("nfs_folder_%d", i)
				nfsPath := tc.NFSPath(folderName)
				err := os.Mkdir(nfsPath, 0755)
				if err != nil {
					t.Fatalf("Failed to create folder via NFS: %v", err)
				}
			}

			// List via SMB
			entries, err := os.ReadDir(tc.SMBMountPath)
			if err != nil {
				t.Fatalf("Failed to read directory via SMB: %v", err)
			}

			if len(entries) < 5 {
				t.Errorf("Expected at least 5 folders, got %d", len(entries))
			}

			// Verify folder names
			expectedFolders := make(map[string]bool)
			for i := 0; i < 5; i++ {
				expectedFolders[fmt.Sprintf("nfs_folder_%d", i)] = false
			}

			for _, entry := range entries {
				if _, ok := expectedFolders[entry.Name()]; ok {
					expectedFolders[entry.Name()] = true
				}
			}

			for name, found := range expectedFolders {
				if !found {
					t.Errorf("Folder %s created via NFS not found via SMB", name)
				}
			}
		})
	}
}

// TestInteropSMBCreateFilesNFSDelete tests creating files via SMB and deleting via NFS
func TestInteropSMBCreateFilesNFSDelete(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create files via SMB
			for i := 0; i < 5; i++ {
				fileName := fmt.Sprintf("smb_file_%d.txt", i)
				smbPath := tc.SMBPath(fileName)
				err := os.WriteFile(smbPath, []byte(fmt.Sprintf("Content %d", i)), 0644)
				if err != nil {
					t.Fatalf("Failed to create file via SMB: %v", err)
				}
			}

			// Delete via NFS
			for i := 0; i < 5; i++ {
				fileName := fmt.Sprintf("smb_file_%d.txt", i)
				nfsPath := tc.NFSPath(fileName)
				err := os.Remove(nfsPath)
				if err != nil {
					t.Fatalf("Failed to delete file via NFS: %v", err)
				}
			}

			// Verify files are gone (check from SMB side)
			entries, err := os.ReadDir(tc.SMBMountPath)
			if err != nil {
				t.Fatalf("Failed to read directory via SMB: %v", err)
			}

			for _, entry := range entries {
				if filepath.Ext(entry.Name()) == ".txt" {
					t.Errorf("File %s should have been deleted", entry.Name())
				}
			}
		})
	}
}

// TestInteropLargeFileTransfer tests transferring a large file between protocols
func TestInteropLargeFileTransfer(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create 1MB random data
			size := 1024 * 1024 // 1MB
			content := make([]byte, size)
			_, err := rand.Read(content)
			if err != nil {
				t.Fatalf("Failed to generate random data: %v", err)
			}

			// Write via NFS
			nfsPath := tc.NFSPath("large_file.bin")
			err = os.WriteFile(nfsPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to write large file via NFS: %v", err)
			}

			// Read via SMB
			smbPath := tc.SMBPath("large_file.bin")
			readContent, err := os.ReadFile(smbPath)
			if err != nil {
				t.Fatalf("Failed to read large file via SMB: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch for large file")
			}

			// Now write via SMB, read via NFS
			smbPath2 := tc.SMBPath("large_file2.bin")
			err = os.WriteFile(smbPath2, content, 0644)
			if err != nil {
				t.Fatalf("Failed to write large file via SMB: %v", err)
			}

			nfsPath2 := tc.NFSPath("large_file2.bin")
			readContent2, err := os.ReadFile(nfsPath2)
			if err != nil {
				t.Fatalf("Failed to read large file via NFS: %v", err)
			}

			if !bytes.Equal(content, readContent2) {
				t.Errorf("Content mismatch for large file (SMB→NFS)")
			}
		})
	}
}

// TestInteropConcurrentAccess tests concurrent access from both protocols
func TestInteropConcurrentAccess(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create shared folder via NFS
			sharedFolder := tc.NFSPath("shared")
			err := os.Mkdir(sharedFolder, 0755)
			if err != nil {
				t.Fatalf("Failed to create shared folder: %v", err)
			}

			// Concurrently create files from both protocols
			done := make(chan error, 2)

			// NFS writer
			go func() {
				for i := 0; i < 10; i++ {
					path := tc.NFSPath(fmt.Sprintf("shared/nfs_%d.txt", i))
					if err := os.WriteFile(path, []byte(fmt.Sprintf("NFS content %d", i)), 0644); err != nil {
						done <- fmt.Errorf("NFS write failed: %w", err)
						return
					}
				}
				done <- nil
			}()

			// SMB writer
			go func() {
				for i := 0; i < 10; i++ {
					path := tc.SMBPath(fmt.Sprintf("shared/smb_%d.txt", i))
					if err := os.WriteFile(path, []byte(fmt.Sprintf("SMB content %d", i)), 0644); err != nil {
						done <- fmt.Errorf("SMB write failed: %w", err)
						return
					}
				}
				done <- nil
			}()

			// Wait for both
			for i := 0; i < 2; i++ {
				if err := <-done; err != nil {
					t.Fatalf("Concurrent access failed: %v", err)
				}
			}

			// Verify all files exist (check from both sides)
			nfsEntries, err := os.ReadDir(tc.NFSPath("shared"))
			if err != nil {
				t.Fatalf("Failed to read directory via NFS: %v", err)
			}

			smbEntries, err := os.ReadDir(tc.SMBPath("shared"))
			if err != nil {
				t.Fatalf("Failed to read directory via SMB: %v", err)
			}

			// Both should see 20 files (10 from NFS + 10 from SMB)
			if len(nfsEntries) != 20 {
				t.Errorf("NFS sees %d files, expected 20", len(nfsEntries))
			}
			if len(smbEntries) != 20 {
				t.Errorf("SMB sees %d files, expected 20", len(smbEntries))
			}
		})
	}
}

// TestInteropMetadataConsistency tests that metadata is consistent between protocols
func TestInteropMetadataConsistency(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create file via NFS
			content := []byte("Test content for metadata")
			nfsPath := tc.NFSPath("metadata_test.txt")
			err := os.WriteFile(nfsPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to write file: %v", err)
			}

			// Get file info from both protocols
			nfsInfo, err := os.Stat(nfsPath)
			if err != nil {
				t.Fatalf("Failed to stat via NFS: %v", err)
			}

			smbPath := tc.SMBPath("metadata_test.txt")
			smbInfo, err := os.Stat(smbPath)
			if err != nil {
				t.Fatalf("Failed to stat via SMB: %v", err)
			}

			// Size should match
			if nfsInfo.Size() != smbInfo.Size() {
				t.Errorf("Size mismatch: NFS=%d, SMB=%d", nfsInfo.Size(), smbInfo.Size())
			}

			// IsDir should match
			if nfsInfo.IsDir() != smbInfo.IsDir() {
				t.Errorf("IsDir mismatch: NFS=%v, SMB=%v", nfsInfo.IsDir(), smbInfo.IsDir())
			}
		})
	}
}

// TestInteropNestedFolderStructure tests creating nested folder structure via one protocol
// and navigating it via the other
func TestInteropNestedFolderStructure(t *testing.T) {
	configs := LocalConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create nested structure via NFS
			deepPath := tc.NFSPath("level1/level2/level3/level4")
			err := os.MkdirAll(deepPath, 0755)
			if err != nil {
				t.Fatalf("Failed to create nested folders via NFS: %v", err)
			}

			// Create a file at the deepest level
			deepFile := filepath.Join(deepPath, "deep_file.txt")
			err = os.WriteFile(deepFile, []byte("Deep content"), 0644)
			if err != nil {
				t.Fatalf("Failed to create deep file: %v", err)
			}

			// Navigate and read via SMB
			smbDeepFile := tc.SMBPath("level1/level2/level3/level4/deep_file.txt")
			content, err := os.ReadFile(smbDeepFile)
			if err != nil {
				t.Fatalf("Failed to read deep file via SMB: %v", err)
			}

			if string(content) != "Deep content" {
				t.Errorf("Content mismatch: expected 'Deep content', got %q", content)
			}
		})
	}
}

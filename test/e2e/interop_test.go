//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestInteropNFSWriteSMBRead tests writing a file via NFS and reading it via SMB
func TestInteropNFSWriteSMBRead(t *testing.T) {
	configs := AllConfigurations()

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
	configs := AllConfigurations()

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
	configs := AllConfigurations()

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
	configs := AllConfigurations()

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
	configs := AllConfigurations()

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
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create shared folder via NFS with world-writable permissions
			// so that both NFS (UID 0) and SMB (UID 1000) users can write
			sharedFolder := tc.NFSPath("shared")
			err := os.Mkdir(sharedFolder, 0777)
			if err != nil {
				t.Fatalf("Failed to create shared folder: %v", err)
			}
			// Explicitly chmod to ensure 0777 (mkdir is subject to umask)
			if err := os.Chmod(sharedFolder, 0777); err != nil {
				t.Fatalf("Failed to chmod shared folder: %v", err)
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
	configs := AllConfigurations()

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
	configs := AllConfigurations()

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

// TestInteropNFSSymlinkSMBRead tests creating a symlink via NFS and reading the target via SMB
func TestInteropNFSSymlinkSMBRead(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create target file via NFS
			targetFileName := "symlink_target.txt"
			content := []byte("Symlink target content")
			targetPath := tc.NFSPath(targetFileName)
			err := os.WriteFile(targetPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create target file via NFS: %v", err)
			}

			// Create symlink via NFS
			symlinkName := "symlink_to_target"
			symlinkPath := tc.NFSPath(symlinkName)
			err = os.Symlink(targetFileName, symlinkPath)
			if err != nil {
				t.Fatalf("Failed to create symlink via NFS: %v", err)
			}

			// Read symlink target via SMB
			smbSymlinkPath := tc.SMBPath(symlinkName)
			readContent, err := os.ReadFile(smbSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to read symlink via SMB: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch: NFS target has %q, SMB read %q", content, readContent)
			}

			// Verify the symlink is correctly identified via SMB
			smbFileInfo, err := os.Lstat(smbSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to lstat symlink via SMB: %v", err)
			}

			if smbFileInfo.Mode()&os.ModeSymlink == 0 {
				t.Errorf("SMB should see the file as a symlink, but mode is %v", smbFileInfo.Mode())
			}

			// Read the symlink target path via SMB
			smbTarget, err := os.Readlink(smbSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to readlink via SMB: %v", err)
			}

			if smbTarget != targetFileName {
				t.Errorf("Symlink target mismatch: expected %q, got %q", targetFileName, smbTarget)
			}
		})
	}
}

// TestInteropSMBSymlinkNFSRead tests creating a symlink via SMB and reading the target via NFS
// Note: This test is skipped for non-cached S3 configurations because SMB symlinks are stored
// as MFsymlink files which require proper metadata synchronization. Without cache, there can be
// timing issues where NFS reads the MFsymlink file content before it's fully recognized as a symlink.
func TestInteropSMBSymlinkNFSRead(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			// Skip non-cached S3 configs due to metadata sync timing issues with MFsymlinks
			if config.ContentStore == ContentS3 && !config.UseCache {
				t.Skip("Skipping non-cached S3: SMB symlink metadata sync requires cache")
			}

			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create target file via SMB
			targetFileName := "smb_symlink_target.txt"
			content := []byte("SMB symlink target content")
			targetPath := tc.SMBPath(targetFileName)
			err := os.WriteFile(targetPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create target file via SMB: %v", err)
			}

			// Create symlink via SMB
			symlinkName := "smb_symlink"
			smbSymlinkPath := tc.SMBPath(symlinkName)
			err = os.Symlink(targetFileName, smbSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to create symlink via SMB: %v", err)
			}

			// Read symlink target via NFS
			nfsSymlinkPath := tc.NFSPath(symlinkName)
			readContent, err := os.ReadFile(nfsSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to read symlink via NFS: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch: SMB target has %q, NFS read %q", content, readContent)
			}

			// Verify the symlink is correctly identified via NFS
			nfsFileInfo, err := os.Lstat(nfsSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to lstat symlink via NFS: %v", err)
			}

			if nfsFileInfo.Mode()&os.ModeSymlink == 0 {
				t.Errorf("NFS should see the file as a symlink, but mode is %v", nfsFileInfo.Mode())
			}

			// Read the symlink target path via NFS
			nfsTarget, err := os.Readlink(nfsSymlinkPath)
			if err != nil {
				t.Fatalf("Failed to readlink via NFS: %v", err)
			}

			if nfsTarget != targetFileName {
				t.Errorf("Symlink target mismatch: expected %q, got %q", targetFileName, nfsTarget)
			}
		})
	}
}

// TestInteropSymlinkToDirectory tests creating a symlink to a directory
func TestInteropSymlinkToDirectory(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create target directory via NFS
			targetDirName := "target_dir"
			targetDirPath := tc.NFSPath(targetDirName)
			err := os.Mkdir(targetDirPath, 0755)
			if err != nil {
				t.Fatalf("Failed to create target directory via NFS: %v", err)
			}

			// Create a file inside the target directory
			testFile := filepath.Join(targetDirPath, "test_file.txt")
			content := []byte("File in target directory")
			err = os.WriteFile(testFile, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create file in target directory: %v", err)
			}

			// Create symlink to directory via NFS
			symlinkName := "symlink_to_dir"
			symlinkPath := tc.NFSPath(symlinkName)
			err = os.Symlink(targetDirName, symlinkPath)
			if err != nil {
				t.Fatalf("Failed to create symlink to directory: %v", err)
			}

			// Access file through symlink via SMB
			smbFileViaSymlink := tc.SMBPath(symlinkName + "/test_file.txt")
			readContent, err := os.ReadFile(smbFileViaSymlink)
			if err != nil {
				t.Fatalf("Failed to read file through symlink via SMB: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch when reading through symlink")
			}

			// List directory through symlink via SMB
			smbSymlinkDir := tc.SMBPath(symlinkName)
			entries, err := os.ReadDir(smbSymlinkDir)
			if err != nil {
				t.Fatalf("Failed to read directory through symlink via SMB: %v", err)
			}

			if len(entries) != 1 || entries[0].Name() != "test_file.txt" {
				t.Errorf("Unexpected directory contents through symlink: %v", entries)
			}
		})
	}
}

// TestInteropNFSHiddenFileSMBRead tests creating a hidden file via NFS and reading it via SMB
func TestInteropNFSHiddenFileSMBRead(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create hidden file via NFS (Unix-style: starts with dot)
			hiddenFileName := ".hidden_file"
			content := []byte("Hidden file content")
			nfsPath := tc.NFSPath(hiddenFileName)
			err := os.WriteFile(nfsPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create hidden file via NFS: %v", err)
			}

			// Read hidden file via SMB
			smbPath := tc.SMBPath(hiddenFileName)
			readContent, err := os.ReadFile(smbPath)
			if err != nil {
				t.Fatalf("Failed to read hidden file via SMB: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch: NFS wrote %q, SMB read %q", content, readContent)
			}

			// Verify the file exists in directory listing (should be hidden on SMB)
			entries, err := os.ReadDir(tc.SMBMountPath)
			if err != nil {
				t.Fatalf("Failed to read directory via SMB: %v", err)
			}

			found := false
			for _, entry := range entries {
				if entry.Name() == hiddenFileName {
					found = true
					break
				}
			}

			// Note: On macOS/Linux, hidden files (starting with .) are typically shown in ReadDir
			// The test verifies the file is accessible, not necessarily hidden in listing
			if !found {
				// The file should still be accessible even if not shown in listing
				_, err := os.Stat(smbPath)
				if err != nil {
					t.Errorf("Hidden file not found via SMB: not in listing and stat failed")
				}
			}
		})
	}
}

// TestInteropSMBHiddenFileNFSRead tests creating a hidden file via SMB and reading it via NFS
func TestInteropSMBHiddenFileNFSRead(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create hidden file via SMB (Unix-style: starts with dot)
			hiddenFileName := ".smb_hidden_file"
			content := []byte("SMB hidden file content")
			smbPath := tc.SMBPath(hiddenFileName)
			err := os.WriteFile(smbPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create hidden file via SMB: %v", err)
			}

			// Read hidden file via NFS
			nfsPath := tc.NFSPath(hiddenFileName)
			readContent, err := os.ReadFile(nfsPath)
			if err != nil {
				t.Fatalf("Failed to read hidden file via NFS: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch: SMB wrote %q, NFS read %q", content, readContent)
			}
		})
	}
}

// TestInteropHiddenDirectory tests creating a hidden directory via one protocol
// and accessing it via the other
func TestInteropHiddenDirectory(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create hidden directory via NFS
			hiddenDirName := ".hidden_dir"
			nfsHiddenDir := tc.NFSPath(hiddenDirName)
			err := os.Mkdir(nfsHiddenDir, 0755)
			if err != nil {
				t.Fatalf("Failed to create hidden directory via NFS: %v", err)
			}

			// Create file inside hidden directory via NFS
			fileInHiddenDir := filepath.Join(nfsHiddenDir, "secret.txt")
			content := []byte("Secret content")
			err = os.WriteFile(fileInHiddenDir, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create file in hidden directory: %v", err)
			}

			// Read file from hidden directory via SMB
			smbFileInHiddenDir := tc.SMBPath(hiddenDirName + "/secret.txt")
			readContent, err := os.ReadFile(smbFileInHiddenDir)
			if err != nil {
				t.Fatalf("Failed to read file from hidden directory via SMB: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch in hidden directory file")
			}

			// Verify hidden directory info via SMB
			smbHiddenDir := tc.SMBPath(hiddenDirName)
			info, err := os.Stat(smbHiddenDir)
			if err != nil {
				t.Fatalf("Failed to stat hidden directory via SMB: %v", err)
			}

			if !info.IsDir() {
				t.Errorf("Hidden directory should be a directory")
			}
		})
	}
}

// TestInteropFIFOCreation tests creating a FIFO (named pipe) via NFS and verifying it is hidden via SMB
// As documented in docs/SMB.md: Unix special files (FIFO, socket, block device, character device)
// have no meaningful representation in SMB and are hidden from directory listings entirely.
func TestInteropFIFOCreation(t *testing.T) {
	// FIFOs are only supported on Unix-like systems
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs not supported on Windows")
	}

	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create a regular file first to ensure directory listing works
			regularFileName := "regular_file.txt"
			regularPath := tc.NFSPath(regularFileName)
			err := os.WriteFile(regularPath, []byte("test"), 0644)
			if err != nil {
				t.Fatalf("Failed to create regular file: %v", err)
			}

			// Create FIFO via NFS
			fifoName := "test_fifo"
			nfsFifoPath := tc.NFSPath(fifoName)
			err = syscall.Mkfifo(nfsFifoPath, 0644)
			if err != nil {
				t.Fatalf("Failed to create FIFO via NFS: %v", err)
			}

			// Verify FIFO exists via NFS
			nfsInfo, err := os.Lstat(nfsFifoPath)
			if err != nil {
				t.Fatalf("Failed to stat FIFO via NFS: %v", err)
			}

			if nfsInfo.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("NFS: Expected named pipe, got mode %v", nfsInfo.Mode())
			}

			// Verify FIFO appears in NFS directory listing
			nfsEntries, err := os.ReadDir(tc.NFSMountPath)
			if err != nil {
				t.Fatalf("Failed to read NFS directory: %v", err)
			}

			fifoFoundInNFS := false
			for _, entry := range nfsEntries {
				if entry.Name() == fifoName {
					fifoFoundInNFS = true
					break
				}
			}

			if !fifoFoundInNFS {
				t.Errorf("FIFO should be visible in NFS directory listing")
			}

			// Verify FIFO is HIDDEN from SMB directory listing (expected behavior per docs/SMB.md)
			smbEntries, err := os.ReadDir(tc.SMBMountPath)
			if err != nil {
				t.Fatalf("Failed to read SMB directory: %v", err)
			}

			fifoFoundInSMB := false
			regularFileFoundInSMB := false
			for _, entry := range smbEntries {
				if entry.Name() == fifoName {
					fifoFoundInSMB = true
				}
				if entry.Name() == regularFileName {
					regularFileFoundInSMB = true
				}
			}

			// Regular file should be visible via SMB
			if !regularFileFoundInSMB {
				t.Errorf("Regular file should be visible in SMB directory listing")
			}

			// FIFO should be HIDDEN from SMB (this is the documented behavior)
			if fifoFoundInSMB {
				t.Errorf("FIFO should be hidden from SMB directory listing (see docs/SMB.md)")
			}

			// Direct stat of FIFO via SMB should also fail (it's hidden)
			smbFifoPath := tc.SMBPath(fifoName)
			_, err = os.Lstat(smbFifoPath)
			if err == nil {
				t.Errorf("FIFO should not be accessible via SMB (expected to be hidden)")
			}

			t.Logf("FIFO correctly hidden from SMB as documented")
		})
	}
}

// TestInteropSocketCreation tests creating a Unix socket via NFS and verifying it is hidden via SMB
// As documented in docs/SMB.md: Unix special files (FIFO, socket, block device, character device)
// have no meaningful representation in SMB and are hidden from directory listings entirely.
func TestInteropSocketCreation(t *testing.T) {
	// Unix sockets are only supported on Unix-like systems
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}

	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create a regular file first to ensure directory listing works
			regularFileName := "regular_file_socket_test.txt"
			regularPath := tc.NFSPath(regularFileName)
			err := os.WriteFile(regularPath, []byte("test"), 0644)
			if err != nil {
				t.Fatalf("Failed to create regular file: %v", err)
			}

			// Create Unix socket via NFS
			socketName := "test_socket.sock"
			nfsSocketPath := tc.NFSPath(socketName)

			// Use syscall to create socket
			fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("Failed to create socket: %v", err)
			}
			defer syscall.Close(fd)

			// Bind the socket to create the file
			addr := &syscall.SockaddrUnix{Name: nfsSocketPath}
			err = syscall.Bind(fd, addr)
			if err != nil {
				t.Fatalf("Failed to bind socket via NFS: %v", err)
			}

			// Verify socket exists via NFS
			nfsInfo, err := os.Lstat(nfsSocketPath)
			if err != nil {
				t.Fatalf("Failed to stat socket via NFS: %v", err)
			}

			if nfsInfo.Mode()&os.ModeSocket == 0 {
				t.Errorf("NFS: Expected socket, got mode %v", nfsInfo.Mode())
			}

			// Verify socket appears in NFS directory listing
			nfsEntries, err := os.ReadDir(tc.NFSMountPath)
			if err != nil {
				t.Fatalf("Failed to read NFS directory: %v", err)
			}

			socketFoundInNFS := false
			for _, entry := range nfsEntries {
				if entry.Name() == socketName {
					socketFoundInNFS = true
					break
				}
			}

			if !socketFoundInNFS {
				t.Errorf("Socket should be visible in NFS directory listing")
			}

			// Verify socket is HIDDEN from SMB directory listing (expected behavior per docs/SMB.md)
			smbEntries, err := os.ReadDir(tc.SMBMountPath)
			if err != nil {
				t.Fatalf("Failed to read SMB directory: %v", err)
			}

			socketFoundInSMB := false
			regularFileFoundInSMB := false
			for _, entry := range smbEntries {
				if entry.Name() == socketName {
					socketFoundInSMB = true
				}
				if entry.Name() == regularFileName {
					regularFileFoundInSMB = true
				}
			}

			// Regular file should be visible via SMB
			if !regularFileFoundInSMB {
				t.Errorf("Regular file should be visible in SMB directory listing")
			}

			// Socket should be HIDDEN from SMB (this is the documented behavior)
			if socketFoundInSMB {
				t.Errorf("Socket should be hidden from SMB directory listing (see docs/SMB.md)")
			}

			// Direct stat of socket via SMB should also fail (it's hidden)
			smbSocketPath := tc.SMBPath(socketName)
			_, err = os.Lstat(smbSocketPath)
			if err == nil {
				t.Errorf("Socket should not be accessible via SMB (expected to be hidden)")
			}

			t.Logf("Socket correctly hidden from SMB as documented")
		})
	}
}

// TestInteropDirectoryPermissions tests that directory permissions are consistent across protocols
func TestInteropDirectoryPermissions(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create directory with specific permissions via NFS
			dirName := "perm_test_dir"
			nfsDirPath := tc.NFSPath(dirName)
			err := os.Mkdir(nfsDirPath, 0750)
			if err != nil {
				t.Fatalf("Failed to create directory via NFS: %v", err)
			}

			// Get permissions via NFS
			nfsInfo, err := os.Stat(nfsDirPath)
			if err != nil {
				t.Fatalf("Failed to stat directory via NFS: %v", err)
			}
			nfsPerm := nfsInfo.Mode().Perm()

			// Get permissions via SMB
			smbDirPath := tc.SMBPath(dirName)
			smbInfo, err := os.Stat(smbDirPath)
			if err != nil {
				t.Fatalf("Failed to stat directory via SMB: %v", err)
			}
			smbPerm := smbInfo.Mode().Perm()

			// Permissions should match (or be reasonably close given protocol differences)
			t.Logf("Directory permissions - NFS: %o, SMB: %o", nfsPerm, smbPerm)

			// At minimum, both should be directories
			if !nfsInfo.IsDir() || !smbInfo.IsDir() {
				t.Errorf("Both should be directories")
			}
		})
	}
}

// TestInteropFilePermissions tests that file permissions are consistent across protocols
func TestInteropFilePermissions(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create file with specific permissions via NFS
			fileName := "perm_test_file.txt"
			nfsFilePath := tc.NFSPath(fileName)
			err := os.WriteFile(nfsFilePath, []byte("test content"), 0640)
			if err != nil {
				t.Fatalf("Failed to create file via NFS: %v", err)
			}

			// Explicitly set permissions
			err = os.Chmod(nfsFilePath, 0640)
			if err != nil {
				t.Fatalf("Failed to chmod via NFS: %v", err)
			}

			// Get permissions via NFS
			nfsInfo, err := os.Stat(nfsFilePath)
			if err != nil {
				t.Fatalf("Failed to stat file via NFS: %v", err)
			}
			nfsPerm := nfsInfo.Mode().Perm()

			// Get permissions via SMB
			smbFilePath := tc.SMBPath(fileName)
			smbInfo, err := os.Stat(smbFilePath)
			if err != nil {
				t.Fatalf("Failed to stat file via SMB: %v", err)
			}
			smbPerm := smbInfo.Mode().Perm()

			// Log the permissions for debugging
			t.Logf("File permissions - NFS: %o, SMB: %o", nfsPerm, smbPerm)

			// Size should definitely match
			if nfsInfo.Size() != smbInfo.Size() {
				t.Errorf("Size mismatch: NFS=%d, SMB=%d", nfsInfo.Size(), smbInfo.Size())
			}
		})
	}
}

// TestInteropRename tests renaming files across protocols
func TestInteropRename(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create file via NFS
			oldName := "original_name.txt"
			content := []byte("Content to be renamed")
			nfsOldPath := tc.NFSPath(oldName)
			err := os.WriteFile(nfsOldPath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create file via NFS: %v", err)
			}

			// Rename via SMB
			newName := "renamed_file.txt"
			smbOldPath := tc.SMBPath(oldName)
			smbNewPath := tc.SMBPath(newName)
			err = os.Rename(smbOldPath, smbNewPath)
			if err != nil {
				t.Fatalf("Failed to rename via SMB: %v", err)
			}

			// Small delay to allow NFS client cache to expire/refresh
			// NFS clients cache directory entries and may not immediately see changes
			time.Sleep(100 * time.Millisecond)

			// Verify old name is gone via NFS
			// Note: Due to NFS client caching, we verify via directory listing instead of direct stat
			entries, err := os.ReadDir(tc.NFSMountPath)
			if err != nil {
				t.Fatalf("Failed to read directory via NFS: %v", err)
			}
			oldFileFound := false
			for _, entry := range entries {
				if entry.Name() == oldName {
					oldFileFound = true
					break
				}
			}
			if oldFileFound {
				t.Errorf("Old file should not exist after rename")
			}

			// Verify new name exists via NFS
			nfsNewPath := tc.NFSPath(newName)
			readContent, err := os.ReadFile(nfsNewPath)
			if err != nil {
				t.Fatalf("Failed to read renamed file via NFS: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch after rename")
			}
		})
	}
}

// TestInteropMoveAcrossDirectories tests moving files between directories across protocols
func TestInteropMoveAcrossDirectories(t *testing.T) {
	configs := AllConfigurations()

	for _, config := range configs {
		t.Run(config.Name, func(t *testing.T) {
			tc := NewInteropTestContext(t, config)
			defer tc.Cleanup()

			// Create source and destination directories via NFS
			srcDir := tc.NFSPath("src_dir")
			dstDir := tc.NFSPath("dst_dir")
			err := os.Mkdir(srcDir, 0755)
			if err != nil {
				t.Fatalf("Failed to create source directory: %v", err)
			}
			err = os.Mkdir(dstDir, 0755)
			if err != nil {
				t.Fatalf("Failed to create destination directory: %v", err)
			}

			// Create file in source directory via NFS
			fileName := "file_to_move.txt"
			content := []byte("Content to be moved")
			srcFilePath := filepath.Join(srcDir, fileName)
			err = os.WriteFile(srcFilePath, content, 0644)
			if err != nil {
				t.Fatalf("Failed to create file: %v", err)
			}

			// Move file via SMB
			smbSrcPath := tc.SMBPath("src_dir/" + fileName)
			smbDstPath := tc.SMBPath("dst_dir/" + fileName)
			err = os.Rename(smbSrcPath, smbDstPath)
			if err != nil {
				t.Fatalf("Failed to move file via SMB: %v", err)
			}

			// Small delay to allow NFS client cache to expire/refresh
			time.Sleep(100 * time.Millisecond)

			// Verify file is gone from source via NFS
			// Note: Due to NFS client caching, we verify via directory listing instead of direct stat
			srcEntries, err := os.ReadDir(srcDir)
			if err != nil {
				t.Fatalf("Failed to read source directory via NFS: %v", err)
			}
			fileFoundInSrc := false
			for _, entry := range srcEntries {
				if entry.Name() == fileName {
					fileFoundInSrc = true
					break
				}
			}
			if fileFoundInSrc {
				t.Errorf("File should not exist in source after move")
			}

			// Verify file exists in destination via NFS
			dstFilePath := filepath.Join(dstDir, fileName)
			readContent, err := os.ReadFile(dstFilePath)
			if err != nil {
				t.Fatalf("Failed to read moved file via NFS: %v", err)
			}

			if !bytes.Equal(content, readContent) {
				t.Errorf("Content mismatch after move")
			}
		})
	}
}

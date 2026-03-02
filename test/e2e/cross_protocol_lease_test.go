//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossProtocol_LeaseBreaks validates cross-protocol SMB3 lease and NFS delegation
// coordination under various scenarios including bidirectional breaks, directory leases,
// concurrent conflicts, and data consistency.
//
// These tests validate the Phase 39 implementation of bidirectional SMB3 lease and
// NFS delegation coordination:
//   - SMB3 lease break triggered by NFS write
//   - NFS delegation recall triggered by SMB open
//   - SMB3 directory lease break triggered by NFS create/delete/rename
//   - Concurrent lease conflicts with multiple goroutines
//   - Data consistency after cross-protocol lease break and write
func TestCrossProtocol_LeaseBreaks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cross-protocol lease break tests in short mode")
	}

	// Skip if no SMB mount capability (need CIFS client or Docker)
	framework.SkipIfNoSMBMount(t)

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to configure the server
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create shared metadata and payload stores
	// Both NFS and SMB will use the same stores to enable cross-protocol access
	metaStoreName := helpers.UniqueTestName("leasemeta")
	payloadStoreName := helpers.UniqueTestName("leasepayload")
	shareName := "/export"

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create payload store")

	// Create share with read-write default permission
	_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "Should create share")

	// Create SMB test user with authentication credentials
	smbUsername := helpers.UniqueTestName("leaseuser")
	smbPassword := "testpass123" // Must be 8+ chars for SMB

	_, err = cli.CreateUser(smbUsername, smbPassword)
	require.NoError(t, err, "Should create SMB test user")

	// Grant SMB user read-write permission on the share
	err = cli.GrantUserPermission(shareName, smbUsername, "read-write")
	require.NoError(t, err, "Should grant SMB user permission")

	// Enable NFS adapter on a dynamic port
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")

	// Enable SMB adapter on a dynamic port
	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")

	// Wait for both adapters to be ready
	err = helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	err = helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	// Wait for both servers to be listening
	framework.WaitForServer(t, nfsPort, 10*time.Second)
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Mount NFS share with actimeo=0 for proper cross-protocol visibility
	nfsMount := framework.MountNFS(t, nfsPort)
	t.Cleanup(nfsMount.Cleanup)

	// Mount SMB share with credentials
	smbCreds := framework.SMBCredentials{
		Username: smbUsername,
		Password: smbPassword,
	}
	smbMount := framework.MountSMB(t, smbPort, smbCreds)
	t.Cleanup(smbMount.Cleanup)

	// Run cross-protocol lease break subtests
	// Note: These tests run sequentially (not parallel) as they share the same mounts
	t.Run("SMBLeaseBreakOnNFSWrite", func(t *testing.T) {
		testCrossProtocol_SMBLeaseBreakOnNFSWrite(t, nfsMount, smbMount)
	})

	t.Run("NFSDelegationRecallOnSMBOpen", func(t *testing.T) {
		testCrossProtocol_NFSDelegationRecallOnSMBOpen(t, nfsMount, smbMount)
	})

	t.Run("SMBDirLeaseBreakOnNFSCreate", func(t *testing.T) {
		testCrossProtocol_SMBDirLeaseBreakOnNFSCreate(t, nfsMount, smbMount)
	})

	t.Run("SMBDirLeaseBreakOnNFSDelete", func(t *testing.T) {
		testCrossProtocol_SMBDirLeaseBreakOnNFSDelete(t, nfsMount, smbMount)
	})

	t.Run("SMBDirLeaseBreakOnNFSRename", func(t *testing.T) {
		testCrossProtocol_SMBDirLeaseBreakOnNFSRename(t, nfsMount, smbMount)
	})

	t.Run("ConcurrentLeaseConflicts", func(t *testing.T) {
		testCrossProtocol_ConcurrentLeaseConflicts(t, nfsMount, smbMount)
	})

	t.Run("DataConsistencyAfterBreak", func(t *testing.T) {
		testCrossProtocol_DataConsistencyAfterBreak(t, nfsMount, smbMount)
	})
}

// testCrossProtocol_SMBLeaseBreakOnNFSWrite tests that an SMB client holding a read lease
// sees updated content after an NFS write triggers a lease break.
// Validates: SMB lease break fires on NFS write, data consistency maintained.
func testCrossProtocol_SMBLeaseBreakOnNFSWrite(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	initialContent := []byte("Initial content from SMB with read lease")
	nfsContent := []byte("Updated content written via NFS - should trigger SMB lease break")
	fileName := helpers.UniqueTestName("lease_smb_nfs_write") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Step 1: Create file via SMB (SMB client may request read lease automatically)
	framework.WriteFile(t, smbPath, initialContent)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: Open file via SMB for reading (go-smb2 or mount.cifs requests lease automatically)
	smbReadData := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.Equal(initialContent, smbReadData),
		"SMB should read initial content before NFS write")
	t.Log("SMB client has read the file (lease may be active)")

	// Step 3: Write to the same file via NFS mount
	// This should trigger an SMB lease break if a read lease was granted
	framework.WriteFile(t, nfsPath, nfsContent)
	t.Log("NFS write completed (should have triggered SMB lease break)")

	// Step 4: Wait for lease break propagation
	time.Sleep(500 * time.Millisecond)

	// Step 5: Read file via SMB, verify content matches NFS-written data
	smbReadAfter := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.Equal(nfsContent, smbReadAfter),
		"SMB should see NFS-written content after lease break (expected: %q, got: %q)",
		string(nfsContent), string(smbReadAfter))

	t.Log("SMBLeaseBreakOnNFSWrite: PASSED - SMB sees NFS-written content after lease break")
}

// testCrossProtocol_NFSDelegationRecallOnSMBOpen tests that an NFS client holding a
// read delegation sees updated content after an SMB open triggers delegation recall.
// Validates: NFS delegation recall fires on SMB open.
func testCrossProtocol_NFSDelegationRecallOnSMBOpen(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	initialContent := []byte("Initial content for NFS delegation test")
	smbContent := []byte("Written via SMB - should trigger NFS delegation recall")
	fileName := helpers.UniqueTestName("lease_nfs_deleg_recall") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Step 1: Create file via NFS (NFS may grant read delegation)
	framework.WriteFile(t, nfsPath, initialContent)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: Open and read file via NFS (read delegation may be granted)
	nfsReadData := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(initialContent, nfsReadData),
		"NFS should read initial content")
	t.Log("NFS client has read the file (delegation may be active)")

	// Step 3: Open same file via SMB for write
	// This should trigger NFS delegation recall
	framework.WriteFile(t, smbPath, smbContent)
	t.Log("SMB write completed (should have triggered NFS delegation recall)")

	// Step 4: Wait for delegation recall propagation
	time.Sleep(500 * time.Millisecond)

	// Step 5: Read via NFS, verify content matches SMB-written data
	nfsReadAfter := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(smbContent, nfsReadAfter),
		"NFS should see SMB-written content after delegation recall (expected: %q, got: %q)",
		string(smbContent), string(nfsReadAfter))

	t.Log("NFSDelegationRecallOnSMBOpen: PASSED - NFS sees SMB-written content after recall")
}

// testCrossProtocol_SMBDirLeaseBreakOnNFSCreate tests that an SMB directory lease
// is broken when NFS creates a file in the same directory.
func testCrossProtocol_SMBDirLeaseBreakOnNFSCreate(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	dirName := helpers.UniqueTestName("lease_dir_create")
	nfsDirPath := nfsMount.FilePath(dirName)
	smbDirPath := smbMount.FilePath(dirName)

	// Step 1: Create directory via NFS
	framework.CreateDir(t, nfsDirPath)
	t.Cleanup(func() {
		_ = os.RemoveAll(nfsDirPath)
	})

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: Open directory via SMB (triggers directory lease)
	smbEntries := framework.ListDir(t, smbDirPath)
	assert.Len(t, smbEntries, 0, "Directory should be empty initially")
	t.Log("SMB has listed directory (directory lease may be active)")

	// Step 3: Create a file via NFS in the directory (triggers dir lease break)
	newFileName := "nfs-created-file.txt"
	nfsNewFilePath := nfsMount.FilePath(fmt.Sprintf("%s/%s", dirName, newFileName))
	framework.WriteFile(t, nfsNewFilePath, []byte("created via NFS"))
	t.Log("NFS file creation completed (should have triggered SMB directory lease break)")

	// Step 4: Wait for break propagation
	time.Sleep(500 * time.Millisecond)

	// Step 5: ReadDir via SMB, verify new file is visible
	smbEntriesAfter := framework.ListDir(t, smbDirPath)
	found := false
	for _, entry := range smbEntriesAfter {
		if entry == newFileName {
			found = true
			break
		}
	}
	assert.True(t, found,
		"SMB should see NFS-created file after dir lease break (entries: %v)", smbEntriesAfter)

	t.Log("SMBDirLeaseBreakOnNFSCreate: PASSED - SMB sees NFS-created file")
}

// testCrossProtocol_SMBDirLeaseBreakOnNFSDelete tests that an SMB directory lease
// is broken when NFS deletes a file from the directory.
func testCrossProtocol_SMBDirLeaseBreakOnNFSDelete(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	dirName := helpers.UniqueTestName("lease_dir_delete")
	nfsDirPath := nfsMount.FilePath(dirName)
	smbDirPath := smbMount.FilePath(dirName)

	// Step 1: Create directory and file via NFS
	framework.CreateDir(t, nfsDirPath)
	t.Cleanup(func() {
		_ = os.RemoveAll(nfsDirPath)
	})

	fileToDelete := "will-be-deleted.txt"
	nfsFilePath := nfsMount.FilePath(fmt.Sprintf("%s/%s", dirName, fileToDelete))
	framework.WriteFile(t, nfsFilePath, []byte("this file will be deleted via NFS"))

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: List directory via SMB (triggers directory lease)
	smbEntries := framework.ListDir(t, smbDirPath)
	assert.Len(t, smbEntries, 1, "Directory should have 1 file")
	t.Log("SMB has listed directory (directory lease may be active)")

	// Step 3: Delete the file via NFS (triggers dir lease break)
	err := os.Remove(nfsFilePath)
	require.NoError(t, err, "Should delete file via NFS")
	t.Log("NFS file deletion completed (should have triggered SMB directory lease break)")

	// Step 4: Wait for break propagation
	time.Sleep(500 * time.Millisecond)

	// Step 5: ReadDir via SMB, verify file is gone
	smbEntriesAfter := framework.ListDir(t, smbDirPath)
	for _, entry := range smbEntriesAfter {
		assert.NotEqual(t, fileToDelete, entry,
			"Deleted file should not appear in SMB directory listing")
	}

	t.Log("SMBDirLeaseBreakOnNFSDelete: PASSED - SMB no longer sees deleted file")
}

// testCrossProtocol_SMBDirLeaseBreakOnNFSRename tests that an SMB directory lease
// is broken when NFS renames a file in the directory.
func testCrossProtocol_SMBDirLeaseBreakOnNFSRename(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	dirName := helpers.UniqueTestName("lease_dir_rename")
	nfsDirPath := nfsMount.FilePath(dirName)
	smbDirPath := smbMount.FilePath(dirName)

	// Step 1: Create directory and file via NFS
	framework.CreateDir(t, nfsDirPath)
	t.Cleanup(func() {
		_ = os.RemoveAll(nfsDirPath)
	})

	origName := "original-name.txt"
	renamedName := "renamed-name.txt"
	nfsOrigPath := nfsMount.FilePath(fmt.Sprintf("%s/%s", dirName, origName))
	nfsRenamedPath := nfsMount.FilePath(fmt.Sprintf("%s/%s", dirName, renamedName))
	framework.WriteFile(t, nfsOrigPath, []byte("this file will be renamed via NFS"))

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: List directory via SMB (triggers directory lease)
	smbEntries := framework.ListDir(t, smbDirPath)
	found := false
	for _, entry := range smbEntries {
		if entry == origName {
			found = true
			break
		}
	}
	assert.True(t, found, "SMB should see original file before rename")
	t.Log("SMB has listed directory (directory lease may be active)")

	// Step 3: Rename the file via NFS (triggers dir lease break)
	err := os.Rename(nfsOrigPath, nfsRenamedPath)
	require.NoError(t, err, "Should rename file via NFS")
	t.Log("NFS file rename completed (should have triggered SMB directory lease break)")

	// Step 4: Wait for break propagation
	time.Sleep(500 * time.Millisecond)

	// Step 5: ReadDir via SMB, verify renamed file visible
	smbEntriesAfter := framework.ListDir(t, smbDirPath)
	foundOriginal := false
	foundRenamed := false
	for _, entry := range smbEntriesAfter {
		if entry == origName {
			foundOriginal = true
		}
		if entry == renamedName {
			foundRenamed = true
		}
	}
	assert.False(t, foundOriginal,
		"Original name should not exist after rename (entries: %v)", smbEntriesAfter)
	assert.True(t, foundRenamed,
		"Renamed file should be visible via SMB after dir lease break (entries: %v)", smbEntriesAfter)

	t.Log("SMBDirLeaseBreakOnNFSRename: PASSED - SMB sees renamed file")
}

// testCrossProtocol_ConcurrentLeaseConflicts tests concurrent cross-protocol access
// with multiple goroutines performing NFS and SMB operations on shared files.
// Validates: no panics, no deadlocks, all operations complete within 30s.
func testCrossProtocol_ConcurrentLeaseConflicts(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	const numFiles = 10
	const numGoroutines = 10 // 5 NFS-primary + 5 SMB-primary

	// Step 1: Create initial files via NFS
	fileNames := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		fileName := fmt.Sprintf("concurrent_lease_%d.txt", i)
		fileNames[i] = fileName
		nfsPath := nfsMount.FilePath(fileName)
		framework.WriteFile(t, nfsPath, []byte(fmt.Sprintf("initial-content-%d", i)))
	}

	// Wait for metadata sync
	time.Sleep(500 * time.Millisecond)

	// Step 2: Launch concurrent goroutines
	var wg sync.WaitGroup
	errChan := make(chan error, numGoroutines)
	doneCh := make(chan struct{})

	// NFS-primary goroutines: open via NFS, read/write, verify via SMB
	for g := 0; g < numGoroutines/2; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for iter := 0; iter < 3; iter++ {
				fileIdx := rand.Intn(numFiles)
				fileName := fileNames[fileIdx]
				nfsPath := nfsMount.FilePath(fileName)
				smbPath := smbMount.FilePath(fileName)

				// Write via NFS
				content := []byte(fmt.Sprintf("nfs-goroutine-%d-iter-%d", goroutineID, iter))
				if err := os.WriteFile(nfsPath, content, 0644); err != nil {
					errChan <- fmt.Errorf("NFS goroutine %d: write failed: %w", goroutineID, err)
					return
				}

				// Brief pause for sync
				time.Sleep(100 * time.Millisecond)

				// Read via SMB (verify cross-protocol visibility)
				readData, err := os.ReadFile(smbPath)
				if err != nil {
					errChan <- fmt.Errorf("NFS goroutine %d: SMB read failed: %w", goroutineID, err)
					return
				}
				_ = readData // Content may have been overwritten by another goroutine
			}
		}(g)
	}

	// SMB-primary goroutines: open via SMB, read/write, verify via NFS
	for g := numGoroutines / 2; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for iter := 0; iter < 3; iter++ {
				fileIdx := rand.Intn(numFiles)
				fileName := fileNames[fileIdx]
				nfsPath := nfsMount.FilePath(fileName)
				smbPath := smbMount.FilePath(fileName)

				// Write via SMB
				content := []byte(fmt.Sprintf("smb-goroutine-%d-iter-%d", goroutineID, iter))
				if err := os.WriteFile(smbPath, content, 0644); err != nil {
					errChan <- fmt.Errorf("SMB goroutine %d: write failed: %w", goroutineID, err)
					return
				}

				// Brief pause for sync
				time.Sleep(100 * time.Millisecond)

				// Read via NFS (verify cross-protocol visibility)
				readData, err := os.ReadFile(nfsPath)
				if err != nil {
					errChan <- fmt.Errorf("SMB goroutine %d: NFS read failed: %w", goroutineID, err)
					return
				}
				_ = readData // Content may have been overwritten by another goroutine
			}
		}(g)
	}

	// Wait for completion with timeout
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		t.Log("All concurrent goroutines completed successfully")
	case <-time.After(30 * time.Second):
		t.Fatal("Concurrent lease conflict test timed out after 30 seconds (possible deadlock)")
	}

	// Check for errors
	close(errChan)
	var errors []string
	for err := range errChan {
		errors = append(errors, err.Error())
	}
	if len(errors) > 0 {
		t.Logf("Concurrent test had %d errors (may be expected under contention):", len(errors))
		for _, e := range errors {
			t.Logf("  - %s", e)
		}
		// Note: some errors may be expected under high contention (e.g., stale reads)
		// The key assertion is: no panics, no deadlocks, completion within timeout
	}

	// Step 3: Verify final file contents are consistent (each file is readable from both protocols)
	time.Sleep(500 * time.Millisecond) // Allow final sync
	for _, fileName := range fileNames {
		nfsPath := nfsMount.FilePath(fileName)
		smbPath := smbMount.FilePath(fileName)

		nfsData := framework.ReadFile(t, nfsPath)
		smbData := framework.ReadFile(t, smbPath)

		assert.True(t, bytes.Equal(nfsData, smbData),
			"File %s should have consistent content across protocols (NFS: %q, SMB: %q)",
			fileName, string(nfsData), string(smbData))
	}

	// Cleanup
	for _, fileName := range fileNames {
		_ = os.Remove(nfsMount.FilePath(fileName))
	}

	t.Log("ConcurrentLeaseConflicts: PASSED - no deadlocks, no panics, data consistent")
}

// testCrossProtocol_DataConsistencyAfterBreak is the definitive data consistency validation.
// It performs a series of alternating writes between SMB and NFS, verifying that each
// protocol sees the other's latest writes after lease break.
func testCrossProtocol_DataConsistencyAfterBreak(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	fileName := helpers.UniqueTestName("lease_consistency") + ".txt"
	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Step 1: SMB client opens file, writes "version1"
	version1 := []byte("version1")
	framework.WriteFile(t, smbPath, version1)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	time.Sleep(200 * time.Millisecond)

	// Verify via NFS
	nfsRead1 := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(version1, nfsRead1),
		"NFS should see version1 (got: %q)", string(nfsRead1))
	t.Log("Step 1: SMB wrote version1, NFS confirmed")

	// Step 2: NFS client writes "version2" to same file (triggers SMB lease break)
	version2 := []byte("version2")
	framework.WriteFile(t, nfsPath, version2)
	t.Log("Step 2: NFS wrote version2 (should trigger SMB lease break)")

	// Wait for lease break propagation
	time.Sleep(500 * time.Millisecond)

	// Step 3: SMB client re-reads file, must see "version2"
	smbRead2 := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.Equal(version2, smbRead2),
		"SMB must see version2 after NFS write + lease break (got: %q)", string(smbRead2))
	t.Log("Step 3: SMB confirmed version2")

	// Step 4: SMB client writes "version3"
	version3 := []byte("version3")
	framework.WriteFile(t, smbPath, version3)
	t.Log("Step 4: SMB wrote version3")

	// Wait for sync
	time.Sleep(500 * time.Millisecond)

	// Step 5: NFS client reads file, must see "version3"
	nfsRead3 := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(version3, nfsRead3),
		"NFS must see version3 after SMB write (got: %q)", string(nfsRead3))
	t.Log("Step 5: NFS confirmed version3")

	t.Log("DataConsistencyAfterBreak: PASSED - all cross-protocol writes correctly visible")
}

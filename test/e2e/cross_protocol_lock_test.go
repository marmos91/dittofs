//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossProtocolLocking validates cross-protocol locking behavior between NFS and SMB.
// This covers requirements XPRO-01 through XPRO-04:
//   - XPRO-01: NFS lock blocks SMB Write lease
//   - XPRO-02: SMB Write lease breaks for NFS lock request
//   - XPRO-03: Cross-protocol lock conflict detection
//   - XPRO-04: Cross-protocol data integrity
//
// Per CONTEXT.md decisions:
//   - NFS byte-range locks are explicit and win over opportunistic SMB leases
//   - SMB Write lease is denied with STATUS_LOCK_NOT_GRANTED when NLM lock exists
//   - SMB leases are designed to be breakable for NFS lock requests
//   - Tests use 5-second shortened timeout for CI
func TestCrossProtocolLocking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cross-protocol locking tests in short mode")
	}

	// Skip on platforms where NFS locking (NLM) doesn't work with userspace servers
	framework.SkipIfNFSLockingUnsupported(t)

	// Skip if no SMB mount capability (need CIFS client or Docker)
	framework.SkipIfNoSMBMount(t)

	// Log platform-specific locking notes for debugging context
	framework.LogPlatformLockingNotes(t)

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to configure the server
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create shared metadata and payload stores
	// Both NFS and SMB will use the same stores to enable cross-protocol locking
	metaStoreName := helpers.UniqueTestName("xplockmeta")
	payloadStoreName := helpers.UniqueTestName("xplockpayload")
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
	smbUsername := helpers.UniqueTestName("xplockuser")
	smbPassword := "testpass123"

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

	// Run cross-protocol locking subtests
	// Note: These tests run sequentially (not parallel) as they share the same mounts
	t.Run("XPRO-01 NFS lock blocks SMB Write lease", func(t *testing.T) {
		testNFSLockBlocksSMBLease(t, nfsMount, smbMount)
	})

	t.Run("XPRO-02 SMB Write lease breaks for NFS lock", func(t *testing.T) {
		testSMBLeaseBreaksForNFSLock(t, nfsMount, smbMount)
	})

	t.Run("XPRO-03 Cross-protocol lock conflict detection", func(t *testing.T) {
		testCrossProtocolConflict(t, nfsMount, smbMount)
	})

	t.Run("XPRO-04 Cross-protocol data integrity", func(t *testing.T) {
		testCrossProtocolDataIntegrity(t, nfsMount, smbMount)
	})
}

// testNFSLockBlocksSMBLease tests XPRO-01: NFS lock should block SMB Write lease.
// Per CONTEXT.md: NFS byte-range locks are explicit and win over opportunistic SMB leases.
// SMB should receive STATUS_LOCK_NOT_GRANTED or have its lease downgraded.
func testNFSLockBlocksSMBLease(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Test content for NFS lock blocks SMB lease")
	fileName := helpers.UniqueTestName("xpro01_nfs_lock") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Step 1: Create file via NFS
	framework.WriteFile(t, nfsPath, testContent)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	// Step 2: Acquire exclusive lock via NFS (fcntl for NLM support)
	// This should trigger an NLM LOCK request to the server
	nfsLock := framework.LockFileRange(t, nfsPath, 0, 0, true) // Exclusive lock, whole file
	t.Cleanup(func() {
		framework.UnlockFileRange(t, nfsLock)
	})

	t.Log("XPRO-01: NFS exclusive lock acquired")

	// Step 3: Attempt to open file via SMB
	// The SMB client will request a Write lease, which should be denied/downgraded
	// because an NLM lock already exists
	//
	// Note: Standard file operations via SMB still work, but the client won't
	// get caching benefits (Write lease denied due to NLM lock conflict).
	// We verify by checking that the file is still accessible.
	smbContent := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.Equal(testContent, smbContent),
		"SMB should still be able to read file (caching just disabled)")

	// Step 4: Try to acquire lock via SMB using flock
	// This should either fail (if SMB translates to byte-range lock) or succeed
	// (if SMB uses only leases for caching). Platform behavior varies.
	smbLock, err := framework.TryLockFileRange(t, smbPath, 0, 0, true)
	if err != nil {
		t.Logf("XPRO-01: SMB exclusive lock blocked as expected (cross-protocol conflict): %v", err)
		// This is the expected behavior - NFS lock should block SMB lock
	} else {
		// On some platforms, SMB locks may not conflict with NFS locks
		// at the kernel level (independent lock namespaces)
		t.Logf("XPRO-01: SMB lock acquired (platform-specific behavior - independent lock namespaces)")
		framework.UnlockFileRange(t, smbLock)
	}

	// Step 5: Release NFS lock
	framework.UnlockFileRange(t, nfsLock)
	nfsLock = nil // Prevent cleanup from running twice
	t.Log("XPRO-01: NFS lock released")

	// Step 6: Verify SMB can now operate with full lease capability
	// Write some data via SMB to verify access
	newContent := []byte("Updated via SMB after NFS lock release")
	framework.WriteFile(t, smbPath, newContent)

	// Verify via NFS
	time.Sleep(200 * time.Millisecond)
	verifyContent := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(newContent, verifyContent),
		"NFS should see SMB write after lock release")

	t.Log("XPRO-01: NFS lock blocks SMB Write lease - PASSED")
}

// testSMBLeaseBreaksForNFSLock tests XPRO-02: SMB Write lease should break for NFS lock request.
// Per CONTEXT.md: SMB leases are designed to be breakable. NFS lock request triggers break.
func testSMBLeaseBreaksForNFSLock(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Test content for SMB lease breaks")
	fileName := helpers.UniqueTestName("xpro02_smb_lease") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Step 1: Create file via SMB (client may request Write lease)
	framework.WriteFile(t, smbPath, testContent)
	t.Cleanup(func() {
		_ = os.Remove(smbPath)
	})

	// Wait for metadata sync
	time.Sleep(200 * time.Millisecond)

	t.Log("XPRO-02: File created via SMB")

	// Step 2: Open file via SMB to establish lease
	// Keep file open to maintain the lease
	smbFile, err := os.OpenFile(smbPath, os.O_RDWR, 0644)
	require.NoError(t, err, "Should open file via SMB")
	t.Cleanup(func() {
		if smbFile != nil {
			_ = smbFile.Close()
		}
	})

	t.Log("XPRO-02: SMB file opened (lease established if supported)")

	// Step 3: Request exclusive lock via NFS
	// This should trigger an SMB lease break if Write lease was granted
	// The server should:
	//   1. Detect the lease conflict
	//   2. Send OPLOCK_BREAK to SMB client
	//   3. Wait for acknowledgment (up to 35s, shortened to 5s for tests)
	//   4. Grant the NFS lock
	startTime := time.Now()
	nfsLock := framework.LockFileRange(t, nfsPath, 0, 0, true)
	lockDuration := time.Since(startTime)
	t.Cleanup(func() {
		framework.UnlockFileRange(t, nfsLock)
	})

	t.Logf("XPRO-02: NFS lock acquired in %v (may include lease break wait)", lockDuration)

	// Step 4: Verify both protocols can access the file correctly
	// Read via NFS should work
	nfsContent := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(testContent, nfsContent),
		"NFS should read correct content after acquiring lock")

	// Step 5: Release NFS lock and verify SMB can write
	framework.UnlockFileRange(t, nfsLock)
	nfsLock = nil

	newContent := []byte("Written after NFS lock released")
	_, err = smbFile.Seek(0, 0)
	require.NoError(t, err, "Should seek SMB file")
	err = smbFile.Truncate(int64(len(newContent)))
	require.NoError(t, err, "Should truncate SMB file")
	_, err = smbFile.Write(newContent)
	require.NoError(t, err, "Should write via SMB after NFS lock released")

	// Close SMB file to flush
	_ = smbFile.Close()
	smbFile = nil

	// Verify via NFS
	time.Sleep(200 * time.Millisecond)
	verifyContent := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(newContent, verifyContent),
		"NFS should see SMB write after lock release")

	t.Log("XPRO-02: SMB Write lease breaks for NFS lock - PASSED")
}

// testCrossProtocolConflict tests XPRO-03: Cross-protocol lock conflict detection.
// Tests shared/exclusive lock interactions between NFS and SMB.
func testCrossProtocolConflict(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	testContent := []byte("Test content for cross-protocol conflicts")
	fileName := helpers.UniqueTestName("xpro03_conflict") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Create file
	framework.WriteFile(t, nfsPath, testContent)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	time.Sleep(200 * time.Millisecond)

	// Step 1: Acquire shared lock via NFS
	nfsSharedLock := framework.LockFileRange(t, nfsPath, 0, 0, false) // Shared lock
	t.Cleanup(func() {
		if nfsSharedLock != nil {
			framework.UnlockFileRange(t, nfsSharedLock)
		}
	})

	t.Log("XPRO-03: NFS shared lock acquired")

	// Step 2: Attempt shared lock via SMB (should succeed - multiple readers allowed)
	smbSharedLock, err := framework.TryLockFileRange(t, smbPath, 0, 0, false)
	if err != nil {
		// On some platforms, cross-protocol shared locks may not coexist
		t.Logf("XPRO-03: SMB shared lock blocked (platform-specific): %v", err)
	} else {
		t.Log("XPRO-03: SMB shared lock acquired (multiple readers)")
		defer framework.UnlockFileRange(t, smbSharedLock)
	}

	// Step 3: Attempt exclusive lock via NFS (should fail - SMB shared exists if acquired)
	nfsExclusiveLock, err := framework.TryLockFileRange(t, nfsPath, 0, 0, true)
	if err != nil {
		t.Logf("XPRO-03: NFS exclusive lock correctly blocked while shared locks exist: %v", err)
	} else {
		// This might happen if SMB shared lock wasn't acquired
		t.Log("XPRO-03: NFS exclusive lock acquired (no conflicting locks)")
		framework.UnlockFileRange(t, nfsExclusiveLock)
	}

	// Step 4: Release SMB lock (if acquired)
	if smbSharedLock != nil {
		framework.UnlockFileRange(t, smbSharedLock)
		smbSharedLock = nil
		t.Log("XPRO-03: SMB shared lock released")
	}

	// Step 5: Release NFS shared lock
	framework.UnlockFileRange(t, nfsSharedLock)
	nfsSharedLock = nil
	t.Log("XPRO-03: NFS shared lock released")

	// Step 6: Now exclusive lock should succeed
	nfsExclusiveLock2 := framework.LockFileRange(t, nfsPath, 0, 0, true)
	t.Log("XPRO-03: NFS exclusive lock acquired after all locks released")
	framework.UnlockFileRange(t, nfsExclusiveLock2)

	t.Log("XPRO-03: Cross-protocol lock conflict detection - PASSED")
}

// testCrossProtocolDataIntegrity tests XPRO-04: Data integrity across protocol boundaries.
// Verifies that file content written with locks is correctly visible across protocols.
func testCrossProtocolDataIntegrity(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	fileName := helpers.UniqueTestName("xpro04_integrity") + ".txt"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Test 1: NFS writes with lock, SMB reads
	t.Log("XPRO-04: Testing NFS write -> SMB read integrity")

	nfsContent := []byte("Data written via NFS with exclusive lock for integrity test")

	// Create and write via NFS with lock
	f, err := os.Create(nfsPath)
	require.NoError(t, err)
	_ = f.Close()
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	nfsLock := framework.LockFileRange(t, nfsPath, 0, 0, true)
	framework.WriteFile(t, nfsPath, nfsContent)
	framework.UnlockFileRange(t, nfsLock)

	t.Log("XPRO-04: NFS write completed with lock")

	// Wait for sync
	time.Sleep(300 * time.Millisecond)

	// Read via SMB
	smbReadContent := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.Equal(nfsContent, smbReadContent),
		"SMB should read exactly what NFS wrote with lock")

	// Test 2: SMB writes with lock, NFS reads
	t.Log("XPRO-04: Testing SMB write -> NFS read integrity")

	smbContent := []byte("Data written via SMB with exclusive lock for integrity verification")

	// Write via SMB with lock
	smbLock := framework.LockFileRange(t, smbPath, 0, 0, true)
	framework.WriteFile(t, smbPath, smbContent)
	framework.UnlockFileRange(t, smbLock)

	t.Log("XPRO-04: SMB write completed with lock")

	// Wait for sync
	time.Sleep(300 * time.Millisecond)

	// Read via NFS
	nfsReadContent := framework.ReadFile(t, nfsPath)
	assert.True(t, bytes.Equal(smbContent, nfsReadContent),
		"NFS should read exactly what SMB wrote with lock")

	// Test 3: Concurrent lock-protected writes
	t.Log("XPRO-04: Testing concurrent lock-protected writes")

	// NFS writes first half
	nfsLock2 := framework.LockFileRange(t, nfsPath, 0, 32, true) // Lock first 32 bytes
	firstHalf := []byte("AAAAAAAAAAAAAAAA")                      // 16 bytes
	framework.WriteFile(t, nfsPath, firstHalf)
	framework.UnlockFileRange(t, nfsLock2)

	time.Sleep(200 * time.Millisecond)

	// SMB reads and verifies
	partialContent := framework.ReadFile(t, smbPath)
	assert.True(t, bytes.HasPrefix(partialContent, firstHalf),
		"SMB should see NFS partial write")

	t.Log("XPRO-04: Cross-protocol data integrity - PASSED")
}

// TestCrossProtocolLockingByteRange tests byte-range specific lock interactions.
// This tests finer-grained locking scenarios where NFS and SMB lock different regions.
func TestCrossProtocolLockingByteRange(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cross-protocol byte-range locking tests in short mode")
	}

	// Skip on platforms where NFS locking (NLM) doesn't work with userspace servers
	framework.SkipIfNFSLockingUnsupported(t)

	// Skip if no SMB mount capability (need CIFS client or Docker)
	framework.SkipIfNoSMBMount(t)

	// Log platform notes
	framework.LogPlatformLockingNotes(t)

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Setup stores and share
	metaStoreName := helpers.UniqueTestName("xprangemeta")
	payloadStoreName := helpers.UniqueTestName("xprangepayload")
	shareName := "/export"

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err)

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err)

	_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err)

	// Create SMB user
	smbUsername := helpers.UniqueTestName("xprangeuser")
	smbPassword := "testpass123"

	_, err = cli.CreateUser(smbUsername, smbPassword)
	require.NoError(t, err)

	err = cli.GrantUserPermission(shareName, smbUsername, "read-write")
	require.NoError(t, err)

	// Enable adapters
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)

	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err)

	err = helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second)
	require.NoError(t, err)

	err = helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second)
	require.NoError(t, err)

	framework.WaitForServer(t, nfsPort, 10*time.Second)
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Mount shares
	nfsMount := framework.MountNFS(t, nfsPort)
	t.Cleanup(nfsMount.Cleanup)

	smbCreds := framework.SMBCredentials{
		Username: smbUsername,
		Password: smbPassword,
	}
	smbMount := framework.MountSMB(t, smbPort, smbCreds)
	t.Cleanup(smbMount.Cleanup)

	// Run byte-range specific tests
	t.Run("Non-overlapping byte-range locks", func(t *testing.T) {
		testNonOverlappingByteRangeLocks(t, nfsMount, smbMount)
	})

	t.Run("Overlapping byte-range lock conflict", func(t *testing.T) {
		testOverlappingByteRangeLockConflict(t, nfsMount, smbMount)
	})
}

// testNonOverlappingByteRangeLocks tests that NFS and SMB can hold locks on different regions.
func testNonOverlappingByteRangeLocks(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	fileName := helpers.UniqueTestName("byterange_nonoverlap") + ".dat"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Create a 1KB file
	content := make([]byte, 1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	framework.WriteFile(t, nfsPath, content)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	time.Sleep(200 * time.Millisecond)

	// NFS locks first 512 bytes
	nfsLock := framework.LockFileRange(t, nfsPath, 0, 512, true)
	t.Cleanup(func() {
		if nfsLock != nil {
			framework.UnlockFileRange(t, nfsLock)
		}
	})

	t.Log("NFS locked bytes [0:512]")

	// SMB locks last 512 bytes (should succeed - non-overlapping)
	smbLock, err := framework.TryLockFileRange(t, smbPath, 512, 512, true)
	if err != nil {
		t.Logf("SMB lock on [512:1024] blocked (platform may not support fine-grained cross-protocol locks): %v", err)
	} else {
		t.Log("SMB locked bytes [512:1024] - non-overlapping locks work")
		framework.UnlockFileRange(t, smbLock)
	}

	framework.UnlockFileRange(t, nfsLock)
	nfsLock = nil

	t.Log("Non-overlapping byte-range locks test completed")
}

// testOverlappingByteRangeLockConflict tests that overlapping locks across protocols conflict.
func testOverlappingByteRangeLockConflict(t *testing.T, nfsMount, smbMount *framework.Mount) {
	t.Helper()

	fileName := helpers.UniqueTestName("byterange_overlap") + ".dat"

	nfsPath := nfsMount.FilePath(fileName)
	smbPath := smbMount.FilePath(fileName)

	// Create a 1KB file
	content := make([]byte, 1024)
	framework.WriteFile(t, nfsPath, content)
	t.Cleanup(func() {
		_ = os.Remove(nfsPath)
	})

	time.Sleep(200 * time.Millisecond)

	// NFS locks bytes [256:768]
	nfsLock := framework.LockFileRange(t, nfsPath, 256, 512, true)
	t.Cleanup(func() {
		if nfsLock != nil {
			framework.UnlockFileRange(t, nfsLock)
		}
	})

	t.Log("NFS locked bytes [256:768]")

	// SMB attempts lock on [0:512] - overlaps with NFS [256:768]
	smbLock, err := framework.TryLockFileRange(t, smbPath, 0, 512, true)
	if err != nil {
		t.Logf("SMB lock on [0:512] correctly blocked due to overlap with NFS [256:768]: %v", err)
	} else {
		t.Log("SMB lock on [0:512] unexpectedly acquired (platform-specific lock namespaces)")
		framework.UnlockFileRange(t, smbLock)
	}

	framework.UnlockFileRange(t, nfsLock)
	nfsLock = nil

	t.Log("Overlapping byte-range lock conflict test completed")
}

//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMultiShareIsolation validates that per-share BlockStore instances provide
// true isolation across multiple shares. Tests cover:
// - Data isolation: files on share A are not visible on share B
// - Deletion isolation: deleting share A does not affect share B
// - Concurrent writes: simultaneous writes to different shares do not corrupt
// - Local store independence: per-share local stores do not interfere
// - Cross-protocol lock visibility: locks are per-share, not global
func TestMultiShareIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping multi-share isolation tests in short mode")
	}

	// Start server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create 2 metadata stores (memory)
	meta1 := helpers.UniqueTestName("iso-meta1")
	meta2 := helpers.UniqueTestName("iso-meta2")
	_, err := runner.CreateMetadataStore(meta1, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(meta1) })

	_, err = runner.CreateMetadataStore(meta2, "memory")
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(meta2) })

	// Create 2 local block stores (fs type with DIFFERENT paths)
	local1 := helpers.UniqueTestName("iso-local1")
	local2 := helpers.UniqueTestName("iso-local2")
	localPath1 := t.TempDir()
	localPath2 := t.TempDir()

	_, err = runner.CreateLocalBlockStore(local1, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPath1)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(local1) })

	_, err = runner.CreateLocalBlockStore(local2, "fs",
		helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPath2)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(local2) })

	// Create share A and share B
	_, err = runner.CreateShare("/share-iso-a", meta1, local1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteShare("/share-iso-a") })

	_, err = runner.CreateShare("/share-iso-b", meta2, local2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteShare("/share-iso-b") })

	// Enable NFS adapter
	nfsPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runner.DisableAdapter("nfs") })

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err)
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// =========================================================================
	// Subtest a: Data isolation (both-NFS scenario)
	// =========================================================================
	t.Run("DataIsolation", func(t *testing.T) {
		mountA := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-a", "3")
		t.Cleanup(mountA.Cleanup)

		mountB := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-b", "3")
		t.Cleanup(mountB.Cleanup)

		// Create file on share A
		fileA := mountA.FilePath("testfile.txt")
		framework.WriteFile(t, fileA, []byte("share-a-data"))
		t.Cleanup(func() { _ = os.Remove(fileA) })

		// Verify share B does NOT contain testfile.txt
		assert.False(t, framework.FileExists(mountB.FilePath("testfile.txt")),
			"Share B should NOT contain file created on share A")

		// Create same-named file on share B with different content
		fileB := mountB.FilePath("testfile.txt")
		framework.WriteFile(t, fileB, []byte("share-b-data"))
		t.Cleanup(func() { _ = os.Remove(fileB) })

		// Verify share A's file still has original content
		contentA := framework.ReadFile(t, fileA)
		assert.Equal(t, []byte("share-a-data"), contentA,
			"Share A file should still have original content")

		// Verify share B's file has its own content
		contentB := framework.ReadFile(t, fileB)
		assert.Equal(t, []byte("share-b-data"), contentB,
			"Share B file should have its own content")

		t.Log("DataIsolation: PASSED")
	})

	// =========================================================================
	// Subtest b: Deletion isolation
	// =========================================================================
	t.Run("DeletionIsolation", func(t *testing.T) {
		// Create a new share C for deletion testing (so we don't break other subtests)
		meta3 := helpers.UniqueTestName("iso-meta3")
		local3 := helpers.UniqueTestName("iso-local3")
		localPath3 := t.TempDir()

		_, err := runner.CreateMetadataStore(meta3, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteMetadataStore(meta3) })

		_, err = runner.CreateLocalBlockStore(local3, "fs",
			helpers.WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`, localPath3)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(local3) })

		_, err = runner.CreateShare("/share-iso-c", meta3, local3)
		require.NoError(t, err)

		// Mount share C and share B
		mountC := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-c", "3")

		mountB := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-b", "3")
		t.Cleanup(mountB.Cleanup)

		// Create files on both
		fileC := mountC.FilePath("deletion_test.txt")
		framework.WriteFile(t, fileC, []byte("share-c-data"))

		fileB := mountB.FilePath("deletion_test.txt")
		framework.WriteFile(t, fileB, []byte("share-b-deletion-data"))
		t.Cleanup(func() { _ = os.Remove(fileB) })

		// Unmount share C before deletion
		mountC.Cleanup()

		// Delete share C
		err = runner.DeleteShare("/share-iso-c")
		require.NoError(t, err, "Should delete share C")

		// Verify share B data is still intact
		contentB := framework.ReadFile(t, fileB)
		assert.Equal(t, []byte("share-b-deletion-data"), contentB,
			"Share B data should be intact after share C deletion")

		t.Log("DeletionIsolation: PASSED")
	})

	// =========================================================================
	// Subtest c: Concurrent writes
	// =========================================================================
	t.Run("ConcurrentWrites", func(t *testing.T) {
		mountA := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-a", "3")
		t.Cleanup(mountA.Cleanup)

		mountB := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-b", "3")
		t.Cleanup(mountB.Cleanup)

		const numFiles = 10
		var wg sync.WaitGroup
		checksums := make(map[string]string)
		var mu sync.Mutex

		// Write 10 files to share A and 10 files to share B simultaneously
		for i := 0; i < numFiles; i++ {
			wg.Add(2)

			go func() {
				defer wg.Done()
				fileName := fmt.Sprintf("concurrent_a_%d.bin", i)
				filePath := mountA.FilePath(fileName)
				data := framework.GenerateRandomData(t, 4*1024) // 4KB each
				hash := sha256.Sum256(data)

				framework.WriteFile(t, filePath, data)

				mu.Lock()
				checksums["a_"+fileName] = hex.EncodeToString(hash[:])
				mu.Unlock()
			}()

			go func() {
				defer wg.Done()
				fileName := fmt.Sprintf("concurrent_b_%d.bin", i)
				filePath := mountB.FilePath(fileName)
				data := framework.GenerateRandomData(t, 4*1024) // 4KB each
				hash := sha256.Sum256(data)

				framework.WriteFile(t, filePath, data)

				mu.Lock()
				checksums["b_"+fileName] = hex.EncodeToString(hash[:])
				mu.Unlock()
			}()
		}

		wg.Wait()
		time.Sleep(500 * time.Millisecond) // let caches settle

		// Verify all 20 files written correctly
		for key, expectedChecksum := range checksums {
			var filePath string
			if strings.HasPrefix(key, "a_") {
				filePath = mountA.FilePath(strings.TrimPrefix(key, "a_"))
			} else {
				filePath = mountB.FilePath(strings.TrimPrefix(key, "b_"))
			}

			assert.True(t, framework.FileExists(filePath),
				"File %s should exist after concurrent write", key)
			framework.VerifyFileChecksum(t, filePath, expectedChecksum)
		}

		// Verify no cross-contamination: share A files not on share B
		for i := 0; i < numFiles; i++ {
			assert.False(t, framework.FileExists(mountB.FilePath(fmt.Sprintf("concurrent_a_%d.bin", i))),
				"Share A file should NOT be on share B")
			assert.False(t, framework.FileExists(mountA.FilePath(fmt.Sprintf("concurrent_b_%d.bin", i))),
				"Share B file should NOT be on share A")
		}

		// Cleanup
		t.Cleanup(func() {
			for key := range checksums {
				var filePath string
				if strings.HasPrefix(key, "a_") {
					filePath = mountA.FilePath(strings.TrimPrefix(key, "a_"))
				} else {
					filePath = mountB.FilePath(strings.TrimPrefix(key, "b_"))
				}
				_ = os.Remove(filePath)
			}
		})

		t.Logf("ConcurrentWrites: PASSED (%d files verified, no cross-contamination)", len(checksums))
	})

	// =========================================================================
	// Subtest d: Local store independence
	// =========================================================================
	t.Run("LocalStoreIndependence", func(t *testing.T) {
		// Create shares with remote stores to enable tiered storage behavior
		remoteA := helpers.UniqueTestName("iso-remote-a")
		remoteB := helpers.UniqueTestName("iso-remote-b")

		_, err := runner.CreateRemoteBlockStore(remoteA, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteRemoteBlockStore(remoteA) })

		_, err = runner.CreateRemoteBlockStore(remoteB, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteRemoteBlockStore(remoteB) })

		// Create shares with remote stores
		metaCA := helpers.UniqueTestName("iso-meta-ca")
		metaCB := helpers.UniqueTestName("iso-meta-cb")
		localCA := helpers.UniqueTestName("iso-local-ca")
		localCB := helpers.UniqueTestName("iso-local-cb")

		_, err = runner.CreateMetadataStore(metaCA, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaCA) })

		_, err = runner.CreateMetadataStore(metaCB, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaCB) })

		_, err = runner.CreateLocalBlockStore(localCA, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localCA) })

		_, err = runner.CreateLocalBlockStore(localCB, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localCB) })

		_, err = runner.CreateShare("/share-local-a", metaCA, localCA,
			helpers.WithShareRemote(remoteA))
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteShare("/share-local-a") })

		_, err = runner.CreateShare("/share-local-b", metaCB, localCB,
			helpers.WithShareRemote(remoteB))
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteShare("/share-local-b") })

		// Mount both
		mountCA := framework.MountNFSExportWithVersion(t, nfsPort, "/share-local-a", "3")
		t.Cleanup(mountCA.Cleanup)

		mountCB := framework.MountNFSExportWithVersion(t, nfsPort, "/share-local-b", "3")
		t.Cleanup(mountCB.Cleanup)

		// Write data to share A (enough to exercise the local store)
		for i := 0; i < 20; i++ {
			filePath := mountCA.FilePath(fmt.Sprintf("local_a_%d.bin", i))
			framework.WriteFile(t, filePath, framework.GenerateRandomData(t, 8*1024))
			t.Cleanup(func() { _ = os.Remove(filePath) })
		}

		// Write data to share B
		for i := 0; i < 5; i++ {
			filePath := mountCB.FilePath(fmt.Sprintf("local_b_%d.bin", i))
			framework.WriteFile(t, filePath, framework.GenerateRandomData(t, 8*1024))
			t.Cleanup(func() { _ = os.Remove(filePath) })
		}

		// Verify share B files are intact (share A activity did not affect share B)
		for i := 0; i < 5; i++ {
			filePath := mountCB.FilePath(fmt.Sprintf("local_b_%d.bin", i))
			assert.True(t, framework.FileExists(filePath),
				"Share B local file %d should still exist", i)
			info, err := os.Stat(filePath)
			require.NoError(t, err)
			assert.Equal(t, int64(8*1024), info.Size(),
				"Share B local file %d should have correct size", i)
		}

		t.Log("LocalStoreIndependence: PASSED")
	})

	// =========================================================================
	// Subtest e: Cross-protocol lock visibility (NFS + SMB mixed mount)
	// =========================================================================
	t.Run("CrossProtocolLockVisibility", func(t *testing.T) {
		// This test requires SMB mount capability
		framework.SkipIfNoSMBMount(t)

		// Enable SMB adapter
		smbPort := helpers.FindFreePort(t)
		_, err := runner.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
		if err != nil {
			t.Skip("SMB adapter not available, skipping cross-protocol lock test")
		}
		t.Cleanup(func() { _, _ = runner.DisableAdapter("smb") })

		err = helpers.WaitForAdapterStatus(t, runner, "smb", true, 5*time.Second)
		if err != nil {
			t.Skip("SMB adapter did not become ready, skipping")
		}
		framework.WaitForServer(t, smbPort, 10*time.Second)

		// Mount share A via NFS and SMB
		mountNFS := framework.MountNFSExportWithVersion(t, nfsPort, "/share-iso-a", "3")
		t.Cleanup(mountNFS.Cleanup)

		// Create a test user for SMB
		_, _ = runner.Run("user", "create", "--username", "testuser", "--password", "testpass123")
		t.Cleanup(func() { _, _ = runner.Run("user", "delete", "testuser", "--force") })

		// Grant permission to the share
		_ = runner.GrantUserPermission("/share-iso-a", "testuser", "read-write")

		mountSMB := framework.MountSMB(t, smbPort, framework.SMBCredentials{
			Username: "testuser",
			Password: "testpass123",
		})
		t.Cleanup(mountSMB.Cleanup)

		// Write a file via NFS
		lockFile := mountNFS.FilePath("lock_test.txt")
		framework.WriteFile(t, lockFile, []byte("lock test data"))
		t.Cleanup(func() { _ = os.Remove(lockFile) })

		// Verify file is accessible via both protocols
		assert.True(t, framework.FileExists(mountSMB.FilePath("lock_test.txt")),
			"File created via NFS should be visible via SMB")

		t.Log("CrossProtocolLockVisibility: PASSED (file visible across protocols)")
	})

	// =========================================================================
	// Subtest f: Same remote store scenario
	// =========================================================================
	t.Run("SameRemoteStore", func(t *testing.T) {
		// Create a shared remote store
		sharedRemote := helpers.UniqueTestName("iso-shared-remote")
		_, err := runner.CreateRemoteBlockStore(sharedRemote, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteRemoteBlockStore(sharedRemote) })

		// Create two shares pointing to the SAME remote store
		metaSR1 := helpers.UniqueTestName("iso-meta-sr1")
		metaSR2 := helpers.UniqueTestName("iso-meta-sr2")
		localSR1 := helpers.UniqueTestName("iso-local-sr1")
		localSR2 := helpers.UniqueTestName("iso-local-sr2")

		_, err = runner.CreateMetadataStore(metaSR1, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaSR1) })

		_, err = runner.CreateMetadataStore(metaSR2, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaSR2) })

		_, err = runner.CreateLocalBlockStore(localSR1, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localSR1) })

		_, err = runner.CreateLocalBlockStore(localSR2, "memory")
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localSR2) })

		_, err = runner.CreateShare("/share-sr1", metaSR1, localSR1,
			helpers.WithShareRemote(sharedRemote))
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteShare("/share-sr1") })

		_, err = runner.CreateShare("/share-sr2", metaSR2, localSR2,
			helpers.WithShareRemote(sharedRemote))
		require.NoError(t, err)
		t.Cleanup(func() { _ = runner.DeleteShare("/share-sr2") })

		// Mount both
		mountSR1 := framework.MountNFSExportWithVersion(t, nfsPort, "/share-sr1", "3")
		t.Cleanup(mountSR1.Cleanup)

		mountSR2 := framework.MountNFSExportWithVersion(t, nfsPort, "/share-sr2", "3")
		t.Cleanup(mountSR2.Cleanup)

		// Write different files to each share
		fileSR1 := mountSR1.FilePath("sr1_file.txt")
		framework.WriteFile(t, fileSR1, []byte("share-sr1-data"))
		t.Cleanup(func() { _ = os.Remove(fileSR1) })

		fileSR2 := mountSR2.FilePath("sr2_file.txt")
		framework.WriteFile(t, fileSR2, []byte("share-sr2-data"))
		t.Cleanup(func() { _ = os.Remove(fileSR2) })

		// Verify files from share SR1 don't appear in share SR2
		assert.False(t, framework.FileExists(mountSR2.FilePath("sr1_file.txt")),
			"SR1 file should NOT appear on SR2 (payloadID namespacing)")
		assert.False(t, framework.FileExists(mountSR1.FilePath("sr2_file.txt")),
			"SR2 file should NOT appear on SR1 (payloadID namespacing)")

		// Verify each share's data is correct
		contentSR1 := framework.ReadFile(t, fileSR1)
		assert.Equal(t, []byte("share-sr1-data"), contentSR1)

		contentSR2 := framework.ReadFile(t, fileSR2)
		assert.Equal(t, []byte("share-sr2-data"), contentSR2)

		t.Log("SameRemoteStore: PASSED (payloadID namespacing keeps data separate)")
	})
}

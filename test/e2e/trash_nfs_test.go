//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNFSTrashRecycleAndRestore validates the per-share recycle bin over an NFS
// mount: deleting a file moves it to #recycle (preserving its name), the file
// can be restored by moving it back, and deleting inside the bin is permanent.
func TestNFSTrashRecycleAndRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NFS trash test in short mode")
	}

	// Start server process
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to get CLI runner
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create metadata and block stores for our test share
	metaStoreName := helpers.UniqueTestName("meta")
	localStoreName := helpers.UniqueTestName("local")
	shareName := "/export"

	_, err := runner.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() {
		_ = runner.DeleteMetadataStore(metaStoreName)
	})

	_, err = runner.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "Should create block store")
	t.Cleanup(func() {
		_ = runner.DeleteLocalBlockStore(localStoreName)
	})

	// Create the share with the recycle bin enabled
	_, err = runner.CreateShare(shareName, metaStoreName, localStoreName, helpers.WithShareTrashEnabled())
	require.NoError(t, err, "Should create trash-enabled share")
	t.Cleanup(func() {
		_ = runner.DeleteShare(shareName)
	})

	// Enable NFS adapter on a dynamic port
	nfsPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() {
		_, _ = runner.DisableAdapter("nfs")
	})

	// Wait for adapter to be ready
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Wait for NFS server to be listening
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// Mount the NFS share
	mount := framework.MountNFS(t, nfsPort)
	t.Cleanup(mount.Cleanup)

	// Write a file with known content.
	content := []byte("Hello, trash! This file should land in #recycle on delete.")
	f := mount.FilePath("doc.txt")
	framework.WriteFile(t, f, content)

	// Delete it over the mount.
	err = os.Remove(f)
	require.NoError(t, err, "Should delete file over NFS")

	// The original path must be gone.
	_, err = os.Stat(f)
	assert.True(t, os.IsNotExist(err), "Original path should not exist after delete, got err=%v", err)

	// It must appear in the recycle bin under its original name.
	recycled := mount.FilePath(filepath.Join("#recycle", "doc.txt"))
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(recycled)
		return statErr == nil
	}, 5*time.Second, 100*time.Millisecond, "Deleted file should appear in #recycle")

	// And its bytes must match the original content.
	assert.Equal(t, content, framework.ReadFile(t, recycled), "Recycled file content should match original")

	// Restore: move it back out of the bin.
	err = os.Rename(recycled, f)
	require.NoError(t, err, "Should restore file by moving it back")
	assert.Equal(t, content, framework.ReadFile(t, f), "Restored file content should match original")

	// Permanent delete: send it to the bin again, then delete inside the bin.
	framework.WriteFile(t, f, content)
	err = os.Remove(f)
	require.NoError(t, err, "Should re-delete file into the bin")

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(recycled)
		return statErr == nil
	}, 5*time.Second, 100*time.Millisecond, "Re-deleted file should appear in #recycle again")

	// Deleting inside the bin is permanent.
	err = os.Remove(recycled)
	require.NoError(t, err, "Should delete file inside the recycle bin")

	_, err = os.Stat(recycled)
	assert.True(t, os.IsNotExist(err), "Delete inside #recycle should be permanent, got err=%v", err)

	t.Log("NFS trash recycle + restore + permanent-in-bin passed")
}

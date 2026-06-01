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

// TestSMBTrashRecycleAndRestore validates the per-share recycle bin over an SMB
// mount: deleting a file with delete-on-close (the Windows/Explorer delete path)
// moves it to #recycle preserving its name, and the file can be restored by
// moving it back out of the bin.
//
// "#recycle" is a valid Windows file name, and os.Remove over a cifs mount maps
// to SMB delete-on-close — the same path Explorer uses. This exercises the
// cross-protocol recycle trap in MetadataService.RemoveFile from the SMB side.
func TestSMBTrashRecycleAndRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB trash test in short mode")
	}

	// Start server process; dump logs on failure like the SMB template.
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(func() {
		if t.Failed() {
			sp.DumpLogs(t)
		}
		sp.ForceKill()
	})

	// Login as admin to configure the server.
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create memory metadata + local block store for the test share.
	metaStoreName := helpers.UniqueTestName("meta")
	localStoreName := helpers.UniqueTestName("local")
	shareName := "/export"

	_, err := runner.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")

	_, err = runner.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "Should create block store")

	// Create the share with read-write default permission and recycle bin enabled.
	_, err = runner.CreateShare(shareName, metaStoreName, localStoreName,
		helpers.WithShareDefaultPermission("read-write"),
		helpers.WithShareTrashEnabled())
	require.NoError(t, err, "Should create trash-enabled share")

	// Create a test user for SMB authentication (password must be 8+ chars).
	testUsername := helpers.UniqueTestName("smbuser")
	testPassword := "testpass123"

	_, err = runner.CreateUser(testUsername, testPassword)
	require.NoError(t, err, "Should create test user")

	err = runner.GrantUserPermission(shareName, testUsername, "read-write")
	require.NoError(t, err, "Should grant user permission")

	// Enable SMB adapter on a dynamic port and wait for it.
	smbPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")

	err = helpers.WaitForAdapterStatus(t, runner, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Mount the SMB share with credentials.
	creds := framework.SMBCredentials{
		Username: testUsername,
		Password: testPassword,
	}
	mount := framework.MountSMB(t, smbPort, creds)
	t.Cleanup(mount.Cleanup)

	// Write a file with known content.
	content := []byte("Hello, trash! This file should land in #recycle on SMB delete.")
	f := mount.FilePath("doc.txt")
	framework.WriteFile(t, f, content)

	// Delete it over the mount — on cifs this is SMB delete-on-close, the
	// Windows/Explorer delete path.
	err = os.Remove(f)
	require.NoError(t, err, "Should delete file over SMB")

	// The original path must be gone.
	_, err = os.Stat(f)
	assert.True(t, os.IsNotExist(err), "Original path should not exist after delete, got err=%v", err)

	// It must appear in the recycle bin under its original name, with matching bytes.
	recycled := mount.FilePath(filepath.Join("#recycle", "doc.txt"))
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(recycled)
		return statErr == nil
	}, 5*time.Second, 100*time.Millisecond, "Deleted file should appear in #recycle")

	assert.Equal(t, content, framework.ReadFile(t, recycled), "Recycled file content should match original")

	// Restore: move it back out of the bin and verify bytes survive the round-trip.
	err = os.Rename(recycled, f)
	require.NoError(t, err, "Should restore file by moving it back")
	assert.Equal(t, content, framework.ReadFile(t, f), "Restored file content should match original")

	t.Log("SMB trash recycle + restore via delete-on-close passed")
}

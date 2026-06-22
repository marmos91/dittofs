//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossProtocolXattrParity proves the unified xattr namespace: a user.*
// extended attribute set over one protocol is readable over the other. NFS uses
// vers=4.2 (RFC 8276 xattr ops); Linux cifs maps user.* xattrs onto SMB extended
// attributes (FILE_FULL_EA_INFORMATION), which DittoFS resolves over the same
// backing the NFS resolver reads.
//
//   - XPX-01: xattr set via NFS is readable via SMB
//   - XPX-02: xattr set via SMB is readable via NFS
//   - XPX-03: an update over one protocol is observed over the other
func TestCrossProtocolXattrParity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cross-protocol xattr parity tests in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	metaStore := helpers.UniqueTestName("xpxmeta")
	localStore := helpers.UniqueTestName("xpxpayload")
	const shareName = "/export"

	_, err := cli.CreateMetadataStore(metaStore, "memory")
	require.NoError(t, err, "create metadata store")
	_, err = cli.CreateLocalBlockStore(localStore, "memory")
	require.NoError(t, err, "create block store")
	_, err = cli.CreateShare(shareName, metaStore, localStore,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "create share")

	// SMB needs an authenticated user; NFS uses AUTH_UNIX.
	smbUser := helpers.UniqueTestName("xpxuser")
	const smbPass = "testpass123"
	_, err = cli.CreateUser(smbUser, smbPass)
	require.NoError(t, err, "create SMB user")
	require.NoError(t, cli.GrantUserPermission(shareName, smbUser, "read-write"),
		"grant SMB user permission")

	nfsPort := helpers.FindFreePort(t)
	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "enable nfs adapter")
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "enable smb adapter")
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second))
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second))
	framework.WaitForServer(t, nfsPort, 10*time.Second)
	framework.WaitForServer(t, smbPort, 10*time.Second)

	nfsMount := framework.MountNFSWithVersion(t, nfsPort, "4.2")
	t.Cleanup(nfsMount.Cleanup)
	smbMount := framework.MountSMB(t, smbPort, framework.SMBCredentials{
		Username: smbUser,
		Password: smbPass,
	})
	t.Cleanup(smbMount.Cleanup)

	t.Run("XPX-01 NFS-set xattr readable via SMB", func(t *testing.T) {
		// Create the file on NFS, set the xattr there, read it back on SMB.
		const rel = "nfs_to_smb.txt"
		framework.WriteFile(t, nfsMount.FilePath(rel), []byte("a"))
		setfattr(t, nfsMount.FilePath(rel), "user.tag", "from-nfs")

		got, err := getfattr(t, smbMount.FilePath(rel), "user.tag")
		require.NoError(t, err, "SMB mount should see the NFS-set xattr")
		assert.Equal(t, "from-nfs", got)
	})

	t.Run("XPX-02 SMB-set xattr readable via NFS", func(t *testing.T) {
		const rel = "smb_to_nfs.txt"
		framework.WriteFile(t, smbMount.FilePath(rel), []byte("b"))
		setfattr(t, smbMount.FilePath(rel), "user.tag2", "from-smb")

		got, err := getfattr(t, nfsMount.FilePath(rel), "user.tag2")
		require.NoError(t, err, "NFS mount should see the SMB-set xattr")
		assert.Equal(t, "from-smb", got)
	})

	t.Run("XPX-03 update over one protocol observed via the other", func(t *testing.T) {
		const rel = "update.txt"
		framework.WriteFile(t, nfsMount.FilePath(rel), []byte("c"))
		setfattr(t, nfsMount.FilePath(rel), "user.k", "v1")

		got, err := getfattr(t, smbMount.FilePath(rel), "user.k")
		require.NoError(t, err)
		require.Equal(t, "v1", got)

		// Overwrite from the SMB side; NFS must observe the new value.
		setfattr(t, smbMount.FilePath(rel), "user.k", "v2")
		got, err = getfattr(t, nfsMount.FilePath(rel), "user.k")
		require.NoError(t, err)
		assert.Equal(t, "v2", got, "NFS should observe the SMB update")
	})
}

//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
)

// TestSMBShareSquash checks that a share's squash maps an SMB session's
// identity as it maps an NFS caller's, share by share: the built-in admin,
// uid 0, is the anonymous principal on a share that squashes root and keeps
// root's privilege on one that squashes nothing, and on a share that squashes
// everyone every user is the anonymous principal.
func TestSMBShareSquash(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB squash tests in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(func() {
		if t.Failed() {
			sp.DumpLogs(t)
		}
		sp.ForceKill()
	})
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	metaStore := helpers.UniqueTestName("squashmeta")
	blockStore := helpers.UniqueTestName("squashblock")
	_, err := cli.CreateInMemoryMetadataStore(metaStore)
	require.NoError(t, err)
	_, err = cli.CreateBlockStore(blockStore, "memory")
	require.NoError(t, err)

	// Each share's root is world-writable (read-write default), so every file
	// below is created by its user and protected only by its own mode, 0644.
	shares := map[string]string{"/rootsquash": "root_to_guest", "/nosquash": "none", "/allsquash": "all_to_guest"}
	for name, squash := range shares {
		_, err = cli.CreateShare(name, metaStore, blockStore, helpers.WithShareDefaultPermission("read-write"))
		require.NoError(t, err)
		_, err = cli.Run("share", "nfs-config", "set", name, "--squash", squash)
		require.NoError(t, err)
	}

	const password = "squashpass123"
	_, err = cli.CreateUser("alice", password, helpers.WithUID(2001))
	require.NoError(t, err)
	_, err = cli.CreateUser("bob", password, helpers.WithUID(2002))
	require.NoError(t, err)

	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err)
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second))
	framework.WaitForServer(t, smbPort, 10*time.Second)

	admin := helpers.ConnectSMB3(t, smbPort, "admin", helpers.GetAdminPassword())
	alice := helpers.ConnectSMB3(t, smbPort, "alice", password)
	bob := helpers.ConnectSMB3(t, smbPort, "bob", password)

	t.Run("root_to_guest refuses uid 0 another user's file", func(t *testing.T) {
		require.NoError(t, helpers.MountSMB3Share(t, alice, "/rootsquash").WriteFile("notes.txt", []byte("alice"), 0o644))

		err := helpers.MountSMB3Share(t, bob, "/rootsquash").WriteFile("notes.txt", []byte("bob"), 0o644)
		require.ErrorIs(t, err, os.ErrPermission, "bob overwriting alice's 0644 file")

		adminShare := helpers.MountSMB3Share(t, admin, "/rootsquash")
		err = adminShare.WriteFile("notes.txt", []byte("admin"), 0o644)
		require.ErrorIs(t, err, os.ErrPermission, "admin, squashed to the anonymous principal, overwriting alice's 0644 file")
		got, err := adminShare.ReadFile("notes.txt")
		require.NoError(t, err, "the squashed admin still reads a world-readable file")
		require.Equal(t, "alice", string(got))
	})

	t.Run("none keeps uid 0 privileged", func(t *testing.T) {
		require.NoError(t, helpers.MountSMB3Share(t, alice, "/nosquash").WriteFile("notes.txt", []byte("alice"), 0o644))

		adminShare := helpers.MountSMB3Share(t, admin, "/nosquash")
		require.NoError(t, adminShare.WriteFile("notes.txt", []byte("admin"), 0o644),
			"admin, unsquashed uid 0, overwriting alice's 0644 file")
		got, err := adminShare.ReadFile("notes.txt")
		require.NoError(t, err)
		require.Equal(t, "admin", string(got))
	})

	t.Run("all_to_guest creates as the anonymous principal", func(t *testing.T) {
		require.NoError(t, helpers.MountSMB3Share(t, alice, "/allsquash").WriteFile("shared.txt", []byte("alice"), 0o644))

		// alice and bob both act as the anonymous principal, which owns the file
		// alice created, so bob may write it as its owner.
		require.NoError(t, helpers.MountSMB3Share(t, bob, "/allsquash").WriteFile("shared.txt", []byte("bob"), 0o644),
			"bob writing the 0644 file alice created, both squashed to its owner")
	})
}

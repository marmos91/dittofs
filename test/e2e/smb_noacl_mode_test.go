//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
)

// TestSMBNoACLOpenGatedByModeOwnerAndParent checks, over the SMB wire, that
// an open of a file with no stored ACL is gated by the file's mode, its owner
// and its parent, which decides delete as it does for an NFS unlink. The
// open's grant is what a later delete on close or write is authorized by, so
// an open that granted the request would let any share user delete or write
// another user's file.
//
// alice and bob both hold a read-write grant on the share, whose root is
// 0777. alice creates private/ (0755) and files in it and in the root
// (0644). They share the default group, whose bits deny write too.
func TestSMBNoACLOpenGatedByModeOwnerAndParent(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping SMB e2e test in short mode")
	}

	env := helpers.SetupSMB3TestEnv(t)
	const password = "testpass123"
	alice, bob := helpers.UniqueTestName("alice"), helpers.UniqueTestName("bob")
	for name, uid := range map[string]uint32{alice: 52001, bob: 52002} {
		_, err := env.CLIRunner.CreateUser(name, password, helpers.WithUID(uid))
		require.NoError(t, err)
		require.NoError(t, env.CLIRunner.GrantUserPermission(env.ShareName, name, "read-write"))
	}
	aliceShare := helpers.MountSMB3Share(t, helpers.ConnectSMB3(t, env.SMBPort, alice, password), env.ShareName)
	bobShare := helpers.MountSMB3Share(t, helpers.ConnectSMB3(t, env.SMBPort, bob, password), env.ShareName)

	require.NoError(t, aliceShare.Mkdir("private", 0o755))
	for _, name := range []string{"private/doc.txt", "private/disposition.txt", "private/own.txt", "private/own-doc.txt", "root-doc.txt", "root-disposition.txt"} {
		require.NoError(t, aliceShare.WriteFile(name, []byte("alice"), 0o644))
	}
	requireExists := func(t *testing.T, name string) {
		t.Helper()
		_, err := aliceShare.Stat(name)
		require.NoError(t, err, "%s should still exist", name)
	}
	requireGone := func(t *testing.T, name string) {
		t.Helper()
		_, err := aliceShare.Stat(name)
		require.True(t, os.IsNotExist(err), "%s should be gone, got %v", name, err)
	}

	t.Run("NonOwnerDeleteOnCloseRefused", func(t *testing.T) {
		out, _ := helpers.RunSMBClient(t, env.SMBPort, bob, password, env.ShareName, "del private/doc.txt")
		assert.Contains(t, out, "NT_STATUS_ACCESS_DENIED")
		requireExists(t, "private/doc.txt")
	})

	t.Run("NonOwnerDispositionRefused", func(t *testing.T) {
		// go-smb2's Remove opens with DELETE and sets FileDispositionInformation.
		requireAccessDenied(t, bobShare.Remove("private/disposition.txt"), "bob's delete of alice's file")
		requireExists(t, "private/disposition.txt")
	})

	t.Run("NonOwnerWriteOpenRefused", func(t *testing.T) {
		// The root lets bob delete root-doc.txt, but the file's mode does not
		// let him write it.
		f, err := bobShare.OpenFile("root-doc.txt", os.O_WRONLY, 0)
		if err == nil {
			_ = f.Close()
		}
		requireAccessDenied(t, err, "bob's write open of alice's file")
	})

	t.Run("OwnerWritesAndDeletes", func(t *testing.T) {
		f, err := aliceShare.OpenFile("private/own.txt", os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.Write([]byte("ALICE"))
		require.NoError(t, err)
		require.NoError(t, f.Close())
		got, err := aliceShare.ReadFile("private/own.txt")
		require.NoError(t, err)
		require.Equal(t, "ALICE", string(got))

		require.NoError(t, aliceShare.Remove("private/own.txt"))
		requireGone(t, "private/own.txt")
		out, err := helpers.RunSMBClient(t, env.SMBPort, alice, password, env.ShareName, "del private/own-doc.txt")
		require.NoError(t, err, out)
		requireGone(t, "private/own-doc.txt")
	})

	t.Run("NonOwnerDeletesWhereTheParentAllows", func(t *testing.T) {
		require.NoError(t, bobShare.Remove("root-disposition.txt"))
		requireGone(t, "root-disposition.txt")
		out, err := helpers.RunSMBClient(t, env.SMBPort, bob, password, env.ShareName, "del root-doc.txt")
		require.NoError(t, err, out)
		requireGone(t, "root-doc.txt")
	})

	// sticky/ is 1777 and owned by neither user: its mode lets bob write it,
	// so only the sticky bit keeps him from removing alice's files. SMB cannot
	// set the bit, so the directory is made over NFS. The refusal may come at
	// the open or at the close, so the outcome is what is asserted.
	t.Run("StickyDirectoryOnlyOwnerDeletes", func(t *testing.T) {
		nfsPort := helpers.FindFreePort(t)
		_, err := env.CLIRunner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
		require.NoError(t, err)
		require.NoError(t, helpers.WaitForAdapterStatus(t, env.CLIRunner, "nfs", true, 5*time.Second))
		framework.WaitForServer(t, nfsPort, 10*time.Second)
		nfsMount := framework.MountNFSExportWithVersion(t, nfsPort, env.ShareName, "3")
		t.Cleanup(nfsMount.Cleanup)
		require.NoError(t, os.Mkdir(nfsMount.FilePath("sticky"), 0o777))
		require.NoError(t, os.Chmod(nfsMount.FilePath("sticky"), 0o777|os.ModeSticky))
		for _, name := range []string{"sticky/doc.txt", "sticky/disposition.txt"} {
			require.NoError(t, aliceShare.WriteFile(name, []byte("alice"), 0o644))
		}

		out, err := helpers.RunSMBClient(t, env.SMBPort, bob, password, env.ShareName, "del sticky/doc.txt")
		t.Logf("bob's delete on close: err=%v %s", err, out)
		requireExists(t, "sticky/doc.txt")
		err = bobShare.Remove("sticky/disposition.txt")
		t.Logf("bob's disposition delete: %v", err)
		requireExists(t, "sticky/disposition.txt")

		out, err = helpers.RunSMBClient(t, env.SMBPort, alice, password, env.ShareName, "del sticky/doc.txt")
		require.NoError(t, err, out)
		requireGone(t, "sticky/doc.txt")
		require.NoError(t, aliceShare.Remove("sticky/disposition.txt"))
		requireGone(t, "sticky/disposition.txt")
	})
}

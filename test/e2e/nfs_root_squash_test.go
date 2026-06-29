//go:build e2e && linux

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNFSRootSquash is the regression test for the colleague-reported NFS
// permission bug (#1449) and the squash-default alignment that followed (#1452).
// It asserts the post-change behaviour for a world-readable, granted-writable
// share (default_permission=read) under the conventional root_to_guest squash:
//
//   - root_to_guest means an NFS client mounting as root is NOT auto-privileged.
//     The built-in admin occupies uid 0 but has no grant on a fresh share, so
//     root is squashed to guest and cannot write.
//   - An unknown uid (no registered DittoFS user, no grant) cannot write.
//   - A user granted read-write CAN write — the grant projects into the share
//     root ACL.
//   - Denials must surface as EACCES ("permission denied"), never the EIO
//     ("input/output error") the colleague hit on NFSv3 (#1449).
//
// Protocol split is deliberate and reflects what each NFS version can express:
//   - The write-denial cases run over NFSv3 — the exact path the colleague used,
//     and where the #1449 EACCES-vs-EIO mapping matters. Over NFSv3 access is
//     decided by POSIX mode bits: default_permission=read → root mode 0755, so a
//     root-squashed-to-guest or unknown uid (both "other") may read but not write.
//   - The granted-user positive control runs over NFSv4. A *per-user* grant is
//     an ACL entry; NFSv3 has no ACL transport (mode bits cannot express
//     "uid 2000 yes, uid 4000 no"), so only NFSv4 honours it.
func TestNFSRootSquash(t *testing.T) {
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	metaStoreName := helpers.UniqueTestName("meta")
	localStoreName := helpers.UniqueTestName("local")
	shareName := "/export"

	_, err := runner.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaStoreName) })

	_, err = runner.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "Should create block store")
	t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localStoreName) })

	// default_permission=read models the common "world-readable, granted-writable"
	// share: everyone may read/traverse (so an NFSv4 client mounting as the
	// squashed-guest root can complete the mount), but WRITE is least-privilege —
	// only an explicit grant opens it. A fully-locked "none" share is intentionally
	// not used here: over NFSv4 the mount runs as root→guest and "none" denies even
	// the traversal needed to mount, so "none" is reachable only over NFSv3's
	// separate mount protocol. All assertions below are about write access, which
	// "read" leaves least-privilege.
	_, err = runner.CreateShare(shareName, metaStoreName, localStoreName,
		helpers.WithShareDefaultPermission("read"))
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() { _ = runner.DeleteShare(shareName) })

	// Grant read-write to a single non-root user.
	_, err = runner.CreateUser(nfsLeastPrivUser, nfsLeastPrivPass, helpers.WithUID(nfsLeastPrivUID))
	require.NoError(t, err, "Should create granted user")
	t.Cleanup(func() { _ = runner.DeleteUser(nfsLeastPrivUser) })

	err = runner.GrantUserPermission(shareName, nfsLeastPrivUser, "read-write")
	require.NoError(t, err, "Should grant read-write to user")

	nfsPort := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() { _, _ = runner.DisableAdapter("nfs") })

	require.NoError(t, helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// Denials over NFSv3 — the colleague's exact path.
	mountV3 := framework.MountNFS(t, nfsPort)
	t.Cleanup(mountV3.Cleanup)

	// Root (uid 0) is squashed to guest under root_to_guest and has no grant. It
	// is the case that reaches the server (root skips client-side mode checks), so
	// it exercises the #1449 EACCES-not-EIO server mapping directly.
	t.Run("root is denied (EACCES not EIO)", func(t *testing.T) {
		err := framework.WriteFileAsUID(t, 0, 0, mountV3.FilePath("root.txt"), []byte("nope"))
		requireAccessDenied(t, err, "root write")
	})

	// An unknown uid (no DittoFS user, no grant) gets only the read default, so
	// its write is denied.
	t.Run("unknown uid is denied", func(t *testing.T) {
		const unknownUID, unknownGID = uint32(4000), uint32(4000)
		err := framework.WriteFileAsUID(t, unknownUID, unknownGID, mountV3.FilePath("stranger.txt"), []byte("nope"))
		requireAccessDenied(t, err, "unknown-uid write")
	})

	// Positive control over NFSv4: the per-user grant is an ACL entry, which only
	// NFSv4 carries. The granted user can write and read back.
	t.Run("granted user can write (NFSv4)", func(t *testing.T) {
		mountV4 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
		t.Cleanup(mountV4.Cleanup)

		path := mountV4.FilePath("granted.txt")
		err := framework.WriteFileAsUID(t, nfsLeastPrivUID, nfsLeastPrivGID, path, []byte("ok"))
		require.NoError(t, err, "Granted read-write user should be able to write")
		t.Cleanup(func() {
			_ = framework.RemoveAsUID(t, nfsLeastPrivUID, nfsLeastPrivGID, path)
		})

		got, err := framework.ReadFileAsUID(t, nfsLeastPrivUID, nfsLeastPrivGID, path)
		require.NoError(t, err, "Granted user should be able to read back")
		assert.Equal(t, []byte("ok"), got)
	})

	// Counter-control over NFSv4: an unknown uid is still denied even where ACLs
	// are honoured — the grant, not the protocol, is what opens access.
	t.Run("unknown uid is denied (NFSv4)", func(t *testing.T) {
		mountV4 := framework.MountNFSWithVersion(t, nfsPort, "4.1")
		t.Cleanup(mountV4.Cleanup)

		const unknownUID, unknownGID = uint32(4001), uint32(4001)
		err := framework.WriteFileAsUID(t, unknownUID, unknownGID, mountV4.FilePath("stranger4.txt"), []byte("nope"))
		requireAccessDenied(t, err, "unknown-uid write over NFSv4")
	})
}

// requireAccessDenied asserts that err is a permission-denied (EACCES) failure
// and specifically NOT the EIO regression (#1449) where export-gate denials on
// NFSv3 surfaced as "input/output error".
func requireAccessDenied(t *testing.T, err error, what string) {
	t.Helper()
	require.Error(t, err, "%s should be denied", what)
	msg := strings.ToLower(err.Error())
	assert.Contains(t, msg, "permission denied",
		"%s should fail with EACCES, got: %v", what, err)
	assert.NotContains(t, msg, "input/output error",
		"%s must not surface EIO for a permission denial (#1449), got: %v", what, err)
}

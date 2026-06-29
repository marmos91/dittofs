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
// permission bug and the squash-default alignment that followed:
//
//   - Default squash is root_to_guest (conventional root_squash), so an NFS
//     client mounting as root is NOT auto-privileged. Because the built-in
//     admin user occupies uid 0 but has no grant on a freshly created share
//     (default_permission=none), root is denied.
//   - An unknown uid (not a registered DittoFS user) is likewise denied.
//   - A user that has been granted read-write CAN write — the grant is
//     projected into the share root ACL, so least-privilege access works.
//   - Denials over NFSv3 must surface as EACCES ("permission denied"), not the
//     EIO ("input/output error") the colleague originally hit (#1449).
//
// The whole flow runs over NFSv3 (framework.MountNFS), the exact path the
// colleague used.
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

	// Fresh share: default_permission=none and default root_to_guest squash.
	_, err = runner.CreateShare(shareName, metaStoreName, localStoreName)
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

	mount := framework.MountNFS(t, nfsPort)
	t.Cleanup(mount.Cleanup)

	// Positive control: the granted user can write.
	t.Run("granted user can write", func(t *testing.T) {
		path := mount.FilePath("granted.txt")
		err := framework.WriteFileAsUID(t, nfsLeastPrivUID, nfsLeastPrivGID, path, []byte("ok"))
		require.NoError(t, err, "Granted read-write user should be able to write")
		t.Cleanup(func() {
			_ = framework.RemoveAsUID(t, nfsLeastPrivUID, nfsLeastPrivGID, path)
		})

		got, err := framework.ReadFileAsUID(t, nfsLeastPrivUID, nfsLeastPrivGID, path)
		require.NoError(t, err, "Granted user should be able to read back")
		assert.Equal(t, []byte("ok"), got)
	})

	// Root (uid 0) is squashed under root_to_guest and has no grant -> denied.
	t.Run("root is denied", func(t *testing.T) {
		err := framework.WriteFileAsUID(t, 0, 0, mount.FilePath("root.txt"), []byte("nope"))
		requireAccessDenied(t, err, "root write")
	})

	// An unknown uid (no DittoFS user) is denied by default_permission=none.
	t.Run("unknown uid is denied", func(t *testing.T) {
		const unknownUID, unknownGID = uint32(4000), uint32(4000)
		err := framework.WriteFileAsUID(t, unknownUID, unknownGID, mount.FilePath("stranger.txt"), []byte("nope"))
		requireAccessDenied(t, err, "unknown-uid write")
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

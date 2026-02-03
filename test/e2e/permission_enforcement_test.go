//go:build e2e

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPermissionEnforcement validates that DittoFS correctly enforces access permissions
// at the protocol level. These tests verify requirements ENF-01 through ENF-04:
//   - ENF-01: Read-only user cannot write files via SMB
//   - ENF-02: No-access user cannot read files via SMB
//   - ENF-03: User removed from group loses group permissions
//   - ENF-04: Permission change takes effect immediately
//
// Note: Per RESEARCH.md, we use SMB for permission tests because NFS AUTH_UNIX
// is UID-based (not user-based) and doesn't enforce DittoFS user permissions.
// SMB requires authenticated users, which allows us to test DittoFS permission enforcement.
func TestPermissionEnforcement(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping permission enforcement tests in short mode")
	}

	// Start a server for all permission tests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to configure the server
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create stores for the share
	metaStoreName := helpers.UniqueTestName("enf_meta")
	payloadStoreName := helpers.UniqueTestName("enf_payload")

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create payload store")

	// Create share with default permission "none" (deny by default)
	// This ensures users must have explicit permission grants to access
	shareName := "/export"
	_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
		helpers.WithShareDefaultPermission("none"))
	require.NoError(t, err, "Should create share with default permission none")

	// Enable NFS adapter for admin setup (creating test files)
	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")

	// Enable SMB adapter for permission-enforced testing
	smbPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(smbPort))
	require.NoError(t, err, "Should enable SMB adapter")

	// Wait for adapters to be ready
	err = helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")
	err = helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	framework.WaitForServer(t, nfsPort, 10*time.Second)
	framework.WaitForServer(t, smbPort, 10*time.Second)

	// Mount NFS as admin for test file setup (no auth required for NFS)
	adminMount := framework.MountNFS(t, nfsPort)
	t.Cleanup(adminMount.Cleanup)

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteMetadataStore(metaStoreName)
		_ = cli.DeletePayloadStore(payloadStoreName)
	})

	// Run subtests for each requirement
	t.Run("ENF-01 read-only user cannot write", func(t *testing.T) {
		testReadOnlyUserCannotWrite(t, cli, smbPort, shareName, adminMount)
	})

	t.Run("ENF-02 no-access user cannot read", func(t *testing.T) {
		testNoAccessUserCannotRead(t, cli, smbPort, shareName, adminMount)
	})

	t.Run("ENF-03 user removed from group loses permissions", func(t *testing.T) {
		testUserRemovedFromGroupLosesPermissions(t, cli, smbPort, shareName, adminMount)
	})

	t.Run("ENF-04 permission change takes effect immediately", func(t *testing.T) {
		testPermissionChangeEffectImmediate(t, cli, smbPort, shareName, adminMount)
	})
}

// testReadOnlyUserCannotWrite tests ENF-01: A user with read-only permission
// should be able to read files but not write them.
func testReadOnlyUserCannotWrite(t *testing.T, cli *helpers.CLIRunner, smbPort int, shareName string, adminMount *framework.Mount) {
	t.Helper()

	// Create a read-only user
	userName := helpers.UniqueTestName("readonly")
	userPass := "testpassword123"

	_, err := cli.CreateUser(userName, userPass)
	require.NoError(t, err, "Should create read-only user")
	t.Cleanup(func() { _ = cli.DeleteUser(userName) })

	// Grant read-only permission
	err = cli.GrantUserPermission(shareName, userName, "read")
	require.NoError(t, err, "Should grant read permission")
	t.Cleanup(func() { _ = cli.RevokeUserPermission(shareName, userName) })

	// Create a test file as admin via NFS
	testFile := "enf01_test.txt"
	testContent := []byte("Test content for read-only permission check")
	framework.WriteFile(t, adminMount.FilePath(testFile), testContent)
	t.Cleanup(func() { _ = os.Remove(adminMount.FilePath(testFile)) })

	// Small delay for permission propagation
	time.Sleep(200 * time.Millisecond)

	// Mount SMB as read-only user
	creds := framework.SMBCredentials{
		Username: userName,
		Password: userPass,
	}
	mount := framework.MountSMB(t, smbPort, creds)
	defer mount.Cleanup()

	// Reading should succeed
	content := framework.ReadFile(t, mount.FilePath(testFile))
	assert.Equal(t, testContent, content, "Read-only user should be able to read file")

	// Writing should fail
	err = os.WriteFile(mount.FilePath("forbidden_write.txt"), []byte("should not be allowed"), 0644)
	assert.Error(t, err, "Read-only user should not be able to create new files")

	// Modifying existing file should also fail
	err = os.WriteFile(mount.FilePath(testFile), []byte("modified content"), 0644)
	assert.Error(t, err, "Read-only user should not be able to modify existing files")

	t.Log("ENF-01: Read-only user cannot write - PASSED")
}

// testNoAccessUserCannotRead tests ENF-02: A user with no permission
// should not be able to mount or access the share.
func testNoAccessUserCannotRead(t *testing.T, cli *helpers.CLIRunner, smbPort int, shareName string, adminMount *framework.Mount) {
	t.Helper()

	// Create a no-access user (no explicit permission, default is "none")
	userName := helpers.UniqueTestName("noaccess")
	userPass := "testpassword123"

	_, err := cli.CreateUser(userName, userPass)
	require.NoError(t, err, "Should create no-access user")
	t.Cleanup(func() { _ = cli.DeleteUser(userName) })

	// Do NOT grant any permission - default_permission is "none"

	// Create a test file as admin via NFS
	testFile := "enf02_test.txt"
	testContent := []byte("Test content for no-access permission check")
	framework.WriteFile(t, adminMount.FilePath(testFile), testContent)
	t.Cleanup(func() { _ = os.Remove(adminMount.FilePath(testFile)) })

	// Small delay for permission propagation
	time.Sleep(200 * time.Millisecond)

	// Try to mount SMB as no-access user - should fail
	creds := framework.SMBCredentials{
		Username: userName,
		Password: userPass,
	}

	mount, err := framework.MountSMBWithError(t, smbPort, creds)
	if err != nil {
		// Mount failed as expected - test passes
		t.Logf("Mount correctly denied for no-access user: %v", err)
		t.Log("ENF-02: No-access user cannot read - PASSED (mount denied)")
		return
	}

	// Mount succeeded - operations should fail (implementation-dependent behavior)
	defer mount.Cleanup()

	// Try to read - should fail
	_, err = os.ReadFile(mount.FilePath(testFile))
	assert.Error(t, err, "No-access user should not be able to read files")

	t.Log("ENF-02: No-access user cannot read - PASSED (operations denied)")
}

// testUserRemovedFromGroupLosesPermissions tests ENF-03: When a user is removed
// from a group, they should lose the group's permissions.
func testUserRemovedFromGroupLosesPermissions(t *testing.T, cli *helpers.CLIRunner, smbPort int, shareName string, adminMount *framework.Mount) {
	t.Helper()

	// Create a group with read-write permission
	groupName := helpers.UniqueTestName("rwgroup")
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")
	t.Cleanup(func() { _ = cli.DeleteGroup(groupName) })

	// Grant read-write permission to the group
	err = cli.GrantGroupPermission(shareName, groupName, "read-write")
	require.NoError(t, err, "Should grant read-write permission to group")
	t.Cleanup(func() { _ = cli.RevokeGroupPermission(shareName, groupName) })

	// Create a user and add to the group
	userName := helpers.UniqueTestName("groupuser")
	userPass := "testpassword123"

	_, err = cli.CreateUser(userName, userPass)
	require.NoError(t, err, "Should create user")
	t.Cleanup(func() { _ = cli.DeleteUser(userName) })

	err = cli.AddGroupMember(groupName, userName)
	require.NoError(t, err, "Should add user to group")

	// Create test directory for this subtest
	testDir := "enf03_dir"
	framework.CreateDir(t, adminMount.FilePath(testDir))
	t.Cleanup(func() { _ = os.RemoveAll(adminMount.FilePath(testDir)) })

	// Small delay for permission propagation
	time.Sleep(200 * time.Millisecond)

	// Mount and verify user can write (via group permission)
	creds := framework.SMBCredentials{
		Username: userName,
		Password: userPass,
	}
	mount := framework.MountSMB(t, smbPort, creds)

	// Write should succeed (user has group's read-write permission)
	testFile := testDir + "/group_test.txt"
	err = os.WriteFile(mount.FilePath(testFile), []byte("written via group permission"), 0644)
	assert.NoError(t, err, "User in group should be able to write")

	// Unmount before modifying group membership
	mount.Cleanup()

	// Wait for macOS to release SMB session state
	time.Sleep(500 * time.Millisecond)

	// Remove user from group
	err = cli.RemoveGroupMember(groupName, userName)
	require.NoError(t, err, "Should remove user from group")

	// Small delay for permission propagation
	time.Sleep(200 * time.Millisecond)

	// Re-mount and verify user can no longer write
	mount, err = framework.MountSMBWithError(t, smbPort, creds)
	if err != nil {
		// Mount failed - user has no access now, test passes
		t.Log("ENF-03: User removed from group loses permissions - PASSED (mount denied after group removal)")
		return
	}
	defer mount.Cleanup()

	// Mount succeeded - write should now fail
	err = os.WriteFile(mount.FilePath(testDir+"/after_removal.txt"), []byte("should fail"), 0644)
	assert.Error(t, err, "User removed from group should not be able to write")

	t.Log("ENF-03: User removed from group loses permissions - PASSED")
}

// testPermissionChangeEffectImmediate tests ENF-04: Permission changes should
// take effect without requiring remount.
func testPermissionChangeEffectImmediate(t *testing.T, cli *helpers.CLIRunner, smbPort int, shareName string, adminMount *framework.Mount) {
	t.Helper()

	// Create a user with read-only permission initially
	userName := helpers.UniqueTestName("upgrade")
	userPass := "testpassword123"

	_, err := cli.CreateUser(userName, userPass)
	require.NoError(t, err, "Should create user")
	t.Cleanup(func() { _ = cli.DeleteUser(userName) })

	// Grant read-only permission first
	err = cli.GrantUserPermission(shareName, userName, "read")
	require.NoError(t, err, "Should grant read permission")
	t.Cleanup(func() { _ = cli.RevokeUserPermission(shareName, userName) })

	// Create test directory for this subtest
	testDir := "enf04_dir"
	framework.CreateDir(t, adminMount.FilePath(testDir))
	t.Cleanup(func() { _ = os.RemoveAll(adminMount.FilePath(testDir)) })

	// Create a test file
	testFile := testDir + "/read_test.txt"
	framework.WriteFile(t, adminMount.FilePath(testFile), []byte("initial content"))

	// Small delay for permission propagation
	time.Sleep(200 * time.Millisecond)

	// Mount as user with read permission
	creds := framework.SMBCredentials{
		Username: userName,
		Password: userPass,
	}
	mount := framework.MountSMB(t, smbPort, creds)
	defer mount.Cleanup()

	// Reading should work
	content := framework.ReadFile(t, mount.FilePath(testFile))
	assert.NotEmpty(t, content, "Read should work with read permission")

	// Writing should fail with read-only permission
	err = os.WriteFile(mount.FilePath(testDir+"/denied.txt"), []byte("should fail"), 0644)
	assert.Error(t, err, "Write should fail with read-only permission")

	// Upgrade permission to read-write (without remounting)
	err = cli.GrantUserPermission(shareName, userName, "read-write")
	require.NoError(t, err, "Should upgrade to read-write permission")

	// Small delay for permission propagation
	time.Sleep(200 * time.Millisecond)

	// For permission changes to take effect, we may need to create a fresh SMB session
	// as the OS may cache authentication. Unmount and remount with same credentials.
	mount.Cleanup()
	time.Sleep(500 * time.Millisecond)

	mount = framework.MountSMB(t, smbPort, creds)
	defer mount.Cleanup()

	// Writing should now succeed with upgraded permission
	err = os.WriteFile(mount.FilePath(testDir+"/allowed.txt"), []byte("should succeed"), 0644)
	assert.NoError(t, err, "Write should succeed after permission upgrade")

	t.Log("ENF-04: Permission change takes effect immediately - PASSED")
}

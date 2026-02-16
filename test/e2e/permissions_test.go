//go:build e2e

package e2e

import (
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findPermission searches for a permission in the list.
// If level is empty string, matches any level for that type/name.
func findPermission(perms []*helpers.SharePermission, permType, name, level string) bool {
	for _, p := range perms {
		if p.Type == permType && p.Name == name {
			if level == "" || p.Level == level {
				return true
			}
		}
	}
	return false
}

// TestSharePermissions validates share permission management operations via the dfsctl CLI.
// These tests verify that permissions can be granted and revoked for users and groups on shares.
//
// Note: These tests require a running DittoFS server with the admin user configured.
// The DITTOFS_ADMIN_PASSWORD environment variable must be set.
func TestSharePermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping share permission tests in short mode")
	}

	// Start a server for all permission tests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Create shared stores for all subtests
	metaStoreName := helpers.UniqueTestName("perm_meta")
	payloadStoreName := helpers.UniqueTestName("perm_payload")

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create payload store")

	t.Cleanup(func() {
		_ = cli.DeleteMetadataStore(metaStoreName)
		_ = cli.DeletePayloadStore(payloadStoreName)
	})

	// Note: These tests run serially (no t.Parallel()) because SQLite
	// doesn't handle concurrent writes well in the E2E test environment.

	t.Run("PRM-01 grant read permission to user", func(t *testing.T) {
		testGrantReadPermissionToUser(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("PRM-02 grant read-write permission to user", func(t *testing.T) {
		testGrantReadWritePermissionToUser(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("PRM-03 grant read permission to group", func(t *testing.T) {
		testGrantReadPermissionToGroup(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("PRM-04 grant read-write permission to group", func(t *testing.T) {
		testGrantReadWritePermissionToGroup(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("PRM-05 revoke permission from user", func(t *testing.T) {
		testRevokePermissionFromUser(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("PRM-06 revoke permission from group", func(t *testing.T) {
		testRevokePermissionFromGroup(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("PRM-07 list permissions for share", func(t *testing.T) {
		testListPermissionsForShare(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("empty permissions list for new share", func(t *testing.T) {
		testEmptyPermissionsList(t, cli, metaStoreName, payloadStoreName)
	})

	t.Run("permission override replaces previous level", func(t *testing.T) {
		testPermissionOverride(t, cli, metaStoreName, payloadStoreName)
	})
}

// testGrantReadPermissionToUser verifies granting read permission to a user (PRM-01).
func testGrantReadPermissionToUser(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm01")
	userName := helpers.UniqueTestName("user_prm01")

	// Cleanup in reverse order of creation
	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteUser(userName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create user
	_, err = cli.CreateUser(userName, "testpassword123")
	require.NoError(t, err, "Should create user")

	// Grant read permission
	err = cli.GrantUserPermission(shareName, userName, "read")
	require.NoError(t, err, "Should grant read permission to user")

	// Verify permission was granted
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	assert.True(t, findPermission(perms, "user", userName, "read"),
		"User should have read permission")
}

// testGrantReadWritePermissionToUser verifies granting read-write permission to a user (PRM-02).
func testGrantReadWritePermissionToUser(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm02")
	userName := helpers.UniqueTestName("user_prm02")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteUser(userName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create user
	_, err = cli.CreateUser(userName, "testpassword123")
	require.NoError(t, err, "Should create user")

	// Grant read-write permission
	err = cli.GrantUserPermission(shareName, userName, "read-write")
	require.NoError(t, err, "Should grant read-write permission to user")

	// Verify permission was granted
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	assert.True(t, findPermission(perms, "user", userName, "read-write"),
		"User should have read-write permission")
}

// testGrantReadPermissionToGroup verifies granting read permission to a group (PRM-03).
func testGrantReadPermissionToGroup(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm03")
	groupName := helpers.UniqueTestName("group_prm03")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create group
	_, err = cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	// Grant read permission to group
	err = cli.GrantGroupPermission(shareName, groupName, "read")
	require.NoError(t, err, "Should grant read permission to group")

	// Verify permission was granted
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	assert.True(t, findPermission(perms, "group", groupName, "read"),
		"Group should have read permission")
}

// testGrantReadWritePermissionToGroup verifies granting read-write permission to a group (PRM-04).
func testGrantReadWritePermissionToGroup(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm04")
	groupName := helpers.UniqueTestName("group_prm04")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create group
	_, err = cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	// Grant read-write permission to group
	err = cli.GrantGroupPermission(shareName, groupName, "read-write")
	require.NoError(t, err, "Should grant read-write permission to group")

	// Verify permission was granted
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	assert.True(t, findPermission(perms, "group", groupName, "read-write"),
		"Group should have read-write permission")
}

// testRevokePermissionFromUser verifies revoking permission from a user (PRM-05).
func testRevokePermissionFromUser(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm05")
	userName := helpers.UniqueTestName("user_prm05")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteUser(userName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create user
	_, err = cli.CreateUser(userName, "testpassword123")
	require.NoError(t, err, "Should create user")

	// Grant permission first
	err = cli.GrantUserPermission(shareName, userName, "read-write")
	require.NoError(t, err, "Should grant permission to user")

	// Verify permission exists
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	require.True(t, findPermission(perms, "user", userName, "read-write"),
		"User should have permission before revoke")

	// Revoke permission
	err = cli.RevokeUserPermission(shareName, userName)
	require.NoError(t, err, "Should revoke permission from user")

	// Verify permission was revoked
	perms, err = cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions after revoke")
	assert.False(t, findPermission(perms, "user", userName, ""),
		"User should not have any permission after revoke")
}

// testRevokePermissionFromGroup verifies revoking permission from a group (PRM-06).
func testRevokePermissionFromGroup(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm06")
	groupName := helpers.UniqueTestName("group_prm06")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create group
	_, err = cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	// Grant permission first
	err = cli.GrantGroupPermission(shareName, groupName, "read")
	require.NoError(t, err, "Should grant permission to group")

	// Verify permission exists
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	require.True(t, findPermission(perms, "group", groupName, "read"),
		"Group should have permission before revoke")

	// Revoke permission
	err = cli.RevokeGroupPermission(shareName, groupName)
	require.NoError(t, err, "Should revoke permission from group")

	// Verify permission was revoked
	perms, err = cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions after revoke")
	assert.False(t, findPermission(perms, "group", groupName, ""),
		"Group should not have any permission after revoke")
}

// testListPermissionsForShare verifies listing permissions with both user and group (PRM-07).
func testListPermissionsForShare(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_prm07")
	userName := helpers.UniqueTestName("user_prm07")
	groupName := helpers.UniqueTestName("group_prm07")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteUser(userName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create user and group
	_, err = cli.CreateUser(userName, "testpassword123")
	require.NoError(t, err, "Should create user")

	_, err = cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	// Grant different permissions to user and group
	err = cli.GrantUserPermission(shareName, userName, "read")
	require.NoError(t, err, "Should grant permission to user")

	err = cli.GrantGroupPermission(shareName, groupName, "read-write")
	require.NoError(t, err, "Should grant permission to group")

	// List permissions and verify both appear
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")

	assert.GreaterOrEqual(t, len(perms), 2, "Should have at least 2 permissions")
	assert.True(t, findPermission(perms, "user", userName, "read"),
		"User should have read permission")
	assert.True(t, findPermission(perms, "group", groupName, "read-write"),
		"Group should have read-write permission")
}

// testEmptyPermissionsList verifies that a new share has no permissions.
func testEmptyPermissionsList(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_empty")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// List permissions - should return empty list, not error
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions without error")
	assert.Empty(t, perms, "New share should have no permissions")
}

// testPermissionOverride verifies that granting a new permission replaces the previous one.
func testPermissionOverride(t *testing.T, cli *helpers.CLIRunner, metaStore, payloadStore string) {
	shareName := "/" + helpers.UniqueTestName("share_override")
	userName := helpers.UniqueTestName("user_override")

	t.Cleanup(func() {
		_ = cli.DeleteShare(shareName)
		_ = cli.DeleteUser(userName)
	})

	// Create share
	_, err := cli.CreateShare(shareName, metaStore, payloadStore)
	require.NoError(t, err, "Should create share")

	// Create user
	_, err = cli.CreateUser(userName, "testpassword123")
	require.NoError(t, err, "Should create user")

	// Grant read permission
	err = cli.GrantUserPermission(shareName, userName, "read")
	require.NoError(t, err, "Should grant read permission")

	// Verify read permission
	perms, err := cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions")
	assert.True(t, findPermission(perms, "user", userName, "read"),
		"User should have read permission initially")

	// Grant read-write permission (should replace, not add)
	err = cli.GrantUserPermission(shareName, userName, "read-write")
	require.NoError(t, err, "Should grant read-write permission (override)")

	// Verify permission was updated
	perms, err = cli.ListSharePermissions(shareName)
	require.NoError(t, err, "Should list permissions after override")

	// Count permissions for this user
	userPermCount := 0
	for _, p := range perms {
		if p.Type == "user" && p.Name == userName {
			userPermCount++
		}
	}

	assert.Equal(t, 1, userPermCount, "Should have exactly one permission entry for user")
	assert.True(t, findPermission(perms, "user", userName, "read-write"),
		"User should have read-write permission after override")
	assert.False(t, findPermission(perms, "user", userName, "read"),
		"User should no longer have read permission after override")
}

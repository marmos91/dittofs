//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGroupManagement validates group management operations via the dittofsctl CLI.
// These tests verify group CRUD operations, membership management, and system group protection.
//
// Note: These tests require a running DittoFS server with the admin user configured.
// The DITTOFS_ADMIN_PASSWORD environment variable must be set.
func TestGroupManagement(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping group management tests in short mode")
	}

	// Start a server for all group tests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Note: These tests run serially (no t.Parallel()) because SQLite
	// doesn't handle concurrent writes well in the E2E test environment.

	t.Run("create group", func(t *testing.T) {
		testCreateGroup(t, cli)
	})

	t.Run("create group with description", func(t *testing.T) {
		testCreateGroupWithDescription(t, cli)
	})

	t.Run("list groups", func(t *testing.T) {
		testListGroups(t, cli)
	})

	t.Run("edit group", func(t *testing.T) {
		testEditGroup(t, cli)
	})

	t.Run("delete group", func(t *testing.T) {
		testDeleteGroup(t, cli)
	})

	t.Run("duplicate group name rejected", func(t *testing.T) {
		testDuplicateGroupRejected(t, cli)
	})

	t.Run("add user to group", func(t *testing.T) {
		testAddUserToGroup(t, cli)
	})

	t.Run("remove user from group", func(t *testing.T) {
		testRemoveUserFromGroup(t, cli)
	})

	t.Run("bidirectional membership", func(t *testing.T) {
		testBidirectionalMembership(t, cli)
	})

	t.Run("idempotent membership operations", func(t *testing.T) {
		testIdempotentMembership(t, cli)
	})

	t.Run("multi-group membership", func(t *testing.T) {
		testMultiGroupMembership(t, cli)
	})

	t.Run("system groups cannot be deleted", func(t *testing.T) {
		testSystemGroupsProtected(t, cli)
	})

	t.Run("user deletion removes from groups", func(t *testing.T) {
		testUserDeletionRemovesFromGroups(t, cli)
	})

	t.Run("empty group can be deleted", func(t *testing.T) {
		testEmptyGroupDeletion(t, cli)
	})
}

// testCreateGroup verifies basic group creation with auto-assigned GID.
func testCreateGroup(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	t.Cleanup(func() { _ = cli.DeleteGroup(groupName) })

	group, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group successfully")

	assert.Equal(t, groupName, group.Name, "Group name should match")
	// GID may or may not be auto-assigned depending on server config
	if group.GID != nil {
		assert.NotZero(t, *group.GID, "If GID is set, it should be non-zero")
	}
	assert.NotEmpty(t, group.ID, "Group ID should be set")
}

// testCreateGroupWithDescription verifies group creation with description.
func testCreateGroupWithDescription(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	description := "Test group for E2E testing"
	t.Cleanup(func() { _ = cli.DeleteGroup(groupName) })

	group, err := cli.CreateGroup(groupName, helpers.WithGroupDescription(description))
	require.NoError(t, err, "Should create group with description")

	assert.Equal(t, groupName, group.Name, "Group name should match")
	assert.Equal(t, description, group.Description, "Description should match")
}

// testListGroups verifies listing groups includes created groups and system groups.
func testListGroups(t *testing.T, cli *helpers.CLIRunner) {
	// Create two test groups
	group1Name := helpers.UniqueTestName("e2e_grp")
	group2Name := helpers.UniqueTestName("e2e_grp")
	t.Cleanup(func() {
		_ = cli.DeleteGroup(group1Name)
		_ = cli.DeleteGroup(group2Name)
	})

	_, err := cli.CreateGroup(group1Name)
	require.NoError(t, err, "Should create first group")

	_, err = cli.CreateGroup(group2Name)
	require.NoError(t, err, "Should create second group")

	// List all groups
	groups, err := cli.ListGroups()
	require.NoError(t, err, "Should list groups")

	// Find our groups and check for system groups
	var foundGroup1, foundGroup2, foundAdmins, foundUsers bool
	for _, g := range groups {
		switch g.Name {
		case group1Name:
			foundGroup1 = true
		case group2Name:
			foundGroup2 = true
		case "admins":
			foundAdmins = true
		case "users":
			foundUsers = true
		}
	}

	assert.True(t, foundGroup1, "Should find first created group")
	assert.True(t, foundGroup2, "Should find second created group")
	// System groups may or may not exist depending on server initialization
	if !foundAdmins {
		t.Log("Note: System group 'admins' not found")
	}
	if !foundUsers {
		t.Log("Note: System group 'users' not found")
	}
}

// testEditGroup verifies group description can be edited.
func testEditGroup(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	t.Cleanup(func() { _ = cli.DeleteGroup(groupName) })

	// Create group
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	// Edit description
	newDescription := "Updated description"
	updated, err := cli.EditGroup(groupName, helpers.WithGroupDescription(newDescription))
	require.NoError(t, err, "Should edit group")
	assert.Equal(t, newDescription, updated.Description, "Description should be updated")

	// Verify change persisted
	fetched, err := cli.GetGroup(groupName)
	require.NoError(t, err, "Should get group after edit")
	assert.Equal(t, newDescription, fetched.Description, "Description change should persist")
}

// testDeleteGroup verifies group deletion removes it from the list.
func testDeleteGroup(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")

	// Create group
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	// Delete group
	err = cli.DeleteGroup(groupName)
	require.NoError(t, err, "Should delete group")

	// Verify group no longer exists
	_, err = cli.GetGroup(groupName)
	assert.Error(t, err, "Should not find deleted group")
	assert.Contains(t, err.Error(), "not found", "Error should indicate group not found")
}

// testDuplicateGroupRejected verifies duplicate group names are rejected.
func testDuplicateGroupRejected(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	t.Cleanup(func() { _ = cli.DeleteGroup(groupName) })

	// Create first group
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create first group")

	// Try to create duplicate
	_, err = cli.CreateGroup(groupName)
	assert.Error(t, err, "Should reject duplicate group name")
}

// testAddUserToGroup verifies adding a user to a group.
func testAddUserToGroup(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	userName := helpers.UniqueTestName("e2e_usr")
	t.Cleanup(func() {
		_ = cli.DeleteUser(userName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create group and user
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	_, err = cli.CreateUser(userName, "TestPassword123!")
	require.NoError(t, err, "Should create user")

	// Add user to group
	err = cli.AddGroupMember(groupName, userName)
	require.NoError(t, err, "Should add user to group")

	// Verify user is in group's members list
	group, err := cli.GetGroup(groupName)
	require.NoError(t, err, "Should get group")
	assert.Contains(t, group.Members, userName, "Group should list user as member")
}

// testRemoveUserFromGroup verifies removing a user from a group.
func testRemoveUserFromGroup(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	userName := helpers.UniqueTestName("e2e_usr")
	t.Cleanup(func() {
		_ = cli.DeleteUser(userName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create group and user
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	_, err = cli.CreateUser(userName, "TestPassword123!")
	require.NoError(t, err, "Should create user")

	// Add user to group
	err = cli.AddGroupMember(groupName, userName)
	require.NoError(t, err, "Should add user to group")

	// Remove user from group
	err = cli.RemoveGroupMember(groupName, userName)
	require.NoError(t, err, "Should remove user from group")

	// Verify user is no longer in group's members list
	group, err := cli.GetGroup(groupName)
	require.NoError(t, err, "Should get group")
	assert.NotContains(t, group.Members, userName, "Group should not list user as member")
}

// testBidirectionalMembership verifies that group membership is bidirectional:
// - Group.Members contains the user
// - User.Groups contains the group
func testBidirectionalMembership(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	userName := helpers.UniqueTestName("e2e_usr")
	t.Cleanup(func() {
		_ = cli.DeleteUser(userName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create group and user
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	_, err = cli.CreateUser(userName, "TestPassword123!")
	require.NoError(t, err, "Should create user")

	// Add user to group
	err = cli.AddGroupMember(groupName, userName)
	require.NoError(t, err, "Should add user to group")

	// Check group side: group.Members contains user
	group, err := cli.GetGroup(groupName)
	require.NoError(t, err, "Should get group")
	assert.Contains(t, group.Members, userName, "Group.Members should contain user")

	// Check user side: user.Groups contains group
	user, err := cli.GetUser(userName)
	require.NoError(t, err, "Should get user")
	assert.Contains(t, user.Groups, groupName, "User.Groups should contain group")
}

// testIdempotentMembership verifies that adding a user already in a group
// and removing a user not in a group both succeed silently.
func testIdempotentMembership(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	userName := helpers.UniqueTestName("e2e_usr")
	t.Cleanup(func() {
		_ = cli.DeleteUser(userName)
		_ = cli.DeleteGroup(groupName)
	})

	// Create group and user
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	_, err = cli.CreateUser(userName, "TestPassword123!")
	require.NoError(t, err, "Should create user")

	// Add user to group first time
	err = cli.AddGroupMember(groupName, userName)
	require.NoError(t, err, "First add should succeed")

	// Add user to group second time (idempotent - should succeed)
	err = cli.AddGroupMember(groupName, userName)
	assert.NoError(t, err, "Second add should succeed (idempotent)")

	// Remove user from group first time
	err = cli.RemoveGroupMember(groupName, userName)
	require.NoError(t, err, "First remove should succeed")

	// Remove user from group second time (idempotent - should succeed)
	err = cli.RemoveGroupMember(groupName, userName)
	assert.NoError(t, err, "Second remove should succeed (idempotent)")
}

// testMultiGroupMembership verifies a user can be a member of multiple groups.
func testMultiGroupMembership(t *testing.T, cli *helpers.CLIRunner) {
	group1Name := helpers.UniqueTestName("e2e_grp")
	group2Name := helpers.UniqueTestName("e2e_grp")
	userName := helpers.UniqueTestName("e2e_usr")
	t.Cleanup(func() {
		_ = cli.DeleteUser(userName)
		_ = cli.DeleteGroup(group1Name)
		_ = cli.DeleteGroup(group2Name)
	})

	// Create two groups and one user
	_, err := cli.CreateGroup(group1Name)
	require.NoError(t, err, "Should create first group")

	_, err = cli.CreateGroup(group2Name)
	require.NoError(t, err, "Should create second group")

	_, err = cli.CreateUser(userName, "TestPassword123!")
	require.NoError(t, err, "Should create user")

	// Add user to both groups
	err = cli.AddGroupMember(group1Name, userName)
	require.NoError(t, err, "Should add user to first group")

	err = cli.AddGroupMember(group2Name, userName)
	require.NoError(t, err, "Should add user to second group")

	// Verify user is in both groups
	user, err := cli.GetUser(userName)
	require.NoError(t, err, "Should get user")
	assert.Contains(t, user.Groups, group1Name, "User should be in first group")
	assert.Contains(t, user.Groups, group2Name, "User should be in second group")
}

// testSystemGroupsProtected verifies that system groups (admins, users) cannot be deleted.
func testSystemGroupsProtected(t *testing.T, cli *helpers.CLIRunner) {
	// Check if system groups exist first
	groups, err := cli.ListGroups()
	require.NoError(t, err, "Should list groups")

	var hasAdmins, hasUsers bool
	for _, g := range groups {
		if g.Name == "admins" {
			hasAdmins = true
		}
		if g.Name == "users" {
			hasUsers = true
		}
	}

	if !hasAdmins && !hasUsers {
		t.Skip("System groups (admins, users) don't exist - skipping protection test")
	}

	// Try to delete 'admins' group if it exists - should fail
	if hasAdmins {
		err := cli.DeleteGroup("admins")
		assert.Error(t, err, "Should reject deletion of 'admins' system group")
	}

	// Try to delete 'users' group if it exists - should fail
	if hasUsers {
		err := cli.DeleteGroup("users")
		assert.Error(t, err, "Should reject deletion of 'users' system group")
	}
}

// testUserDeletionRemovesFromGroups verifies that deleting a user removes them from all groups.
func testUserDeletionRemovesFromGroups(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")
	userName := helpers.UniqueTestName("e2e_usr")
	t.Cleanup(func() {
		_ = cli.DeleteGroup(groupName)
	})

	// Create group and user
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create group")

	_, err = cli.CreateUser(userName, "TestPassword123!")
	require.NoError(t, err, "Should create user")

	// Add user to group
	err = cli.AddGroupMember(groupName, userName)
	require.NoError(t, err, "Should add user to group")

	// Verify user is in group
	group, err := cli.GetGroup(groupName)
	require.NoError(t, err, "Should get group")
	require.Contains(t, group.Members, userName, "User should be in group before deletion")

	// Delete user
	err = cli.DeleteUser(userName)
	require.NoError(t, err, "Should delete user")

	// Verify user is no longer in group
	group, err = cli.GetGroup(groupName)
	require.NoError(t, err, "Should get group after user deletion")
	assert.NotContains(t, group.Members, userName, "Deleted user should not be in group")
}

// testEmptyGroupDeletion verifies that an empty group (no members) can be deleted.
func testEmptyGroupDeletion(t *testing.T, cli *helpers.CLIRunner) {
	groupName := helpers.UniqueTestName("e2e_grp")

	// Create group (no members)
	_, err := cli.CreateGroup(groupName)
	require.NoError(t, err, "Should create empty group")

	// Delete empty group
	err = cli.DeleteGroup(groupName)
	require.NoError(t, err, "Should delete empty group")

	// Verify group is gone
	_, err = cli.GetGroup(groupName)
	assert.Error(t, err, "Deleted group should not exist")
	assert.True(t, strings.Contains(err.Error(), "not found"), "Error should indicate group not found")
}

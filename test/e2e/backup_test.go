//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackupRestore tests control plane backup and restore operations.
// Tests are sequential since each manages server lifecycle.
func TestBackupRestore(t *testing.T) {
	// These tests are sequential - each needs server control
	// Do not use t.Parallel() at suite level

	t.Run("BAK-01 backup native format creates valid file", func(t *testing.T) {
		// Start server
		server := helpers.StartServerProcess(t, "")
		defer server.ForceKill()

		// Create test data via CLI
		cli := helpers.LoginAsAdmin(t, server.APIURL())

		// Create a test user
		username := helpers.UniqueTestName("bakuser")
		_, err := cli.CreateUser(username, "testpass123", helpers.WithEmail("backup@test.com"))
		require.NoError(t, err, "should create test user")

		// Create a test group
		groupName := helpers.UniqueTestName("bakgroup")
		_, err = cli.CreateGroup(groupName, helpers.WithGroupDescription("test group for backup"))
		require.NoError(t, err, "should create test group")

		// Run backup with native format
		backupDir := t.TempDir()
		backupFile := filepath.Join(backupDir, "backup.db")
		err = helpers.RunDittofsBackup(t, backupFile, server.ConfigFile(), "native")
		require.NoError(t, err, "backup should succeed")

		// Verify backup file exists and has content
		stat, err := os.Stat(backupFile)
		require.NoError(t, err, "backup file should exist")
		assert.Greater(t, stat.Size(), int64(0), "backup file should have content")

		// Stop server gracefully
		err = server.StopGracefully()
		require.NoError(t, err, "server should stop gracefully")
	})

	t.Run("BAK-01 backup as JSON contains test data", func(t *testing.T) {
		// Start server
		server := helpers.StartServerProcess(t, "")
		defer server.ForceKill()

		// Create test data via CLI
		cli := helpers.LoginAsAdmin(t, server.APIURL())

		// Create a test user with specific details
		username := helpers.UniqueTestName("jsonuser")
		_, err := cli.CreateUser(username, "testpass123",
			helpers.WithEmail("json@test.com"),
			helpers.WithRole("user"))
		require.NoError(t, err, "should create test user")

		// Create a test group
		groupName := helpers.UniqueTestName("jsongroup")
		_, err = cli.CreateGroup(groupName, helpers.WithGroupDescription("JSON backup test"))
		require.NoError(t, err, "should create test group")

		// Run backup with JSON format
		backupDir := t.TempDir()
		backupFile := filepath.Join(backupDir, "backup.json")
		err = helpers.RunDittofsBackup(t, backupFile, server.ConfigFile(), "json")
		require.NoError(t, err, "JSON backup should succeed")

		// Parse and verify backup contents
		backup, err := helpers.ParseBackupFile(t, backupFile)
		require.NoError(t, err, "should parse backup file")

		// Verify timestamp and version are set
		assert.NotEmpty(t, backup.Timestamp, "timestamp should be set")
		assert.NotEmpty(t, backup.Version, "version should be set")

		// Find our test user in backup
		var foundUser *helpers.BackupUser
		for i := range backup.Users {
			if backup.Users[i].Username == username {
				foundUser = &backup.Users[i]
				break
			}
		}
		require.NotNil(t, foundUser, "test user should be in backup")
		assert.Equal(t, "json@test.com", foundUser.Email)
		assert.Equal(t, "user", foundUser.Role)

		// Find our test group in backup
		var foundGroup *helpers.BackupGroup
		for i := range backup.Groups {
			if backup.Groups[i].Name == groupName {
				foundGroup = &backup.Groups[i]
				break
			}
		}
		require.NotNil(t, foundGroup, "test group should be in backup")
		assert.Equal(t, "JSON backup test", foundGroup.Description)

		// Stop server gracefully
		err = server.StopGracefully()
		require.NoError(t, err, "server should stop gracefully")
	})

	t.Run("BAK-02 backup includes API-created resources", func(t *testing.T) {
		// Start server
		server := helpers.StartServerProcess(t, "")
		defer server.ForceKill()

		// Login as admin
		cli := helpers.LoginAsAdmin(t, server.APIURL())

		// Note: Config-file defined stores/shares are NOT persisted to DB.
		// Only stores/shares created via API are included in backup.
		// Create resources via CLI to ensure they're in the DB.

		// Create a metadata store via CLI
		metaStoreName := helpers.UniqueTestName("bakmetastore")
		_, err := cli.CreateMetadataStore(metaStoreName, "memory")
		require.NoError(t, err, "should create metadata store")

		// Create a payload store via CLI
		payloadStoreName := helpers.UniqueTestName("bakpayloadstore")
		_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
		require.NoError(t, err, "should create payload store")

		// Create a share using the CLI-created stores
		shareName := "/" + helpers.UniqueTestName("bakshare")
		_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "should create share")

		// Run backup
		backupDir := t.TempDir()
		backupFile := filepath.Join(backupDir, "backup.json")
		err = helpers.RunDittofsBackup(t, backupFile, server.ConfigFile(), "json")
		require.NoError(t, err, "backup should succeed")

		backup, err := helpers.ParseBackupFile(t, backupFile)
		require.NoError(t, err, "should parse backup file")

		// Verify API-created shares are in backup
		assert.NotEmpty(t, backup.Shares, "backup should contain API-created shares")

		// Verify API-created metadata stores are included
		assert.NotEmpty(t, backup.MetadataStores, "backup should contain API-created metadata stores")

		// Verify API-created payload stores are included
		assert.NotEmpty(t, backup.PayloadStores, "backup should contain API-created payload stores")

		// Also verify adapters (which are auto-created and stored in DB)
		assert.NotEmpty(t, backup.Adapters, "backup should contain adapters")

		// Stop server gracefully
		_ = server.StopGracefully()
	})

	t.Run("BAK-03 backup data integrity - all created resources present", func(t *testing.T) {
		// Start server
		server := helpers.StartServerProcess(t, "")
		defer server.ForceKill()

		cli := helpers.LoginAsAdmin(t, server.APIURL())

		// Create comprehensive test data
		// 1. Create a group
		groupName := helpers.UniqueTestName("integritygrp")
		_, err := cli.CreateGroup(groupName, helpers.WithGroupDescription("Integrity test"))
		require.NoError(t, err, "should create group")

		// 2. Create a user in the group
		username := helpers.UniqueTestName("integrityusr")
		_, err = cli.CreateUser(username, "testpass123",
			helpers.WithEmail("integrity@test.com"),
			helpers.WithGroups(groupName))
		require.NoError(t, err, "should create user")

		// 3. Create stores via API (config-defined stores are NOT in DB)
		metaStoreName := helpers.UniqueTestName("metamem")
		_, err = cli.CreateMetadataStore(metaStoreName, "memory")
		require.NoError(t, err, "should create metadata store")

		payloadStoreName := helpers.UniqueTestName("payloadmem")
		_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
		require.NoError(t, err, "should create payload store")

		// 4. Create a share using the API-created stores
		shareName := "/" + helpers.UniqueTestName("intshare")
		_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName,
			helpers.WithShareDescription("Integrity test share"))
		require.NoError(t, err, "should create share")

		// 5. Grant permission on the share
		err = cli.GrantUserPermission(shareName, username, "read-write")
		require.NoError(t, err, "should grant permission")

		// Run backup
		backupDir := t.TempDir()
		backupFile := filepath.Join(backupDir, "backup.json")
		err = helpers.RunDittofsBackup(t, backupFile, server.ConfigFile(), "json")
		require.NoError(t, err, "backup should succeed")

		// Parse backup
		backup, err := helpers.ParseBackupFile(t, backupFile)
		require.NoError(t, err, "should parse backup file")

		// Verify all API-created resources are in backup
		// 1. User
		var foundUser *helpers.BackupUser
		for i := range backup.Users {
			if backup.Users[i].Username == username {
				foundUser = &backup.Users[i]
				break
			}
		}
		require.NotNil(t, foundUser, "user should be in backup")
		assert.Equal(t, "integrity@test.com", foundUser.Email)

		// 2. Group
		var foundGroup *helpers.BackupGroup
		for i := range backup.Groups {
			if backup.Groups[i].Name == groupName {
				foundGroup = &backup.Groups[i]
				break
			}
		}
		require.NotNil(t, foundGroup, "group should be in backup")
		assert.Equal(t, "Integrity test", foundGroup.Description)

		// 3. Metadata store - only API-created stores are in backup, not config-defined ones
		assert.GreaterOrEqual(t, len(backup.MetadataStores), 1, "should have at least 1 metadata store (API-created)")

		// 4. Payload store - only API-created stores are in backup
		assert.GreaterOrEqual(t, len(backup.PayloadStores), 1, "should have at least 1 payload store (API-created)")

		// 5. Share - only API-created shares are in backup, not config-defined ones
		assert.GreaterOrEqual(t, len(backup.Shares), 1, "should have at least 1 share (API-created)")

		// Stop server gracefully
		err = server.StopGracefully()
		require.NoError(t, err, "server should stop gracefully")
	})

	t.Run("BAK-04 invalid config fails gracefully", func(t *testing.T) {
		// Create an invalid config file
		tempDir := t.TempDir()
		invalidConfig := filepath.Join(tempDir, "invalid.yaml")
		err := os.WriteFile(invalidConfig, []byte("this is not valid yaml: [[["), 0644)
		require.NoError(t, err, "should write invalid config")

		// Run backup with invalid config
		backupFile := filepath.Join(tempDir, "backup.db")
		err = helpers.RunDittofsBackup(t, backupFile, invalidConfig, "native")

		// Should fail with clear error
		assert.Error(t, err, "backup with invalid config should fail")
		assert.Contains(t, err.Error(), "failed", "error message should indicate failure")

		// Backup file should not exist (or be empty)
		_, statErr := os.Stat(backupFile)
		if statErr == nil {
			// If file exists, it should be empty (partial write)
			stat, _ := os.Stat(backupFile)
			assert.Equal(t, int64(0), stat.Size(), "backup file should be empty on failure")
		}
	})

	t.Run("BAK-04 nonexistent database fails gracefully", func(t *testing.T) {
		// Create a config pointing to a non-existent database
		tempDir := t.TempDir()
		configContent := `
logging:
  level: ERROR
  format: text

controlplane:
  port: 9999
  jwt:
    secret: "test-secret-key-for-e2e-testing-only-must-be-32-chars"

database:
  type: sqlite
  sqlite:
    path: "/nonexistent/path/database.db"

cache:
  path: "/tmp/cache"
  size: 104857600
`
		configFile := filepath.Join(tempDir, "config.yaml")
		err := os.WriteFile(configFile, []byte(configContent), 0644)
		require.NoError(t, err, "should write config")

		// Run backup - should fail because database doesn't exist
		backupFile := filepath.Join(tempDir, "backup.db")
		err = helpers.RunDittofsBackup(t, backupFile, configFile, "native")

		// Should fail with error
		assert.Error(t, err, "backup with nonexistent database should fail")
	})
}

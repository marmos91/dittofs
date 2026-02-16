//go:build e2e

// Package e2e contains end-to-end tests for DittoFS share management.
//
// This file tests Share CRUD operations via the dfsctl CLI.
// Covers requirements SHR-01 through SHR-04 from the E2E test plan.
//
// NOTE: SHR-05/SHR-06 (soft delete with deferred cleanup) are NOT testable
// because the server implements hard delete, not soft delete.
package e2e

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharesCRUD tests comprehensive share management via CLI.
// Uses a shared server process and stores for all subtests.
func TestSharesCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping shares tests in short mode")
	}

	// Start server with automatic cleanup on test completion
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	serverURL := sp.APIURL()

	// Login as admin and get CLI runner
	cli := helpers.LoginAsAdmin(t, serverURL)

	// Create shared stores for all subtests
	metaStoreName := helpers.UniqueTestName("share_meta")
	payloadStoreName := helpers.UniqueTestName("share_payload")

	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create shared metadata store")

	_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
	require.NoError(t, err, "Should create shared payload store")

	// Cleanup stores after all tests (registered first, runs last)
	t.Cleanup(func() {
		_ = cli.DeleteMetadataStore(metaStoreName)
		_ = cli.DeletePayloadStore(payloadStoreName)
	})

	// SHR-01: Create share with assigned stores
	t.Run("SHR-01 create share with assigned stores", func(t *testing.T) {
		t.Parallel()

		shareName := "/" + helpers.UniqueTestName("share_basic")

		t.Cleanup(func() {
			_ = cli.DeleteShare(shareName)
		})

		share, err := cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create share with assigned stores")

		assert.Equal(t, shareName, share.Name, "Share name should match")
		// Store IDs are UUIDs, not names - just verify they're set
		assert.NotEmpty(t, share.MetadataStoreID, "Metadata store ID should be set")
		assert.NotEmpty(t, share.PayloadStoreID, "Payload store ID should be set")
	})

	// SHR-01: Create share with options
	t.Run("SHR-01 create share with options", func(t *testing.T) {
		t.Parallel()

		shareName := "/" + helpers.UniqueTestName("share_opts")

		t.Cleanup(func() {
			_ = cli.DeleteShare(shareName)
		})

		share, err := cli.CreateShare(shareName, metaStoreName, payloadStoreName,
			helpers.WithShareReadOnly(true),
			helpers.WithShareDefaultPermission("read"),
			helpers.WithShareDescription("Test share with options"),
		)
		require.NoError(t, err, "Should create share with options")

		assert.Equal(t, shareName, share.Name, "Share name should match")
		assert.True(t, share.ReadOnly, "Share should be read-only")
		assert.Equal(t, "read", share.DefaultPermission, "Default permission should be 'read'")
	})

	// SHR-02: List shares
	t.Run("SHR-02 list shares", func(t *testing.T) {
		t.Parallel()

		share1Name := "/" + helpers.UniqueTestName("share_list1")
		share2Name := "/" + helpers.UniqueTestName("share_list2")

		t.Cleanup(func() {
			_ = cli.DeleteShare(share1Name)
			_ = cli.DeleteShare(share2Name)
		})

		// Create two shares
		_, err := cli.CreateShare(share1Name, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create first share")

		_, err = cli.CreateShare(share2Name, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create second share")

		// List all shares
		shares, err := cli.ListShares()
		require.NoError(t, err, "Should list shares")

		// Find our created shares
		var found1, found2 bool
		for _, s := range shares {
			if s.Name == share1Name {
				found1 = true
			}
			if s.Name == share2Name {
				found2 = true
			}
		}

		assert.True(t, found1, "Should find first share in list")
		assert.True(t, found2, "Should find second share in list")
	})

	// SHR-03: Edit share configuration
	t.Run("SHR-03 edit share configuration", func(t *testing.T) {
		t.Parallel()

		shareName := "/" + helpers.UniqueTestName("share_edit")

		t.Cleanup(func() {
			_ = cli.DeleteShare(shareName)
		})

		// Create share with default settings
		_, err := cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create share")

		// Edit share
		updated, err := cli.EditShare(shareName,
			helpers.WithShareReadOnly(true),
			helpers.WithShareDefaultPermission("admin"),
		)
		require.NoError(t, err, "Should edit share configuration")

		assert.Equal(t, shareName, updated.Name, "Share name should remain unchanged")
		assert.True(t, updated.ReadOnly, "Share should now be read-only")
		assert.Equal(t, "admin", updated.DefaultPermission, "Default permission should be 'admin'")

		// Verify changes persisted by fetching share
		fetched, err := cli.GetShare(shareName)
		require.NoError(t, err, "Should get edited share")
		assert.True(t, fetched.ReadOnly, "Persisted share should be read-only")
		assert.Equal(t, "admin", fetched.DefaultPermission, "Persisted default permission should be 'admin'")
	})

	// SHR-04: Delete share
	// NOTE: Not running in parallel because delete operations can conflict
	// with other tests that may be creating/listing shares concurrently.
	t.Run("SHR-04 delete share", func(t *testing.T) {
		shareName := "/" + helpers.UniqueTestName("share_del")

		// Create share
		_, err := cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create share")

		// Delete share
		err = cli.DeleteShare(shareName)
		require.NoError(t, err, "Should delete share")

		// Verify share is gone
		_, err = cli.GetShare(shareName)
		require.Error(t, err, "Should fail to get deleted share")
		assert.Contains(t, err.Error(), "not found", "Error should indicate share not found")
	})

	// Duplicate share name rejected
	t.Run("duplicate share name rejected", func(t *testing.T) {
		t.Parallel()

		shareName := "/" + helpers.UniqueTestName("share_dup")

		t.Cleanup(func() {
			_ = cli.DeleteShare(shareName)
		})

		// Create share
		_, err := cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create share")

		// Try to create again with same name
		_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.Error(t, err, "Should reject duplicate share name")

		// Error should indicate conflict/already exists
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "exists") ||
				strings.Contains(errStr, "conflict") ||
				strings.Contains(errStr, "duplicate"),
			"Error should indicate share already exists: %s", err.Error())
	})

	// Create share with nonexistent store fails
	t.Run("create share with nonexistent store fails", func(t *testing.T) {
		t.Parallel()

		shareName := "/" + helpers.UniqueTestName("share_bad")
		fakeStoreName := "nonexistent_store_12345"

		// Try to create share with nonexistent metadata store
		_, err := cli.CreateShare(shareName, fakeStoreName, payloadStoreName)
		require.Error(t, err, "Should fail to create share with nonexistent metadata store")

		// Error should indicate store not found
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "not found") ||
				strings.Contains(errStr, "does not exist") ||
				strings.Contains(errStr, "invalid"),
			"Error should indicate store not found: %s", err.Error())
	})

	// Get share by name
	t.Run("get share by name", func(t *testing.T) {
		t.Parallel()

		shareName := "/" + helpers.UniqueTestName("share_get")

		t.Cleanup(func() {
			_ = cli.DeleteShare(shareName)
		})

		// Create share
		created, err := cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create share")

		// Get share by name
		fetched, err := cli.GetShare(shareName)
		require.NoError(t, err, "Should get share by name")

		assert.Equal(t, created.Name, fetched.Name, "Names should match")
		assert.Equal(t, created.MetadataStoreID, fetched.MetadataStoreID, "Metadata store IDs should match")
		assert.Equal(t, created.PayloadStoreID, fetched.PayloadStoreID, "Payload store IDs should match")
	})
}

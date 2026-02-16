//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPayloadStoresCRUD validates payload store management operations via the dfsctl CLI.
// These tests verify creation, listing, editing, and deletion of payload stores,
// including proper error handling for stores in use by shares.
//
// Covers requirements PLS-01 through PLS-07 from the E2E test plan.
func TestPayloadStoresCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping payload stores tests in short mode")
	}

	// Start server with automatic cleanup on test completion
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	serverURL := sp.APIURL()

	// Login as admin and get CLI runner
	cli := helpers.LoginAsAdmin(t, serverURL)

	// PLS-01: Create memory payload store
	t.Run("create memory store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("payload_mem")
		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(storeName)
		})

		store, err := cli.CreatePayloadStore(storeName, "memory")
		require.NoError(t, err, "Should create memory payload store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "memory", store.Type, "Store type should be memory")
	})

	// PLS-02: Create filesystem payload store
	t.Run("create filesystem store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("payload_fs")
		storagePath := filepath.Join(t.TempDir(), "content")

		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(storeName)
		})

		store, err := cli.CreatePayloadStore(storeName, "filesystem",
			helpers.WithPayloadPath(storagePath))
		require.NoError(t, err, "Should create filesystem payload store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "filesystem", store.Type, "Store type should be filesystem")
	})

	// PLS-03: Create S3 payload store
	t.Run("create s3 store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("payload_s3")

		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(storeName)
		})

		// Use raw config to test S3 store creation without actual S3 connectivity
		// This tests CLI acceptance, not S3 connectivity
		s3Config := `{"bucket":"test-bucket","region":"us-east-1"}`
		store, err := cli.CreatePayloadStore(storeName, "s3",
			helpers.WithPayloadRawConfig(s3Config))
		require.NoError(t, err, "Should create S3 payload store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "s3", store.Type, "Store type should be s3")
	})

	// PLS-04: List payload stores
	t.Run("list stores", func(t *testing.T) {
		t.Parallel()

		store1Name := helpers.UniqueTestName("payload_list1")
		store2Name := helpers.UniqueTestName("payload_list2")

		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(store1Name)
			_ = cli.DeletePayloadStore(store2Name)
		})

		// Create two stores
		_, err := cli.CreatePayloadStore(store1Name, "memory")
		require.NoError(t, err, "Should create first store")

		_, err = cli.CreatePayloadStore(store2Name, "memory")
		require.NoError(t, err, "Should create second store")

		// List all stores
		stores, err := cli.ListPayloadStores()
		require.NoError(t, err, "Should list payload stores")

		// Find our created stores
		var found1, found2 bool
		for _, s := range stores {
			if s.Name == store1Name {
				found1 = true
			}
			if s.Name == store2Name {
				found2 = true
			}
		}

		assert.True(t, found1, "Should find first store in list")
		assert.True(t, found2, "Should find second store in list")
	})

	// PLS-05: Edit filesystem store path
	t.Run("edit filesystem store path", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("payload_edit")
		originalPath := filepath.Join(t.TempDir(), "original")
		newPath := filepath.Join(t.TempDir(), "updated")

		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(storeName)
		})

		// Create filesystem store
		_, err := cli.CreatePayloadStore(storeName, "filesystem",
			helpers.WithPayloadPath(originalPath))
		require.NoError(t, err, "Should create filesystem store")

		// Edit with new path
		updated, err := cli.EditPayloadStore(storeName,
			helpers.WithPayloadPath(newPath))
		require.NoError(t, err, "Should edit payload store path")

		assert.Equal(t, storeName, updated.Name, "Store name should remain unchanged")
		assert.Equal(t, "filesystem", updated.Type, "Store type should remain filesystem")
	})

	// PLS-06: Delete store
	t.Run("delete store", func(t *testing.T) {
		// Not parallel - write operations can cause SQLite lock contention
		storeName := helpers.UniqueTestName("payload_del")

		// Create store
		_, err := cli.CreatePayloadStore(storeName, "memory")
		require.NoError(t, err, "Should create store")

		// Delete store
		err = cli.DeletePayloadStore(storeName)
		require.NoError(t, err, "Should delete store")

		// Verify store no longer exists
		_, err = cli.GetPayloadStore(storeName)
		assert.Error(t, err, "Should fail to get deleted store")
		assert.Contains(t, err.Error(), "not found", "Error should indicate store not found")
	})

	// Duplicate name rejection
	t.Run("duplicate name rejected", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("payload_dup")

		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(storeName)
		})

		// Create first store
		_, err := cli.CreatePayloadStore(storeName, "memory")
		require.NoError(t, err, "Should create first store")

		// Try to create with same name
		_, err = cli.CreatePayloadStore(storeName, "memory")
		require.Error(t, err, "Should reject duplicate store name")

		// Error should indicate conflict/already exists
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "already exists") ||
				strings.Contains(errStr, "conflict") ||
				strings.Contains(errStr, "duplicate"),
			"Error should indicate store already exists: %s", err.Error())
	})

	// PLS-07: Cannot delete store in use by share
	// This test is NOT parallel because it creates and deletes a share
	t.Run("cannot delete store in use", func(t *testing.T) {
		metaStoreName := helpers.UniqueTestName("meta_inuse")
		payloadStoreName := helpers.UniqueTestName("payload_inuse")
		shareName := "/" + helpers.UniqueTestName("share_inuse")

		t.Cleanup(func() {
			// Cleanup in reverse dependency order
			_ = cli.DeleteShare(shareName)
			_ = cli.DeletePayloadStore(payloadStoreName)
			_ = cli.DeleteMetadataStore(metaStoreName)
		})

		// Create metadata store for the share
		_, err := cli.CreateMetadataStore(metaStoreName, "memory")
		require.NoError(t, err, "Should create metadata store")

		// Create payload store
		_, err = cli.CreatePayloadStore(payloadStoreName, "memory")
		require.NoError(t, err, "Should create payload store")

		// Create share referencing both stores
		_, err = cli.CreateShare(shareName, metaStoreName, payloadStoreName)
		require.NoError(t, err, "Should create share")

		// Try to delete payload store - should fail because share is using it
		err = cli.DeletePayloadStore(payloadStoreName)
		require.Error(t, err, "Should reject deletion of payload store in use")

		// Error should indicate store is in use
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "in use") ||
				strings.Contains(errStr, "used by") ||
				strings.Contains(errStr, "referenced"),
			"Error should indicate store is in use: %s", err.Error())

		// Delete share first
		err = cli.DeleteShare(shareName)
		require.NoError(t, err, "Should delete share")

		// Now deletion should succeed
		err = cli.DeletePayloadStore(payloadStoreName)
		require.NoError(t, err, "Should delete payload store after share deletion")
	})

	// Additional test: Get store by name
	t.Run("get store by name", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("payload_get")

		t.Cleanup(func() {
			_ = cli.DeletePayloadStore(storeName)
		})

		// Create store
		created, err := cli.CreatePayloadStore(storeName, "memory")
		require.NoError(t, err, "Should create store")

		// Get store by name
		fetched, err := cli.GetPayloadStore(storeName)
		require.NoError(t, err, "Should get store by name")

		assert.Equal(t, created.Name, fetched.Name, "Names should match")
		assert.Equal(t, created.Type, fetched.Type, "Types should match")
	})
}

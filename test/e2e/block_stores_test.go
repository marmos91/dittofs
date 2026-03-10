//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBlockStoresCRUD validates block store management operations via the dfsctl CLI.
// These tests verify creation, listing, editing, and deletion of local and remote
// block stores, including proper error handling for stores in use by shares.
func TestBlockStoresCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping block stores tests in short mode")
	}

	// Start server with automatic cleanup on test completion
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	serverURL := sp.APIURL()

	// Login as admin and get CLI runner
	cli := helpers.LoginAsAdmin(t, serverURL)

	// =====================================================================
	// Local Block Store Tests
	// =====================================================================

	// Create memory block store (local)
	t.Run("local/create memory store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("block_mem")
		t.Cleanup(func() {
			_ = cli.DeleteLocalBlockStore(storeName)
		})

		store, err := cli.CreateLocalBlockStore(storeName, "memory")
		require.NoError(t, err, "Should create memory block store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "memory", store.Type, "Store type should be memory")
	})

	// Create fs block store (local)
	t.Run("local/create fs store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("block_fs")
		t.Cleanup(func() {
			_ = cli.DeleteLocalBlockStore(storeName)
		})

		fsPath := t.TempDir()
		store, err := cli.CreateLocalBlockStore(storeName, "fs",
			helpers.WithBlockRawConfig(`{"path":"`+fsPath+`"}`))
		require.NoError(t, err, "Should create fs block store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "fs", store.Type, "Store type should be fs")
	})

	// List local block stores
	t.Run("local/list stores", func(t *testing.T) {
		t.Parallel()

		store1Name := helpers.UniqueTestName("block_list1")
		store2Name := helpers.UniqueTestName("block_list2")

		t.Cleanup(func() {
			_ = cli.DeleteLocalBlockStore(store1Name)
			_ = cli.DeleteLocalBlockStore(store2Name)
		})

		// Create two stores
		_, err := cli.CreateLocalBlockStore(store1Name, "memory")
		require.NoError(t, err, "Should create first store")

		_, err = cli.CreateLocalBlockStore(store2Name, "memory")
		require.NoError(t, err, "Should create second store")

		// List all stores
		stores, err := cli.ListLocalBlockStores()
		require.NoError(t, err, "Should list block stores")

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

	// Delete local store
	t.Run("local/delete store", func(t *testing.T) {
		storeName := helpers.UniqueTestName("block_del")

		// Create store
		_, err := cli.CreateLocalBlockStore(storeName, "memory")
		require.NoError(t, err, "Should create store")

		// Delete store
		err = cli.DeleteLocalBlockStore(storeName)
		require.NoError(t, err, "Should delete store")

		// Verify store no longer exists
		_, err = cli.GetLocalBlockStore(storeName)
		assert.Error(t, err, "Should fail to get deleted store")
		assert.Contains(t, err.Error(), "not found", "Error should indicate store not found")
	})

	// Duplicate name rejection (local)
	t.Run("local/duplicate name rejected", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("block_dup")

		t.Cleanup(func() {
			_ = cli.DeleteLocalBlockStore(storeName)
		})

		// Create first store
		_, err := cli.CreateLocalBlockStore(storeName, "memory")
		require.NoError(t, err, "Should create first store")

		// Try to create with same name
		_, err = cli.CreateLocalBlockStore(storeName, "memory")
		require.Error(t, err, "Should reject duplicate store name")

		// Error should indicate conflict/already exists
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "already exists") ||
				strings.Contains(errStr, "conflict") ||
				strings.Contains(errStr, "duplicate"),
			"Error should indicate store already exists: %s", err.Error())
	})

	// Cannot delete local store in use by share
	t.Run("local/cannot delete store in use", func(t *testing.T) {
		metaStoreName := helpers.UniqueTestName("meta_inuse")
		localStoreName := helpers.UniqueTestName("block_inuse")
		shareName := "/" + helpers.UniqueTestName("share_inuse")

		t.Cleanup(func() {
			_ = cli.DeleteShare(shareName)
			_ = cli.DeleteLocalBlockStore(localStoreName)
			_ = cli.DeleteMetadataStore(metaStoreName)
		})

		// Create metadata store for the share
		_, err := cli.CreateMetadataStore(metaStoreName, "memory")
		require.NoError(t, err, "Should create metadata store")

		// Create local block store
		_, err = cli.CreateLocalBlockStore(localStoreName, "memory")
		require.NoError(t, err, "Should create block store")

		// Create share referencing both stores
		_, err = cli.CreateShare(shareName, metaStoreName, localStoreName)
		require.NoError(t, err, "Should create share")

		// Try to delete block store - should fail because share is using it
		err = cli.DeleteLocalBlockStore(localStoreName)
		require.Error(t, err, "Should reject deletion of block store in use")

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
		err = cli.DeleteLocalBlockStore(localStoreName)
		require.NoError(t, err, "Should delete block store after share deletion")
	})

	// Get local store by name
	t.Run("local/get store by name", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("block_get")

		t.Cleanup(func() {
			_ = cli.DeleteLocalBlockStore(storeName)
		})

		// Create store
		created, err := cli.CreateLocalBlockStore(storeName, "memory")
		require.NoError(t, err, "Should create store")

		// Get store by name
		fetched, err := cli.GetLocalBlockStore(storeName)
		require.NoError(t, err, "Should get store by name")

		assert.Equal(t, created.Name, fetched.Name, "Names should match")
		assert.Equal(t, created.Type, fetched.Type, "Types should match")
	})

	// =====================================================================
	// Remote Block Store Tests
	// =====================================================================

	// Create memory remote block store
	t.Run("remote/create memory store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("remote_mem")
		t.Cleanup(func() {
			_ = cli.DeleteRemoteBlockStore(storeName)
		})

		store, err := cli.CreateRemoteBlockStore(storeName, "memory")
		require.NoError(t, err, "Should create memory remote block store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "memory", store.Type, "Store type should be memory")
	})

	// Create S3 remote block store (without actual S3)
	t.Run("remote/create s3 store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("remote_s3")
		t.Cleanup(func() {
			_ = cli.DeleteRemoteBlockStore(storeName)
		})

		s3Config := `{"bucket":"test-bucket","region":"us-east-1"}`
		store, err := cli.CreateRemoteBlockStore(storeName, "s3",
			helpers.WithBlockRawConfig(s3Config))
		require.NoError(t, err, "Should create S3 remote block store")

		assert.Equal(t, storeName, store.Name, "Store name should match")
		assert.Equal(t, "s3", store.Type, "Store type should be s3")
	})

	// Delete remote block store
	t.Run("remote/delete store", func(t *testing.T) {
		storeName := helpers.UniqueTestName("remote_del")

		_, err := cli.CreateRemoteBlockStore(storeName, "memory")
		require.NoError(t, err, "Should create remote store")

		err = cli.DeleteRemoteBlockStore(storeName)
		require.NoError(t, err, "Should delete remote store")
	})

	// =====================================================================
	// Error Cases
	// =====================================================================

	// Share creation requires a local store
	t.Run("error/share requires local store", func(t *testing.T) {
		metaStoreName := helpers.UniqueTestName("meta_noloc")
		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(metaStoreName)
		})

		// Create metadata store
		_, err := cli.CreateMetadataStore(metaStoreName, "memory")
		require.NoError(t, err, "Should create metadata store")

		// Try to create share with a non-existent local store
		_, err = cli.CreateShare("/"+helpers.UniqueTestName("share_noloc"), metaStoreName, "nonexistent-local")
		require.Error(t, err, "Should fail to create share with missing local store")
	})

	// Invalid remote store type reference
	t.Run("error/invalid remote type on share", func(t *testing.T) {
		metaStoreName := helpers.UniqueTestName("meta_invrem")
		localStoreName := helpers.UniqueTestName("local_invrem")
		t.Cleanup(func() {
			_ = cli.DeleteLocalBlockStore(localStoreName)
			_ = cli.DeleteMetadataStore(metaStoreName)
		})

		_, err := cli.CreateMetadataStore(metaStoreName, "memory")
		require.NoError(t, err)

		_, err = cli.CreateLocalBlockStore(localStoreName, "memory")
		require.NoError(t, err)

		// Try to create share with a non-existent remote store
		_, err = cli.CreateShare("/"+helpers.UniqueTestName("share_invrem"),
			metaStoreName, localStoreName,
			helpers.WithShareRemote("nonexistent-remote"))
		require.Error(t, err, "Should fail to create share with non-existent remote store")
	})
}

//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetadataStoresCRUD tests comprehensive metadata store management via CLI.
// Uses a shared server process for all subtests.
func TestMetadataStoresCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping metadata stores tests in short mode")
	}

	// Start server with automatic cleanup on test completion
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	serverURL := sp.APIURL()

	// Login as admin and get CLI runner
	cli := helpers.LoginAsAdmin(t, serverURL)

	t.Run("create memory store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("meta_mem")

		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName)
		})

		store, err := cli.CreateInMemoryMetadataStore(storeName)
		require.NoError(t, err, "Should create in-memory metadata store")

		// Verify fields: in-memory is a badger mode, not a store type
		assert.Equal(t, storeName, store.Name)
		assert.Equal(t, "badger", store.Type)
	})

	t.Run("create badger store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("meta_badger")
		dbPath := filepath.Join(t.TempDir(), "badger")

		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName)
		})

		// Create badger store with db-path
		store, err := cli.CreateMetadataStore(storeName, "badger",
			helpers.WithMetaDBPath(dbPath),
		)
		require.NoError(t, err, "Should create badger metadata store")

		// Verify fields
		assert.Equal(t, storeName, store.Name)
		assert.Equal(t, "badger", store.Type)
	})

	t.Run("removed store types are rejected", func(t *testing.T) {
		t.Parallel()

		// dfsctl refuses a non-badger type before sending anything, so go
		// through the API client to pin the server's own rejection.
		client := helpers.GetAPIClient(t, serverURL)
		for _, storeType := range []string{"memory", "sqlite", "postgres"} {
			storeName := helpers.UniqueTestName("meta_" + storeType)
			_, err := client.CreateMetadataStore(&apiclient.CreateStoreRequest{
				Name:   storeName,
				Type:   storeType,
				Config: map[string]any{},
			})
			if err == nil {
				_ = client.RemoveMetadataStore(storeName)
			}
			require.Error(t, err, "type %q must be rejected", storeType)
			assert.Contains(t, err.Error(), "badger is the only metadata store type")
		}
	})

	t.Run("list stores", func(t *testing.T) {
		t.Parallel()

		storeName1 := helpers.UniqueTestName("meta_list1")
		storeName2 := helpers.UniqueTestName("meta_list2")

		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName1)
			_ = cli.DeleteMetadataStore(storeName2)
		})

		// Create two stores
		_, err := cli.CreateInMemoryMetadataStore(storeName1)
		require.NoError(t, err)

		_, err = cli.CreateInMemoryMetadataStore(storeName2)
		require.NoError(t, err)

		// List all stores
		stores, err := cli.ListMetadataStores()
		require.NoError(t, err, "Should list metadata stores")

		// Find our created stores
		var found1, found2 bool
		for _, s := range stores {
			switch s.Name {
			case storeName1:
				found1 = true
			case storeName2:
				found2 = true
			}
		}

		assert.True(t, found1, "Should find store1 in list")
		assert.True(t, found2, "Should find store2 in list")
	})

	t.Run("get store", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("meta_get")

		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName)
		})

		// Create store
		created, err := cli.CreateInMemoryMetadataStore(storeName)
		require.NoError(t, err)

		// Get store by name
		store, err := cli.GetMetadataStore(storeName)
		require.NoError(t, err, "Should get metadata store by name")

		// Verify fields match
		assert.Equal(t, created.Name, store.Name)
		assert.Equal(t, created.Type, store.Type)
	})

	t.Run("edit badger store path", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("meta_edit")
		initialPath := filepath.Join(t.TempDir(), "badger_initial")
		newPath := filepath.Join(t.TempDir(), "badger_updated")

		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName)
		})

		// Create badger store
		_, err := cli.CreateMetadataStore(storeName, "badger",
			helpers.WithMetaDBPath(initialPath),
		)
		require.NoError(t, err)

		// Edit store with new path
		updated, err := cli.EditMetadataStore(storeName,
			helpers.WithMetaDBPath(newPath),
		)
		require.NoError(t, err, "Should edit metadata store")
		assert.Equal(t, storeName, updated.Name)
		assert.Equal(t, "badger", updated.Type)

		// Verify by getting the store
		fetched, err := cli.GetMetadataStore(storeName)
		require.NoError(t, err)
		assert.Equal(t, storeName, fetched.Name)
	})

	t.Run("delete store", func(t *testing.T) {
		// Not parallel - write operations can cause SQLite lock contention
		storeName := helpers.UniqueTestName("meta_del")

		// Create store
		_, err := cli.CreateInMemoryMetadataStore(storeName)
		require.NoError(t, err)

		// Delete store
		err = cli.DeleteMetadataStore(storeName)
		require.NoError(t, err, "Should delete metadata store")

		// Verify store is gone
		_, err = cli.GetMetadataStore(storeName)
		require.Error(t, err, "Should fail to get deleted store")
		assert.Contains(t, err.Error(), "not found", "Error should indicate store not found")
	})

	t.Run("duplicate name rejected", func(t *testing.T) {
		t.Parallel()

		storeName := helpers.UniqueTestName("meta_dup")

		t.Cleanup(func() {
			_ = cli.DeleteMetadataStore(storeName)
		})

		// Create store
		_, err := cli.CreateInMemoryMetadataStore(storeName)
		require.NoError(t, err)

		// Try to create again with same name
		_, err = cli.CreateInMemoryMetadataStore(storeName)
		require.Error(t, err, "Should reject duplicate store name")

		// Error should indicate conflict/already exists
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "already exists") ||
				strings.Contains(errStr, "conflict") ||
				strings.Contains(errStr, "duplicate"),
			"Error should indicate store already exists: %s", err.Error())
	})

	// Non-parallel test - creates share which affects global state
	t.Run("cannot delete store in use", func(t *testing.T) {
		// Create metadata store
		metaStoreName := helpers.UniqueTestName("meta_inuse")
		_, err := cli.CreateInMemoryMetadataStore(metaStoreName)
		require.NoError(t, err, "Should create metadata store")

		// Create block store (needed for share)
		blockStoreName := helpers.UniqueTestName("block_inuse")
		_, err = cli.CreateBlockStore(blockStoreName, "memory")
		require.NoError(t, err, "Should create block store")

		// Create share referencing the metadata store
		shareName := "/" + helpers.UniqueTestName("share_inuse")
		_, err = cli.CreateShare(shareName, metaStoreName, blockStoreName)
		require.NoError(t, err, "Should create share")

		// Try to delete metadata store - should fail
		err = cli.DeleteMetadataStore(metaStoreName)
		require.Error(t, err, "Should fail to delete metadata store in use by share")

		// Error should indicate store is in use
		errStr := strings.ToLower(err.Error())
		assert.True(t,
			strings.Contains(errStr, "in use") ||
				strings.Contains(errStr, "referenced") ||
				strings.Contains(errStr, "cannot delete"),
			"Error should indicate store is in use: %s", err.Error())

		// Delete share first
		err = cli.DeleteShare(shareName)
		require.NoError(t, err, "Should delete share")

		// Now delete should succeed
		err = cli.DeleteMetadataStore(metaStoreName)
		require.NoError(t, err, "Should delete metadata store after share removed")

		// Cleanup block store
		_ = cli.DeleteBlockStore(blockStoreName)
	})
}

package testing

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (suite *StoreTestSuite) RunServerTests(test *testing.T) {
	test.Run("SetServerConfig_Success", suite.TestSetServerConfig_Success)
	test.Run("GetServerConfig_Default", suite.TestGetServerConfig_Default)
	test.Run("SetServerConfig_Update", suite.TestSetServerConfig_Update)
	test.Run("GetServerConfig_AfterSet", suite.TestGetServerConfig_AfterSet)
	test.Run("SetServerConfig_VariousTypes", suite.TestSetServerConfig_VariousTypes)
	test.Run("SetServerConfig_ProtocolSettings", suite.TestSetServerConfig_ProtocolSettings)
	test.Run("TestGetServerConfig_InitiallyEmpty", suite.TestGetServerConfig_InitiallyEmpty)
	test.Run("TestGetServerConfig_InitiallyEmpty", suite.TestGetServerConfig_InitiallyEmpty)
	test.Run("TestGetServerConfig_ContextCanceled", suite.TestGetServerConfig_ContextCanceled)
	test.Run("TestSetServerConfig_ContextCanceled", suite.TestSetServerConfig_ContextCanceled)
}

// TestSetServerConfig_Success verifies that server configuration can be set.
func (suite *StoreTestSuite) TestSetServerConfig_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	config := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"test.setting": "value",
		},
	}

	// Act
	err := store.SetServerConfig(ctx, config)

	// Assert
	require.NoError(test, err, "Setting server configuration should succeed")
}

// TestGetServerConfig_Default verifies that GetServerConfig works with default/initial state.
func (suite *StoreTestSuite) TestGetServerConfig_Default(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	config, err := store.GetServerConfig(ctx)

	// Assert
	require.NoError(test, err, "Getting server configuration should succeed")
	assert.NotNil(test, config, "Server configuration should not be nil")
}

// TestSetServerConfig_Update verifies that server configuration can be updated.
func (suite *StoreTestSuite) TestSetServerConfig_Update(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Set initial configuration
	initialConfig := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"setting1": "initial_value",
			"setting2": 100,
		},
	}
	err := store.SetServerConfig(ctx, initialConfig)
	require.NoError(test, err)

	// Update configuration with different settings
	updatedConfig := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"setting1": "updated_value",
			"setting3": true, // New setting
		},
	}

	// Act
	err = store.SetServerConfig(ctx, updatedConfig)

	// Assert
	require.NoError(test, err, "Updating server configuration should succeed")

	// Verify the update took effect
	retrieved, err := store.GetServerConfig(ctx)
	require.NoError(test, err)
	assert.Equal(test, "updated_value", retrieved.CustomSettings["setting1"],
		"Updated setting should have new value")
	assert.Equal(test, true, retrieved.CustomSettings["setting3"],
		"New setting should be present")
	assert.NotContains(test, retrieved.CustomSettings, "setting2",
		"Old setting not in update should be removed")
}

// TestGetServerConfig_AfterSet verifies that GetServerConfig returns the configuration that was set.
func (suite *StoreTestSuite) TestGetServerConfig_AfterSet(test *testing.T) {
	suite.NewStore()
	ctx := context.Background()

	tests := []struct {
		name        string
		config      metadata.MetadataServerConfig
		description string
	}{
		{
			name: "empty_config",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{},
			},
			description: "Empty server configuration",
		},
		{
			name: "single_setting",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{
					"nfs.mount.allowed_clients": []string{"192.168.1.0/24"},
				},
			},
			description: "Single custom setting",
		},
		{
			name: "multiple_settings",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{
					"nfs.mount.allowed_clients": []string{"192.168.1.0/24"},
					"smb.signing_required":      true,
					"max_connections":           1000,
				},
			},
			description: "Multiple custom settings",
		},
		{
			name: "nil_custom_settings",
			config: metadata.MetadataServerConfig{
				CustomSettings: nil,
			},
			description: "Configuration with nil CustomSettings",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			// Create fresh store for each test case
			localStore := suite.NewStore()

			// Set configuration
			err := localStore.SetServerConfig(ctx, tt.config)
			require.NoError(t, err, tt.description)

			// Act - Retrieve configuration
			retrieved, err := localStore.GetServerConfig(ctx)

			// Assert
			require.NoError(t, err)

			if tt.config.CustomSettings == nil {
				// Implementation may return empty map instead of nil
				assert.True(t, len(retrieved.CustomSettings) == 0,
					"Nil CustomSettings should return nil or empty map")
			} else {
				assert.Equal(t, len(tt.config.CustomSettings), len(retrieved.CustomSettings),
					"Should have same number of settings")

				for key, expectedValue := range tt.config.CustomSettings {
					assert.Equal(t, expectedValue, retrieved.CustomSettings[key],
						"Setting %s should match", key)
				}
			}
		})
	}
}

// TestSetServerConfig_VariousTypes verifies that CustomSettings can store various data types.
func (suite *StoreTestSuite) TestSetServerConfig_VariousTypes(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	config := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{
			"string_setting": "test_value",
			"int_setting":    42,
			"int64_setting":  int64(9223372036854775807),
			"bool_setting":   true,
			"float_setting":  3.14159,
			"slice_setting":  []string{"item1", "item2", "item3"},
			"map_setting":    map[string]string{"key1": "value1", "key2": "value2"},
			"uint_setting":   uint(100),
		},
	}

	// Act
	err := store.SetServerConfig(ctx, config)
	require.NoError(test, err)

	// Verify all types are preserved
	retrieved, err := store.GetServerConfig(ctx)
	require.NoError(test, err)

	// Assert - Check each type
	assert.Equal(test, "test_value", retrieved.CustomSettings["string_setting"])
	assert.Equal(test, 42, retrieved.CustomSettings["int_setting"])
	assert.Equal(test, int64(9223372036854775807), retrieved.CustomSettings["int64_setting"])
	assert.Equal(test, true, retrieved.CustomSettings["bool_setting"])
	assert.Equal(test, 3.14159, retrieved.CustomSettings["float_setting"])
	assert.Equal(test, []string{"item1", "item2", "item3"}, retrieved.CustomSettings["slice_setting"])
	assert.Equal(test, map[string]string{"key1": "value1", "key2": "value2"},
		retrieved.CustomSettings["map_setting"])
	assert.Equal(test, uint(100), retrieved.CustomSettings["uint_setting"])
}

// TestSetServerConfig_ProtocolSettings verifies realistic protocol-specific configurations.
func (suite *StoreTestSuite) TestSetServerConfig_ProtocolSettings(test *testing.T) {
	suite.NewStore()
	ctx := context.Background()

	tests := []struct {
		name        string
		config      metadata.MetadataServerConfig
		description string
	}{
		{
			name: "nfs_settings",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{
					"nfs.mount.allowed_clients": []string{"192.168.1.0/24", "10.0.0.0/8"},
					"nfs.mount.denied_clients":  []string{"192.168.1.50"},
					"nfs.port":                  2049,
					"nfs.tcp":                   true,
					"nfs.udp":                   false,
				},
			},
			description: "NFS protocol settings",
		},
		{
			name: "smb_settings",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{
					"smb.signing_required":    true,
					"smb.encryption_required": false,
					"smb.max_protocol":        "SMB3_11",
					"smb.min_protocol":        "SMB2_02",
				},
			},
			description: "SMB protocol settings",
		},
		{
			name: "ftp_settings",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{
					"ftp.passive_ports": "10000-10100",
					"ftp.port":          21,
					"ftp.tls_required":  true,
				},
			},
			description: "FTP protocol settings",
		},
		{
			name: "mixed_protocols",
			config: metadata.MetadataServerConfig{
				CustomSettings: map[string]any{
					"nfs.port":               2049,
					"smb.signing_required":   true,
					"global.max_connections": 1000,
					"global.timeout":         30,
				},
			},
			description: "Mixed protocol settings",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			// Create fresh store for each protocol test
			localStore := suite.NewStore()

			// Set configuration
			err := localStore.SetServerConfig(ctx, tt.config)
			require.NoError(t, err, tt.description)

			// Retrieve and verify
			retrieved, err := localStore.GetServerConfig(ctx)
			require.NoError(t, err)

			for key, expectedValue := range tt.config.CustomSettings {
				assert.Equal(t, expectedValue, retrieved.CustomSettings[key],
					"Protocol setting %s should match", key)
			}
		})
	}
}

func (suite *StoreTestSuite) TestGetServerConfig_InitiallyEmpty(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	config, err := store.GetServerConfig(ctx)
	require.NoError(test, err)
	assert.Equal(test, metadata.MetadataServerConfig{}, config, "Initial config should be empty")
}

func (suite *StoreTestSuite) TestGetServerConfig_ContextCanceled(t *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err := store.GetServerConfig(canceledCtx)
	assert.Error(t, err, "Should fail with canceled context")
}

func (suite *StoreTestSuite) TestSetServerConfig_ContextCanceled(t *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()

	config := metadata.MetadataServerConfig{
		CustomSettings: map[string]any{"test": "value"},
	}
	err := store.SetServerConfig(canceledCtx, config)
	assert.Error(t, err, "Should fail with canceled context")
}

package testing

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (suite *StoreTestSuite) RunShareTests(test *testing.T) {
	test.Run("AddShare_Success", suite.TestAddShare_Success)
	test.Run("AddShare_Duplicate", suite.TestAddShare_Duplicate)
	test.Run("GetShares_Empty", suite.TestGetShares_Empty)
	test.Run("GetShares_Multiple", suite.TestGetShares_Multiple)
	test.Run("FindShares_Success", suite.TestFindShare_Success)
	test.Run("FindShares_NotFound", suite.TestFindShare_NotFound)
	test.Run("GetShareRoot_Success", suite.TestGetShareRoot_Success)
	test.Run("GetShareRoot_NotFound", suite.TestGetShareRoot_NotFound)
	test.Run("DeleteShare_Success", suite.TestDeleteShare_Success)
	test.Run("DeleteShare_NotFound", suite.TestDeleteShare_NotFound)
}

// TestAddShare_Success verifies that shares can be created with various configurations.
func (suite *StoreTestSuite) TestAddShare_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	tests := []struct {
		name        string
		shareName   string
		options     metadata.ShareOptions
		rootAttr    *metadata.FileAttr
		description string
	}{
		{
			name:      "basic_share",
			shareName: "/export/data",
			options: metadata.ShareOptions{
				ReadOnly:    false,
				RequireAuth: false,
			},
			rootAttr:    DefaultRootDirAttr(),
			description: "Create a basic read-write share without authentication",
		},
		{
			name:      "readonly_share",
			shareName: "/export/readonly",
			options: metadata.ShareOptions{
				ReadOnly:    true,
				RequireAuth: false,
			},
			rootAttr:    DefaultRootDirAttr(),
			description: "Create a read-only share",
		},
		{
			name:      "async_share",
			shareName: "/export/async",
			options: metadata.ShareOptions{
				ReadOnly: false,
				Async:    true,
			},
			rootAttr:    DefaultRootDirAttr(),
			description: "Create a share with async writes enabled",
		},
		{
			name:      "authenticated_share",
			shareName: "/export/secure",
			options: metadata.ShareOptions{
				ReadOnly:           false,
				RequireAuth:        true,
				AllowedAuthMethods: []string{"unix", "kerberos"},
			},
			rootAttr:    DefaultRootDirAttr(),
			description: "Create a share requiring authentication",
		},
		{
			name:      "share_with_access_control",
			shareName: "/export/restricted",
			options: metadata.ShareOptions{
				ReadOnly:       false,
				RequireAuth:    true,
				AllowedClients: []string{"192.168.1.0/24", "10.0.0.5"},
				DeniedClients:  []string{"192.168.1.100"},
			},
			rootAttr:    DefaultRootDirAttr(),
			description: "Create a share with IP-based access control",
		},
		{
			name:      "share_with_identity_mapping",
			shareName: "/export/anonymous",
			options: metadata.ShareOptions{
				ReadOnly: false,
				IdentityMapping: &metadata.IdentityMapping{
					MapAllToAnonymous:        true,
					MapPrivilegedToAnonymous: false,
				},
			},
			rootAttr:    DefaultRootDirAttr(),
			description: "Create a share with identity mapping",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			// Act
			err := store.AddShare(ctx, tt.shareName, tt.options, tt.rootAttr)

			// Assert
			require.NoError(t, err, tt.description)

			// Verify the share was created
			share, err := store.FindShare(ctx, tt.shareName)
			require.NoError(t, err)
			assert.Equal(t, tt.shareName, share.Name)
			assert.Equal(t, tt.options.ReadOnly, share.Options.ReadOnly)
			assert.Equal(t, tt.options.RequireAuth, share.Options.RequireAuth)
			assert.Equal(t, tt.options.Async, share.Options.Async)
		})
	}
}

// TestAddShare_Duplicate verifies that duplicate share names are rejected.
func (suite *StoreTestSuite) TestAddShare_Duplicate(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	shareName := "/export/data"
	options := metadata.ShareOptions{ReadOnly: false}
	rootAttr := DefaultRootDirAttr()

	// Create first share
	err := store.AddShare(ctx, shareName, options, rootAttr)
	require.NoError(test, err)

	// Attempt to create duplicate
	err = store.AddShare(ctx, shareName, options, rootAttr)

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrAlreadyExists, err, "Should return ErrAlreadyExists for duplicate share name")
}

// TestGetShares_Empty verifies that GetShares returns empty list when no shares exist.
func (suite *StoreTestSuite) TestGetShares_Empty(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	shares, err := store.GetShares(ctx)

	// Assert
	require.NoError(test, err)
	assert.Empty(test, shares, "Should return empty list when no shares exist")
}

// TestGetShares_Multiple verifies that GetShares returns all created shares.
func (suite *StoreTestSuite) TestGetShares_Multiple(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create multiple shares
	shareNames := []string{"/export/data", "/export/backup", "/export/public"}
	for _, name := range shareNames {
		err := store.AddShare(ctx, name, metadata.ShareOptions{}, DefaultRootDirAttr())
		require.NoError(test, err)
	}

	// Act
	shares, err := store.GetShares(ctx)

	// Assert
	require.NoError(test, err)
	assert.Len(test, shares, len(shareNames), "Should return all created shares")

	// Verify all share names are present
	foundNames := make(map[string]bool)
	for _, share := range shares {
		foundNames[share.Name] = true
	}
	for _, name := range shareNames {
		assert.True(test, foundNames[name], "Share %s should be in the list", name)
	}
}

// TestFindShare_Success verifies that shares can be found by name.
func (suite *StoreTestSuite) TestFindShare_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	shareName := "/export/data"
	options := metadata.ShareOptions{
		ReadOnly:    true,
		RequireAuth: true,
		Async:       false,
	}
	rootAttr := DefaultRootDirAttr()

	// Create share
	err := store.AddShare(ctx, shareName, options, rootAttr)
	require.NoError(test, err)

	// Act
	share, err := store.FindShare(ctx, shareName)

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, share)
	assert.Equal(test, shareName, share.Name)
	assert.Equal(test, options.ReadOnly, share.Options.ReadOnly)
	assert.Equal(test, options.RequireAuth, share.Options.RequireAuth)
	assert.Equal(test, options.Async, share.Options.Async)
}

// TestFindShare_NotFound verifies that FindShare returns ErrNotFound for non-existent shares.
func (suite *StoreTestSuite) TestFindShare_NotFound(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	share, err := store.FindShare(ctx, "/nonexistent")

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Should return ErrNotFound for non-existent share")
	assert.Nil(test, share)
}

// TestGetShareRoot_Success verifies that share root handles can be retrieved.
func (suite *StoreTestSuite) TestGetShareRoot_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	// Act
	rootHandle, err := store.GetShareRoot(ctx, shareName)

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, rootHandle, "Root handle should not be nil")
	assert.NotEmpty(test, rootHandle, "Root handle should not be empty")

	// Verify we can get file attributes for the root handle
	attr, err := store.GetFile(ctx, rootHandle)
	require.NoError(test, err)
	assert.Equal(test, metadata.FileTypeDirectory, attr.Type, "Root should be a directory")
}

// TestGetShareRoot_NotFound verifies that GetShareRoot returns ErrNotFound for non-existent shares.
func (suite *StoreTestSuite) TestGetShareRoot_NotFound(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	rootHandle, err := store.GetShareRoot(ctx, "/nonexistent")

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Should return ErrNotFound for non-existent share")
	assert.Nil(test, rootHandle)
}

// TestDeleteShare_Success verifies that shares can be deleted.
func (suite *StoreTestSuite) TestDeleteShare_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	// Verify share exists
	_, err = store.FindShare(ctx, shareName)
	require.NoError(test, err)

	// Act - Delete the share
	err = store.DeleteShare(ctx, shareName)

	// Assert
	require.NoError(test, err)

	// Verify share no longer exists
	_, err = store.FindShare(ctx, shareName)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Share should not exist after deletion")
}

// TestDeleteShare_NotFound verifies that deleting non-existent shares returns ErrNotFound.
func (suite *StoreTestSuite) TestDeleteShare_NotFound(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	err := store.DeleteShare(ctx, "/nonexistent")

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Should return ErrNotFound when deleting non-existent share")
}

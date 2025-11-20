//go:build obsolete

package testing

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
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
	test.Run("RecordShareMount_Success", suite.TestRecordShareMount_Success)
	test.Run("RecordShareMount_UpdatesTimestamp", suite.TestRecordShareMount_UpdatesTimestamp)
	test.Run("RecordShareMount_ShareNotFound", suite.TestRecordShareMount_ShareNotFound)
	test.Run("RecordShareMount_MultipleClients", suite.TestRecordShareMount_MultipleClients)
	test.Run("GetActiveShares_Empty", suite.TestGetActiveShares_Empty)
	test.Run("GetActiveShares_Single", suite.TestGetActiveShares_Single)
	test.Run("GetActiveShares_Multiple", suite.TestGetActiveShares_Multiple)
	test.Run("GetActiveShares_MultipleMountsPerShare", suite.TestGetActiveShares_MultipleMountsPerShare)
	test.Run("RemoveShareMount_Success", suite.TestRemoveShareMount_Success)
	test.Run("RemoveShareMount_Idempotent", suite.TestRemoveShareMount_Idempotent)
	test.Run("RemoveShareMount_MultipleClients", suite.TestRemoveShareMount_MultipleClients)
	test.Run("ShareSession_CompleteLifecycle", suite.TestShareSession_CompleteLifecycle)
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

// TestRecordShareMount_Success verifies that share mount sessions can be recorded.
func (suite *StoreTestSuite) TestRecordShareMount_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create a share
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	// Act: Record a mount
	clientAddr := "192.168.1.100"
	err = store.RecordShareMount(ctx, shareName, clientAddr)

	// Assert
	require.NoError(test, err, "Recording mount should succeed")

	// Verify the session was recorded
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, 1, "Should have one active session")

	session := sessions[0]
	assert.Equal(test, shareName, session.ShareName)
	assert.Equal(test, clientAddr, session.ClientAddr)
	assert.False(test, session.MountedAt.IsZero(), "MountedAt should be set")
}

// TestRecordShareMount_UpdatesTimestamp verifies that recording the same mount updates the timestamp.
func (suite *StoreTestSuite) TestRecordShareMount_UpdatesTimestamp(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create a share and record initial mount
	shareName := "/export/data"
	clientAddr := "192.168.1.100"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	err = store.RecordShareMount(ctx, shareName, clientAddr)
	require.NoError(test, err)

	// Get initial session
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, 1)
	firstMountTime := sessions[0].MountedAt

	// Wait a bit to ensure timestamp difference
	time.Sleep(10 * time.Millisecond)

	// Act: Record the same mount again
	err = store.RecordShareMount(ctx, shareName, clientAddr)
	require.NoError(test, err)

	// Assert: Should still have only one session with updated timestamp
	sessions, err = store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, 1, "Should still have only one session (no duplicates)")

	secondMountTime := sessions[0].MountedAt
	assert.True(test, secondMountTime.After(firstMountTime),
		"Second mount time should be after first mount time")
}

// TestRecordShareMount_ShareNotFound verifies that recording mount for non-existent share fails.
func (suite *StoreTestSuite) TestRecordShareMount_ShareNotFound(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act: Try to record mount for non-existent share
	err := store.RecordShareMount(ctx, "/nonexistent", "192.168.1.100")

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err,
		"Should return ErrNotFound for non-existent share")
}

// TestRecordShareMount_MultipleClients verifies that multiple clients can mount the same share.
func (suite *StoreTestSuite) TestRecordShareMount_MultipleClients(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create a share
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	// Act: Record mounts from different clients
	clients := []string{"192.168.1.100", "192.168.1.101", "192.168.1.102"}
	for _, client := range clients {
		err := store.RecordShareMount(ctx, shareName, client)
		require.NoError(test, err)
	}

	// Assert: All sessions should be recorded
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, len(clients), "Should have session for each client")

	// Verify each client is present
	foundClients := make(map[string]bool)
	for _, session := range sessions {
		assert.Equal(test, shareName, session.ShareName)
		foundClients[session.ClientAddr] = true
	}

	for _, client := range clients {
		assert.True(test, foundClients[client], "Client %s should have a session", client)
	}
}

// TestGetActiveShares_Empty verifies that GetActiveShares returns empty list when no sessions exist.
func (suite *StoreTestSuite) TestGetActiveShares_Empty(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	sessions, err := store.GetActiveShares(ctx)

	// Assert
	require.NoError(test, err)
	assert.Empty(test, sessions, "Should return empty list when no sessions exist")
	assert.NotNil(test, sessions, "Should return non-nil empty slice")
}

// TestGetActiveShares_Single verifies that GetActiveShares returns a single session.
func (suite *StoreTestSuite) TestGetActiveShares_Single(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	clientAddr := "192.168.1.100"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)
	err = store.RecordShareMount(ctx, shareName, clientAddr)
	require.NoError(test, err)

	// Act
	sessions, err := store.GetActiveShares(ctx)

	// Assert
	require.NoError(test, err)
	require.Len(test, sessions, 1)

	session := sessions[0]
	assert.Equal(test, shareName, session.ShareName)
	assert.Equal(test, clientAddr, session.ClientAddr)
	assert.False(test, session.MountedAt.IsZero())
}

// TestGetActiveShares_Multiple verifies that GetActiveShares returns all sessions.
func (suite *StoreTestSuite) TestGetActiveShares_Multiple(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create multiple shares and mount sessions
	type mountInfo struct {
		shareName  string
		clientAddr string
	}

	mounts := []mountInfo{
		{"/export/data", "192.168.1.100"},
		{"/export/data", "192.168.1.101"},
		{"/export/backup", "192.168.1.100"},
		{"/export/public", "192.168.1.102"},
	}

	for _, mount := range mounts {
		// Create share if it doesn't exist yet
		_, err := store.FindShare(ctx, mount.shareName)
		if err != nil {
			err = store.AddShare(ctx, mount.shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
			require.NoError(test, err)
		}

		// Record mount
		err = store.RecordShareMount(ctx, mount.shareName, mount.clientAddr)
		require.NoError(test, err)
	}

	// Act
	sessions, err := store.GetActiveShares(ctx)

	// Assert
	require.NoError(test, err)
	require.Len(test, sessions, len(mounts), "Should return all recorded sessions")

	// Verify all mounts are present
	foundMounts := make(map[string]bool)
	for _, session := range sessions {
		key := session.ShareName + "|" + session.ClientAddr
		foundMounts[key] = true
		assert.False(test, session.MountedAt.IsZero(), "MountedAt should be set")
	}

	for _, mount := range mounts {
		key := mount.shareName + "|" + mount.clientAddr
		assert.True(test, foundMounts[key], "Mount %v should be in sessions", mount)
	}
}

// TestGetActiveShares_MultipleMountsPerShare verifies sessions across multiple shares.
func (suite *StoreTestSuite) TestGetActiveShares_MultipleMountsPerShare(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create two shares with multiple clients each
	share1 := "/export/data"
	share2 := "/export/backup"

	err := store.AddShare(ctx, share1, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)
	err = store.AddShare(ctx, share2, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	// Mount share1 from 2 clients
	err = store.RecordShareMount(ctx, share1, "192.168.1.100")
	require.NoError(test, err)
	err = store.RecordShareMount(ctx, share1, "192.168.1.101")
	require.NoError(test, err)

	// Mount share2 from 1 client
	err = store.RecordShareMount(ctx, share2, "192.168.1.102")
	require.NoError(test, err)

	// Act
	sessions, err := store.GetActiveShares(ctx)

	// Assert
	require.NoError(test, err)
	require.Len(test, sessions, 3, "Should have 3 total sessions")

	// Count sessions per share
	share1Count := 0
	share2Count := 0
	for _, session := range sessions {
		switch session.ShareName {
		case share1:
			share1Count++
		case share2:
			share2Count++
		}
	}

	assert.Equal(test, 2, share1Count, "Share1 should have 2 sessions")
	assert.Equal(test, 1, share2Count, "Share2 should have 1 session")
}

// TestRemoveShareMount_Success verifies that mount sessions can be removed.
func (suite *StoreTestSuite) TestRemoveShareMount_Success(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create share and record mount
	shareName := "/export/data"
	clientAddr := "192.168.1.100"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)
	err = store.RecordShareMount(ctx, shareName, clientAddr)
	require.NoError(test, err)

	// Verify session exists
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, 1)

	// Act: Remove the mount
	err = store.RemoveShareMount(ctx, shareName, clientAddr)

	// Assert
	require.NoError(test, err, "Removing mount should succeed")

	// Verify session is gone
	sessions, err = store.GetActiveShares(ctx)
	require.NoError(test, err)
	assert.Empty(test, sessions, "Session should be removed")
}

// TestRemoveShareMount_Idempotent verifies that removing non-existent mount succeeds.
func (suite *StoreTestSuite) TestRemoveShareMount_Idempotent(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act: Remove mount that was never recorded
	err := store.RemoveShareMount(ctx, "/export/data", "192.168.1.100")

	// Assert: Should succeed (idempotent)
	require.NoError(test, err, "Removing non-existent mount should succeed")

	// Verify no sessions exist
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	assert.Empty(test, sessions)
}

// TestRemoveShareMount_MultipleClients verifies removing one client doesn't affect others.
func (suite *StoreTestSuite) TestRemoveShareMount_MultipleClients(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create share with multiple client mounts
	shareName := "/export/data"
	clients := []string{"192.168.1.100", "192.168.1.101", "192.168.1.102"}

	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	for _, client := range clients {
		err := store.RecordShareMount(ctx, shareName, client)
		require.NoError(test, err)
	}

	// Act: Remove one client's mount
	removedClient := clients[1] // Remove middle client
	err = store.RemoveShareMount(ctx, shareName, removedClient)
	require.NoError(test, err)

	// Assert: Other clients should still have sessions
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, 2, "Should have 2 remaining sessions")

	// Verify removed client is gone, others remain
	foundClients := make(map[string]bool)
	for _, session := range sessions {
		foundClients[session.ClientAddr] = true
	}

	assert.True(test, foundClients[clients[0]], "First client should remain")
	assert.False(test, foundClients[clients[1]], "Middle client should be removed")
	assert.True(test, foundClients[clients[2]], "Last client should remain")
}

// TestShareSession_CompleteLifecycle verifies the complete mount/unmount lifecycle.
func (suite *StoreTestSuite) TestShareSession_CompleteLifecycle(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup: Create shares
	share1 := "/export/data"
	share2 := "/export/backup"
	client := "192.168.1.100"

	err := store.AddShare(ctx, share1, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)
	err = store.AddShare(ctx, share2, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	// Initially, no sessions
	sessions, err := store.GetActiveShares(ctx)
	require.NoError(test, err)
	assert.Empty(test, sessions, "Should start with no sessions")

	// Mount first share
	err = store.RecordShareMount(ctx, share1, client)
	require.NoError(test, err)

	sessions, err = store.GetActiveShares(ctx)
	require.NoError(test, err)
	assert.Len(test, sessions, 1, "Should have 1 session after first mount")

	// Mount second share (same client)
	err = store.RecordShareMount(ctx, share2, client)
	require.NoError(test, err)

	sessions, err = store.GetActiveShares(ctx)
	require.NoError(test, err)
	assert.Len(test, sessions, 2, "Should have 2 sessions after second mount")

	// Unmount first share
	err = store.RemoveShareMount(ctx, share1, client)
	require.NoError(test, err)

	sessions, err = store.GetActiveShares(ctx)
	require.NoError(test, err)
	require.Len(test, sessions, 1, "Should have 1 session after first unmount")
	assert.Equal(test, share2, sessions[0].ShareName, "Remaining session should be share2")

	// Unmount second share
	err = store.RemoveShareMount(ctx, share2, client)
	require.NoError(test, err)

	sessions, err = store.GetActiveShares(ctx)
	require.NoError(test, err)
	assert.Empty(test, sessions, "Should have no sessions after all unmounts")

	// Verify idempotent unmount
	err = store.RemoveShareMount(ctx, share1, client)
	require.NoError(test, err, "Unmounting again should succeed")
}

package testing

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (suite *StoreTestSuite) RunAuthenticationTests(test *testing.T) {
	test.Run("CheckShareAccess_AnonymousAccess", suite.TestCheckShareAccess_AnonymousAccess)
	test.Run("CheckShareAccess_RequireAuth", suite.TestCheckShareAccess_RequireAuth)
	test.Run("CheckShareAccess_AllowedClients", suite.TestCheckShareAccess_AllowedClients)
	test.Run("CheckShareAccess_DeniedClients", suite.TestCheckShareAccess_DeniedClients)
	test.Run("CheckShareAccess_ReadOnlyShare", suite.TestCheckShareAccess_ReadOnlyShare)
	test.Run("CheckShareAccess_AuthMethods", suite.TestCheckShareAccess_AuthMethods)
	test.Run("CheckShareAccess_IdentityMapping", suite.TestCheckShareAccess_IdentityMapping)
	test.Run("CheckShareAccess_NonexistentShare", suite.TestCheckShareAccess_NonexistentShare)

	test.Run("CheckPermissions_OwnerPermissions", suite.TestCheckPermissions_OwnerPermissions)
	test.Run("CheckPermissions_GroupPermissions", suite.TestCheckPermissions_GroupPermissions)
	test.Run("CheckPermissions_OtherPermissions", suite.TestCheckPermissions_OtherPermissions)
	test.Run("CheckPermissions_RootBypass", suite.TestCheckPermissions_RootBypass)
	test.Run("CheckPermissions_DirectoryPermissions", suite.TestCheckPermissions_DirectoryPermissions)
	test.Run("CheckPermissions_ReadOnlyShare", suite.TestCheckPermissions_ReadOnlyShare)
	test.Run("CheckPermissions_MultipleFlags", suite.TestCheckPermissions_MultipleFlags)
}

// TestCheckShareAccess_AnonymousAccess verifies anonymous access to shares.
func (suite *StoreTestSuite) TestCheckShareAccess_AnonymousAccess(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create share that allows anonymous access
	shareName := "/export/public"
	options := metadata.ShareOptions{
		ReadOnly:    false,
		RequireAuth: false,
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	// Act
	decision, authCtx, err := store.CheckShareAccess(ctx, shareName, "192.168.1.100", "anonymous", &metadata.Identity{})

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, decision, "Decision should not be nil")
	assert.NotNil(test, authCtx, "AuthContext should not be nil")
	assert.True(test, decision.Allowed, "Anonymous access should be allowed")
	assert.Empty(test, decision.Reason, "No denial reason for allowed access")
	assert.False(test, decision.ReadOnly, "Share is not read-only")
}

// TestCheckShareAccess_RequireAuth verifies authentication requirement enforcement.
func (suite *StoreTestSuite) TestCheckShareAccess_RequireAuth(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create share that requires authentication
	shareName := "/export/secure"
	options := metadata.ShareOptions{
		ReadOnly:    false,
		RequireAuth: true,
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	tests := []struct {
		name        string
		authMethod  string
		identity    *metadata.Identity
		shouldAllow bool
		description string
	}{
		{
			name:        "anonymous_denied",
			authMethod:  "anonymous",
			identity:    &metadata.Identity{},
			shouldAllow: false,
			description: "Anonymous access should be denied when auth required",
		},
		{
			name:       "unix_auth_allowed",
			authMethod: "unix",
			identity: &metadata.Identity{
				UID: uint32Ptr(1000),
				GID: uint32Ptr(1000),
			},
			shouldAllow: true,
			description: "Unix authenticated access should be allowed",
		},
		{
			name:       "kerberos_auth_allowed",
			authMethod: "kerberos",
			identity: &metadata.Identity{
				Username: "user@REALM.COM",
			},
			shouldAllow: true,
			description: "Kerberos authenticated access should be allowed",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			// Act
			decision, authCtx, err := store.CheckShareAccess(ctx, shareName, "192.168.1.100", tt.authMethod, tt.identity)

			// Assert
			require.NoError(t, err)
			assert.NotNil(t, decision, "Decision should not be nil")
			assert.Equal(t, tt.shouldAllow, decision.Allowed, tt.description)

			if tt.shouldAllow {
				assert.NotNil(t, authCtx, "AuthContext should be provided when access allowed")
			}

			if !tt.shouldAllow {
				assert.NotEmpty(t, decision.Reason, "Denial should have a reason")
			}
		})
	}
}

// TestCheckShareAccess_AllowedClients verifies IP-based allow list enforcement.
func (suite *StoreTestSuite) TestCheckShareAccess_AllowedClients(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create share with allowed clients
	shareName := "/export/restricted"
	options := metadata.ShareOptions{
		ReadOnly:       false,
		RequireAuth:    false,
		AllowedClients: []string{"192.168.1.0/24", "10.0.0.5"},
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	tests := []struct {
		name        string
		clientAddr  string
		shouldAllow bool
		description string
	}{
		{
			name:        "allowed_subnet",
			clientAddr:  "192.168.1.50",
			shouldAllow: true,
			description: "Client in allowed subnet should be allowed",
		},
		{
			name:        "allowed_specific_ip",
			clientAddr:  "10.0.0.5",
			shouldAllow: true,
			description: "Specifically allowed IP should be allowed",
		},
		{
			name:        "denied_other_ip",
			clientAddr:  "172.16.0.1",
			shouldAllow: false,
			description: "Client not in allowed list should be denied",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			// Act
			decision, _, err := store.CheckShareAccess(ctx, shareName, tt.clientAddr, "anonymous", &metadata.Identity{})

			// Assert
			require.NoError(t, err)
			assert.NotNil(t, decision, "Decision should not be nil")
			assert.Equal(t, tt.shouldAllow, decision.Allowed, tt.description)
		})
	}
}

// TestCheckShareAccess_DeniedClients verifies IP-based deny list enforcement.
func (suite *StoreTestSuite) TestCheckShareAccess_DeniedClients(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create share with denied clients
	shareName := "/export/blocked"
	options := metadata.ShareOptions{
		ReadOnly:      false,
		RequireAuth:   false,
		DeniedClients: []string{"192.168.1.100", "10.0.0.0/8"},
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	tests := []struct {
		name        string
		clientAddr  string
		shouldAllow bool
		description string
	}{
		{
			name:        "denied_specific_ip",
			clientAddr:  "192.168.1.100",
			shouldAllow: false,
			description: "Specifically denied IP should be blocked",
		},
		{
			name:        "denied_subnet",
			clientAddr:  "10.0.5.10",
			shouldAllow: false,
			description: "IP in denied subnet should be blocked",
		},
		{
			name:        "allowed_other_ip",
			clientAddr:  "192.168.1.50",
			shouldAllow: true,
			description: "IP not in denied list should be allowed",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			// Act
			decision, _, err := store.CheckShareAccess(ctx, shareName, tt.clientAddr, "anonymous", &metadata.Identity{})

			// Assert
			require.NoError(t, err)
			assert.NotNil(t, decision, "Decision should not be nil")
			assert.Equal(t, tt.shouldAllow, decision.Allowed, tt.description)
		})
	}
}

// TestCheckShareAccess_ReadOnlyShare verifies read-only flag in access decision.
func (suite *StoreTestSuite) TestCheckShareAccess_ReadOnlyShare(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create read-only share
	shareName := "/export/readonly"
	options := metadata.ShareOptions{
		ReadOnly:    true,
		RequireAuth: false,
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	// Act
	decision, _, err := store.CheckShareAccess(ctx, shareName, "192.168.1.100", "anonymous", &metadata.Identity{})

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, decision, "Decision should not be nil")
	assert.True(test, decision.Allowed, "Access should be allowed")
	assert.True(test, decision.ReadOnly, "Share should be marked as read-only")
}

// TestCheckShareAccess_AuthMethods verifies authentication method restrictions.
func (suite *StoreTestSuite) TestCheckShareAccess_AuthMethods(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Create share with specific allowed auth methods
	shareName := "/export/kerberosonly"
	options := metadata.ShareOptions{
		ReadOnly:           false,
		RequireAuth:        true,
		AllowedAuthMethods: []string{"kerberos", "ntlm"},
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	tests := []struct {
		name        string
		authMethod  string
		shouldAllow bool
		description string
	}{
		{
			name:        "allowed_kerberos",
			authMethod:  "kerberos",
			shouldAllow: true,
			description: "Kerberos should be allowed",
		},
		{
			name:        "allowed_ntlm",
			authMethod:  "ntlm",
			shouldAllow: true,
			description: "NTLM should be allowed",
		},
		{
			name:        "denied_unix",
			authMethod:  "unix",
			shouldAllow: false,
			description: "Unix auth should be denied",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			identity := &metadata.Identity{
				Username: "testuser",
			}

			// Act
			decision, _, err := store.CheckShareAccess(ctx, shareName, "192.168.1.100", tt.authMethod, identity)

			// Assert
			require.NoError(t, err)
			assert.NotNil(t, decision, "Decision should not be nil")
			assert.Equal(t, tt.shouldAllow, decision.Allowed, tt.description)

			if !decision.Allowed {
				// Should suggest allowed methods
				assert.NotEmpty(t, decision.AllowedAuthMethods,
					"Should provide list of allowed auth methods")
			}
		})
	}
}

// TestCheckShareAccess_IdentityMapping verifies identity mapping rules.
func (suite *StoreTestSuite) TestCheckShareAccess_IdentityMapping(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	anonymousUID := uint32(65534)
	anonymousGID := uint32(65534)

	// Create share with identity mapping
	shareName := "/export/mapped"
	options := metadata.ShareOptions{
		ReadOnly:    false,
		RequireAuth: false,
		IdentityMapping: &metadata.IdentityMapping{
			MapAllToAnonymous: true,
			AnonymousUID:      &anonymousUID,
			AnonymousGID:      &anonymousGID,
		},
	}
	err := store.AddShare(ctx, shareName, options, DefaultRootDirAttr())
	require.NoError(test, err)

	identity := &metadata.Identity{
		UID: uint32Ptr(1000),
		GID: uint32Ptr(1000),
	}

	// Act
	decision, authCtx, err := store.CheckShareAccess(ctx, shareName, "192.168.1.100", "unix", identity)

	// Assert
	require.NoError(test, err)
	assert.NotNil(test, decision, "Decision should not be nil")
	assert.NotNil(test, authCtx, "AuthContext should not be nil")
	assert.True(test, decision.Allowed, "Access should be allowed")
	// The returned authCtx should have the mapped identity
	// Check if mapping was applied
	if authCtx.Identity.UID != nil {
		assert.Equal(test, anonymousUID, *authCtx.Identity.UID,
			"Identity should be mapped to anonymous UID")
	}
}

// TestCheckShareAccess_NonexistentShare verifies error for non-existent shares.
func (suite *StoreTestSuite) TestCheckShareAccess_NonexistentShare(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Act
	decision, authCtx, err := store.CheckShareAccess(ctx, "/nonexistent", "192.168.1.100", "anonymous", &metadata.Identity{})

	// Assert
	require.Error(test, err)
	AssertErrorCode(test, metadata.ErrNotFound, err, "Should return ErrNotFound")
	assert.Nil(test, decision)
	assert.Nil(test, authCtx)
}

// TestCheckPermissions_OwnerPermissions verifies owner permission checks.
func (suite *StoreTestSuite) TestCheckPermissions_OwnerPermissions(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file owned by UID 1000 with mode 0644 (rw-r--r--)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 0,
		Mode: 0644, // Owner: rw-, Group: r--, Other: r--
		UID:  1000,
		GID:  1000,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, "testfile.txt", fileAttr)
	require.NoError(test, err)

	tests := []struct {
		name        string
		uid         uint32
		gid         uint32
		permission  metadata.Permission
		shouldGrant metadata.Permission
		description string
	}{
		{
			name:        "owner_read",
			uid:         1000,
			gid:         1000,
			permission:  metadata.PermissionRead,
			shouldGrant: metadata.PermissionRead,
			description: "Owner should have read permission",
		},
		{
			name:        "owner_write",
			uid:         1000,
			gid:         1000,
			permission:  metadata.PermissionWrite,
			shouldGrant: metadata.PermissionWrite,
			description: "Owner should have write permission",
		},
		{
			name:        "owner_no_execute",
			uid:         1000,
			gid:         1000,
			permission:  metadata.PermissionExecute,
			shouldGrant: 0,
			description: "Owner should not have execute permission (file mode 0644)",
		},
	}

	for _, tt := range tests {
		test.Run(tt.name, func(t *testing.T) {
			checkCtx := &metadata.AuthContext{
				Context:    ctx,
				AuthMethod: "unix",
				Identity: &metadata.Identity{
					UID: &tt.uid,
					GID: &tt.gid,
				},
				ClientAddr: "127.0.0.1",
			}

			// Act
			granted, err := store.CheckPermissions(checkCtx, fileHandle, tt.permission)

			// Assert
			require.NoError(t, err)
			assert.Equal(t, tt.shouldGrant, granted, tt.description)
		})
	}
}

// TestCheckPermissions_GroupPermissions verifies group permission checks.
func (suite *StoreTestSuite) TestCheckPermissions_GroupPermissions(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file with mode 0640 (rw-r-----)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 0,
		Mode: 0640, // Owner: rw-, Group: r--, Other: ---
		UID:  1000,
		GID:  1000,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, "groupfile.txt", fileAttr)
	require.NoError(test, err)

	// Test with user in group (GID 1000) but not owner
	checkCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(2000), // Not the owner
			GID: uint32Ptr(1000), // In the file's group
		},
		ClientAddr: "127.0.0.1",
	}

	// Act & Assert
	granted, err := store.CheckPermissions(checkCtx, fileHandle, metadata.PermissionRead)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionRead, granted, "Group member should have read permission")

	granted, err = store.CheckPermissions(checkCtx, fileHandle, metadata.PermissionWrite)
	require.NoError(test, err)
	assert.Equal(test, metadata.Permission(0), granted, "Group member should not have write permission (mode 0640)")
}

// TestCheckPermissions_OtherPermissions verifies other (world) permission checks.
func (suite *StoreTestSuite) TestCheckPermissions_OtherPermissions(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file with mode 0644 (rw-r--r--)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 0,
		Mode: 0644, // Owner: rw-, Group: r--, Other: r--
		UID:  1000,
		GID:  1000,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, "publicfile.txt", fileAttr)
	require.NoError(test, err)

	// Test with user not owner and not in group
	checkCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(2000), // Not the owner
			GID: uint32Ptr(2000), // Not in the file's group
		},
		ClientAddr: "127.0.0.1",
	}

	// Act & Assert
	granted, err := store.CheckPermissions(checkCtx, fileHandle, metadata.PermissionRead)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionRead, granted, "Other users should have read permission")

	granted, err = store.CheckPermissions(checkCtx, fileHandle, metadata.PermissionWrite)
	require.NoError(test, err)
	assert.Equal(test, metadata.Permission(0), granted, "Other users should not have write permission (mode 0644)")
}

// TestCheckPermissions_RootBypass verifies that root bypasses permission checks.
func (suite *StoreTestSuite) TestCheckPermissions_RootBypass(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file with mode 0000 (no permissions)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 0,
		Mode: 0000, // No permissions for anyone
		UID:  1000,
		GID:  1000,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, "noPerms.txt", fileAttr)
	require.NoError(test, err)

	// Test with root (UID 0)
	rootCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act & Assert - Root should bypass all permission checks
	granted, err := store.CheckPermissions(rootCtx, fileHandle, metadata.PermissionRead)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionRead, granted, "Root should bypass permission checks for read")

	granted, err = store.CheckPermissions(rootCtx, fileHandle, metadata.PermissionWrite)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionWrite, granted, "Root should bypass permission checks for write")
}

// TestCheckPermissions_DirectoryPermissions verifies directory-specific permissions.
func (suite *StoreTestSuite) TestCheckPermissions_DirectoryPermissions(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create directory with mode 0755 (rwxr-xr-x)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Size: 0,
		Mode: 0755, // Owner: rwx, Group: r-x, Other: r-x
		UID:  1000,
		GID:  1000,
	}

	dirHandle, err := store.Create(authCtx, rootHandle, "testdir", dirAttr)
	require.NoError(test, err)

	// Test with non-owner user
	checkCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(2000),
			GID: uint32Ptr(2000),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act & Assert
	granted, err := store.CheckPermissions(checkCtx, dirHandle, metadata.PermissionListDirectory)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionListDirectory, granted,
		"Should be able to list directory (read permission)")

	granted, err = store.CheckPermissions(checkCtx, dirHandle, metadata.PermissionTraverse)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionTraverse, granted,
		"Should be able to traverse directory (execute permission)")

	granted, err = store.CheckPermissions(checkCtx, dirHandle, metadata.PermissionWrite)
	require.NoError(test, err)
	assert.Equal(test, metadata.Permission(0), granted,
		"Should not be able to write to directory (mode 0755)")
}

// TestCheckPermissions_ReadOnlyShare verifies read-only share enforcement.
func (suite *StoreTestSuite) TestCheckPermissions_ReadOnlyShare(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup - Create READ-ONLY share
	shareName := "/export/readonly"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{ReadOnly: true}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file with full permissions (mode 0777)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 0,
		Mode: 0777, // Full permissions
		UID:  1000,
		GID:  1000,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, "rofile.txt", fileAttr)
	require.NoError(test, err)

	// Test with file owner
	ownerCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(1000),
			GID: uint32Ptr(1000),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act & Assert
	granted, err := store.CheckPermissions(ownerCtx, fileHandle, metadata.PermissionRead)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionRead, granted, "Read should be allowed on read-only share")

	granted, err = store.CheckPermissions(ownerCtx, fileHandle, metadata.PermissionWrite)
	require.NoError(test, err)
	assert.Equal(test, metadata.Permission(0), granted,
		"Write should be blocked on read-only share, even with file permissions")
}

// TestCheckPermissions_MultipleFlags verifies checking multiple permissions at once.
func (suite *StoreTestSuite) TestCheckPermissions_MultipleFlags(test *testing.T) {
	store := suite.NewStore()
	ctx := context.Background()

	// Setup
	shareName := "/export/data"
	err := store.AddShare(ctx, shareName, metadata.ShareOptions{}, DefaultRootDirAttr())
	require.NoError(test, err)

	rootHandle, err := store.GetShareRoot(ctx, shareName)
	require.NoError(test, err)

	// Create file with mode 0755 (rwxr-xr-x)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(0),
			GID: uint32Ptr(0),
		},
		ClientAddr: "127.0.0.1",
	}

	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Size: 0,
		Mode: 0755, // Owner: rwx, Group: r-x, Other: r-x
		UID:  1000,
		GID:  1000,
	}

	fileHandle, err := store.Create(authCtx, rootHandle, "multifile.txt", fileAttr)
	require.NoError(test, err)

	// Test with non-owner user checking multiple permissions
	checkCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: uint32Ptr(2000),
			GID: uint32Ptr(2000),
		},
		ClientAddr: "127.0.0.1",
	}

	// Act & Assert - Check combined permissions
	readAndExecute := metadata.PermissionRead | metadata.PermissionExecute
	granted, err := store.CheckPermissions(checkCtx, fileHandle, readAndExecute)
	require.NoError(test, err)
	assert.Equal(test, readAndExecute, granted, "Should have both read and execute permission")

	readAndWrite := metadata.PermissionRead | metadata.PermissionWrite
	granted, err = store.CheckPermissions(checkCtx, fileHandle, readAndWrite)
	require.NoError(test, err)
	assert.Equal(test, metadata.PermissionRead, granted,
		"Should only have read (missing write)")
}

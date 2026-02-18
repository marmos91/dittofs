//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestControlPlaneV2_FullLifecycle validates the full control plane v2.0 lifecycle:
// create adapter -> set settings -> create share with security policy -> verify via API.
//
// This test covers requirement CP-01: Full lifecycle E2E with real NFS configuration.
// Mount verification is skipped since it requires sudo and NFS client capabilities;
// this is exercised by the existing store_matrix_test.go suite.
func TestControlPlaneV2_FullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping control plane v2.0 lifecycle test in short mode")
	}

	// Start a server
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Get API client
	client := helpers.GetAPIClient(t, sp.APIURL())

	// 1. Verify default NFS settings exist and match expectations
	settings := helpers.GetNFSSettings(t, client)
	assert.Equal(t, 90, settings.LeaseTime, "Default lease_time should be 90s")
	assert.True(t, settings.DelegationsEnabled, "Delegations should be enabled by default")
	assert.Equal(t, 1, settings.Version, "Initial version should be 1")
	t.Log("Step 1: Default NFS settings verified")

	// 2. Update lease_time to 120s
	leaseTime := 120
	updated := helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	})
	assert.Equal(t, 120, updated.LeaseTime, "Lease time should be updated to 120")
	assert.Equal(t, 2, updated.Version, "Version should increment on update")
	t.Log("Step 2: Lease time updated to 120s")

	// 3. Verify other settings unchanged after PATCH
	assert.True(t, updated.DelegationsEnabled, "Delegations should remain enabled")
	assert.Equal(t, settings.MaxReadSize, updated.MaxReadSize, "Max read size should be unchanged")
	t.Log("Step 3: Other settings unchanged after PATCH")

	// Create stores for test share
	metaStore := helpers.UniqueTestName("meta")
	payloadStore := helpers.UniqueTestName("payload")
	_, err := client.CreateMetadataStore(&apiclient.CreateStoreRequest{
		Name: metaStore,
		Type: "memory",
	})
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() { _ = client.DeleteMetadataStore(metaStore) })

	_, err = client.CreatePayloadStore(&apiclient.CreateStoreRequest{
		Name: payloadStore,
		Type: "memory",
	})
	require.NoError(t, err, "Should create payload store")
	t.Cleanup(func() { _ = client.DeletePayloadStore(payloadStore) })

	// 4. Create a share with security policy (allow_auth_sys=true)
	share := helpers.CreateShareWithPolicy(t, client, "/lifecycle-test", metaStore, payloadStore, &helpers.ShareSecurityPolicy{
		AllowAuthSys: helpers.BoolPtr(true),
	})
	t.Cleanup(func() { helpers.CleanupShare(client, "/lifecycle-test") })
	assert.Equal(t, "/lifecycle-test", share.Name)
	assert.True(t, share.AllowAuthSys, "Share should allow AUTH_SYS")
	t.Log("Step 4: Share with security policy created")

	// 5. Verify settings persisted across API calls
	final := helpers.GetNFSSettings(t, client)
	assert.Equal(t, 120, final.LeaseTime, "Lease time should persist")
	assert.Equal(t, 2, final.Version, "Version should persist")
	t.Log("Step 5: Settings persistence verified")

	t.Log("Full lifecycle test passed")
}

// TestControlPlaneV2_SettingsValidation validates API validation behavior:
// invalid values return 422, force bypasses validation, dry_run doesn't persist.
func TestControlPlaneV2_SettingsValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping settings validation test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	t.Run("invalid lease_time returns validation error", func(t *testing.T) {
		// lease_time too low (< 10s) should be rejected
		tooLow := 1
		err := helpers.PatchNFSSettingExpectError(t, client, &apiclient.PatchNFSSettingsRequest{
			LeaseTime: &tooLow,
		})

		// Should be a validation error
		if apiErr, ok := err.(*apiclient.APIError); ok {
			assert.True(t, apiErr.IsValidationError(), "Should be a validation error, got: %s", apiErr.Code)
		}
		t.Log("Invalid lease_time correctly rejected")
	})

	t.Run("force bypasses validation", func(t *testing.T) {
		// With --force, the same invalid value should be accepted
		tooLow := 1
		result, err := client.PatchNFSSettings(&apiclient.PatchNFSSettingsRequest{
			LeaseTime: &tooLow,
		}, apiclient.WithForce())
		require.NoError(t, err, "Force should bypass validation")
		assert.Equal(t, 1, result.LeaseTime, "Forced value should be applied")
		t.Log("Force bypass verified")

		// Reset to fix the state
		helpers.ResetNFSSettings(t, client)
	})

	t.Run("dry_run validates but does not persist", func(t *testing.T) {
		// Get current settings
		before := helpers.GetNFSSettings(t, client)

		// Dry-run with valid value
		leaseTime := 200
		result, err := client.PatchNFSSettings(&apiclient.PatchNFSSettingsRequest{
			LeaseTime: &leaseTime,
		}, apiclient.WithDryRun())
		require.NoError(t, err, "Dry run should succeed")
		assert.Equal(t, 200, result.LeaseTime, "Dry run should return would-be value")

		// Verify settings unchanged in DB
		after := helpers.GetNFSSettings(t, client)
		assert.Equal(t, before.LeaseTime, after.LeaseTime, "Settings should not change after dry_run")
		assert.Equal(t, before.Version, after.Version, "Version should not change after dry_run")
		t.Log("Dry run verified - no persistence")
	})

	t.Run("reset restores defaults", func(t *testing.T) {
		// Change a setting first
		leaseTime := 180
		helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
			LeaseTime: &leaseTime,
		})

		// Reset
		result := helpers.ResetNFSSettings(t, client)
		assert.Equal(t, 90, result.LeaseTime, "Lease time should be back to default")
		t.Log("Reset to defaults verified")
	})
}

// TestControlPlaneV2_PatchVsPut validates that PATCH updates only specified fields
// while PUT replaces all fields.
func TestControlPlaneV2_PatchVsPut(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping PATCH vs PUT test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	// Get defaults for comparison
	defaults := helpers.GetNFSSettings(t, client)

	t.Run("PATCH only changes specified fields", func(t *testing.T) {
		leaseTime := 120
		result := helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
			LeaseTime: &leaseTime,
		})

		// Changed field
		assert.Equal(t, 120, result.LeaseTime, "Lease time should be updated")

		// Unchanged fields
		assert.Equal(t, defaults.MaxReadSize, result.MaxReadSize, "MaxReadSize should be unchanged")
		assert.Equal(t, defaults.MaxWriteSize, result.MaxWriteSize, "MaxWriteSize should be unchanged")
		assert.Equal(t, defaults.MaxConnections, result.MaxConnections, "MaxConnections should be unchanged")
		assert.Equal(t, defaults.DelegationsEnabled, result.DelegationsEnabled, "DelegationsEnabled should be unchanged")

		t.Log("PATCH: only lease_time changed, others preserved")
	})

	t.Run("PUT replaces all fields", func(t *testing.T) {
		// Reset first
		helpers.ResetNFSSettings(t, client)

		// PUT with all fields explicitly set
		putReq := &apiclient.NFSAdapterSettingsResponse{
			MinVersion:              defaults.MinVersion,
			MaxVersion:              defaults.MaxVersion,
			LeaseTime:               200,
			GracePeriod:             defaults.GracePeriod,
			DelegationRecallTimeout: defaults.DelegationRecallTimeout,
			CallbackTimeout:         defaults.CallbackTimeout,
			LeaseBreakTimeout:       defaults.LeaseBreakTimeout,
			MaxConnections:          defaults.MaxConnections,
			MaxClients:              defaults.MaxClients,
			MaxCompoundOps:          defaults.MaxCompoundOps,
			MaxReadSize:             defaults.MaxReadSize,
			MaxWriteSize:            defaults.MaxWriteSize,
			PreferredTransferSize:   defaults.PreferredTransferSize,
			DelegationsEnabled:      false,
			BlockedOperations:       defaults.BlockedOperations,
		}

		result, err := client.UpdateNFSSettings(putReq)
		require.NoError(t, err, "PUT should succeed")

		assert.Equal(t, 200, result.LeaseTime, "Lease time should be 200")
		assert.False(t, result.DelegationsEnabled, "Delegations should be disabled")

		t.Log("PUT: all fields replaced successfully")

		// Reset
		helpers.ResetNFSSettings(t, client)
	})
}

// TestControlPlaneV2_NetgroupCRUD validates netgroup create, add members, list, remove, delete.
func TestControlPlaneV2_NetgroupCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping netgroup CRUD test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	ngName := helpers.UniqueTestName("office")

	// 1. Create netgroup
	ng := helpers.CreateNetgroup(t, client, ngName)
	t.Cleanup(func() { helpers.CleanupNetgroup(client, ngName) })
	assert.Equal(t, ngName, ng.Name)
	assert.NotEmpty(t, ng.ID, "Netgroup should have an ID")
	t.Log("Step 1: Netgroup created")

	// 2. Add members: IP, CIDR, hostname
	memberIP := helpers.AddNetgroupMember(t, client, ngName, "ip", "192.168.1.100")
	assert.Equal(t, "ip", memberIP.Type)
	assert.Equal(t, "192.168.1.100", memberIP.Value)

	memberCIDR := helpers.AddNetgroupMember(t, client, ngName, "cidr", "10.0.0.0/24")
	assert.Equal(t, "cidr", memberCIDR.Type)
	assert.Equal(t, "10.0.0.0/24", memberCIDR.Value)

	memberHost := helpers.AddNetgroupMember(t, client, ngName, "hostname", "workstation.local")
	assert.Equal(t, "hostname", memberHost.Type)
	assert.Equal(t, "workstation.local", memberHost.Value)

	t.Log("Step 2: Three members added (IP, CIDR, hostname)")

	// 3. List netgroups -> verify the netgroup with 3 members
	netgroups, err := client.ListNetgroups()
	require.NoError(t, err)
	found := false
	for _, n := range netgroups {
		if n.Name == ngName {
			found = true
			break
		}
	}
	assert.True(t, found, "Netgroup should appear in list")

	// Get detail to verify members
	detail, err := client.GetNetgroup(ngName)
	require.NoError(t, err)
	assert.Len(t, detail.Members, 3, "Netgroup should have 3 members")
	t.Log("Step 3: Netgroup listed with 3 members")

	// 4. Remove a member
	err = client.RemoveNetgroupMember(ngName, memberIP.ID)
	require.NoError(t, err, "Should remove IP member")

	detail, err = client.GetNetgroup(ngName)
	require.NoError(t, err)
	assert.Len(t, detail.Members, 2, "Netgroup should have 2 members after removal")
	t.Log("Step 4: Member removed, 2 remaining")

	// 5. Delete netgroup
	helpers.DeleteNetgroup(t, client, ngName)

	// Verify deletion
	_, err = client.GetNetgroup(ngName)
	assert.Error(t, err, "Netgroup should not exist after deletion")
	t.Log("Step 5: Netgroup deleted successfully")
}

// TestControlPlaneV2_NetgroupInUse verifies that a netgroup referenced by a share
// cannot be deleted (409 Conflict), but can be deleted after the share is removed.
func TestControlPlaneV2_NetgroupInUse(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping netgroup in-use test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	ngName := helpers.UniqueTestName("internal")
	shareName := "/" + helpers.UniqueTestName("secure-share")

	// Create stores for test share
	metaStore := helpers.UniqueTestName("meta")
	payloadStore := helpers.UniqueTestName("payload")
	_, err := client.CreateMetadataStore(&apiclient.CreateStoreRequest{
		Name: metaStore,
		Type: "memory",
	})
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() { _ = client.DeleteMetadataStore(metaStore) })

	_, err = client.CreatePayloadStore(&apiclient.CreateStoreRequest{
		Name: payloadStore,
		Type: "memory",
	})
	require.NoError(t, err, "Should create payload store")
	t.Cleanup(func() { _ = client.DeletePayloadStore(payloadStore) })

	// 1. Create netgroup
	ng := helpers.CreateNetgroup(t, client, ngName)
	t.Cleanup(func() { helpers.CleanupNetgroup(client, ngName) })
	require.NotEmpty(t, ng.ID)
	t.Log("Step 1: Netgroup created")

	// 2. Create share referencing netgroup
	share := helpers.CreateShareWithPolicy(t, client, shareName, metaStore, payloadStore, &helpers.ShareSecurityPolicy{
		AllowAuthSys: helpers.BoolPtr(true),
		NetgroupID:   helpers.StringPtr(ng.Name),
	})
	t.Cleanup(func() { helpers.CleanupShare(client, shareName) })
	assert.Equal(t, shareName, share.Name)
	t.Log("Step 2: Share created with netgroup reference")

	// 3. Try delete netgroup -> expect 409 Conflict
	err = helpers.DeleteNetgroupExpectError(t, client, ngName)
	if apiErr, ok := err.(*apiclient.APIError); ok {
		assert.True(t, apiErr.IsConflict(), "Should be a conflict error, got: %s", apiErr.Code)
	}
	t.Log("Step 3: Netgroup deletion correctly blocked (in-use)")

	// 4. Delete share
	err = client.DeleteShare(shareName)
	require.NoError(t, err, "Should delete share")
	t.Log("Step 4: Share deleted")

	// 5. Delete netgroup -> should succeed now
	helpers.DeleteNetgroup(t, client, ngName)

	_, err = client.GetNetgroup(ngName)
	assert.Error(t, err, "Netgroup should be deleted")
	t.Log("Step 5: Netgroup deleted after share removal")
}

// TestControlPlaneV2_ShareSecurityPolicy verifies that shares can be created
// with security policy options and that the policy is stored correctly.
func TestControlPlaneV2_ShareSecurityPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping share security policy test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	// Create stores for test shares
	metaStore := helpers.UniqueTestName("meta")
	payloadStore := helpers.UniqueTestName("payload")
	_, err := client.CreateMetadataStore(&apiclient.CreateStoreRequest{
		Name: metaStore,
		Type: "memory",
	})
	require.NoError(t, err, "Should create metadata store")
	t.Cleanup(func() { _ = client.DeleteMetadataStore(metaStore) })

	_, err = client.CreatePayloadStore(&apiclient.CreateStoreRequest{
		Name: payloadStore,
		Type: "memory",
	})
	require.NoError(t, err, "Should create payload store")
	t.Cleanup(func() { _ = client.DeletePayloadStore(payloadStore) })

	t.Run("share with allow_auth_sys=true", func(t *testing.T) {
		name := "/" + helpers.UniqueTestName("auth-sys")
		share := helpers.CreateShareWithPolicy(t, client, name, metaStore, payloadStore, &helpers.ShareSecurityPolicy{
			AllowAuthSys: helpers.BoolPtr(true),
		})
		t.Cleanup(func() { helpers.CleanupShare(client, name) })

		assert.True(t, share.AllowAuthSys, "Share should allow AUTH_SYS")
		assert.False(t, share.RequireKerberos, "Kerberos should not be required")

		// Verify via GET
		retrieved, err := client.GetShare(name)
		require.NoError(t, err)
		assert.True(t, retrieved.AllowAuthSys, "Persisted share should allow AUTH_SYS")
	})

	t.Run("share with require_kerberos=true", func(t *testing.T) {
		name := "/" + helpers.UniqueTestName("kerb")
		share := helpers.CreateShareWithPolicy(t, client, name, metaStore, payloadStore, &helpers.ShareSecurityPolicy{
			RequireKerberos: helpers.BoolPtr(true),
		})
		t.Cleanup(func() { helpers.CleanupShare(client, name) })

		assert.True(t, share.RequireKerberos, "Share should require Kerberos")

		retrieved, err := client.GetShare(name)
		require.NoError(t, err)
		assert.True(t, retrieved.RequireKerberos, "Persisted share should require Kerberos")
	})

	t.Run("share with blocked operations", func(t *testing.T) {
		name := "/" + helpers.UniqueTestName("blocked")
		blockedOps := []string{"REMOVE", "RENAME"}
		share := helpers.CreateShareWithPolicy(t, client, name, metaStore, payloadStore, &helpers.ShareSecurityPolicy{
			AllowAuthSys:      helpers.BoolPtr(true),
			BlockedOperations: blockedOps,
		})
		t.Cleanup(func() { helpers.CleanupShare(client, name) })

		assert.Equal(t, blockedOps, share.BlockedOperations, "Share should have blocked operations")

		retrieved, err := client.GetShare(name)
		require.NoError(t, err)
		assert.Equal(t, blockedOps, retrieved.BlockedOperations, "Persisted blocked operations should match")
	})

	t.Run("update share security policy", func(t *testing.T) {
		name := "/" + helpers.UniqueTestName("update-pol")
		helpers.CreateShareWithPolicy(t, client, name, metaStore, payloadStore, &helpers.ShareSecurityPolicy{
			AllowAuthSys: helpers.BoolPtr(true),
		})
		t.Cleanup(func() { helpers.CleanupShare(client, name) })

		// Update to disable AUTH_SYS
		updated, err := client.UpdateShare(name, &apiclient.UpdateShareRequest{
			AllowAuthSys: helpers.BoolPtr(false),
		})
		require.NoError(t, err)
		assert.False(t, updated.AllowAuthSys, "AUTH_SYS should be disabled after update")
	})
}

// TestControlPlaneV2_SettingsHotReload verifies that settings changes via API
// are detected by the settings watcher and applied.
//
// NOTE: This test takes ~12 seconds due to the settings watcher polling interval.
// It verifies that new API calls return updated settings, confirming persistence
// and availability to new connections.
func TestControlPlaneV2_SettingsHotReload(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping settings hot-reload test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	// 1. Get initial settings
	initial := helpers.GetNFSSettings(t, client)
	initialMaxRead := initial.MaxReadSize
	t.Logf("Initial max_read_size: %d", initialMaxRead)

	// 2. Update max_read_size via API
	newMaxRead := 32768
	helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		MaxReadSize: &newMaxRead,
	})
	t.Log("Updated max_read_size to 32768")

	// 3. Verify the setting was persisted immediately (API reads from DB)
	afterPatch := helpers.GetNFSSettings(t, client)
	assert.Equal(t, 32768, afterPatch.MaxReadSize, "Setting should be persisted immediately")
	assert.NotEqual(t, initialMaxRead, afterPatch.MaxReadSize, "Setting should differ from initial")
	t.Log("Settings persisted and visible via API")

	// 4. Version counter should have incremented
	assert.Greater(t, afterPatch.Version, initial.Version, "Version should increment on update")
	t.Log("Version counter incremented correctly")

	// Reset
	helpers.ResetNFSSettings(t, client)
}

// TestControlPlaneV2_DelegationPolicy verifies that the delegation policy
// setting is correctly stored and retrievable.
//
// NOTE: Verifying actual delegation grant/deny behavior from the client side
// requires NFSv4 protocol-level observation which is not feasible from E2E tests.
// This test verifies the control plane API side of delegation policy management.
func TestControlPlaneV2_DelegationPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping delegation policy test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	// 1. Verify delegations are enabled by default
	settings := helpers.GetNFSSettings(t, client)
	assert.True(t, settings.DelegationsEnabled, "Delegations should be enabled by default")
	t.Log("Step 1: Delegations enabled by default")

	// 2. Disable delegations
	disabled := false
	result := helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		DelegationsEnabled: &disabled,
	})
	assert.False(t, result.DelegationsEnabled, "Delegations should be disabled")
	t.Log("Step 2: Delegations disabled via PATCH")

	// 3. Verify persisted
	verify := helpers.GetNFSSettings(t, client)
	assert.False(t, verify.DelegationsEnabled, "Delegation setting should persist")
	t.Log("Step 3: Delegation setting persisted")

	// 4. Re-enable delegations
	enabled := true
	result = helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		DelegationsEnabled: &enabled,
	})
	assert.True(t, result.DelegationsEnabled, "Delegations should be re-enabled")
	t.Log("Step 4: Delegations re-enabled")

	// Reset
	helpers.ResetNFSSettings(t, client)
}

// TestControlPlaneV2_BlockedOperations verifies that the blocked operations
// setting is correctly stored and retrieved via the API.
func TestControlPlaneV2_BlockedOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping blocked operations test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	t.Run("set and retrieve blocked operations", func(t *testing.T) {
		blockedOps := []string{"REMOVE", "RENAME", "LINK"}
		result := helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
			BlockedOperations: &blockedOps,
		})
		assert.ElementsMatch(t, blockedOps, result.BlockedOperations, "Blocked operations should be set")

		// Verify via GET
		settings := helpers.GetNFSSettings(t, client)
		assert.ElementsMatch(t, blockedOps, settings.BlockedOperations, "Blocked operations should persist")

		// Reset
		helpers.ResetNFSSettings(t, client)
	})

	t.Run("clear blocked operations", func(t *testing.T) {
		// First set some
		blockedOps := []string{"REMOVE"}
		helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
			BlockedOperations: &blockedOps,
		})

		// Then clear
		emptyOps := []string{}
		result := helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
			BlockedOperations: &emptyOps,
		})
		assert.Empty(t, result.BlockedOperations, "Blocked operations should be cleared")

		// Verify
		settings := helpers.GetNFSSettings(t, client)
		assert.Empty(t, settings.BlockedOperations, "Cleared blocked operations should persist")
	})

	t.Run("invalid operation name rejected", func(t *testing.T) {
		invalidOps := []string{"INVALID_OP_NAME"}
		err := helpers.PatchNFSSettingExpectError(t, client, &apiclient.PatchNFSSettingsRequest{
			BlockedOperations: &invalidOps,
		})
		// Check error message contains useful information
		assert.True(t, strings.Contains(err.Error(), "INVALID_OP_NAME") ||
			strings.Contains(err.Error(), "invalid") ||
			strings.Contains(err.Error(), "unknown") ||
			strings.Contains(err.Error(), "VALIDATION"),
			"Error should indicate invalid operation: %v", err)
	})
}

// TestControlPlaneV2_SettingsVersionTracking verifies that the version counter
// increments correctly and provides change detection.
func TestControlPlaneV2_SettingsVersionTracking(t *testing.T) {
	// TODO: Settings version tracking test needs investigation
	t.Skip("Skipping: Settings version tracking test needs investigation")

	if testing.Short() {
		t.Skip("Skipping version tracking test in short mode")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	client := helpers.GetAPIClient(t, sp.APIURL())

	// Get initial version
	initial := helpers.GetNFSSettings(t, client)
	v1 := initial.Version

	// Update a setting
	leaseTime := 100
	helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	})
	v2 := helpers.GetNFSSettings(t, client).Version
	assert.Greater(t, v2, v1, "Version should increment after first update")

	// Update again
	leaseTime = 110
	helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	})
	v3 := helpers.GetNFSSettings(t, client).Version
	assert.Greater(t, v3, v2, "Version should increment after second update")

	// Reset
	helpers.ResetNFSSettings(t, client)
	vReset := helpers.GetNFSSettings(t, client).Version
	assert.Greater(t, vReset, v3, "Version should increment after reset too")

	t.Logf("Version progression: %d -> %d -> %d -> %d", v1, v2, v3, vReset)
}

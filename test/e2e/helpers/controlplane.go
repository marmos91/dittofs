//go:build e2e

package helpers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Control Plane v2.0 E2E Test Helpers
// =============================================================================

// GetAPIClient creates an authenticated API client connected to the test server.
// It logs in as admin and returns a client with the access token set.
func GetAPIClient(t *testing.T, serverURL string) *apiclient.Client {
	t.Helper()

	client := apiclient.New(serverURL)
	tokens, err := client.Login("admin", GetAdminPassword())
	require.NoError(t, err, "Failed to login as admin")

	return client.WithToken(tokens.AccessToken)
}

// =============================================================================
// Settings Helpers
// =============================================================================

// GetNFSSettings retrieves current NFS adapter settings via the API.
func GetNFSSettings(t *testing.T, client *apiclient.Client) *apiclient.NFSAdapterSettingsResponse {
	t.Helper()

	settings, err := client.GetNFSSettings()
	require.NoError(t, err, "Failed to get NFS settings")

	return settings
}

// PatchNFSSetting updates a single NFS setting field via PATCH.
func PatchNFSSetting(t *testing.T, client *apiclient.Client, req *apiclient.PatchNFSSettingsRequest, opts ...apiclient.SettingsOption) *apiclient.NFSAdapterSettingsResponse {
	t.Helper()

	settings, err := client.PatchNFSSettings(req, opts...)
	require.NoError(t, err, "Failed to patch NFS settings")

	return settings
}

// PatchNFSSettingExpectError attempts to patch NFS settings and expects an error.
// Returns the error for assertion.
func PatchNFSSettingExpectError(t *testing.T, client *apiclient.Client, req *apiclient.PatchNFSSettingsRequest, opts ...apiclient.SettingsOption) error {
	t.Helper()

	_, err := client.PatchNFSSettings(req, opts...)
	require.Error(t, err, "Expected PATCH to fail")

	return err
}

// ResetNFSSettings resets all NFS settings to defaults.
func ResetNFSSettings(t *testing.T, client *apiclient.Client) *apiclient.NFSAdapterSettingsResponse {
	t.Helper()

	settings, err := client.ResetNFSSettings("")
	require.NoError(t, err, "Failed to reset NFS settings")

	return settings
}

// =============================================================================
// Netgroup Helpers
// =============================================================================

// CreateNetgroup creates a netgroup via the API client.
func CreateNetgroup(t *testing.T, client *apiclient.Client, name string) *apiclient.Netgroup {
	t.Helper()

	ng, err := client.CreateNetgroup(name)
	require.NoError(t, err, "Failed to create netgroup %s", name)

	return ng
}

// AddNetgroupMember adds a member to a netgroup via the API client.
func AddNetgroupMember(t *testing.T, client *apiclient.Client, netgroupName, memberType, value string) *apiclient.NetgroupMember {
	t.Helper()

	member, err := client.AddNetgroupMember(netgroupName, memberType, value)
	require.NoError(t, err, "Failed to add member %s/%s to netgroup %s", memberType, value, netgroupName)

	return member
}

// DeleteNetgroup deletes a netgroup via the API client.
func DeleteNetgroup(t *testing.T, client *apiclient.Client, name string) {
	t.Helper()

	err := client.DeleteNetgroup(name)
	require.NoError(t, err, "Failed to delete netgroup %s", name)
}

// DeleteNetgroupExpectError attempts to delete a netgroup and expects an error.
func DeleteNetgroupExpectError(t *testing.T, client *apiclient.Client, name string) error {
	t.Helper()

	err := client.DeleteNetgroup(name)
	require.Error(t, err, "Expected delete netgroup to fail")

	return err
}

// CleanupNetgroup deletes a netgroup if it exists (best-effort, for t.Cleanup).
func CleanupNetgroup(client *apiclient.Client, name string) {
	_ = client.DeleteNetgroup(name)
}

// =============================================================================
// Share with Security Policy Helpers
// =============================================================================

// ShareSecurityPolicy defines security policy options for share creation.
type ShareSecurityPolicy struct {
	AllowAuthSys      *bool
	RequireKerberos   *bool
	NetgroupID        *string
	BlockedOperations []string
}

// CreateShareWithPolicy creates a share with the given security policy via the API client.
// Returns the created share.
func CreateShareWithPolicy(t *testing.T, client *apiclient.Client, name, metadataStore, payloadStore string, policy *ShareSecurityPolicy) *apiclient.Share {
	t.Helper()

	req := &apiclient.CreateShareRequest{
		Name:            name,
		MetadataStoreID: metadataStore,
		PayloadStoreID:  payloadStore,
	}

	if policy != nil {
		req.AllowAuthSys = policy.AllowAuthSys
		req.RequireKerberos = policy.RequireKerberos
		req.NetgroupID = policy.NetgroupID
		req.BlockedOperations = &policy.BlockedOperations
	}

	share, err := client.CreateShare(req)
	require.NoError(t, err, "Failed to create share %s", name)

	return share
}

// CleanupShare deletes a share if it exists (best-effort, for t.Cleanup).
func CleanupShare(client *apiclient.Client, name string) {
	_ = client.DeleteShare(name)
}

// =============================================================================
// Wait Helpers
// =============================================================================

// WaitForSettingsReload waits long enough for the settings watcher to pick up
// changes. The default polling interval is 10 seconds, so we wait 12 seconds
// to account for timing jitter.
func WaitForSettingsReload(t *testing.T) {
	t.Helper()
	t.Log("Waiting for settings watcher reload (12s)...")
	time.Sleep(12 * time.Second)
}

// =============================================================================
// Pointer Helpers
// =============================================================================

// BoolPtr returns a pointer to a bool value.
func BoolPtr(v bool) *bool {
	return &v
}

// IntPtr returns a pointer to an int value.
func IntPtr(v int) *int {
	return &v
}

// StringPtr returns a pointer to a string value.
func StringPtr(v string) *string {
	return &v
}

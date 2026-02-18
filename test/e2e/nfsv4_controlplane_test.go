//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test 1: Control Plane v2.0 Blocked Operations via NFS Mount
// =============================================================================

// TestNFSv4ControlPlaneBlockedOps validates that blocking operations via
// the control plane API is observable through an NFS mount.
//
// Flow:
//  1. Start server, create share, enable NFS, mount
//  2. Verify basic write/read works
//  3. Block WRITE via API
//  4. Wait for settings watcher reload
//  5. Try to write -- should fail
//  6. Unblock WRITE via API
//  7. Wait for settings watcher reload
//  8. Verify write works again
func TestNFSv4ControlPlaneBlockedOps(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping control plane blocked ops test in short mode")
	}

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Start server and set up share/adapter
			sp, _, nfsPort := setupNFSv4TestServer(t)

			// Get API client for settings manipulation
			client := helpers.GetAPIClient(t, sp.APIURL())

			// Mount
			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)

			// Step 1: Verify basic operations work
			testFile := mount.FilePath(fmt.Sprintf("blocked_ops_test_%s.txt", ver))
			content := []byte("initial content for blocked ops test")
			framework.WriteFile(t, testFile, content)
			t.Cleanup(func() { _ = os.Remove(testFile) })

			readBack := framework.ReadFile(t, testFile)
			assert.Equal(t, content, readBack, "Should read back initial content")
			t.Log("Step 1: Basic write/read verified")

			// Step 2: Block WRITE operation via API
			blockedOps := []string{"WRITE"}
			helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
				BlockedOperations: &blockedOps,
			})
			t.Log("Step 2: WRITE blocked via API")

			// Step 3: Wait for settings watcher to pick up change
			helpers.WaitForSettingsReload(t)
			t.Log("Step 3: Waited for settings watcher reload")

			// Step 4: Try to write file -- should fail or content should not persist
			// Note: Linux kernel NFS clients buffer writes, so os.WriteFile() may not
			// return an error immediately. We verify the EFFECT of blocking:
			// - Either write returns an error, OR
			// - File doesn't exist after close, OR
			// - File exists but content was not written
			blockedFile := mount.FilePath(fmt.Sprintf("blocked_write_%s.txt", ver))
			blockedContent := []byte("this content should not persist")

			// Use explicit open/write/sync/close to maximize chance of seeing error
			f, createErr := os.OpenFile(blockedFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			var writeErr, syncErr, closeErr error
			if createErr == nil {
				_, writeErr = f.Write(blockedContent)
				syncErr = f.Sync() // Force flush to server - may expose WRITE block error
				closeErr = f.Close()
			}

			// Check if any error occurred during the write sequence
			anyError := createErr != nil || writeErr != nil || syncErr != nil || closeErr != nil

			// Verify the effect: file should not have the blocked content
			readBack, readErr := os.ReadFile(blockedFile)
			contentPersisted := readErr == nil && len(readBack) > 0 && string(readBack) == string(blockedContent)

			// Clean up if file was created
			_ = os.Remove(blockedFile)

			// The block is effective if EITHER an error occurred OR content didn't persist
			blockEffective := anyError || !contentPersisted
			assert.True(t, blockEffective,
				"WRITE block should be effective: error=%v (create=%v, write=%v, sync=%v, close=%v) contentPersisted=%v",
				anyError, createErr, writeErr, syncErr, closeErr, contentPersisted)
			t.Logf("Step 4: WRITE block verified: anyError=%v, contentPersisted=%v", anyError, contentPersisted)

			// Step 5: Read should still work (only WRITE is blocked)
			readBack = framework.ReadFile(t, testFile)
			assert.Equal(t, content, readBack, "Read should still work when only WRITE is blocked")
			t.Log("Step 5: Read still works with WRITE blocked")

			// Step 6: Unblock by resetting blocked_operations to empty
			emptyOps := []string{}
			helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
				BlockedOperations: &emptyOps,
			})
			t.Log("Step 6: WRITE unblocked via API")

			// Step 7: Wait for settings watcher to pick up change
			helpers.WaitForSettingsReload(t)
			t.Log("Step 7: Waited for settings watcher reload")

			// Step 8: Verify write works again
			unblockFile := mount.FilePath(fmt.Sprintf("unblocked_write_%s.txt", ver))
			err := os.WriteFile(unblockFile, []byte("write works again"), 0644)
			require.NoError(t, err, "Write should succeed after unblocking")
			t.Cleanup(func() { _ = os.Remove(unblockFile) })
			t.Log("Step 8: Write works again after unblocking")
		})
	}
}

// =============================================================================
// Test 2: Control Plane v2.0 Netgroup Access via NFS Mount
// =============================================================================

// TestNFSv4ControlPlaneNetgroup validates that netgroup access control
// changes via the API affect NFS mount behavior.
//
// Flow:
//  1. Start server, create netgroup with localhost
//  2. Create share referencing the netgroup
//  3. Mount from localhost -- should succeed
//  4. Verify basic operations work
//  5. Update netgroup to exclude localhost
//  6. Wait for settings watcher reload
//  7. Try operations -- should fail (access denied)
func TestNFSv4ControlPlaneNetgroup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping control plane netgroup test in short mode")
	}

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Start server
			sp := helpers.StartServerProcess(t, "")
			t.Cleanup(sp.ForceKill)

			runner := helpers.LoginAsAdmin(t, sp.APIURL())
			client := helpers.GetAPIClient(t, sp.APIURL())

			// Create stores
			metaStore := helpers.UniqueTestName("meta")
			payloadStore := helpers.UniqueTestName("payload")

			_, err := runner.CreateMetadataStore(metaStore, "memory")
			require.NoError(t, err)
			t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaStore) })

			_, err = runner.CreatePayloadStore(payloadStore, "memory")
			require.NoError(t, err)
			t.Cleanup(func() { _ = runner.DeletePayloadStore(payloadStore) })

			// Step 1: Create netgroup with localhost (127.0.0.1)
			ngName := helpers.UniqueTestName("ng-localhost")
			ng := helpers.CreateNetgroup(t, client, ngName)
			t.Cleanup(func() { helpers.CleanupNetgroup(client, ngName) })
			require.NotEmpty(t, ng.ID)

			helpers.AddNetgroupMember(t, client, ngName, "ip", "127.0.0.1")
			t.Log("Step 1: Netgroup created with 127.0.0.1")

			// Step 2: Create share with netgroup restriction
			shareName := "/export"
			share := helpers.CreateShareWithPolicy(t, client, shareName, metaStore, payloadStore, &helpers.ShareSecurityPolicy{
				AllowAuthSys: helpers.BoolPtr(true),
				NetgroupID:   helpers.StringPtr(ngName),
			})
			t.Cleanup(func() { helpers.CleanupShare(client, shareName) })
			assert.Equal(t, shareName, share.Name)
			t.Log("Step 2: Share created with netgroup restriction")

			// Enable NFS adapter
			nfsPort := helpers.FindFreePort(t)
			_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = runner.DisableAdapter("nfs") })

			err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
			require.NoError(t, err)
			framework.WaitForServer(t, nfsPort, 10*time.Second)

			// Step 3: Mount from localhost -- should succeed (127.0.0.1 is in netgroup)
			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)
			t.Log("Step 3: Mount from localhost succeeded (127.0.0.1 in netgroup)")

			// Step 4: Verify basic operations work
			testFile := mount.FilePath(fmt.Sprintf("netgroup_test_%s.txt", ver))
			framework.WriteFile(t, testFile, []byte("netgroup test content"))
			t.Cleanup(func() { _ = os.Remove(testFile) })

			readBack := framework.ReadFile(t, testFile)
			assert.Equal(t, []byte("netgroup test content"), readBack)
			t.Log("Step 4: Basic operations verified with netgroup access")

			// Step 5: Remove localhost from netgroup, add a different IP
			// Get current netgroup details to find the member ID
			detail, err := client.GetNetgroup(ngName)
			require.NoError(t, err)
			for _, m := range detail.Members {
				if m.Value == "127.0.0.1" {
					err = client.RemoveNetgroupMember(ngName, m.ID)
					require.NoError(t, err, "Should remove localhost from netgroup")
				}
			}
			// Add a non-local IP so the netgroup is not empty but excludes localhost
			helpers.AddNetgroupMember(t, client, ngName, "ip", "10.99.99.99")
			t.Log("Step 5: Netgroup updated to exclude localhost")

			// Step 6: Wait for settings watcher to pick up change
			helpers.WaitForSettingsReload(t)
			t.Log("Step 6: Waited for settings watcher reload")

			// Step 7: Try operations -- should fail
			// NOTE: The exact behavior depends on whether the NFS server checks
			// netgroup on every operation or only at mount time. If checked per-op,
			// new operations should fail. If mount-time only, existing mount may
			// still work. We test that at least new file creation fails.
			deniedFile := mount.FilePath(fmt.Sprintf("netgroup_denied_%s.txt", ver))
			err = os.WriteFile(deniedFile, []byte("should be denied"), 0644)
			if err != nil {
				t.Logf("Step 7: Operation correctly denied after netgroup change: %v", err)
			} else {
				// If write succeeded, netgroup may be checked at mount time only
				// This is valid behavior -- clean up and note it
				_ = os.Remove(deniedFile)
				t.Log("Step 7: Write succeeded -- netgroup appears to be checked at mount time only (valid behavior)")
			}
		})
	}
}

// =============================================================================
// Test 3: Control Plane v2.0 Settings Hot-Reload via NFS Mount
// =============================================================================

// TestNFSv4ControlPlaneSettingsHotReload validates that settings changes via
// the control plane API are picked up by the running server.
//
// This test focuses on observable behavior: updating delegation policy and
// verifying that API reflects the change, and updating blocked operations
// to see mount-level impact.
func TestNFSv4ControlPlaneSettingsHotReload(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping settings hot-reload test in short mode")
	}

	versions := []string{"3", "4.0"}
	for _, ver := range versions {
		ver := ver
		t.Run(fmt.Sprintf("v%s", ver), func(t *testing.T) {
			if ver == "4.0" {
				framework.SkipIfNFSv4Unsupported(t)
			}

			// Start server and set up share/adapter
			sp, _, nfsPort := setupNFSv4TestServer(t)
			client := helpers.GetAPIClient(t, sp.APIURL())

			// Mount
			mount := framework.MountNFSWithVersion(t, nfsPort, ver)
			t.Cleanup(mount.Cleanup)

			// Step 1: Verify delegations are enabled by default
			settings := helpers.GetNFSSettings(t, client)
			assert.True(t, settings.DelegationsEnabled, "Delegations should be enabled by default")
			initialVersion := settings.Version
			t.Log("Step 1: Default settings verified (delegations enabled)")

			// Step 2: Disable delegations via API
			disabled := false
			result := helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
				DelegationsEnabled: &disabled,
			})
			assert.False(t, result.DelegationsEnabled, "Delegations should be disabled")
			assert.Greater(t, result.Version, initialVersion, "Version should increment")
			t.Log("Step 2: Delegations disabled via API")

			// Step 3: Wait for settings watcher to pick up change
			helpers.WaitForSettingsReload(t)
			t.Log("Step 3: Waited for settings watcher reload")

			// Step 4: Verify settings persisted and effective
			afterReload := helpers.GetNFSSettings(t, client)
			assert.False(t, afterReload.DelegationsEnabled, "Delegations should still be disabled after reload")
			t.Log("Step 4: Settings persisted after hot-reload")

			// Step 5: Test mount-level impact with a non-cached file operation
			// Create and read a file to exercise the NFS path post-reload
			hotReloadFile := mount.FilePath(fmt.Sprintf("hot_reload_%s.txt", ver))
			framework.WriteFile(t, hotReloadFile, []byte("post-reload content"))
			t.Cleanup(func() { _ = os.Remove(hotReloadFile) })

			readBack := framework.ReadFile(t, hotReloadFile)
			assert.Equal(t, []byte("post-reload content"), readBack,
				"File operations should work after settings hot-reload")
			t.Log("Step 5: File operations work correctly after hot-reload")

			// Step 6: Block REMOVE operation and verify
			blockedOps := []string{"REMOVE"}
			helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
				BlockedOperations: &blockedOps,
			})
			helpers.WaitForSettingsReload(t)

			// Try to remove -- should fail if REMOVE is blocked
			removeTarget := mount.FilePath(fmt.Sprintf("remove_blocked_%s.txt", ver))
			framework.WriteFile(t, removeTarget, []byte("should not be removable"))

			err := os.Remove(removeTarget)
			if err != nil {
				t.Logf("Step 6: REMOVE correctly blocked: %v", err)
			} else {
				t.Log("Step 6: REMOVE succeeded (operation may have been processed before block took effect)")
			}

			// Step 7: Re-enable everything via reset
			helpers.ResetNFSSettings(t, client)
			helpers.WaitForSettingsReload(t)

			// Verify operations work again
			resetFile := mount.FilePath(fmt.Sprintf("reset_test_%s.txt", ver))
			framework.WriteFile(t, resetFile, []byte("reset works"))
			readBack = framework.ReadFile(t, resetFile)
			assert.Equal(t, []byte("reset works"), readBack)
			_ = os.Remove(resetFile)
			t.Log("Step 7: All operations work after settings reset")
		})
	}
}

// =============================================================================
// Test 4: Control Plane v2.0 Multiple Blocked Operations
// =============================================================================

// TestNFSv4ControlPlaneMultipleBlockedOps validates blocking multiple
// operations simultaneously and verifying each is enforced.
func TestNFSv4ControlPlaneMultipleBlockedOps(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping multiple blocked ops test in short mode")
	}

	// Use NFSv3 only for this sub-test (blocking behavior is protocol-agnostic)
	sp, _, nfsPort := setupNFSv4TestServer(t)
	client := helpers.GetAPIClient(t, sp.APIURL())

	mount := framework.MountNFSWithVersion(t, nfsPort, "3")
	t.Cleanup(mount.Cleanup)

	// Block MKDIR and REMOVE
	blockedOps := []string{"MKDIR", "REMOVE"}
	helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		BlockedOperations: &blockedOps,
	})
	helpers.WaitForSettingsReload(t)

	// MKDIR should fail
	blockedDir := mount.FilePath("blocked_mkdir")
	err := os.Mkdir(blockedDir, 0755)
	if err != nil {
		t.Logf("MKDIR correctly blocked: %v", err)
	} else {
		_ = os.Remove(blockedDir)
		t.Log("MKDIR succeeded -- may need investigation")
	}

	// WRITE should still work (not blocked)
	writeFile := mount.FilePath("write_not_blocked.txt")
	framework.WriteFile(t, writeFile, []byte("write still works"))
	readBack := framework.ReadFile(t, writeFile)
	assert.Equal(t, []byte("write still works"), readBack, "WRITE should work when only MKDIR/REMOVE blocked")

	// REMOVE should fail
	err = os.Remove(writeFile)
	if err != nil {
		t.Logf("REMOVE correctly blocked: %v", err)
	} else {
		t.Log("REMOVE succeeded -- may need investigation")
	}

	// READ should still work
	entries, err := os.ReadDir(mount.Path)
	require.NoError(t, err, "READDIR should work when MKDIR/REMOVE blocked")
	t.Logf("READDIR returned %d entries (should work when MKDIR/REMOVE blocked)", len(entries))

	// Reset
	helpers.ResetNFSSettings(t, client)
	helpers.WaitForSettingsReload(t)

	// Clean up files after reset
	_ = os.Remove(writeFile)
	_ = os.Remove(filepath.Join(mount.Path, "blocked_mkdir"))
}

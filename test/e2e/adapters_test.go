//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdapterLifecycle validates protocol adapter lifecycle management via CLI.
// This covers requirements ADP-01 through ADP-08 for adapter enable/disable,
// port configuration, and hot reload capabilities.
//
// Note: These tests run sequentially (not parallel) to avoid port conflicts
// between adapter operations.
func TestAdapterLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping adapter lifecycle tests in short mode")
	}

	// Start a server for all adapter tests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	// Login as admin to get CLI runner
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	// Run subtests sequentially (not parallel) to avoid port conflicts
	t.Run("ADP-01 ADP-02 NFS enable disable cycle", func(t *testing.T) {
		testNFSEnableDisableCycle(t, runner)
	})

	t.Run("ADP-03 ADP-04 SMB enable disable cycle", func(t *testing.T) {
		testSMBEnableDisableCycle(t, runner)
	})

	t.Run("ADP-05 port configuration change", func(t *testing.T) {
		testPortConfigurationChange(t, runner)
	})

	t.Run("ADP-06 hot reload without restart", func(t *testing.T) {
		testHotReloadWithoutRestart(t, sp, runner)
	})

	t.Run("ADP-07 adapter clean restart", func(t *testing.T) {
		testAdapterCleanRestart(t, runner)
	})

	t.Run("ADP-08 invalid config rejection", func(t *testing.T) {
		testInvalidConfigRejection(t, runner)
	})
}

// testNFSEnableDisableCycle tests ADP-01 (enable NFS) and ADP-02 (disable NFS).
func testNFSEnableDisableCycle(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()

	// Get a unique port for this test
	port := helpers.FindFreePort(t)

	// Enable NFS adapter
	adapter, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(port))
	require.NoError(t, err, "Should enable NFS adapter")
	assert.Equal(t, "nfs", adapter.Type)
	assert.True(t, adapter.Enabled, "NFS adapter should be enabled")
	assert.Equal(t, port, adapter.Port, "NFS adapter should use specified port")

	// Wait for adapter to be fully enabled
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Verify via list
	adapters, err := runner.ListAdapters()
	require.NoError(t, err, "Should list adapters")
	nfsFound := false
	for _, a := range adapters {
		if a.Type == "nfs" {
			nfsFound = true
			assert.True(t, a.Enabled, "NFS should be enabled in list")
			break
		}
	}
	assert.True(t, nfsFound, "NFS adapter should appear in list")

	// Disable NFS adapter
	adapter, err = runner.DisableAdapter("nfs")
	require.NoError(t, err, "Should disable NFS adapter")
	assert.False(t, adapter.Enabled, "NFS adapter should be disabled")

	// Wait for adapter to be fully disabled
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", false, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become disabled")

	// Verify disabled via GetAdapter
	adapter, err = runner.GetAdapter("nfs")
	require.NoError(t, err, "Should get NFS adapter")
	assert.False(t, adapter.Enabled, "NFS should be disabled after disable call")

	t.Log("ADP-01/ADP-02: NFS enable/disable cycle passed")
}

// testSMBEnableDisableCycle tests ADP-03 (enable SMB) and ADP-04 (disable SMB).
func testSMBEnableDisableCycle(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()

	// Get a unique port for this test
	port := helpers.FindFreePort(t)

	// Enable SMB adapter
	adapter, err := runner.EnableAdapter("smb", helpers.WithAdapterPort(port))
	require.NoError(t, err, "Should enable SMB adapter")
	assert.Equal(t, "smb", adapter.Type)
	assert.True(t, adapter.Enabled, "SMB adapter should be enabled")
	assert.Equal(t, port, adapter.Port, "SMB adapter should use specified port")

	// Wait for adapter to be fully enabled
	err = helpers.WaitForAdapterStatus(t, runner, "smb", true, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become enabled")

	// Verify via list
	adapters, err := runner.ListAdapters()
	require.NoError(t, err, "Should list adapters")
	smbFound := false
	for _, a := range adapters {
		if a.Type == "smb" {
			smbFound = true
			assert.True(t, a.Enabled, "SMB should be enabled in list")
			break
		}
	}
	assert.True(t, smbFound, "SMB adapter should appear in list")

	// Disable SMB adapter
	adapter, err = runner.DisableAdapter("smb")
	require.NoError(t, err, "Should disable SMB adapter")
	assert.False(t, adapter.Enabled, "SMB adapter should be disabled")

	// Wait for adapter to be fully disabled
	err = helpers.WaitForAdapterStatus(t, runner, "smb", false, 5*time.Second)
	require.NoError(t, err, "SMB adapter should become disabled")

	// Verify disabled via GetAdapter
	adapter, err = runner.GetAdapter("smb")
	require.NoError(t, err, "Should get SMB adapter")
	assert.False(t, adapter.Enabled, "SMB should be disabled after disable call")

	t.Log("ADP-03/ADP-04: SMB enable/disable cycle passed")
}

// testPortConfigurationChange tests ADP-05 (port change takes effect).
func testPortConfigurationChange(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()

	// Get two unique ports
	portA := helpers.FindFreePort(t)
	portB := helpers.FindFreePort(t)

	// Enable NFS adapter on port A
	adapter, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(portA))
	require.NoError(t, err, "Should enable NFS adapter on port A")
	assert.Equal(t, portA, adapter.Port, "Should be on port A")

	// Wait for adapter to be enabled
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Change port to B via EditAdapter
	adapter, err = runner.EditAdapter("nfs", helpers.WithAdapterPort(portB))
	require.NoError(t, err, "Should edit NFS adapter port")
	assert.Equal(t, portB, adapter.Port, "Port should change to B")

	// Give server time to restart adapter on new port
	time.Sleep(500 * time.Millisecond)

	// Verify new port via GetAdapter
	adapter, err = runner.GetAdapter("nfs")
	require.NoError(t, err, "Should get NFS adapter")
	assert.Equal(t, portB, adapter.Port, "NFS should be on new port B")

	// Cleanup: disable adapter
	_, err = runner.DisableAdapter("nfs")
	require.NoError(t, err, "Should disable NFS adapter")

	t.Log("ADP-05: Port configuration change passed")
}

// testHotReloadWithoutRestart tests ADP-06 (hot reload without full server restart).
func testHotReloadWithoutRestart(t *testing.T, sp *helpers.ServerProcess, runner *helpers.CLIRunner) {
	t.Helper()

	// Record server PID before adapter change
	pidBefore := sp.PID()
	require.NotZero(t, pidBefore, "Should have valid server PID")

	// Get two unique ports
	portA := helpers.FindFreePort(t)
	portB := helpers.FindFreePort(t)

	// Enable NFS adapter on port A
	_, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(portA))
	require.NoError(t, err, "Should enable NFS adapter")

	// Wait for enabled
	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Change port (this should hot reload the adapter, not restart server)
	_, err = runner.EditAdapter("nfs", helpers.WithAdapterPort(portB))
	require.NoError(t, err, "Should edit NFS adapter port")

	// Give adapter time to restart on new port
	time.Sleep(500 * time.Millisecond)

	// Verify server PID is unchanged (no full restart)
	pidAfter := sp.PID()
	assert.Equal(t, pidBefore, pidAfter, "Server PID should be unchanged (hot reload, no full restart)")

	// Verify adapter is on new port
	adapter, err := runner.GetAdapter("nfs")
	require.NoError(t, err, "Should get NFS adapter")
	assert.Equal(t, portB, adapter.Port, "Adapter should be on new port after hot reload")
	assert.True(t, adapter.Enabled, "Adapter should still be enabled after hot reload")

	// Cleanup
	_, err = runner.DisableAdapter("nfs")
	require.NoError(t, err, "Should disable NFS adapter")

	t.Log("ADP-06: Hot reload without restart passed")
}

// testAdapterCleanRestart tests ADP-07 (adapter stop/start cleanly).
func testAdapterCleanRestart(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()

	port := helpers.FindFreePort(t)

	// Enable adapter
	_, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(port))
	require.NoError(t, err, "Should enable NFS adapter")

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Disable adapter
	_, err = runner.DisableAdapter("nfs")
	require.NoError(t, err, "Should disable NFS adapter")

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", false, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become disabled")

	// Re-enable adapter (clean restart)
	adapter, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(port))
	require.NoError(t, err, "Should re-enable NFS adapter cleanly")
	assert.True(t, adapter.Enabled, "NFS adapter should be enabled after restart")

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled after restart")

	// Verify functionality by checking it appears correctly in list
	adapters, err := runner.ListAdapters()
	require.NoError(t, err, "Should list adapters")
	found := false
	for _, a := range adapters {
		if a.Type == "nfs" && a.Enabled {
			found = true
			break
		}
	}
	assert.True(t, found, "NFS adapter should be running after clean restart")

	// Cleanup
	_, err = runner.DisableAdapter("nfs")
	require.NoError(t, err, "Should disable NFS adapter")

	t.Log("ADP-07: Adapter clean restart passed")
}

// testInvalidConfigRejection tests ADP-08 (invalid config rejected with clear error).
func testInvalidConfigRejection(t *testing.T, runner *helpers.CLIRunner) {
	t.Helper()

	// Test 1: Invalid port number (port > 65535)
	_, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(70000))
	assert.Error(t, err, "Should reject port > 65535")
	if err != nil {
		t.Log("✓ Port > 65535 correctly rejected")
	}

	// Test 2: Invalid port number (negative port)
	// Note: CLI may not pass negative ports, but let's test the server validation
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(-1))
	assert.Error(t, err, "Should reject negative port")
	if err != nil {
		t.Log("✓ Negative port correctly rejected")
	}

	// Test 3: Unknown adapter type
	_, err = runner.EnableAdapter("unknownprotocol")
	assert.Error(t, err, "Should reject unknown adapter type")
	if err != nil {
		t.Log("✓ Unknown adapter type correctly rejected")
	}

	// Verify no invalid adapters were created
	adapters, err := runner.ListAdapters()
	require.NoError(t, err, "Should list adapters")
	for _, a := range adapters {
		assert.NotEqual(t, "unknownprotocol", a.Type, "Unknown adapter type should not exist")
	}

	t.Log("ADP-08: Invalid config rejection passed")
}

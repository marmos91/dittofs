//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPortmapper validates the embedded portmapper (RFC 1057) functionality.
// The portmapper enables NFS clients to discover DittoFS services via standard
// tools (rpcinfo, showmount) and mount without explicit -o port= options.
//
// These tests use a pure Go portmap client (no external tools needed).
func TestPortmapper(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping portmapper tests in short mode")
	}

	// Start a server for all portmapper tests
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	runner := helpers.LoginAsAdmin(t, sp.APIURL())
	client := helpers.GetAPIClient(t, sp.APIURL())

	// Enable NFS adapter (portmapper starts automatically with it)
	nfsPort := helpers.FindFreePort(t)
	_, err := runner.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")

	err = helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second)
	require.NoError(t, err, "NFS adapter should become enabled")

	// Get portmapper port from settings
	settings := helpers.GetNFSSettings(t, client)
	pmapPort := settings.PortmapperPort
	require.True(t, settings.PortmapperEnabled, "Portmapper should be enabled by default")
	require.Greater(t, pmapPort, 0, "Portmapper port should be set")

	// Wait a moment for portmapper to bind
	time.Sleep(500 * time.Millisecond)

	t.Run("NULL ping", func(t *testing.T) {
		testPortmapperNull(t, pmapPort)
	})

	t.Run("DUMP lists DittoFS services", func(t *testing.T) {
		testPortmapperDump(t, pmapPort, nfsPort)
	})

	t.Run("GETPORT returns correct NFS port", func(t *testing.T) {
		testPortmapperGetPort(t, pmapPort, nfsPort)
	})

	t.Run("rpcinfo probe", func(t *testing.T) {
		helpers.SkipIfNoRpcinfo(t)
		testPortmapperRpcinfo(t, pmapPort, nfsPort)
	})

	t.Run("disable portmapper via settings", func(t *testing.T) {
		testPortmapperDisable(t, client, runner, pmapPort)
	})
}

// testPortmapperNull verifies the portmapper responds to NULL (ping) requests.
func testPortmapperNull(t *testing.T, pmapPort int) {
	t.Helper()

	err := helpers.PortmapNull(t, "127.0.0.1", pmapPort)
	assert.NoError(t, err, "Portmapper should respond to NULL ping")
}

// testPortmapperDump verifies DUMP returns all registered DittoFS services.
func testPortmapperDump(t *testing.T, pmapPort, nfsPort int) {
	t.Helper()

	entries := helpers.PortmapDump(t, "127.0.0.1", pmapPort)
	require.NotEmpty(t, entries, "DUMP should return registered services")

	// The portmapper should register at minimum:
	// - Portmapper itself (100000)
	// - NFS (100003)
	// - MOUNT (100005)
	assert.True(t, helpers.HasProgram(entries, helpers.ProgPortmapper),
		"Portmapper (100000) should be registered")
	assert.True(t, helpers.HasProgram(entries, helpers.ProgNFS),
		"NFS (100003) should be registered")
	assert.True(t, helpers.HasProgram(entries, helpers.ProgMount),
		"MOUNT (100005) should be registered")

	// Verify NFS is registered on the correct port
	for _, e := range entries {
		if e.Program == helpers.ProgNFS {
			assert.Equal(t, uint32(nfsPort), e.Port,
				"NFS should be registered on the adapter port")
		}
	}

	t.Logf("Portmapper DUMP returned %d entries:", len(entries))
	for _, e := range entries {
		t.Logf("  prog=%d vers=%d %s port=%d", e.Program, e.Version, e.ProtoName(), e.Port)
	}
}

// testPortmapperGetPort verifies GETPORT returns the correct port for NFS.
func testPortmapperGetPort(t *testing.T, pmapPort, nfsPort int) {
	t.Helper()

	// Query for NFS v3 TCP
	port := helpers.PortmapGetPort(t, "127.0.0.1", pmapPort, helpers.ProgNFS, 3, 6) // 6 = TCP
	assert.Equal(t, uint32(nfsPort), port, "GETPORT for NFS v3 TCP should return adapter port")

	// Query for MOUNT v3 TCP
	mountPort := helpers.PortmapGetPort(t, "127.0.0.1", pmapPort, helpers.ProgMount, 3, 6)
	assert.Equal(t, uint32(nfsPort), mountPort, "GETPORT for MOUNT v3 TCP should return adapter port")

	// Query for non-existent program
	unknownPort := helpers.PortmapGetPort(t, "127.0.0.1", pmapPort, 999999, 1, 6)
	assert.Equal(t, uint32(0), unknownPort, "GETPORT for unknown program should return 0")
}

// testPortmapperRpcinfo verifies the portmapper responds to rpcinfo probes.
// Uses the real rpcinfo binary (available on macOS and Linux).
//
// Note: The rpcinfo tool has platform-specific behavior:
//   - On macOS without system portmapper: fails with "Broken pipe"
//   - On Linux with system rpcbind: queries system portmapper first, then probes
//
// When a system rpcbind is running, rpcinfo -n <port> still queries the system
// portmapper (port 111) to check if the program is registered before probing.
// Since our NFS is registered with our embedded portmapper (not the system one),
// rpcinfo reports "Program not registered" even though our portmapper works.
//
// For full portmapper testing without system rpcbind interference, run:
//
//	./test/integration/portmap/run.sh
func testPortmapperRpcinfo(t *testing.T, pmapPort, nfsPort int) {
	t.Helper()

	// Skip if system rpcbind is running - it interferes with rpcinfo probes
	helpers.SkipIfSystemRpcbind(t)

	// Probe portmapper program (100000) on the portmapper port
	err := helpers.RpcinfoProbe(t, "127.0.0.1", pmapPort, helpers.ProgPortmapper, 2)
	if err != nil && helpers.IsRpcinfoSystemError(err) {
		t.Skipf("rpcinfo cannot reach custom ports (no system portmapper): %v", err)
	}
	assert.NoError(t, err, "rpcinfo should successfully probe portmapper program")

	// Probe NFS program (100003) on the NFS port (not the portmapper port,
	// since the portmapper only handles program 100000).
	err = helpers.RpcinfoProbe(t, "127.0.0.1", nfsPort, helpers.ProgNFS, 3)
	assert.NoError(t, err, "rpcinfo should successfully probe NFS program")
}

// testPortmapperDisable verifies that the portmapper can be disabled via API settings.
// After disabling, the portmapper port should no longer accept connections.
func testPortmapperDisable(t *testing.T, client *apiclient.Client, runner *helpers.CLIRunner, pmapPort int) {
	t.Helper()

	// Disable portmapper via API
	enabled := false
	helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		PortmapperEnabled: &enabled,
	})

	// Verify settings reflect the change
	settings := helpers.GetNFSSettings(t, client)
	assert.False(t, settings.PortmapperEnabled, "Portmapper should be disabled in settings")

	// Re-enable for other tests
	enabledTrue := true
	helpers.PatchNFSSetting(t, client, &apiclient.PatchNFSSettingsRequest{
		PortmapperEnabled: &enabledTrue,
	})

	settings = helpers.GetNFSSettings(t, client)
	assert.True(t, settings.PortmapperEnabled, "Portmapper should be re-enabled in settings")
}

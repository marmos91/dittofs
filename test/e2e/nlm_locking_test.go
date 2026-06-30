//go:build e2e

package e2e

// =============================================================================
// NLM (NFSv3) reachability via the system rpcbind.
//
// A kernel NFSv3 client discovers the NLM (lock manager) port by querying
// rpcbind on port 111 (RFC-fixed). With adapters.nfs.portmapper.register_with_
// system enabled, DittoFS registers its NFS/MOUNT/NLM services with the host's
// system rpcbind so that discovery succeeds and v3 byte-range locking works
// without the `nolock` mount option.
//
// This test asserts that registration lands: after enabling the adapter, a
// portmap GETPORT for NLM/MOUNT/NFS against the system rpcbind must resolve to
// DittoFS's NFS port. It deliberately does NOT mount-and-lock: on a single host
// the kernel's own lockd reclaims the 100021 registration the moment an
// NFSv3-with-locking mount activates, so an end-to-end lock test cannot be
// validated here (it requires a separate client host or network-namespace
// isolation — the same constraint NFS-Ganesha hits, which CI-tests NLM across
// separate hosts). The serving side is mode-independent; what this feature adds
// is the discovery registration, which is exactly what is asserted.
//
// Requires Linux + root (enabling the NFS adapter + reaching rpcbind) + a
// running system rpcbind (the feature registers with it). The portmap GETPORT
// queries use a pure-Go client (helpers.PortmapGetPort), so the rpcinfo binary
// is not needed. Skips otherwise.
// =============================================================================

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

func TestNLMSystemRpcbindRegistration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping NLM system-rpcbind registration test in short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Skipping: NLM registration test is Linux-only (system rpcbind)")
	}
	if os.Geteuid() != 0 {
		t.Skip("Skipping: enabling the NFS adapter + reaching rpcbind needs root")
	}
	if !helpers.HasSystemRpcbind() {
		t.Skip("Skipping: no system rpcbind on port 111 (register_with_system has nothing to register with)")
	}

	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	// Turn on system-rpcbind registration + UDP via the adapter-settings API
	// BEFORE enabling the adapter — these settings are DB-backed and read when
	// the NFS adapter (re)starts, so they must be in place before EnableAdapter.
	apiClient := helpers.GetAPIClient(t, sp.APIURL())
	enable := true
	helpers.PatchNFSSetting(t, apiClient, &apiclient.PatchNFSSettingsRequest{
		PortmapperRegisterWithSystem: &enable,
		UDPEnabled:                   &enable,
	})

	metaStoreName := helpers.UniqueTestName("meta")
	localStoreName := helpers.UniqueTestName("local")
	_, err := cli.CreateMetadataStore(metaStoreName, "memory")
	require.NoError(t, err, "Should create metadata store")
	_, err = cli.CreateLocalBlockStore(localStoreName, "memory")
	require.NoError(t, err, "Should create local block store")

	_, err = cli.CreateShare("/export-nlm", metaStoreName, localStoreName,
		helpers.WithShareDefaultPermission("read-write"))
	require.NoError(t, err, "Should create share")

	nfsPort := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("nfs") })
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	// Every service a kernel NFSv3-with-locking client discovers via rpcbind must
	// resolve to DittoFS's NFS port. NLM (100021) is the one that unblocks
	// nolock-free locking; MOUNT and NFS confirm the full advertise set landed.
	// UDP is enabled above, so NLM/MOUNT must also resolve over UDP (NFS is
	// TCP-only and is never advertised on UDP).
	want := uint32(nfsPort)
	protoName := map[uint32]string{types.ProtoTCP: "tcp", types.ProtoUDP: "udp"}
	type svc struct {
		name       string
		prog, vers uint32
		prot       uint32
	}
	for _, s := range []svc{
		{"NLM v4", rpc.ProgramNLM, 4, types.ProtoTCP},
		{"NLM v1", rpc.ProgramNLM, 1, types.ProtoTCP},
		{"NFS v3", 100003, 3, types.ProtoTCP},
		{"MOUNT v3", 100005, 3, types.ProtoTCP},
		{"NLM v4", rpc.ProgramNLM, 4, types.ProtoUDP},
		{"MOUNT v3", 100005, 3, types.ProtoUDP},
	} {
		s := s
		require.Eventuallyf(t, func() bool {
			return helpers.PortmapGetPort(t, "127.0.0.1", 111, s.prog, s.vers, s.prot) == want
		}, 8*time.Second, 200*time.Millisecond,
			"%s/%s (prog=%d vers=%d) should be registered with the system rpcbind at NFS port %d",
			s.name, protoName[s.prot], s.prog, s.vers, nfsPort)
	}

	// NSM (100024) is deliberately NOT claimed on either transport: the host's
	// rpc.statd owns it and hijacking it would redirect every host SM_NOTIFY to
	// DittoFS. Confirm we did not register it at our port.
	for _, prot := range []uint32{types.ProtoTCP, types.ProtoUDP} {
		require.NotEqualf(t, want,
			helpers.PortmapGetPort(t, "127.0.0.1", 111, rpc.ProgramNSM, 1, prot),
			"NSM (100024/%s) must not be hijacked from the host rpc.statd", protoName[prot])
	}

	// After the adapter is disabled, the mappings must be unregistered (both
	// transports) so a client never resolves a stale NLM port.
	_, err = cli.DisableAdapter("nfs")
	require.NoError(t, err, "Should disable NFS adapter")
	for _, prot := range []uint32{types.ProtoTCP, types.ProtoUDP} {
		prot := prot
		require.Eventuallyf(t, func() bool {
			return helpers.PortmapGetPort(t, "127.0.0.1", 111, rpc.ProgramNLM, 4, prot) != want
		}, 8*time.Second, 200*time.Millisecond,
			"NLM (prog=100021 v4/%s) should be unregistered from the system rpcbind after the adapter stops",
			protoName[prot])
	}
}

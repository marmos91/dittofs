package portmap

// These tests live in package portmap (not sysreg) so they can reuse the
// startTestServer fixture, which stands up the embedded portmapper as a
// stand-in for a host's system rpcbind. sysreg is the SET/UNSET client under
// test; the embedded server validates its wire encoding end-to-end.

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/sysreg"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

func TestSysregPingRegisterUnregister(t *testing.T) {
	srv, registry := startTestServer(t)
	addr := srv.Addr()
	ctx := context.Background()

	// Ping: a portmapper is listening, NULL must be accepted.
	if err := sysreg.Ping(ctx, addr); err != nil {
		t.Fatalf("Ping(%s): %v", addr, err)
	}

	const nfsPort = 12049
	mappings := DittoFSServiceMappings(nfsPort, true /* udpEnabled */)
	if len(mappings) == 0 {
		t.Fatal("DittoFSServiceMappings returned no mappings")
	}

	// Register: every mapping must SET into the portmapper's registry.
	if err := sysreg.Register(ctx, addr, mappings); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The NLM v4 / TCP port must now resolve to the NFS port — proving the
	// kernel's GETPORT prog=100021 would find lockd.
	if got := registry.Getport(rpc.ProgramNLM, 4, types.ProtoTCP); got != nfsPort {
		t.Fatalf("after Register: NLM v4/TCP port = %d, want %d", got, nfsPort)
	}
	// And NSM, and the UDP-advertised NLM, must be present too.
	if got := registry.Getport(rpc.ProgramNSM, 1, types.ProtoTCP); got != nfsPort {
		t.Fatalf("after Register: NSM v1/TCP port = %d, want %d", got, nfsPort)
	}
	if got := registry.Getport(rpc.ProgramNLM, 4, types.ProtoUDP); got != nfsPort {
		t.Fatalf("after Register: NLM v4/UDP port = %d, want %d", got, nfsPort)
	}

	// Unregister: every mapping must UNSET back out.
	if err := sysreg.Unregister(ctx, addr, mappings); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if got := registry.Getport(rpc.ProgramNLM, 4, types.ProtoTCP); got != 0 {
		t.Fatalf("after Unregister: NLM v4/TCP port = %d, want 0 (gone)", got)
	}
}

func TestSysregPingNoPortmapper(t *testing.T) {
	// Nothing listening on this address: Ping must fail (best-effort caller then
	// skips registration rather than aborting NFS startup).
	if err := sysreg.Ping(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("Ping to a dead address: want error, got nil")
	}
}

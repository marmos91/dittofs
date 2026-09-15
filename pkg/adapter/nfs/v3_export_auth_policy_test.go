package nfs

import (
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newV3PolicyFixture builds an NFS adapter over a runtime holding one share,
// plus a connection to dispatch v3 procedures on. AddShare defaults
// AllowAuthSys to true, so the share starts out accepting AUTH_SYS.
func newV3PolicyFixture(t *testing.T) (*NFSConnection, *runtime.Runtime, metadata.FileHandle) {
	t.Helper()

	rt, bsID := newTestShareRuntime(t)
	if err := rt.RegisterMetadataStore("test-meta", metadatamemory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	shareCfg := &runtime.ShareConfig{
		Name:          "/export",
		MetadataStore: "test-meta",
		BlockStoreID:  bsID,
		Enabled:       true,
		RootAttr:      &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755},
	}
	if err := rt.AddShare(context.Background(), shareCfg); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle("/export")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	adapter.Registry = rt
	adapter.nfsHandler.Registry = rt

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	return NewNFSConnection(adapter, server, 1), rt, rootHandle
}

// getattrCall returns the RPC call message and XDR payload for a GETATTR on
// the given handle, carrying an AUTH_UNIX credential.
func getattrCall(handle metadata.FileHandle) (*rpc.RPCCallMessage, []byte) {
	// AUTH_UNIX body: stamp, machine name (empty), uid, gid, ngids.
	authBody := make([]byte, 24)
	binary.BigEndian.PutUint32(authBody[8:12], 1000)  // uid
	binary.BigEndian.PutUint32(authBody[12:16], 1000) // gid

	call := &rpc.RPCCallMessage{
		XID:       0xC0FFEE,
		Program:   rpc.ProgramNFS,
		Version:   rpc.NFSVersion3,
		Procedure: nfs_types.NFSProcGetAttr,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthUnix, Body: authBody},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}

	// GETATTR3args is a single nfs_fh3: an XDR opaque (length + data, padded).
	padded := (len(handle) + 3) / 4 * 4
	data := make([]byte, 4+padded)
	binary.BigEndian.PutUint32(data[0:4], uint32(len(handle)))
	copy(data[4:], handle)

	return call, data
}

// TestV3ExportAuthPolicy_TighteningRefusesAnExistingHandle drives the sequence
// the per-operation gate exists for: a client acquires a handle while the share
// accepts AUTH_SYS, an administrator then sets allow_auth_sys=false, and the
// client keeps using the handle it already holds.
//
// MOUNT gates handle acquisition only, so without a per-operation gate the
// second GETATTR still succeeds and the policy change reaches new mounts alone.
// File handles are designed to stay stable across restarts, so that is not a
// short window.
func TestV3ExportAuthPolicy_TighteningRefusesAnExistingHandle(t *testing.T) {
	conn, rt, rootHandle := newV3PolicyFixture(t)
	call, data := getattrCall(rootHandle)

	// Before: the share accepts AUTH_SYS, so the handle works.
	reply, err := conn.handleNFSProcedure(context.Background(), call, data, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("GETATTR before the policy change: %v", err)
	}
	if status := binary.BigEndian.Uint32(reply[0:4]); status != nfs_types.NFS3OK {
		t.Fatalf("GETATTR status before the policy change = %d, want NFS3OK", status)
	}

	// An administrator forbids AUTH_SYS on the share. The client holds on to
	// the handle it already has.
	if err := rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	reply, err = conn.handleNFSProcedure(context.Background(), call, data, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("GETATTR after the policy change: %v", err)
	}
	if status := binary.BigEndian.Uint32(reply[0:4]); status != nfs_types.NFS3ErrAccess {
		t.Fatalf("GETATTR status after allow_auth_sys=false = %d, want NFS3ERR_ACCES (%d): "+
			"an existing handle still has full access over a flavor the export no longer accepts",
			status, nfs_types.NFS3ErrAccess)
	}
}

// TestV3ExportAuthPolicy_RequireKerberosRefusesAuthSys pins the other half of
// the export flavor policy on the v3 operation path: a share that requires
// Kerberos must refuse an AUTH_SYS operation even on a handle acquired earlier.
func TestV3ExportAuthPolicy_RequireKerberosRefusesAuthSys(t *testing.T) {
	conn, rt, rootHandle := newV3PolicyFixture(t)
	call, data := getattrCall(rootHandle)

	if _, err := conn.handleNFSProcedure(context.Background(), call, data, "10.0.0.1:12345"); err != nil {
		t.Fatalf("GETATTR before the policy change: %v", err)
	}

	if err := rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	reply, err := conn.handleNFSProcedure(context.Background(), call, data, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("GETATTR after the policy change: %v", err)
	}
	if status := binary.BigEndian.Uint32(reply[0:4]); status != nfs_types.NFS3ErrAccess {
		t.Fatalf("GETATTR status after require_kerberos=true = %d, want NFS3ERR_ACCES (%d)",
			status, nfs_types.NFS3ErrAccess)
	}
}

// TestV3StatusOnlyReply_MatchesTheProceduresFailureArm pins the length of a
// refusal encoded before any handler runs against what that procedure's own
// codec emits for a non-OK status. A reply shorter than the union's failure arm
// is a decode error at the client, and RENAME and LINK carry the longest arms.
func TestV3StatusOnlyReply_MatchesTheProceduresFailureArm(t *testing.T) {
	cases := map[uint32]int{
		nfs_types.NFSProcGetAttr: 4,  // status only
		nfs_types.NFSProcFsInfo:  4,  // status only
		nfs_types.NFSProcAccess:  8,  // + post_op_attr
		nfs_types.NFSProcRead:    8,  // + post_op_attr
		nfs_types.NFSProcWrite:   12, // + wcc_data
		nfs_types.NFSProcCommit:  12, // + wcc_data
		nfs_types.NFSProcLink:    16, // + post_op_attr + wcc_data
		nfs_types.NFSProcRename:  20, // + two wcc_data
	}
	for proc, want := range cases {
		reply := v3StatusOnlyReply(proc, nfs_types.NFS3ErrAccess)
		if len(reply) != want {
			t.Errorf("procedure %d reply = %d bytes, want %d", proc, len(reply), want)
		}
		if got := binary.BigEndian.Uint32(reply[0:4]); got != nfs_types.NFS3ErrAccess {
			t.Errorf("procedure %d status = %d, want NFS3ERR_ACCES", proc, got)
		}
		for i, b := range reply[4:] {
			if b != 0 {
				t.Errorf("procedure %d byte %d = %d, want 0 (attribute absent)", proc, 4+i, b)
			}
		}
	}
}

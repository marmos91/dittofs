// The share-policy gate, as it actually stands across the protocols.
//
// Four things a share's policy can refuse for, and they are not symmetric:
// only the enabled flag is shared by every protocol. The auth-flavor policy,
// the minimum Kerberos level and the netgroup allowlist are NFS export policy
// -- they arrive on the NFS adapter config and are read only under
// internal/adapter/nfs, so an SMB share has nowhere to set them. Writing the
// acceptance test as "the same four rejections per protocol" would therefore
// have asserted three gates SMB cannot have.
//
//	rejection            SMB              NFS MOUNT   NFSv3 per-op   NFSv4
//	enabled flag         TREE_CONNECT     MOUNT       here           PUTFH + junction LOOKUP
//	                     + revalidation
//	auth flavor          --               MOUNT       dispatch       auth-context build
//	min Kerberos level   --               MOUNT       dispatch       auth-context build
//	netgroup allowlist   --               MOUNT       NOT CHECKED    shareEntryStatus
//
// Two of those cells are deliberate and stated where they are decided rather
// than here: NFSv3's per-operation gate passes no netgroup lookup to
// CheckExportAccess, and NFSv4's handle-entry gate covers the enabled flag and
// the netgroup but not the flavor policy.
//
// The enabled-flag row is the one every protocol owes, so it is the row worth
// a test per protocol. The other five live beside their own gate:
// TestMount_DisabledShare_ReturnsAccess, TestPUTFH_DisabledShare_ReturnsStale,
// TestJunctionLookup_DisabledShare_ReturnsStale,
// TestTreeConnect_DisabledShare_ReturnsNameDeleted and
// TestRevalidateAuthorization_DisabledShareRemovesTree. This file is the
// NFSv3 cell, which had no test and no gate.
package nfs

import (
	"context"
	"encoding/binary"
	"testing"

	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

// TestV3DispatchGate_DisabledShareRefusesAnExistingHandle drives the same
// sequence as the export-flavor tests one file over, against the other thing a
// share's policy can say: the share is switched off while a client holds a
// handle on it.
//
// Disabling is specified as admitting nobody, and the adapters that gate at
// establishment (SMB TREE_CONNECT, NFS MOUNT) are made to honour it by dropping
// what they established. NFSv3 establishes nothing per operation — the handle
// is the credential, and it stays valid across restarts by design — so the
// enabled flag has to be read on the operation itself or it is not read at all
// for an already-mounted client.
func TestV3DispatchGate_DisabledShareRefusesAnExistingHandle(t *testing.T) {
	conn, rt, rootHandle := newV3PolicyFixture(t)
	call, data := getattrCall(rootHandle)

	reply, err := conn.handleNFSProcedure(context.Background(), call, data, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("GETATTR while the share is enabled: %v", err)
	}
	if status := binary.BigEndian.Uint32(reply[0:4]); status != nfs_types.NFS3OK {
		t.Fatalf("GETATTR status while enabled = %d, want NFS3OK", status)
	}

	if err := rt.SetEnabledForTesting("/export", false); err != nil {
		t.Fatalf("SetEnabledForTesting: %v", err)
	}

	reply, err = conn.handleNFSProcedure(context.Background(), call, data, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("GETATTR after the share was disabled: %v", err)
	}
	if status := binary.BigEndian.Uint32(reply[0:4]); status != nfs_types.NFS3ErrStale {
		t.Fatalf("GETATTR status on a disabled share = %d, want NFS3ERR_STALE (%d): "+
			"a client holding a handle keeps full access to a share an administrator switched off",
			status, nfs_types.NFS3ErrStale)
	}
}

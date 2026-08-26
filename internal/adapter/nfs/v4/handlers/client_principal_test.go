package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// doOpen drives one OPEN through the handler and returns its raw result.
func doOpen(fx *ioTestFixture, ctx *types.CompoundContext,
	seqid uint32, clientID uint64, owner []byte, filename string, openType, shareAccess uint32,
) *types.CompoundResult {
	args := encodeOpenArgs(seqid, shareAccess, types.OPEN4_SHARE_DENY_NONE,
		clientID, owner, openType, types.UNCHECKED4, types.CLAIM_NULL, filename)
	return fx.handler.handleOpen(ctx, bytes.NewReader(args))
}

// openStateid drives one OPEN through the handler and returns the stateid it
// produced, failing the test if the OPEN itself was refused.
func openStateid(t *testing.T, fx *ioTestFixture, ctx *types.CompoundContext,
	seqid uint32, clientID uint64, owner []byte, filename string, openType, shareAccess uint32,
) types.Stateid4 {
	t.Helper()

	result := doOpen(fx, ctx, seqid, clientID, owner, filename, openType, shareAccess)
	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN status = %d, want NFS4_OK", result.Status)
	}
	reader := bytes.NewReader(result.Data)
	if status, _ := xdr.DecodeUint32(reader); status != types.NFS4_OK {
		t.Fatalf("encoded OPEN status = %d, want NFS4_OK", status)
	}
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("decode stateid: %v", err)
	}
	return *stateid
}

// renewStatus drives one RENEW for clientID as the caller ctx authenticates.
func renewStatus(fx *ioTestFixture, ctx *types.CompoundContext, clientID uint64) uint32 {
	var args bytes.Buffer
	_ = xdr.WriteUint64(&args, clientID)
	return fx.handler.handleRenew(ctx, bytes.NewReader(args.Bytes())).Status
}

// TestOpen_ClientIDSpansPrincipals records where the boundary actually sits for
// an OPEN naming another principal's client ID and open-owner.
//
// A client ID covers every principal on the client, so the server does not, and
// must not, refuse an OPEN because the caller is not the principal that
// established the ID: RFC 7530 Section 3.1 has one connection multiplexing all
// of a machine's users, Section 9.1.1 gives each of them their own owner under
// the one ID, and neither Linux nfsd (struct nfs4_stateowner carries no
// credential) nor nfs-ganesha compares one on OPEN. What stops a caller from
// reaching a file it has no business in is the per-OPEN permission check, not
// the state model -- so that check is what this pins.
func TestOpen_ClientIDSpansPrincipals(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	sm := fx.handler.StateManager

	clientID := testClientID(t, sm, "shared-client", "uid:1000")
	owner := []byte("open-owner-1000")

	// uid 1000 establishes the client ID and opens a file it owns.
	victim := newRealFSContext(1000, 1000)
	setCurrentFH(victim, fx.rootHandle)
	victimStateid := openStateid(t, fx, victim, 1, clientID, owner, "victim.txt",
		types.OPEN4_CREATE, types.OPEN4_SHARE_ACCESS_BOTH)
	if _, err := sm.ConfirmOpen(&victimStateid, 2); err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// A second user of the same client, under its own credential.
	other := newRealFSContext(1001, 1001)
	setCurrentFH(other, fx.rootHandle)

	// The permission check is load-bearing: uid 1001 cannot write the file, and
	// naming uid 1000's client ID and owner does not get it write access.
	// checkOpenAccess refuses before the state layer sees the request, so seqid
	// 3 is still unused when the read OPEN below presents it.
	if status := doOpen(fx, other, 3, clientID, owner, "victim.txt",
		types.OPEN4_NOCREATE, types.OPEN4_SHARE_ACCESS_BOTH).Status; status != types.NFS4ERR_ACCESS {
		t.Errorf("OPEN for write by a uid without write permission: status = %d, want NFS4ERR_ACCESS", status)
	}

	// Read access it does have, and there it joins the existing open state --
	// the documented shape of a shared client ID, not a defect.
	shared := openStateid(t, fx, other, 3, clientID, owner, "victim.txt",
		types.OPEN4_NOCREATE, types.OPEN4_SHARE_ACCESS_READ)
	if shared.Other != victimStateid.Other {
		t.Errorf("second principal got a distinct open state (%x vs %x)", shared.Other, victimStateid.Other)
	}
}

// TestRenew_PrincipalBinding covers RFC 7530 Section 16.28.5, which permits a
// RENEW only from the principal that established the client ID via
// SETCLIENTID_CONFIRM or from one that currently holds an OPEN under it, and
// requires NFS4ERR_ACCESS for anyone else.
func TestRenew_PrincipalBinding(t *testing.T) {
	t.Run("establishing principal is allowed", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		clientID := testClientID(t, fx.handler.StateManager, "c", "uid:1000")

		if status := renewStatus(fx, newRealFSContext(1000, 1000), clientID); status != types.NFS4_OK {
			t.Errorf("RENEW by the establishing principal: status = %d, want NFS4_OK", status)
		}
	})

	t.Run("stranger is refused", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		clientID := testClientID(t, fx.handler.StateManager, "c", "uid:1000")

		if status := renewStatus(fx, newRealFSContext(1001, 1001), clientID); status != types.NFS4ERR_ACCESS {
			t.Errorf("RENEW by a stranger: status = %d, want NFS4ERR_ACCESS", status)
		}
	})

	t.Run("principal holding an open is allowed", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		sm := fx.handler.StateManager
		// The client ID is established by uid 2000, which never opens anything.
		clientID := testClientID(t, sm, "c", "uid:2000")

		// uid 1000 is a second user of the same client, with its own owner.
		// This is the multi-user mount the second algorithm exists for.
		user := newRealFSContext(1000, 1000)
		setCurrentFH(user, fx.rootHandle)
		stateid := openStateid(t, fx, user, 1, clientID, []byte("open-owner-1000"), "u1000.txt",
			types.OPEN4_CREATE, types.OPEN4_SHARE_ACCESS_BOTH)
		if _, err := sm.ConfirmOpen(&stateid, 2); err != nil {
			t.Fatalf("ConfirmOpen: %v", err)
		}

		if status := renewStatus(fx, user, clientID); status != types.NFS4_OK {
			t.Errorf("RENEW by a principal holding an open: status = %d, want NFS4_OK", status)
		}
	})

	t.Run("machine credential that established the ID is allowed", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		// What a Linux client does: SETCLIENTID_CONFIRM and RENEW both under
		// the machine credential, which for AUTH_SYS is uid 0.
		clientID := testClientID(t, fx.handler.StateManager, "c", "uid:0")

		if status := renewStatus(fx, newRealFSContext(0, 0), clientID); status != types.NFS4_OK {
			t.Errorf("RENEW under the establishing machine credential: status = %d, want NFS4_OK", status)
		}
	})

	t.Run("root is refused against another principal's record", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		clientID := testClientID(t, fx.handler.StateManager, "c", "uid:1000")

		// Principal() cannot tell a verified machine credential from an
		// AUTH_SYS caller that simply wrote uid 0 into its credential, and
		// RENEW carries no filehandle for an export's sec= policy to judge, so
		// uid 0 gets no standing it has not earned on this record.
		if status := renewStatus(fx, newRealFSContext(0, 0), clientID); status != types.NFS4ERR_ACCESS {
			t.Errorf("RENEW as uid 0 against a uid:1000 record: status = %d, want NFS4ERR_ACCESS", status)
		}
	})

	t.Run("record with no principal accepts any caller", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		clientID := testClientID(t, fx.handler.StateManager, "no-principal")

		if status := renewStatus(fx, newRealFSContext(1001, 1001), clientID); status != types.NFS4_OK {
			t.Errorf("RENEW against a record with no stored principal: status = %d, want NFS4_OK", status)
		}
	})

	t.Run("refused renew does not extend the lease", func(t *testing.T) {
		fx := newIOTestFixture(t, "/export")
		sm := fx.handler.StateManager
		clientID := testClientID(t, sm, "c", "uid:1000")

		before := sm.GetClient(clientID).LastRenewal
		if status := renewStatus(fx, newRealFSContext(1001, 1001), clientID); status != types.NFS4ERR_ACCESS {
			t.Fatalf("RENEW by a stranger: status = %d, want NFS4ERR_ACCESS", status)
		}
		if after := sm.GetClient(clientID).LastRenewal; !after.Equal(before) {
			t.Errorf("refused RENEW moved LastRenewal %v -> %v", before, after)
		}
	})
}

// TestSetClientID_IdentitylessCallerCannotTakeOver covers the half of RFC 7530
// Section 9.1.1 / Section 19 that a "both principals non-empty" comparison
// leaves open: neither SETCLIENTID nor SETCLIENTID_CONFIRM carries a filehandle,
// so an export's sec= policy never sees them, and a caller that simply omits a
// credential must not thereby clear the bar.
func TestSetClientID_IdentitylessCallerCannotTakeOver(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	sm := fx.handler.StateManager

	const name = "victim-client"
	testClientID(t, sm, name, "uid:1000")

	cb := state.CallbackInfo{Program: 0x40000000, NetID: "tcp", Addr: "127.0.0.1.8.1"}

	// A reboot claim (different verifier) under no credential at all.
	if _, err := sm.SetClientID(name, [8]byte{9, 9, 9, 9, 9, 9, 9, 9}, cb, "10.0.0.9:9999", ""); err != state.ErrClientIDInUse {
		t.Errorf("SETCLIENTID reboot claim with no principal: got %v, want ErrClientIDInUse", err)
	}

	// And the same-verifier re-SETCLIENTID path.
	if _, err := sm.SetClientID(name, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, cb, "10.0.0.9:9999", ""); err != state.ErrClientIDInUse {
		t.Errorf("re-SETCLIENTID with no principal: got %v, want ErrClientIDInUse", err)
	}
}

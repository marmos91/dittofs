package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// principalClientID registers a confirmed v4.0 client whose record carries
// principal, the identity SETCLIENTID_CONFIRM was issued under.
func principalClientID(t *testing.T, sm *state.StateManager, name, principal, addr string) uint64 {
	t.Helper()

	result, err := sm.SetClientID(name, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, state.CallbackInfo{
		Program: 0x40000000,
		NetID:   "tcp",
		Addr:    "127.0.0.1.8.1",
	}, addr, principal)
	if err != nil {
		t.Fatalf("SetClientID(%s): %v", name, err)
	}
	if err := sm.ConfirmClientID(result.ClientID, result.ConfirmVerifier); err != nil {
		t.Fatalf("ConfirmClientID(%s): %v", name, err)
	}
	return result.ClientID
}

// openStateid drives one OPEN through the handler and returns the stateid it
// produced, failing the test if the OPEN itself was refused.
func openStateid(t *testing.T, fx *ioTestFixture, ctx *types.CompoundContext,
	seqid uint32, clientID uint64, owner []byte, filename string, openType, shareAccess uint32,
) types.Stateid4 {
	t.Helper()

	args := encodeOpenArgs(seqid, shareAccess, types.OPEN4_SHARE_DENY_NONE,
		clientID, owner, openType, types.UNCHECKED4, types.CLAIM_NULL, filename)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))
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

// TestOpen_ForeignPrincipalReusesClientOpenOwner reproduces the gap: a caller
// authenticated as one uid presents another client's clientid and open-owner,
// is handed that client's open stateid, and CLOSEs it.
func TestOpen_ForeignPrincipalReusesClientOpenOwner(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	sm := fx.handler.StateManager

	clientID := principalClientID(t, sm, "victim-client", "uid:1000", "10.0.0.1:1000")
	owner := []byte("victim-open-owner")

	// Victim (uid 1000, the principal that established the client ID) opens.
	victim := newRealFSContext(1000, 1000)
	setCurrentFH(victim, fx.rootHandle)
	victimStateid := openStateid(t, fx, victim, 1, clientID, owner, "victim.txt", types.OPEN4_CREATE, types.OPEN4_SHARE_ACCESS_BOTH)

	if _, err := sm.ConfirmOpen(&victimStateid, 2); err != nil {
		t.Fatalf("ConfirmOpen: %v", err)
	}

	// Attacker: different uid, different address, same clientid and open-owner.
	attacker := newRealFSContext(1001, 1001)
	attacker.ClientAddr = "10.0.0.9:9999"
	setCurrentFH(attacker, fx.rootHandle)
	stolen := openStateid(t, fx, attacker, 3, clientID, owner, "victim.txt", types.OPEN4_NOCREATE, types.OPEN4_SHARE_ACCESS_READ)

	if stolen.Other != victimStateid.Other {
		t.Fatalf("attacker got a distinct open state (%x vs %x); reproduction is not exercising the shared owner",
			stolen.Other, victimStateid.Other)
	}

	// And it can release the victim's state.
	closeArgs := encodeCloseArgs(4, &stolen)
	closeResult := fx.handler.handleClose(attacker, bytes.NewReader(closeArgs))
	if closeResult.Status != types.NFS4_OK {
		t.Fatalf("CLOSE status = %d, want NFS4_OK", closeResult.Status)
	}
	if sm.GetOpenState(victimStateid.Other) != nil {
		t.Fatal("victim open state survived the foreign-principal CLOSE")
	}
}

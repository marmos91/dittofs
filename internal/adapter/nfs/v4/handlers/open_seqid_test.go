package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// TestOpen_HandlerRefusalConsumesSeqid covers RFC 7530 Section 9.1.7: an OPEN
// that reaches seqid checking consumes the open-owner's seqid whether it then
// succeeds or fails, and the client advances its own sequence either way.
//
// An OPEN the handler refuses on its own never reached the state manager, which
// is the only place that recorded a seqid, so the server stayed one behind the
// client from the first refusal onwards and answered every later OPEN for that
// owner NFS4ERR_BAD_SEQID -- an error a client can only escape by tearing the
// open-owner down.
//
// GUARDED4 over a name that already exists is the shortest way to a refusal the
// handler issues by itself: it returns NFS4ERR_EXIST from the create path,
// above the state manager entirely.
func TestOpen_HandlerRefusalConsumesSeqid(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	clientID := testClientID(t, fx.handler.StateManager, "seqid-client")
	owner := []byte("seqid-owner")

	ctx := newRealFSContext(1000, 1000)
	setCurrentFH(ctx, fx.rootHandle)

	open := func(seqid, createMode uint32) *types.CompoundResult {
		// OPEN leaves the created file as the current filehandle; every call
		// here starts from the parent directory again.
		setCurrentFH(ctx, fx.rootHandle)
		args := encodeOpenArgs(seqid, types.OPEN4_SHARE_ACCESS_BOTH,
			types.OPEN4_SHARE_DENY_NONE, clientID, owner,
			types.OPEN4_CREATE, createMode, types.CLAIM_NULL, "guarded.txt")
		return fx.handler.handleOpen(ctx, bytes.NewReader(args))
	}

	if r := open(1, types.GUARDED4); r.Status != types.NFS4_OK {
		t.Fatalf("first GUARDED4 create: status = %d, want NFS4_OK", r.Status)
	}

	// Refused by the handler, above the state manager.
	if r := open(2, types.GUARDED4); r.Status != types.NFS4ERR_EXIST {
		t.Fatalf("GUARDED4 recreate: status = %d, want NFS4ERR_EXIST (%d)",
			r.Status, types.NFS4ERR_EXIST)
	}

	// The refusal spent seqid 2, so the client's next OPEN presents 3. Before
	// the fix the server still expected 2 here and answered NFS4ERR_BAD_SEQID.
	if r := open(3, types.UNCHECKED4); r.Status != types.NFS4_OK {
		t.Fatalf("OPEN after a refused OPEN: status = %d, want NFS4_OK", r.Status)
	}
}

// TestOpen_RefusalIsReplayedNotReExecuted pins the other half of
// Section 9.1.7: a retransmission at the seqid a refusal consumed must return
// that refusal's reply, not run the OPEN a second time.
//
// The distinction only shows when the condition behind the refusal has changed
// in between, so the test removes the colliding name before retransmitting. Two
// GUARDED4 creates in a row would answer NFS4ERR_EXIST whether the server
// replayed the cached reply or simply re-ran the create, and would pass against
// a server that does neither correctly.
func TestOpen_RefusalIsReplayedNotReExecuted(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	clientID := testClientID(t, fx.handler.StateManager, "replay-client")
	owner := []byte("replay-owner")

	ctx := newRealFSContext(0, 0)

	open := func(seqid, createMode uint32) *types.CompoundResult {
		setCurrentFH(ctx, fx.rootHandle)
		args := encodeOpenArgs(seqid, types.OPEN4_SHARE_ACCESS_BOTH,
			types.OPEN4_SHARE_DENY_NONE, clientID, owner,
			types.OPEN4_CREATE, createMode, types.CLAIM_NULL, "replayed.txt")
		return fx.handler.handleOpen(ctx, bytes.NewReader(args))
	}

	if r := open(1, types.GUARDED4); r.Status != types.NFS4_OK {
		t.Fatalf("first GUARDED4 create: status = %d, want NFS4_OK", r.Status)
	}
	if r := open(2, types.GUARDED4); r.Status != types.NFS4ERR_EXIST {
		t.Fatalf("GUARDED4 recreate: status = %d, want NFS4ERR_EXIST", r.Status)
	}

	// Take the collision away. A re-executed OPEN would now create the file and
	// answer NFS4_OK; a replayed one still answers the refusal it cached.
	setCurrentFH(ctx, fx.rootHandle)
	rm := fx.handler.handleRemove(ctx, bytes.NewReader(encodeRemoveArgs("replayed.txt")))
	if rm.Status != types.NFS4_OK {
		t.Fatalf("REMOVE of the colliding name: status = %d, want NFS4_OK", rm.Status)
	}

	if r := open(2, types.GUARDED4); r.Status != types.NFS4ERR_EXIST {
		t.Fatalf("retransmitted refusal after the collision was removed: status = %d, want the replayed NFS4ERR_EXIST (%d)",
			r.Status, types.NFS4ERR_EXIST)
	}

	// The status alone does not separate a replay from a re-execution: an OPEN
	// that runs again creates the file and is then answered NFS4ERR_EXIST by the
	// state layer's own replay check, having already done the work. The side
	// effect is what tells them apart, so assert the file was not recreated.
	if _, err := fx.metaSvc.Lookup(newTestAuthCtx(0, 0), fx.rootHandle, "replayed.txt"); err == nil {
		t.Fatal("the retransmitted OPEN recreated the file: it was re-executed, not replayed")
	}

	// The replay must not have consumed a second seqid.
	if r := open(3, types.UNCHECKED4); r.Status != types.NFS4_OK {
		t.Fatalf("OPEN after a replayed refusal: status = %d, want NFS4_OK", r.Status)
	}
}

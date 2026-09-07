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

// TestOpen_RefusalReplaysRatherThanConsumingTwice pins the other half of
// Section 9.1.7: a retransmission at the seqid the refusal consumed replays that
// refusal's status and does not advance the sequence a second time.
func TestOpen_RefusalReplaysRatherThanConsumingTwice(t *testing.T) {
	fx := newIOTestFixture(t, "/export")
	clientID := testClientID(t, fx.handler.StateManager, "replay-client")
	owner := []byte("replay-owner")

	ctx := newRealFSContext(1000, 1000)
	setCurrentFH(ctx, fx.rootHandle)

	open := func(seqid, createMode uint32) *types.CompoundResult {
		// OPEN leaves the created file as the current filehandle; every call
		// here starts from the parent directory again.
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
	if r := open(2, types.GUARDED4); r.Status != types.NFS4ERR_EXIST {
		t.Fatalf("retransmitted refusal: status = %d, want the replayed NFS4ERR_EXIST", r.Status)
	}

	// The replay must not have consumed a second seqid.
	if r := open(3, types.UNCHECKED4); r.Status != types.NFS4_OK {
		t.Fatalf("OPEN after a replayed refusal: status = %d, want NFS4_OK", r.Status)
	}
}

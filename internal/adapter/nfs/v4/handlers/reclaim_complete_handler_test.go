package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// sendReclaimComplete runs SEQUENCE + RECLAIM_COMPLETE on the given session
// slot 0 at the given sequence ID and returns the COMPOUND's overall status.
// oneFS selects the rca_one_fs flavour: false is the global reclaim, true the
// file system-specific one.
func sendReclaimComplete(
	t *testing.T,
	h *Handler,
	ctx *types.CompoundContext,
	sessionID types.SessionId4,
	seqID uint32,
	oneFS bool,
) uint32 {
	t.Helper()

	var rcBuf bytes.Buffer
	rcArgs := types.ReclaimCompleteArgs{OneFS: oneFS}
	if err := rcArgs.Encode(&rcBuf); err != nil {
		t.Fatalf("encode ReclaimCompleteArgs: %v", err)
	}
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, seqID, 0, false)},
		{opCode: types.OP_RECLAIM_COMPLETE, data: rcBuf.Bytes()},
	}

	resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte("rc"), 1, ops))
	if err != nil {
		t.Fatalf("RECLAIM_COMPLETE ProcessCompound error: %v", err)
	}
	status, err := xdr.DecodeUint32(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode overall status: %v", err)
	}
	return status
}

func TestHandleReclaimComplete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		// Set up client with session, then send RECLAIM_COMPLETE via SEQUENCE-gated COMPOUND
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// Build SEQUENCE + RECLAIM_COMPLETE compound
		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		var rcBuf bytes.Buffer
		rcArgs := types.ReclaimCompleteArgs{OneFS: false}
		_ = rcArgs.Encode(&rcBuf)

		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_RECLAIM_COMPLETE, data: rcBuf.Bytes()},
		}
		data := buildCompoundArgsWithOps([]byte("rc-ok"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		// Decode: skip overall status + tag + numResults + SEQUENCE result, then check RECLAIM_COMPLETE
		reader := bytes.NewReader(resp)
		status, _ := xdr.DecodeUint32(reader) // overall status
		if status != types.NFS4_OK {
			t.Fatalf("overall status = %d, want NFS4_OK", status)
		}
		_, _ = xdr.DecodeOpaque(reader) // tag
		numResults, _ := xdr.DecodeUint32(reader)
		if numResults != 2 {
			t.Fatalf("numResults = %d, want 2", numResults)
		}

		// Skip SEQUENCE result
		_, _ = xdr.DecodeUint32(reader) // SEQUENCE opcode
		var seqRes types.SequenceRes
		if err := seqRes.Decode(reader); err != nil {
			t.Fatalf("decode SequenceRes: %v", err)
		}
		if seqRes.Status != types.NFS4_OK {
			t.Fatalf("SEQUENCE status = %d, want NFS4_OK", seqRes.Status)
		}

		// Decode RECLAIM_COMPLETE result
		rcOpCode, _ := xdr.DecodeUint32(reader)
		if rcOpCode != types.OP_RECLAIM_COMPLETE {
			t.Errorf("result opcode = %d, want OP_RECLAIM_COMPLETE", rcOpCode)
		}
		var rcRes types.ReclaimCompleteRes
		if err := rcRes.Decode(reader); err != nil {
			t.Fatalf("decode ReclaimCompleteRes: %v", err)
		}
		if rcRes.Status != types.NFS4_OK {
			t.Errorf("RECLAIM_COMPLETE status = %d, want NFS4_OK", rcRes.Status)
		}
	})

	t.Run("complete_already", func(t *testing.T) {
		// Send RECLAIM_COMPLETE twice -- second call should fail.
		// First, start a grace period so ReclaimComplete tracks per-client state.
		h, sessionID := createTestSession(t)

		// Get the client ID for grace period registration
		session := h.StateManager.GetSession(sessionID)
		if session == nil {
			t.Fatal("session not found")
		}
		clientID := session.ClientID

		// Start grace period with this client
		h.StateManager.StartGracePeriod([]uint64{clientID})

		ctx := newTestCompoundContext()

		if got := sendReclaimComplete(t, h, ctx, sessionID, 1, false); got != types.NFS4_OK {
			t.Fatalf("first RECLAIM_COMPLETE overall status = %d, want NFS4_OK", got)
		}
		if got := sendReclaimComplete(t, h, ctx, sessionID, 2, false); got != types.NFS4ERR_COMPLETE_ALREADY {
			t.Errorf("second RECLAIM_COMPLETE overall status = %d, want NFS4ERR_COMPLETE_ALREADY (%d)",
				got, types.NFS4ERR_COMPLETE_ALREADY)
		}
	})

	t.Run("complete_already_outside_grace", func(t *testing.T) {
		// A client that reclaimed has finished reclaiming whether or not the
		// server ever opened a grace window, so the second RECLAIM_COMPLETE is
		// still a duplicate. No grace period is started here.
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		if got := sendReclaimComplete(t, h, ctx, sessionID, 1, false); got != types.NFS4_OK {
			t.Fatalf("first RECLAIM_COMPLETE overall status = %d, want NFS4_OK", got)
		}
		if got := sendReclaimComplete(t, h, ctx, sessionID, 2, false); got != types.NFS4ERR_COMPLETE_ALREADY {
			t.Errorf("second RECLAIM_COMPLETE overall status = %d, want NFS4ERR_COMPLETE_ALREADY (%d)",
				got, types.NFS4ERR_COMPLETE_ALREADY)
		}
	})

	t.Run("per_fs_then_global", func(t *testing.T) {
		// The two rca_one_fs flavours are separate operations with separate
		// scopes: the global one covers the server instance, the file
		// system-specific one covers a migration of one file system. A client
		// may issue both, in either order, so a per-FS reclaim must not retire
		// the client's global reclaim.
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		if got := sendReclaimComplete(t, h, ctx, sessionID, 1, true); got != types.NFS4_OK {
			t.Fatalf("per-FS RECLAIM_COMPLETE overall status = %d, want NFS4_OK", got)
		}
		if got := sendReclaimComplete(t, h, ctx, sessionID, 2, false); got != types.NFS4_OK {
			t.Errorf("global RECLAIM_COMPLETE after a per-FS one = %d, want NFS4_OK", got)
		}
	})

	t.Run("repeated_per_fs_is_not_a_duplicate", func(t *testing.T) {
		// No file system here is ever migrating, so a file system-specific
		// reclaim is accepted and otherwise ignored however often it arrives.
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		for seq := uint32(1); seq <= 3; seq++ {
			if got := sendReclaimComplete(t, h, ctx, sessionID, seq, true); got != types.NFS4_OK {
				t.Fatalf("per-FS RECLAIM_COMPLETE #%d = %d, want NFS4_OK", seq, got)
			}
		}
	})

	t.Run("bad_xdr", func(t *testing.T) {
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// SEQUENCE + truncated RECLAIM_COMPLETE args
		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_RECLAIM_COMPLETE}, // missing args (bool needs 4 bytes)
		}
		data := buildCompoundArgsWithOps([]byte("rc-badxdr"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		// Decode: look at overall status (should be NFS4ERR_BADXDR due to RECLAIM_COMPLETE)
		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader)
		if overallStatus != types.NFS4ERR_BADXDR {
			t.Errorf("overall status = %d, want NFS4ERR_BADXDR (%d)",
				overallStatus, types.NFS4ERR_BADXDR)
		}
	})
}

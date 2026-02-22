package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

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

		// First RECLAIM_COMPLETE (slot 0, seqid 1)
		seqArgs1 := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		var rcBuf1 bytes.Buffer
		rcArgs1 := types.ReclaimCompleteArgs{OneFS: false}
		_ = rcArgs1.Encode(&rcBuf1)

		ops1 := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs1},
			{opCode: types.OP_RECLAIM_COMPLETE, data: rcBuf1.Bytes()},
		}
		data1 := buildCompoundArgsWithOps([]byte("rc1"), 1, ops1)
		resp1, err := h.ProcessCompound(ctx, data1)
		if err != nil {
			t.Fatalf("first RECLAIM_COMPLETE error: %v", err)
		}
		decoded1, _ := decodeCompoundResponse(resp1)
		if decoded1.Status != types.NFS4_OK {
			t.Fatalf("first RECLAIM_COMPLETE overall status = %d, want NFS4_OK", decoded1.Status)
		}

		// Second RECLAIM_COMPLETE (slot 0, seqid 2)
		seqArgs2 := encodeSequenceArgs(sessionID, 0, 2, 0, false)
		var rcBuf2 bytes.Buffer
		rcArgs2 := types.ReclaimCompleteArgs{OneFS: false}
		_ = rcArgs2.Encode(&rcBuf2)

		ops2 := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs2},
			{opCode: types.OP_RECLAIM_COMPLETE, data: rcBuf2.Bytes()},
		}
		data2 := buildCompoundArgsWithOps([]byte("rc2"), 1, ops2)
		resp2, err := h.ProcessCompound(ctx, data2)
		if err != nil {
			t.Fatalf("second RECLAIM_COMPLETE error: %v", err)
		}

		// Decode second response to check RECLAIM_COMPLETE status
		reader := bytes.NewReader(resp2)
		overallStatus, _ := xdr.DecodeUint32(reader)
		_, _ = xdr.DecodeOpaque(reader)          // tag
		numResults, _ := xdr.DecodeUint32(reader) // numResults

		if numResults < 2 {
			// If only SEQUENCE result, the compound failed at RECLAIM_COMPLETE
			t.Logf("overall status = %d, numResults = %d", overallStatus, numResults)
		}

		// The overall status should be NFS4ERR_COMPLETE_ALREADY
		if overallStatus != types.NFS4ERR_COMPLETE_ALREADY {
			t.Errorf("second RECLAIM_COMPLETE overall status = %d, want NFS4ERR_COMPLETE_ALREADY (%d)",
				overallStatus, types.NFS4ERR_COMPLETE_ALREADY)
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

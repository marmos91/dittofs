package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

func TestHandleDestroyClientID(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		// Set up client via EXCHANGE_ID (no session)
		h := newTestHandler()
		clientID, _ := registerExchangeID(t, h, "dc-success-client")

		// Encode DESTROY_CLIENTID args
		var buf bytes.Buffer
		args := types.DestroyClientidArgs{ClientID: clientID}
		_ = args.Encode(&buf)

		ops := []compoundOp{{opCode: types.OP_DESTROY_CLIENTID, data: buf.Bytes()}}
		data := buildCompoundArgsWithOps([]byte("dc-ok"), 1, ops)

		ctx := newTestCompoundContext()
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4_OK {
			t.Errorf("status = %d, want NFS4_OK", decoded.Status)
		}
		if decoded.NumResults != 1 {
			t.Fatalf("numResults = %d, want 1", decoded.NumResults)
		}
		if decoded.Results[0].OpCode != types.OP_DESTROY_CLIENTID {
			t.Errorf("result opcode = %d, want OP_DESTROY_CLIENTID", decoded.Results[0].OpCode)
		}
	})

	t.Run("clientid_busy", func(t *testing.T) {
		// Set up client + session -- DESTROY_CLIENTID should fail with CLIENTID_BUSY
		h := newTestHandler()
		clientID, seqID := registerExchangeID(t, h, "dc-busy-client")

		// Create session
		ctx := newTestCompoundContext()
		secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
		csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
		csOps := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
		csData := buildCompoundArgsWithOps([]byte("cs"), 1, csOps)
		csResp, err := h.ProcessCompound(ctx, csData)
		if err != nil {
			t.Fatalf("CREATE_SESSION error: %v", err)
		}
		csDecoded, _ := decodeCompoundResponse(csResp)
		if csDecoded.Status != types.NFS4_OK {
			t.Fatalf("CREATE_SESSION status = %d, want NFS4_OK", csDecoded.Status)
		}

		// Now try to destroy the client -- should fail because session exists
		var buf bytes.Buffer
		args := types.DestroyClientidArgs{ClientID: clientID}
		_ = args.Encode(&buf)

		ops := []compoundOp{{opCode: types.OP_DESTROY_CLIENTID, data: buf.Bytes()}}
		data := buildCompoundArgsWithOps([]byte("dc-busy"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_CLIENTID_BUSY {
			t.Errorf("status = %d, want NFS4ERR_CLIENTID_BUSY (%d)",
				decoded.Status, types.NFS4ERR_CLIENTID_BUSY)
		}
	})

	t.Run("stale_clientid", func(t *testing.T) {
		h := newTestHandler()

		// Use a non-existent client ID
		var buf bytes.Buffer
		args := types.DestroyClientidArgs{ClientID: 0xDEADBEEF}
		_ = args.Encode(&buf)

		ops := []compoundOp{{opCode: types.OP_DESTROY_CLIENTID, data: buf.Bytes()}}
		data := buildCompoundArgsWithOps([]byte("dc-stale"), 1, ops)

		ctx := newTestCompoundContext()
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_STALE_CLIENTID {
			t.Errorf("status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
				decoded.Status, types.NFS4ERR_STALE_CLIENTID)
		}
	})

	t.Run("bad_xdr", func(t *testing.T) {
		h := newTestHandler()

		// Truncated args
		ops := []compoundOp{{opCode: types.OP_DESTROY_CLIENTID, data: []byte{0x00, 0x01}}}
		data := buildCompoundArgsWithOps([]byte("dc-badxdr"), 1, ops)

		ctx := newTestCompoundContext()
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_BADXDR {
			t.Errorf("status = %d, want NFS4ERR_BADXDR (%d)",
				decoded.Status, types.NFS4ERR_BADXDR)
		}
	})

	t.Run("session_exempt", func(t *testing.T) {
		// DESTROY_CLIENTID should work without SEQUENCE (session-exempt)
		h := newTestHandler()
		clientID, _ := registerExchangeID(t, h, "dc-exempt-client")

		var buf bytes.Buffer
		args := types.DestroyClientidArgs{ClientID: clientID}
		_ = args.Encode(&buf)

		// Send as only op in v4.1 COMPOUND (no SEQUENCE)
		ops := []compoundOp{{opCode: types.OP_DESTROY_CLIENTID, data: buf.Bytes()}}
		data := buildCompoundArgsWithOps([]byte("dc-exempt"), 1, ops)

		ctx := newTestCompoundContext()
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		// Should succeed (exempt from SEQUENCE)
		if decoded.Status != types.NFS4_OK {
			t.Errorf("status = %d, want NFS4_OK (session-exempt)", decoded.Status)
		}
		if decoded.NumResults != 1 {
			t.Fatalf("numResults = %d, want 1", decoded.NumResults)
		}
		if decoded.Results[0].OpCode != types.OP_DESTROY_CLIENTID {
			t.Errorf("result opcode = %d, want OP_DESTROY_CLIENTID", decoded.Results[0].OpCode)
		}
	})
}

// encodeSetClientIDArgsForTest encodes SETCLIENTID args for v4.0 COMPOUND testing.
func encodeSetClientIDArgsForTest() []byte {
	var buf bytes.Buffer
	// verifier (8 bytes)
	var verifier [8]byte
	copy(verifier[:], "testverf")
	buf.Write(verifier[:])
	// client id string (XDR opaque string)
	_ = xdr.WriteXDRString(&buf, "test-client-id")
	// cb_program (uint32)
	_ = xdr.WriteUint32(&buf, 0x40000000)
	// netid (string)
	_ = xdr.WriteXDRString(&buf, "tcp")
	// addr (string)
	_ = xdr.WriteXDRString(&buf, "127.0.0.1.8.1")
	// callback_ident (uint32)
	_ = xdr.WriteUint32(&buf, 1)
	return buf.Bytes()
}

// runDestroyClientID sends the given operations as one v4.1 COMPOUND and
// returns the decoded reply. It drives ProcessCompound, the same entry point
// the RPC layer uses, so COMPOUND-level gating is in force.
func runDestroyClientID(t *testing.T, h *Handler, ctx *types.CompoundContext, tag string, ops []compoundOp) *decodedCompoundResponse {
	t.Helper()
	resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte(tag), 1, ops))
	if err != nil {
		t.Fatalf("ProcessCompound(%s) error: %v", tag, err)
	}
	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response(%s) error: %v", tag, err)
	}
	return decoded
}

func encodeDestroyClientIDArgsForTest(t *testing.T, clientID uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := (&types.DestroyClientidArgs{ClientID: clientID}).Encode(&buf); err != nil {
		t.Fatalf("encode DESTROY_CLIENTID args: %v", err)
	}
	return buf.Bytes()
}

// TestDestroyClientID_UnrecognizedTargetBeatsRequesterIdentity pins the order in
// which DESTROY_CLIENTID answers a request that trips two checks at once: the
// requester is identifiable and is a different client from the target (so any
// requester-identity check would fire), and the target client ID is not one the
// server has handed out (so the target-recognition check fires too). RFC 8881
// Section 18.50 makes the target-recognition verdict the answer -- the reply is
// NFS4ERR_STALE_CLIENTID. A single-fault request cannot tell the two orders
// apart, which is why this one carries both faults.
func TestDestroyClientID_UnrecognizedTargetBeatsRequesterIdentity(t *testing.T) {
	h, sessionID := createTestSessionWithConnectionID(t, 9500)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 9500

	const unknownClientID = 0xDEADBEEF
	decoded := runDestroyClientID(t, h, ctx, "dc-order", []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 1, 0, false)},
		{opCode: types.OP_DESTROY_CLIENTID, data: encodeDestroyClientIDArgsForTest(t, unknownClientID)},
	})

	if decoded.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Fatalf("status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			decoded.Status, types.NFS4ERR_STALE_CLIENTID)
	}
	if decoded.NumResults != 2 {
		t.Fatalf("numResults = %d, want 2", decoded.NumResults)
	}
	// The SEQUENCE succeeded, so the requester was identifiable and distinct
	// from the target: the reply above is the target-recognition verdict, not a
	// side effect of the request failing before DESTROY_CLIENTID ran.
	if decoded.Results[0].OpCode != types.OP_SEQUENCE || decoded.Results[0].Status != types.NFS4_OK {
		t.Fatalf("result[0] = op %d status %d, want OP_SEQUENCE with NFS4_OK",
			decoded.Results[0].OpCode, decoded.Results[0].Status)
	}
}

// TestDestroyClientID_TargetsAnotherClientBehindSequence covers the case
// RFC 8881 Section 18.50 explicitly permits: a DESTROY_CLIENTID preceded by a
// SEQUENCE whose session belongs to a different client than the one being
// destroyed. The target holds no session and no state, so it is destroyed and
// the reply is NFS4_OK; a second attempt then reports the client ID as no
// longer recognized.
func TestDestroyClientID_TargetsAnotherClientBehindSequence(t *testing.T) {
	h, sessionID := createTestSessionWithConnectionID(t, 9600)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 9600

	targetClientID, _ := registerExchangeID(t, h, "dc-other-client")
	targetArgs := encodeDestroyClientIDArgsForTest(t, targetClientID)

	decoded := runDestroyClientID(t, h, ctx, "dc-other", []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 1, 0, false)},
		{opCode: types.OP_DESTROY_CLIENTID, data: targetArgs},
	})
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}

	// The client ID is gone: destroying it again is answered as unrecognized.
	again := runDestroyClientID(t, h, ctx, "dc-other2", []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 2, 0, false)},
		{opCode: types.OP_DESTROY_CLIENTID, data: targetArgs},
	})
	if again.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Fatalf("second destroy status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			again.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}

// TestDestroyClientID_SelfTargetBehindOwnSequenceIsBusy covers the converse of
// the cross-client case: when the client ID derived from the preceding
// SEQUENCE is the same one being destroyed, the client still holds the session
// that carried the request, so the reply is NFS4ERR_CLIENTID_BUSY (RFC 8881
// Section 18.50).
func TestDestroyClientID_SelfTargetBehindOwnSequenceIsBusy(t *testing.T) {
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "dc-self-client")
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 9700

	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	sessionID := runCreateSession(t, h, encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)).SessionID

	decoded := runDestroyClientID(t, h, ctx, "dc-self", []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 1, 0, false)},
		{opCode: types.OP_DESTROY_CLIENTID, data: encodeDestroyClientIDArgsForTest(t, clientID)},
	})
	if decoded.Status != types.NFS4ERR_CLIENTID_BUSY {
		t.Fatalf("status = %d, want NFS4ERR_CLIENTID_BUSY (%d)",
			decoded.Status, types.NFS4ERR_CLIENTID_BUSY)
	}
}

// TestDestroyClientID_StandaloneOnConnectionBoundToAnotherClient covers the
// standalone form of the operation: a sole-op COMPOUND with no SEQUENCE,
// arriving on a connection that is bound to a different client's session. The
// connection binding names a client, but that identity does not gate the
// operation: the target holds no state, so it is destroyed and the second
// attempt reports it as no longer recognized.
func TestDestroyClientID_StandaloneOnConnectionBoundToAnotherClient(t *testing.T) {
	h, _ := createTestSessionWithConnectionID(t, 9800)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 9800

	targetClientID, _ := registerExchangeID(t, h, "dc-standalone-client")
	ops := []compoundOp{
		{opCode: types.OP_DESTROY_CLIENTID, data: encodeDestroyClientIDArgsForTest(t, targetClientID)},
	}

	decoded := runDestroyClientID(t, h, ctx, "dc-alone", ops)
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}

	again := runDestroyClientID(t, h, ctx, "dc-alone2", ops)
	if again.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Fatalf("second destroy status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			again.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}

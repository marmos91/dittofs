package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
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

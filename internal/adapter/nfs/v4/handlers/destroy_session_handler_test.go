package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

func TestHandleDestroySession_BadXDR(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Truncated args -- not enough for a 16-byte session ID
	truncatedArgs := []byte{0x00, 0x00, 0x00, 0x01}
	ops := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: truncatedArgs}}
	data := buildCompoundArgsWithOps([]byte("badxdr"), 1, ops)

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
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION", decoded.Results[0].OpCode)
	}
}

func TestHandleDestroySession_NonExistent(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Destroy a session that was never created
	var fakeSID types.SessionId4
	copy(fakeSID[:], []byte("fakesession12345"))

	var buf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: fakeSID}
	_ = dsArgs.Encode(&buf)

	ops := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: buf.Bytes()}}
	data := buildCompoundArgsWithOps([]byte("noexist"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("status = %d, want NFS4ERR_BADSESSION (%d)",
			decoded.Status, types.NFS4ERR_BADSESSION)
	}
}

func TestHandleDestroySession_ValidSession(t *testing.T) {
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "ds-valid-client")

	ctx := newTestCompoundContext()

	// Create session first
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}} // AUTH_NONE
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	csOps := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	csData := buildCompoundArgsWithOps([]byte("create"), 1, csOps)
	csResp, err := h.ProcessCompound(ctx, csData)
	if err != nil {
		t.Fatalf("CREATE_SESSION error: %v", err)
	}

	// Extract session ID
	reader := bytes.NewReader(csResp)
	st, _ := xdr.DecodeUint32(reader)
	if st != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION overall status = %d", st)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var csRes types.CreateSessionRes
	if err := csRes.Decode(reader); err != nil {
		t.Fatalf("decode CreateSessionRes: %v", err)
	}
	if csRes.Status != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION res status = %d", csRes.Status)
	}

	// Destroy the session
	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: csRes.SessionID}
	_ = dsArgs.Encode(&dsBuf)

	dsOps := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()}}
	dsData := buildCompoundArgsWithOps([]byte("destroy"), 1, dsOps)
	dsResp, err := h.ProcessCompound(ctx, dsData)
	if err != nil {
		t.Fatalf("DESTROY_SESSION error: %v", err)
	}

	decoded, err := decodeCompoundResponse(dsResp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("DESTROY_SESSION status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION", decoded.Results[0].OpCode)
	}
}

func TestHandleDestroySession_FollowedByPutRootFH(t *testing.T) {
	// Verify DESTROY_SESSION properly consumes its args without desyncing
	// the XDR reader.
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Use a fake session ID that won't exist -- the important thing is
	// that the args are consumed correctly even on error.
	var fakeSID types.SessionId4
	copy(fakeSID[:], []byte("desynctestsessid"))

	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: fakeSID}
	_ = dsArgs.Encode(&dsBuf)

	ops := []compoundOp{
		{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("desync"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// DESTROY_SESSION fails with BADSESSION which stops the compound --
	// but the test verifies it didn't corrupt the XDR reader (no BADXDR).
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1 (error stops compound)", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION", decoded.Results[0].OpCode)
	}
	if decoded.Results[0].Status != types.NFS4ERR_BADSESSION {
		t.Errorf("result status = %d, want NFS4ERR_BADSESSION", decoded.Results[0].Status)
	}
}

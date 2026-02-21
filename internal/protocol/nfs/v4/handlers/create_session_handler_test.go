package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// encodeCreateSessionArgsWithSec builds CREATE_SESSION XDR args with specific
// callback security parameters (reuses the ClientID from a prior EXCHANGE_ID).
func encodeCreateSessionArgsWithSec(clientID uint64, seqID uint32, flags uint32, secParms []types.CallbackSecParms4) []byte {
	var buf bytes.Buffer
	args := types.CreateSessionArgs{
		ClientID:   clientID,
		SequenceID: seqID,
		Flags:      flags,
		ForeChannelAttrs: types.ChannelAttrs{
			MaxRequestSize:        1048576,
			MaxResponseSize:       1048576,
			MaxResponseSizeCached: 4096,
			MaxOperations:         16,
			MaxRequests:           64,
		},
		BackChannelAttrs: types.ChannelAttrs{
			MaxRequestSize:        4096,
			MaxResponseSize:       4096,
			MaxResponseSizeCached: 0,
			MaxOperations:         2,
			MaxRequests:           1,
		},
		CbProgram:  0x40000000,
		CbSecParms: secParms,
	}
	_ = args.Encode(&buf)
	return buf.Bytes()
}

// registerExchangeID performs EXCHANGE_ID via COMPOUND and returns the
// allocated client ID and the next sequence ID to use for CREATE_SESSION.
// Per RFC 8881, CREATE_SESSION must send record.SequenceID + 1.
func registerExchangeID(t *testing.T, h *Handler, ownerID string) (uint64, uint32) {
	t.Helper()
	ctx := newTestCompoundContext()

	var verifier [8]byte
	copy(verifier[:], "testverf")
	eidArgs := encodeExchangeIdArgs([]byte(ownerID), verifier, 0, types.SP4_NONE, nil)
	ops := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: eidArgs}}
	data := buildCompoundArgsWithOps([]byte("eid"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("EXCHANGE_ID ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader) // overall status
	if status != types.NFS4_OK {
		t.Fatalf("EXCHANGE_ID overall status = %d, want NFS4_OK", status)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var res types.ExchangeIdRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode ExchangeIdRes: %v", err)
	}
	if res.Status != types.NFS4_OK {
		t.Fatalf("EXCHANGE_ID status = %d, want NFS4_OK", res.Status)
	}

	// Return seqID+1: CREATE_SESSION must send record.SequenceID + 1
	return res.ClientID, res.SequenceID + 1
}

func TestHandleCreateSession_Success(t *testing.T) {
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "cs-test-client")

	ctx := newTestCompoundContext()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}} // AUTH_NONE
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("cs"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", status)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 1 {
		t.Fatalf("numResults = %d, want 1", numResults)
	}
	opCode, _ := xdr.DecodeUint32(reader)
	if opCode != types.OP_CREATE_SESSION {
		t.Errorf("result opcode = %d, want OP_CREATE_SESSION (%d)", opCode, types.OP_CREATE_SESSION)
	}

	var res types.CreateSessionRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode CreateSessionRes: %v", err)
	}
	if res.Status != types.NFS4_OK {
		t.Errorf("CreateSessionRes status = %d, want NFS4_OK", res.Status)
	}

	// Session ID should be non-zero
	var zeroSID types.SessionId4
	if res.SessionID == zeroSID {
		t.Error("SessionID should not be zero")
	}

	// SequenceID should match what we sent
	if res.SequenceID != seqID {
		t.Errorf("SequenceID = %d, want %d", res.SequenceID, seqID)
	}

	// Channel attrs should be negotiated (non-zero)
	if res.ForeChannelAttrs.MaxRequests == 0 {
		t.Error("ForeChannelAttrs.MaxRequests should be non-zero")
	}
	if res.ForeChannelAttrs.MaxRequestSize == 0 {
		t.Error("ForeChannelAttrs.MaxRequestSize should be non-zero")
	}
}

func TestHandleCreateSession_Replay(t *testing.T) {
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "cs-replay-client")

	ctx := newTestCompoundContext()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)

	// First call: creates session
	ops1 := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data1 := buildCompoundArgsWithOps([]byte("cs1"), 1, ops1)
	resp1, err := h.ProcessCompound(ctx, data1)
	if err != nil {
		t.Fatalf("ProcessCompound #1 error: %v", err)
	}

	// Second call with same args: should replay cached response
	ops2 := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data2 := buildCompoundArgsWithOps([]byte("cs2"), 1, ops2)
	resp2, err := h.ProcessCompound(ctx, data2)
	if err != nil {
		t.Fatalf("ProcessCompound #2 error: %v", err)
	}

	// Decode both responses and compare session IDs
	decode := func(resp []byte) types.CreateSessionRes {
		reader := bytes.NewReader(resp)
		status, _ := xdr.DecodeUint32(reader)
		if status != types.NFS4_OK {
			t.Fatalf("overall status = %d, want NFS4_OK", status)
		}
		_, _ = xdr.DecodeOpaque(reader) // tag
		_, _ = xdr.DecodeUint32(reader) // numResults
		_, _ = xdr.DecodeUint32(reader) // opcode
		var res types.CreateSessionRes
		if err := res.Decode(reader); err != nil {
			t.Fatalf("decode CreateSessionRes: %v", err)
		}
		return res
	}

	res1 := decode(resp1)
	res2 := decode(resp2)

	if res1.Status != types.NFS4_OK || res2.Status != types.NFS4_OK {
		t.Fatalf("expected both NFS4_OK, got %d and %d", res1.Status, res2.Status)
	}

	// Replay should return the same session ID
	if res1.SessionID != res2.SessionID {
		t.Errorf("replay session IDs differ: %s vs %s",
			res1.SessionID.String(), res2.SessionID.String())
	}
}

func TestHandleCreateSession_BadXDR(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Truncated args
	truncatedArgs := []byte{0x00, 0x00, 0x00, 0x01}
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: truncatedArgs}}
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
	if decoded.Results[0].OpCode != types.OP_CREATE_SESSION {
		t.Errorf("result opcode = %d, want OP_CREATE_SESSION", decoded.Results[0].OpCode)
	}
}

func TestHandleCreateSession_RPCSECGSSOnly(t *testing.T) {
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "cs-rpcsec-client")

	ctx := newTestCompoundContext()
	// Only RPCSEC_GSS callback security -- rejected by our server
	secParms := []types.CallbackSecParms4{
		{CbSecFlavor: 6, RpcGssData: []byte("dummy-gss")},
	}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("rpcsec"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_ENCR_ALG_UNSUPP {
		t.Errorf("status = %d, want NFS4ERR_ENCR_ALG_UNSUPP (%d)",
			decoded.Status, types.NFS4ERR_ENCR_ALG_UNSUPP)
	}
}

func TestHandleCreateSession_UnknownClient(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Use a clientID that has never been registered via EXCHANGE_ID
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(0xDEADBEEF, 1, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("unknown"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// Should return STALE_CLIENTID for unknown client
	if decoded.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Errorf("status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			decoded.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}

func TestHandleCreateSession_FollowedByPutRootFH(t *testing.T) {
	// Verify CREATE_SESSION properly consumes its args without desyncing
	// the XDR reader -- PUTROOTFH after CREATE_SESSION should succeed.
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "cs-desync-client")

	ctx := newTestCompoundContext()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)

	ops := []compoundOp{
		{opCode: types.OP_CREATE_SESSION, data: csArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("desync"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", status)
	}

	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 2 {
		t.Fatalf("numResults = %d, want 2", numResults)
	}

	// First op: CREATE_SESSION
	op1Code, _ := xdr.DecodeUint32(reader)
	if op1Code != types.OP_CREATE_SESSION {
		t.Errorf("result[0] opcode = %d, want OP_CREATE_SESSION", op1Code)
	}
	var csRes types.CreateSessionRes
	_ = csRes.Decode(reader)
	if csRes.Status != types.NFS4_OK {
		t.Errorf("CREATE_SESSION status = %d, want NFS4_OK", csRes.Status)
	}

	// Second op: PUTROOTFH
	op2Code, _ := xdr.DecodeUint32(reader)
	if op2Code != types.OP_PUTROOTFH {
		t.Errorf("result[1] opcode = %d, want OP_PUTROOTFH", op2Code)
	}
	op2Status, _ := xdr.DecodeUint32(reader)
	if op2Status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", op2Status)
	}
}

func TestHandleCreateSession_RoundTripWithDestroy(t *testing.T) {
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "cs-roundtrip-client")

	ctx := newTestCompoundContext()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)

	// Create session
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("create"), 1, ops)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("CREATE_SESSION error: %v", err)
	}

	// Extract session ID from response
	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION overall status = %d, want NFS4_OK", status)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var csRes types.CreateSessionRes
	if err := csRes.Decode(reader); err != nil {
		t.Fatalf("decode CreateSessionRes: %v", err)
	}
	if csRes.Status != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION status = %d, want NFS4_OK", csRes.Status)
	}

	// Destroy session using the returned session ID
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
		t.Fatalf("decode destroy response: %v", err)
	}
	if decoded.Status != types.NFS4_OK {
		t.Errorf("DESTROY_SESSION status = %d, want NFS4_OK", decoded.Status)
	}

	// Verify session is gone: destroy again should fail
	dsOps2 := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()}}
	dsData2 := buildCompoundArgsWithOps([]byte("destroy2"), 1, dsOps2)
	dsResp2, err := h.ProcessCompound(ctx, dsData2)
	if err != nil {
		t.Fatalf("DESTROY_SESSION #2 error: %v", err)
	}

	decoded2, err := decodeCompoundResponse(dsResp2)
	if err != nil {
		t.Fatalf("decode destroy #2 response: %v", err)
	}
	if decoded2.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("DESTROY_SESSION #2 status = %d, want NFS4ERR_BADSESSION (%d)",
			decoded2.Status, types.NFS4ERR_BADSESSION)
	}
}

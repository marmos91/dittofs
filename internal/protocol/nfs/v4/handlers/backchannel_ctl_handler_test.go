package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// BACKCHANNEL_CTL Handler Tests (Phase 22)
// ============================================================================

// encodeBackchannelCtlArgs builds BACKCHANNEL_CTL XDR args for testing.
func encodeBackchannelCtlArgs(cbProgram uint32, secParms []types.CallbackSecParms4) []byte {
	var buf bytes.Buffer
	args := types.BackchannelCtlArgs{
		CbProgram: cbProgram,
		SecParms:  secParms,
	}
	_ = args.Encode(&buf)
	return buf.Bytes()
}

// createTestSessionWithBackchannel creates a session with CONN_BACK_CHAN flag
// and returns the handler and session ID. The session will have BackChannelSlots.
func createTestSessionWithBackchannel(t *testing.T) (*Handler, types.SessionId4) {
	t.Helper()
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "backchannel-ctl-test-client")

	ctx := newTestCompoundContext()

	// Create session with CONN_BACK_CHAN flag
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}} // AUTH_NONE
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID,
		types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("cs-back"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("CREATE_SESSION ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader) // overall status
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

	return h, csRes.SessionID
}

// createTestSessionWithoutBackchannel creates a session WITHOUT the CONN_BACK_CHAN flag.
// The session will not have BackChannelSlots.
func createTestSessionWithoutBackchannel(t *testing.T) (*Handler, types.SessionId4) {
	t.Helper()
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "no-backchannel-client")

	ctx := newTestCompoundContext()

	// Create session without CONN_BACK_CHAN flag (flags = 0)
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("cs-no-back"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("CREATE_SESSION ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION status = %d, want NFS4_OK", status)
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

	return h, csRes.SessionID
}

// TestBackchannelCtl_Success validates BACKCHANNEL_CTL with valid args through
// a SEQUENCE + BACKCHANNEL_CTL compound.
func TestBackchannelCtl_Success(t *testing.T) {
	h, sessionID := createTestSessionWithBackchannel(t)
	ctx := newTestCompoundContext()

	// SEQUENCE + BACKCHANNEL_CTL
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	bctlArgs := encodeBackchannelCtlArgs(0x50000000, []types.CallbackSecParms4{
		{CbSecFlavor: 0}, // AUTH_NONE
	})

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_BACKCHANNEL_CTL, data: bctlArgs},
	}
	data := buildCompoundArgsWithOps([]byte("bctl-ok"), 1, ops)

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

	// Skip SEQUENCE result
	_, _ = xdr.DecodeUint32(reader) // opcode (SEQUENCE)
	var seqRes types.SequenceRes
	_ = seqRes.Decode(reader)

	// BACKCHANNEL_CTL result
	opCode, _ := xdr.DecodeUint32(reader)
	if opCode != types.OP_BACKCHANNEL_CTL {
		t.Errorf("result opcode = %d, want OP_BACKCHANNEL_CTL (%d)", opCode, types.OP_BACKCHANNEL_CTL)
	}
	var bctlRes types.BackchannelCtlRes
	if err := bctlRes.Decode(reader); err != nil {
		t.Fatalf("decode BackchannelCtlRes: %v", err)
	}
	if bctlRes.Status != types.NFS4_OK {
		t.Errorf("BACKCHANNEL_CTL status = %d, want NFS4_OK", bctlRes.Status)
	}

	// Verify params were stored
	session := h.StateManager.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found")
	}
	if session.CbProgram != 0x50000000 {
		t.Errorf("CbProgram = 0x%x, want 0x50000000", session.CbProgram)
	}
}

// TestBackchannelCtl_NoBackchannel verifies NFS4ERR_INVAL when session has no backchannel.
func TestBackchannelCtl_NoBackchannel(t *testing.T) {
	h, sessionID := createTestSessionWithoutBackchannel(t)
	ctx := newTestCompoundContext()

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	bctlArgs := encodeBackchannelCtlArgs(0x50000000, []types.CallbackSecParms4{
		{CbSecFlavor: 0},
	})

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_BACKCHANNEL_CTL, data: bctlArgs},
	}
	data := buildCompoundArgsWithOps([]byte("bctl-no-back"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	// SEQUENCE succeeds but BACKCHANNEL_CTL fails, overall status reflects failure
	if status != types.NFS4ERR_INVAL {
		t.Errorf("overall status = %d, want NFS4ERR_INVAL (%d)", status, types.NFS4ERR_INVAL)
	}
}

// TestBackchannelCtl_BadXDR verifies NFS4ERR_BADXDR for malformed input.
func TestBackchannelCtl_BadXDR(t *testing.T) {
	h, sessionID := createTestSessionWithBackchannel(t)
	ctx := newTestCompoundContext()

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	// Truncated args: not enough data for cb_program
	truncatedArgs := []byte{0x00}

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_BACKCHANNEL_CTL, data: truncatedArgs},
	}
	data := buildCompoundArgsWithOps([]byte("bctl-bad-xdr"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4ERR_BADXDR {
		t.Errorf("overall status = %d, want NFS4ERR_BADXDR (%d)", status, types.NFS4ERR_BADXDR)
	}
}

// TestBackchannelCtl_NoAcceptableSecurity verifies NFS4ERR_INVAL when only
// RPCSEC_GSS is offered (no AUTH_NONE or AUTH_SYS).
func TestBackchannelCtl_NoAcceptableSecurity(t *testing.T) {
	h, sessionID := createTestSessionWithBackchannel(t)
	ctx := newTestCompoundContext()

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	// Only RPCSEC_GSS (flavor 6) -- not acceptable
	bctlArgs := encodeBackchannelCtlArgs(0x50000000, []types.CallbackSecParms4{
		{CbSecFlavor: 6}, // RPCSEC_GSS
	})

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_BACKCHANNEL_CTL, data: bctlArgs},
	}
	data := buildCompoundArgsWithOps([]byte("bctl-bad-sec"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4ERR_INVAL {
		t.Errorf("overall status = %d, want NFS4ERR_INVAL (%d)", status, types.NFS4ERR_INVAL)
	}
}

// TestBackchannelCtl_UpdatesProgram verifies that a second BACKCHANNEL_CTL
// with a different cb_program value overwrites the first.
func TestBackchannelCtl_UpdatesProgram(t *testing.T) {
	h, sessionID := createTestSessionWithBackchannel(t)
	ctx := newTestCompoundContext()

	// First BACKCHANNEL_CTL: cb_program = 0x50000000
	seqArgs1 := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	bctlArgs1 := encodeBackchannelCtlArgs(0x50000000, []types.CallbackSecParms4{
		{CbSecFlavor: 0},
	})
	ops1 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs1},
		{opCode: types.OP_BACKCHANNEL_CTL, data: bctlArgs1},
	}
	data1 := buildCompoundArgsWithOps([]byte("bctl-1"), 1, ops1)
	resp1, err := h.ProcessCompound(ctx, data1)
	if err != nil {
		t.Fatalf("first ProcessCompound error: %v", err)
	}
	decoded1, err := decodeCompoundResponse(resp1)
	if err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if decoded1.Status != types.NFS4_OK {
		t.Fatalf("first compound status = %d, want NFS4_OK", decoded1.Status)
	}

	// Verify cb_program is 0x50000000
	session := h.StateManager.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found")
	}
	if session.CbProgram != 0x50000000 {
		t.Errorf("after first BCTL: CbProgram = 0x%x, want 0x50000000", session.CbProgram)
	}

	// Second BACKCHANNEL_CTL: cb_program = 0x60000000 (seqID increments to 2)
	seqArgs2 := encodeSequenceArgs(sessionID, 0, 2, 0, false)
	bctlArgs2 := encodeBackchannelCtlArgs(0x60000000, []types.CallbackSecParms4{
		{CbSecFlavor: 1}, // AUTH_SYS
	})
	ops2 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
		{opCode: types.OP_BACKCHANNEL_CTL, data: bctlArgs2},
	}
	data2 := buildCompoundArgsWithOps([]byte("bctl-2"), 1, ops2)
	resp2, err := h.ProcessCompound(ctx, data2)
	if err != nil {
		t.Fatalf("second ProcessCompound error: %v", err)
	}
	decoded2, err := decodeCompoundResponse(resp2)
	if err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if decoded2.Status != types.NFS4_OK {
		t.Fatalf("second compound status = %d, want NFS4_OK", decoded2.Status)
	}

	// Verify cb_program was updated to 0x60000000
	session = h.StateManager.GetSession(sessionID)
	if session.CbProgram != 0x60000000 {
		t.Errorf("after second BCTL: CbProgram = 0x%x, want 0x60000000", session.CbProgram)
	}
}

package handlers

import (
	"bytes"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// encodeSequenceArgs encodes SEQUENCE4args for testing.
func encodeSequenceArgs(sessionID types.SessionId4, slotID, seqID, highestSlotID uint32, cacheThis bool) []byte {
	var buf bytes.Buffer
	args := types.SequenceArgs{
		SessionID:     sessionID,
		SequenceID:    seqID,
		SlotID:        slotID,
		HighestSlotID: highestSlotID,
		CacheThis:     cacheThis,
	}
	_ = args.Encode(&buf)
	return buf.Bytes()
}

// createTestSession performs EXCHANGE_ID + CREATE_SESSION via COMPOUND and
// returns the session ID and the handler, ready for SEQUENCE testing.
func createTestSession(t *testing.T) (*Handler, types.SessionId4) {
	t.Helper()
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "seq-test-client")

	ctx := newTestCompoundContext()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}} // AUTH_NONE
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("cs"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("CREATE_SESSION ProcessCompound error: %v", err)
	}

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

	return h, csRes.SessionID
}

// decodeSequenceRes decodes SEQUENCE4res from a COMPOUND response that has
// exactly one result (the SEQUENCE result).
func decodeSequenceRes(t *testing.T, resp []byte) (*decodedCompoundResponse, *types.SequenceRes) {
	t.Helper()
	reader := bytes.NewReader(resp)

	status, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	tag, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode tag: %v", err)
	}
	numResults, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode numResults: %v", err)
	}

	if numResults == 0 {
		return &decodedCompoundResponse{Status: status, Tag: tag, NumResults: 0}, nil
	}

	opCode, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode opcode: %v", err)
	}

	var seqRes types.SequenceRes
	if err := seqRes.Decode(reader); err != nil {
		t.Fatalf("decode SequenceRes: %v", err)
	}

	decoded := &decodedCompoundResponse{
		Status:     status,
		Tag:        tag,
		NumResults: numResults,
		Results: []decodedResult{
			{OpCode: opCode, Status: seqRes.Status},
		},
	}
	return decoded, &seqRes
}

// ============================================================================
// SEQUENCE Validation Tests
// ============================================================================

func TestSequence_NewRequest_Success(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// SEQUENCE with slot 0, seqid 1, cache=true
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops := []compoundOp{{opCode: types.OP_SEQUENCE, data: seqArgs}}
	data := buildCompoundArgsWithOps([]byte("seq"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, seqRes := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if seqRes.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE status = %d, want NFS4_OK", seqRes.Status)
	}
	if seqRes.SessionID != sessionID {
		t.Errorf("session ID mismatch")
	}
	if seqRes.SequenceID != 1 {
		t.Errorf("SequenceID = %d, want 1", seqRes.SequenceID)
	}
	if seqRes.SlotID != 0 {
		t.Errorf("SlotID = %d, want 0", seqRes.SlotID)
	}
}

func TestSequence_BadSessionID(t *testing.T) {
	h, _ := createTestSession(t)
	ctx := newTestCompoundContext()

	var badSessionID types.SessionId4
	copy(badSessionID[:], "bad-session-id!!")

	seqArgs := encodeSequenceArgs(badSessionID, 0, 1, 0, true)
	ops := []compoundOp{{opCode: types.OP_SEQUENCE, data: seqArgs}}
	data := buildCompoundArgsWithOps([]byte("badsess"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, seqRes := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("overall status = %d, want NFS4ERR_BADSESSION (%d)",
			decoded.Status, types.NFS4ERR_BADSESSION)
	}
	if seqRes != nil && seqRes.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("SEQUENCE status = %d, want NFS4ERR_BADSESSION", seqRes.Status)
	}
}

func TestSequence_BadSlotID(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Use slot 999 (far exceeds max slots)
	seqArgs := encodeSequenceArgs(sessionID, 999, 1, 0, true)
	ops := []compoundOp{{opCode: types.OP_SEQUENCE, data: seqArgs}}
	data := buildCompoundArgsWithOps([]byte("badslot"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4ERR_BADSLOT {
		t.Errorf("overall status = %d, want NFS4ERR_BADSLOT (%d)",
			decoded.Status, types.NFS4ERR_BADSLOT)
	}
}

func TestSequence_SeqMisordered(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Slot starts at seqid 0, so next expected is 1.
	// Sending seqid=5 should be misordered.
	seqArgs := encodeSequenceArgs(sessionID, 0, 5, 0, true)
	ops := []compoundOp{{opCode: types.OP_SEQUENCE, data: seqArgs}}
	data := buildCompoundArgsWithOps([]byte("misord"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4ERR_SEQ_MISORDERED {
		t.Errorf("overall status = %d, want NFS4ERR_SEQ_MISORDERED (%d)",
			decoded.Status, types.NFS4ERR_SEQ_MISORDERED)
	}
}

func TestSequence_ReplayWithCache(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// First request: SEQUENCE + PUTROOTFH with cache=true
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("replay1"), 1, ops)

	resp1, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound #1 error: %v", err)
	}

	// Verify first response succeeds
	decoded1, _ := decodeSequenceRes(t, resp1)
	if decoded1.Status != types.NFS4_OK {
		t.Fatalf("first request status = %d, want NFS4_OK", decoded1.Status)
	}

	// Second request: same slot+seqid (replay)
	seqArgs2 := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops2 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
		{opCode: types.OP_PUTROOTFH},
	}
	data2 := buildCompoundArgsWithOps([]byte("replay2"), 1, ops2)

	resp2, err := h.ProcessCompound(ctx, data2)
	if err != nil {
		t.Fatalf("ProcessCompound #2 error: %v", err)
	}

	// Replay should return byte-identical cached response
	if !bytes.Equal(resp1, resp2) {
		t.Errorf("replay response differs from original (len %d vs %d)", len(resp1), len(resp2))
	}
}

func TestSequence_ReplayWithoutCache(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// First request: SEQUENCE with cache=false
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("uncached"), 1, ops)

	resp1, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound #1 error: %v", err)
	}

	decoded1, _ := decodeSequenceRes(t, resp1)
	if decoded1.Status != types.NFS4_OK {
		t.Fatalf("first request status = %d, want NFS4_OK", decoded1.Status)
	}

	// Second request: same slot+seqid (retry of uncached)
	seqArgs2 := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	ops2 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
	}
	data2 := buildCompoundArgsWithOps([]byte("uncached2"), 1, ops2)

	resp2, err := h.ProcessCompound(ctx, data2)
	if err != nil {
		t.Fatalf("ProcessCompound #2 error: %v", err)
	}

	decoded2, _ := decodeSequenceRes(t, resp2)
	// Should return NFS4ERR_RETRY_UNCACHED_REP because no cached reply exists
	if decoded2.Status != types.NFS4ERR_RETRY_UNCACHED_REP {
		t.Errorf("uncached retry status = %d, want NFS4ERR_RETRY_UNCACHED_REP (%d)",
			decoded2.Status, types.NFS4ERR_RETRY_UNCACHED_REP)
	}
}

func TestSequence_BadXDR(t *testing.T) {
	h, _ := createTestSession(t)
	ctx := newTestCompoundContext()

	// Truncated SEQUENCE args
	truncated := []byte{0x00, 0x01, 0x02}
	ops := []compoundOp{{opCode: types.OP_SEQUENCE, data: truncated}}
	data := buildCompoundArgsWithOps([]byte("badxdr"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4ERR_BADXDR {
		t.Errorf("status = %d, want NFS4ERR_BADXDR (%d)",
			decoded.Status, types.NFS4ERR_BADXDR)
	}
}

// ============================================================================
// COMPOUND Dispatch Tests
// ============================================================================

func TestCompound_V41_SequenceWithPutrootfh(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// SEQUENCE + PUTROOTFH
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("seq-putroot"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Decode full response
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

	// Result 1: SEQUENCE
	op1Code, _ := xdr.DecodeUint32(reader)
	if op1Code != types.OP_SEQUENCE {
		t.Errorf("result[0] opcode = %d, want OP_SEQUENCE", op1Code)
	}
	var seqRes types.SequenceRes
	_ = seqRes.Decode(reader)
	if seqRes.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE status = %d, want NFS4_OK", seqRes.Status)
	}

	// Result 2: PUTROOTFH
	op2Code, _ := xdr.DecodeUint32(reader)
	if op2Code != types.OP_PUTROOTFH {
		t.Errorf("result[1] opcode = %d, want OP_PUTROOTFH", op2Code)
	}
	op2Status, _ := xdr.DecodeUint32(reader)
	if op2Status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", op2Status)
	}
}

func TestCompound_V41_ExemptOpNoSequence(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// EXCHANGE_ID is exempt: should work without SEQUENCE
	ownerID := []byte("exempt-test-client")
	var verifier [8]byte
	copy(verifier[:], "testverf")
	eidArgs := encodeExchangeIdArgs(ownerID, verifier, 0, types.SP4_NONE, nil)

	ops := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: eidArgs}}
	data := buildCompoundArgsWithOps([]byte("exempt"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Errorf("exempt op status = %d, want NFS4_OK", status)
	}
}

func TestCompound_V41_NonExemptNoSequence(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// PUTROOTFH is not exempt -- without SEQUENCE, should get OP_NOT_IN_SESSION
	ops := []compoundOp{{opCode: types.OP_PUTROOTFH}}
	data := buildCompoundArgsWithOps([]byte("nosequence"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_OP_NOT_IN_SESSION {
		t.Errorf("status = %d, want NFS4ERR_OP_NOT_IN_SESSION (%d)",
			decoded.Status, types.NFS4ERR_OP_NOT_IN_SESSION)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0", decoded.NumResults)
	}
}

func TestCompound_V41_SequenceAtPositionGtZero(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// SEQUENCE + SEQUENCE at position 1 should fail with NFS4ERR_SEQUENCE_POS
	seqArgs1 := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	seqArgs2 := encodeSequenceArgs(sessionID, 0, 2, 0, true)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs1},
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
	}
	data := buildCompoundArgsWithOps([]byte("seqpos"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Decode -- should have 2 results: SEQUENCE (OK) + SEQUENCE (SEQUENCE_POS)
	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4ERR_SEQUENCE_POS {
		t.Errorf("overall status = %d, want NFS4ERR_SEQUENCE_POS (%d)",
			status, types.NFS4ERR_SEQUENCE_POS)
	}

	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 2 {
		t.Fatalf("numResults = %d, want 2", numResults)
	}

	// Result 1: SEQUENCE OK
	op1Code, _ := xdr.DecodeUint32(reader)
	if op1Code != types.OP_SEQUENCE {
		t.Errorf("result[0] opcode = %d, want OP_SEQUENCE", op1Code)
	}
	var seqRes1 types.SequenceRes
	_ = seqRes1.Decode(reader)
	if seqRes1.Status != types.NFS4_OK {
		t.Errorf("result[0] SEQUENCE status = %d, want NFS4_OK", seqRes1.Status)
	}

	// Result 2: SEQUENCE at wrong position
	op2Code, _ := xdr.DecodeUint32(reader)
	if op2Code != types.OP_SEQUENCE {
		t.Errorf("result[1] opcode = %d, want OP_SEQUENCE", op2Code)
	}
	var seqRes2 types.SequenceRes
	_ = seqRes2.Decode(reader)
	if seqRes2.Status != types.NFS4ERR_SEQUENCE_POS {
		t.Errorf("result[1] SEQUENCE status = %d, want NFS4ERR_SEQUENCE_POS (%d)",
			seqRes2.Status, types.NFS4ERR_SEQUENCE_POS)
	}
}

func TestCompound_V41_SlotReleasedOnError(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// First request: SEQUENCE + ILLEGAL (fails, slot should be released after)
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_ILLEGAL},
	}
	data := buildCompoundArgsWithOps([]byte("err"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound #1 error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4ERR_OP_ILLEGAL {
		t.Fatalf("first request status = %d, want NFS4ERR_OP_ILLEGAL", status)
	}

	// Second request: SEQUENCE with next seqid (slot should be available)
	seqArgs2 := encodeSequenceArgs(sessionID, 0, 2, 0, true)
	ops2 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
		{opCode: types.OP_PUTROOTFH},
	}
	data2 := buildCompoundArgsWithOps([]byte("next"), 1, ops2)

	resp2, err := h.ProcessCompound(ctx, data2)
	if err != nil {
		t.Fatalf("ProcessCompound #2 error: %v", err)
	}

	decoded2, _ := decodeSequenceRes(t, resp2)
	if decoded2.Status != types.NFS4_OK {
		t.Errorf("second request status = %d, want NFS4_OK (slot should be released)", decoded2.Status)
	}
}

func TestCompound_V41_SequentialSlotUsage(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Send 3 sequential requests on slot 0 with incrementing seqids
	for seqid := uint32(1); seqid <= 3; seqid++ {
		seqArgs := encodeSequenceArgs(sessionID, 0, seqid, 0, true)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_PUTROOTFH},
		}
		data := buildCompoundArgsWithOps([]byte("multi"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound seqid=%d error: %v", seqid, err)
		}

		decoded, seqRes := decodeSequenceRes(t, resp)
		if decoded.Status != types.NFS4_OK {
			t.Errorf("seqid=%d status = %d, want NFS4_OK", seqid, decoded.Status)
		}
		if seqRes.SequenceID != seqid {
			t.Errorf("seqid=%d: response SequenceID = %d", seqid, seqRes.SequenceID)
		}
	}
}

func TestCompound_V41_MultipleSlots(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// Use slot 0 and slot 1 independently
	for _, slot := range []uint32{0, 1} {
		seqArgs := encodeSequenceArgs(sessionID, slot, 1, 1, true)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_PUTROOTFH},
		}
		data := buildCompoundArgsWithOps([]byte("multiSlot"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound slot=%d error: %v", slot, err)
		}

		decoded, seqRes := decodeSequenceRes(t, resp)
		if decoded.Status != types.NFS4_OK {
			t.Errorf("slot=%d status = %d, want NFS4_OK", slot, decoded.Status)
		}
		if seqRes.SlotID != slot {
			t.Errorf("slot=%d: response SlotID = %d", slot, seqRes.SlotID)
		}
	}
}

// ============================================================================
// Exempt Operations (Phase 18/19 regression tests with new dispatch)
// ============================================================================

func TestCompound_V41_ExemptOps_AllFour(t *testing.T) {
	// Verify all four exempt ops are accepted as first op without SEQUENCE.
	// EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, and BIND_CONN_TO_SESSION.
	tests := []struct {
		name   string
		opCode uint32
	}{
		{"EXCHANGE_ID", types.OP_EXCHANGE_ID},
		{"CREATE_SESSION", types.OP_CREATE_SESSION},
		{"DESTROY_SESSION", types.OP_DESTROY_SESSION},
		{"BIND_CONN_TO_SESSION", types.OP_BIND_CONN_TO_SESSION},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := isSessionExemptOp(tt.opCode)
			if !ok {
				t.Errorf("isSessionExemptOp(%s) = false, want true", tt.name)
			}
		})
	}
}

func TestCompound_V41_NonExemptOps(t *testing.T) {
	// Verify non-exempt ops are NOT marked as exempt
	tests := []struct {
		name   string
		opCode uint32
	}{
		{"PUTROOTFH", types.OP_PUTROOTFH},
		{"GETATTR", types.OP_GETATTR},
		{"READ", types.OP_READ},
		{"SEQUENCE", types.OP_SEQUENCE},
		{"RECLAIM_COMPLETE", types.OP_RECLAIM_COMPLETE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := isSessionExemptOp(tt.opCode)
			if ok {
				t.Errorf("isSessionExemptOp(%s) = true, want false", tt.name)
			}
		})
	}
}

// ============================================================================
// v4.0 Regression Tests
// ============================================================================

func TestCompound_V40_UnchangedByV41Dispatch(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// v4.0 COMPOUND with PUTROOTFH should still work
	data := buildCompoundArgs([]byte("v40"), 0, []uint32{types.OP_PUTROOTFH})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("v4.0 COMPOUND status = %d, want NFS4_OK (regression)", decoded.Status)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_PUTROOTFH {
		t.Errorf("result opcode = %d, want OP_PUTROOTFH", decoded.Results[0].OpCode)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("result status = %d, want NFS4_OK", decoded.Results[0].Status)
	}
}

// ============================================================================
// SkipOwnerSeqid Bypass Test
// ============================================================================

func TestCompound_V41_SkipOwnerSeqid(t *testing.T) {
	h, sessionID := createTestSession(t)

	// Install a test v4.0 handler that checks SkipOwnerSeqid
	var sawSkipOwnerSeqid bool
	h.opDispatchTable[types.OP_PUTROOTFH] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
		sawSkipOwnerSeqid = ctx.SkipOwnerSeqid
		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_PUTROOTFH,
			Data:   encodeStatusOnly(types.NFS4_OK),
		}
	}

	ctx := newTestCompoundContext()
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("skip"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}

	if !sawSkipOwnerSeqid {
		t.Error("SkipOwnerSeqid was not set to true for v4.0 handler called from v4.1 compound")
	}
}

// ============================================================================
// Empty v4.1 COMPOUND
// ============================================================================

func TestCompound_V41_EmptyCompound_StillWorks(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Empty v4.1 COMPOUND should succeed (no SEQUENCE required for 0 ops)
	data := buildCompoundArgs([]byte("v41-empty"), 1, nil)
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
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0", decoded.NumResults)
	}
}

// ============================================================================
// Op count limit
// ============================================================================

func TestCompound_V41_OpCountLimit(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Build COMPOUND with 129 ops (exceeds MaxCompoundOps=128)
	opcodes := make([]uint32, 129)
	for i := range opcodes {
		opcodes[i] = types.OP_PUTROOTFH
	}

	data := buildCompoundArgs([]byte("toolong"), 1, opcodes)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_RESOURCE {
		t.Errorf("status = %d, want NFS4ERR_RESOURCE (%d)",
			decoded.Status, types.NFS4ERR_RESOURCE)
	}
}

// ============================================================================
// Benchmarks (Phase 20-02)
// ============================================================================

func BenchmarkSequenceValidation(b *testing.B) {
	// Benchmark SEQUENCE validation throughput.
	// Creates a session and sends SEQUENCE ops in a loop with incrementing seqids.
	h := newTestHandler()
	clientID, seqID := registerExchangeIDBench(b, h, "bench-seq-client")
	sessionID := createTestSessionBench(b, h, clientID, seqID)

	ctx := newTestCompoundContext()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seqArgs := encodeSequenceArgs(sessionID, 0, uint32(i+1), 0, false)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
		}
		data := buildCompoundArgsWithOps([]byte("bench"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			b.Fatalf("ProcessCompound error: %v", err)
		}
		if resp == nil {
			b.Fatal("nil response")
		}
	}
}

func BenchmarkCompoundDispatch(b *testing.B) {
	// Benchmark full COMPOUND dispatch: SEQUENCE + PUTROOTFH.
	h := newTestHandler()
	clientID, seqID := registerExchangeIDBench(b, h, "bench-dispatch-client")
	sessionID := createTestSessionBench(b, h, clientID, seqID)

	ctx := newTestCompoundContext()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seqArgs := encodeSequenceArgs(sessionID, 0, uint32(i+1), 0, false)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_PUTROOTFH},
		}
		data := buildCompoundArgsWithOps([]byte("bench"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			b.Fatalf("ProcessCompound error: %v", err)
		}
		if resp == nil {
			b.Fatal("nil response")
		}
	}
}

func BenchmarkCompoundDispatch_V40(b *testing.B) {
	// Benchmark v4.0 COMPOUND dispatch for comparison: PUTROOTFH only.
	h := newTestHandler()
	ctx := newTestCompoundContext()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := buildCompoundArgs([]byte("bench-v40"), 0, []uint32{types.OP_PUTROOTFH})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			b.Fatalf("ProcessCompound error: %v", err)
		}
		if resp == nil {
			b.Fatal("nil response")
		}
	}
}

// registerExchangeIDBench is a benchmark-friendly version of registerExchangeID.
func registerExchangeIDBench(b *testing.B, h *Handler, ownerID string) (uint64, uint32) {
	b.Helper()
	ctx := newTestCompoundContext()

	ownerIDBytes := []byte(ownerID)
	var verifier [8]byte
	copy(verifier[:], "benchvrf")
	eidArgs := encodeExchangeIdArgs(ownerIDBytes, verifier, 0, types.SP4_NONE, nil)

	ops := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: eidArgs}}
	data := buildCompoundArgsWithOps([]byte("eid"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		b.Fatalf("EXCHANGE_ID ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		b.Fatalf("EXCHANGE_ID status = %d, want NFS4_OK", status)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var eidRes types.ExchangeIdRes
	if err := eidRes.Decode(reader); err != nil {
		b.Fatalf("decode ExchangeIdRes: %v", err)
	}
	// Return seqID+1: CREATE_SESSION must send record.SequenceID + 1
	return eidRes.ClientID, eidRes.SequenceID + 1
}

// createTestSessionBench is a benchmark-friendly version of createTestSession.
func createTestSessionBench(b *testing.B, h *Handler, clientID uint64, seqID uint32) types.SessionId4 {
	b.Helper()
	ctx := newTestCompoundContext()

	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("cs"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		b.Fatalf("CREATE_SESSION ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		b.Fatalf("CREATE_SESSION overall status = %d, want NFS4_OK", status)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var csRes types.CreateSessionRes
	if err := csRes.Decode(reader); err != nil {
		b.Fatalf("decode CreateSessionRes: %v", err)
	}
	if csRes.Status != types.NFS4_OK {
		b.Fatalf("CREATE_SESSION status = %d, want NFS4_OK", csRes.Status)
	}
	return csRes.SessionID
}

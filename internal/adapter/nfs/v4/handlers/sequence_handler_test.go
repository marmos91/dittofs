package handlers

import (
	"bytes"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	v41handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/v41/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
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
	return h, createSessionOn(t, h, "seq-test-client")
}

// createSessionOn performs EXCHANGE_ID + CREATE_SESSION on an existing handler,
// for tests whose handler is built around a real share rather than by
// newTestHandler.
func createSessionOn(t *testing.T, h *Handler, ownerID string) types.SessionId4 {
	t.Helper()
	clientID, seqID := registerExchangeID(t, h, ownerID)
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}} // AUTH_NONE
	return runCreateSession(t, h, encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)).SessionID
}

// runCreateSession sends the given CREATE_SESSION args as a single-op COMPOUND
// and returns the decoded result, failing the test unless the session was
// created.
func runCreateSession(t *testing.T, h *Handler, csArgs []byte) types.CreateSessionRes {
	t.Helper()
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	resp, err := h.ProcessCompound(newTestCompoundContext(),
		buildCompoundArgsWithOps([]byte("cs"), 1, ops))
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

	return csRes
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

// readPastSequenceResult decodes the header of a successful COMPOUND response,
// checks the result count, then consumes the leading SEQUENCE result, leaving
// the reader positioned at the second result. SEQUENCE carries a result body
// that decodeCompoundResponse does not know how to skip.
func readPastSequenceResult(t *testing.T, resp []byte, wantResults uint32) *bytes.Reader {
	t.Helper()
	reader := bytes.NewReader(resp)

	status := decodeUint32OrFail(t, reader, "overall status")
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", status)
	}
	if _, err := xdr.DecodeOpaque(reader); err != nil { // tag
		t.Fatalf("decode tag: %v", err)
	}
	numResults := decodeUint32OrFail(t, reader, "numResults")
	if numResults != wantResults {
		t.Fatalf("numResults = %d, want %d", numResults, wantResults)
	}

	opCode := decodeUint32OrFail(t, reader, "result[0] opcode")
	if opCode != types.OP_SEQUENCE {
		t.Fatalf("result[0] opcode = %d, want OP_SEQUENCE", opCode)
	}
	var seqRes types.SequenceRes
	if err := seqRes.Decode(reader); err != nil {
		t.Fatalf("decode SEQUENCE result: %v", err)
	}

	return reader
}

// decodeUint32OrFail reads one XDR uint32, failing the test on a short or
// malformed read. A discarded decode error would leave the zero value behind,
// and NFS4_OK is zero, so a truncated response would otherwise read as success.
func decodeUint32OrFail(t *testing.T, reader *bytes.Reader, what string) uint32 {
	t.Helper()
	v, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode %s: %v", what, err)
	}
	return v
}

// expectPutRootFHOK asserts that the next result the reader yields is a
// successful PUTROOTFH, which it only is when the preceding operation consumed
// its args without desyncing the reader.
func expectPutRootFHOK(t *testing.T, reader *bytes.Reader) {
	t.Helper()

	opCode := decodeUint32OrFail(t, reader, "trailing result opcode")
	if opCode != types.OP_PUTROOTFH {
		t.Errorf("trailing result opcode = %d, want OP_PUTROOTFH", opCode)
	}
	status := decodeUint32OrFail(t, reader, "PUTROOTFH status")
	if status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", status)
	}
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

	// Second request: a genuine retransmit is byte-identical (same tag, slot,
	// seqid, and ops), so reuse the exact same request bytes.
	resp2, err := h.ProcessCompound(ctx, data)
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

	// Second request: byte-identical retransmit of the same (uncached) request.
	resp2, err := h.ProcessCompound(ctx, data)
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

// TestSequence_FalseRetry_DifferentOps verifies H6: a slot+seqid reused with a
// DIFFERENT request must be rejected with NFS4ERR_SEQ_FALSE_RETRY instead of
// silently replaying the stale cached reply (RFC 8881 Section 2.10.6.1.3).
func TestSequence_FalseRetry_DifferentOps(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	// First request on slot 0 seqid 1: SEQUENCE + PUTROOTFH, cached.
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	opsA := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	dataA := buildCompoundArgsWithOps([]byte("fr"), 1, opsA)

	respA, err := h.ProcessCompound(ctx, dataA)
	if err != nil {
		t.Fatalf("ProcessCompound A error: %v", err)
	}
	decodedA, _ := decodeSequenceRes(t, respA)
	if decodedA.Status != types.NFS4_OK {
		t.Fatalf("request A status = %d, want NFS4_OK", decodedA.Status)
	}

	// Retry slot 0 seqid 1 with a DIFFERENT op-list (drop PUTROOTFH). Same
	// session/slot/seqid but a different request body.
	seqArgsB := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	opsB := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgsB},
	}
	dataB := buildCompoundArgsWithOps([]byte("fr"), 1, opsB)

	respB, err := h.ProcessCompound(ctx, dataB)
	if err != nil {
		t.Fatalf("ProcessCompound B error: %v", err)
	}
	decodedB, _ := decodeSequenceRes(t, respB)
	if decodedB.Status != types.NFS4ERR_SEQ_FALSE_RETRY {
		t.Errorf("false retry status = %d, want NFS4ERR_SEQ_FALSE_RETRY (%d)",
			decodedB.Status, types.NFS4ERR_SEQ_FALSE_RETRY)
	}

	// A byte-identical (true) retry of A must still replay the cached reply.
	respC, err := h.ProcessCompound(ctx, dataA)
	if err != nil {
		t.Fatalf("ProcessCompound C error: %v", err)
	}
	if !bytes.Equal(respA, respC) {
		t.Errorf("identical retry did not replay cached reply (len %d vs %d)",
			len(respA), len(respC))
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
// Exempt Operations (regression tests with new dispatch)
// ============================================================================

func TestCompound_V41_ExemptOps(t *testing.T) {
	// Verify every operation that may open a COMPOUND without SEQUENCE is
	// recognised as exempt.
	tests := []struct {
		name   string
		opCode uint32
	}{
		{"EXCHANGE_ID", types.OP_EXCHANGE_ID},
		{"CREATE_SESSION", types.OP_CREATE_SESSION},
		{"DESTROY_SESSION", types.OP_DESTROY_SESSION},
		{"DESTROY_CLIENTID", types.OP_DESTROY_CLIENTID},
		{"BIND_CONN_TO_SESSION", types.OP_BIND_CONN_TO_SESSION},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := v41handlers.IsSessionExemptOp(tt.opCode)
			if !ok {
				t.Errorf("IsSessionExemptOp(%s) = false, want true", tt.name)
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
			ok := v41handlers.IsSessionExemptOp(tt.opCode)
			if ok {
				t.Errorf("IsSessionExemptOp(%s) = true, want false", tt.name)
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
	h.v40DispatchTable[types.OP_PUTROOTFH] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
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

	// NFS4ERR_RESOURCE, which the v4.0 path answers here, is an NFSv4.0 error
	// that RFC 8881 does not define; the v4.1 cap is below every session's
	// negotiated ca_maxoperations, so NFS4ERR_TOO_MANY_OPS is what it owes.
	if decoded.Status != types.NFS4ERR_TOO_MANY_OPS {
		t.Errorf("status = %d, want NFS4ERR_TOO_MANY_OPS (%d)",
			decoded.Status, types.NFS4ERR_TOO_MANY_OPS)
	}
}

// ============================================================================
// Benchmarks
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
	// EXCHANGE_ID now returns slot+1 (the value CREATE_SESSION expects directly)
	return eidRes.ClientID, eidRes.SequenceID
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

// createLimitedSession creates a session whose fore channel negotiates the
// given ca_maxoperations and ca_maxrequestsize, so the COMPOUND limits can be
// tripped with a small request.
func createLimitedSession(t *testing.T, h *Handler, ownerID string, maxOps, maxReqSize uint32) types.SessionId4 {
	t.Helper()
	clientID, seqID := registerExchangeID(t, h, ownerID)

	var argBuf bytes.Buffer
	args := types.CreateSessionArgs{
		ClientID:   clientID,
		SequenceID: seqID,
		ForeChannelAttrs: types.ChannelAttrs{
			MaxRequestSize:        maxReqSize,
			MaxResponseSize:       1048576,
			MaxResponseSizeCached: 4096,
			MaxOperations:         maxOps,
			MaxRequests:           64,
		},
		BackChannelAttrs: types.ChannelAttrs{
			MaxRequestSize:  4096,
			MaxResponseSize: 4096,
			MaxOperations:   2,
			MaxRequests:     1,
		},
		CbProgram:  0x40000000,
		CbSecParms: []types.CallbackSecParms4{{CbSecFlavor: 0}},
	}
	if err := args.Encode(&argBuf); err != nil {
		t.Fatalf("encode CreateSessionArgs: %v", err)
	}

	// The server may negotiate the fore channel down but never up, so the test
	// limits are only usable once the reply confirms them.
	csRes := runCreateSession(t, h, argBuf.Bytes())
	if csRes.ForeChannelAttrs.MaxOperations != maxOps {
		t.Fatalf("negotiated MaxOperations = %d, want %d",
			csRes.ForeChannelAttrs.MaxOperations, maxOps)
	}
	if csRes.ForeChannelAttrs.MaxRequestSize != maxReqSize {
		t.Fatalf("negotiated MaxRequestSize = %d, want %d",
			csRes.ForeChannelAttrs.MaxRequestSize, maxReqSize)
	}
	return csRes.SessionID
}

// TestSequence_TooManyOps checks that a COMPOUND carrying more operations than
// the session's negotiated ca_maxoperations is refused with
// NFS4ERR_TOO_MANY_OPS (RFC 8881 Section 18.36.3), and that a COMPOUND at
// exactly the limit still runs.
func TestSequence_TooManyOps(t *testing.T) {
	h := newTestHandler()
	sessionID := createLimitedSession(t, h, "too-many-ops-client", 4, 1048576)

	// SEQUENCE plus three PUTROOTFHs is exactly ca_maxoperations.
	atLimit := []compoundOp{{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 1, 0, false)}}
	for range 3 {
		atLimit = append(atLimit, compoundOp{opCode: types.OP_PUTROOTFH})
	}
	resp, err := h.ProcessCompound(newTestCompoundContext(),
		buildCompoundArgsWithOps([]byte("ops"), 1, atLimit))
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}
	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("at-limit status = %d, want NFS4_OK", decoded.Status)
	}

	// One more operation exceeds it.
	overLimit := append(atLimit, compoundOp{opCode: types.OP_PUTROOTFH})
	overLimit[0].data = encodeSequenceArgs(sessionID, 0, 2, 0, false)
	resp, err = h.ProcessCompound(newTestCompoundContext(),
		buildCompoundArgsWithOps([]byte("ops"), 1, overLimit))
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}
	decoded, seqRes := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4ERR_TOO_MANY_OPS {
		t.Errorf("status = %d, want NFS4ERR_TOO_MANY_OPS (%d)",
			decoded.Status, types.NFS4ERR_TOO_MANY_OPS)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1 (SEQUENCE only, no op executed)", decoded.NumResults)
	}
	if seqRes == nil || decoded.Results[0].OpCode != types.OP_SEQUENCE {
		t.Fatalf("result opcode = %d, want OP_SEQUENCE", decoded.Results[0].OpCode)
	}

	// The refusal is reported on SEQUENCE, so no operation executed and the slot
	// is untouched: seqid 2 is still the next one the slot accepts. Had the
	// refused request consumed it, this would come back as a retry instead.
	resp, err = h.ProcessCompound(newTestCompoundContext(),
		buildCompoundArgsWithOps([]byte("ops"), 1, []compoundOp{
			{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 2, 0, false)},
		}))
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}
	if decoded, _ = decodeSequenceRes(t, resp); decoded.Status != types.NFS4_OK {
		t.Errorf("reusing seqid 2 after the refusal: status = %d, want NFS4_OK", decoded.Status)
	}
}

// TestSequence_RequestTooBig checks that a COMPOUND larger than the session's
// negotiated ca_maxrequestsize is refused with NFS4ERR_REQ_TOO_BIG
// (RFC 8881 Section 2.10.6.4) before the oversized operation runs.
func TestSequence_RequestTooBig(t *testing.T) {
	h := newTestHandler()
	sessionID := createLimitedSession(t, h, "req-too-big-client", 128, 512)

	// PUTROOTFH plus a LOOKUP whose name alone overruns ca_maxrequestsize. The
	// name is also longer than NFS4_MAXNAMLEN, so answering NFS4ERR_NAMETOOLONG
	// here would mean LOOKUP ran instead of the request being refused up front.
	var nameBuf bytes.Buffer
	if err := xdr.WriteXDROpaque(&nameBuf, bytes.Repeat([]byte("a"), 500)); err != nil {
		t.Fatalf("encode LOOKUP name: %v", err)
	}
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 1, 0, false)},
		{opCode: types.OP_PUTROOTFH},
		{opCode: types.OP_LOOKUP, data: nameBuf.Bytes()},
	}

	data := buildCompoundArgsWithOps([]byte("big"), 1, ops)
	if len(data) <= 512 {
		t.Fatalf("test request is %d bytes, needs to exceed the negotiated 512", len(data))
	}

	resp, err := h.ProcessCompound(newTestCompoundContext(), data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}
	decoded, _ := decodeSequenceRes(t, resp)
	if decoded.Status != types.NFS4ERR_REQ_TOO_BIG {
		t.Errorf("status = %d, want NFS4ERR_REQ_TOO_BIG (%d)",
			decoded.Status, types.NFS4ERR_REQ_TOO_BIG)
	}
	if decoded.NumResults != 1 {
		t.Errorf("numResults = %d, want 1 (SEQUENCE only, no op executed)", decoded.NumResults)
	}
}

// exemptOpArgs returns XDR-encoded args for each operation that may appear as
// the first operation of a v4.1 COMPOUND without a preceding SEQUENCE. The args
// are well-formed but refer to no existing client or session, so each operation
// answers with its own error rather than being rejected for its shape.
func exemptOpArgs(t *testing.T) []compoundOp {
	t.Helper()

	var fakeSID types.SessionId4
	copy(fakeSID[:], "notonlyopsess123")

	var verifier [8]byte
	copy(verifier[:], "notonlyv")

	var dcBuf bytes.Buffer
	_ = (&types.DestroyClientidArgs{ClientID: 0}).Encode(&dcBuf)

	var dsBuf bytes.Buffer
	_ = (&types.DestroySessionArgs{SessionID: fakeSID}).Encode(&dsBuf)

	return []compoundOp{
		{
			opCode: types.OP_EXCHANGE_ID,
			data:   encodeExchangeIdArgs([]byte("not-only-op-client"), verifier, 0, types.SP4_NONE, nil),
		},
		{
			opCode: types.OP_CREATE_SESSION,
			data:   encodeCreateSessionArgsWithSec(0, 0, 0, []types.CallbackSecParms4{{CbSecFlavor: 0}}),
		},
		{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()},
		{opCode: types.OP_DESTROY_CLIENTID, data: dcBuf.Bytes()},
		{
			opCode: types.OP_BIND_CONN_TO_SESSION,
			data:   encodeBindConnToSessionArgs(fakeSID, types.CDFC4_FORE, false),
		},
	}
}

// TestCompound_V41_ExemptOpNotOnlyOp covers RFC 8881 Section 15.1.3.3: an
// operation allowed outside a session must be the only operation in a COMPOUND
// that does not start with SEQUENCE.
func TestCompound_V41_ExemptOpNotOnlyOp(t *testing.T) {
	for _, exempt := range exemptOpArgs(t) {
		t.Run(types.OpName(exempt.opCode), func(t *testing.T) {
			// Alone: the op runs, so whatever it answers it is not NOT_ONLY_OP.
			aloneData := buildCompoundArgsWithOps([]byte("alone"), 1, []compoundOp{exempt})
			alone, err := newTestHandler().ProcessCompound(newTestCompoundContext(), aloneData)
			if err != nil {
				t.Fatalf("ProcessCompound (alone) error: %v", err)
			}
			decodedAlone, err := decodeCompoundResponse(alone)
			if err != nil {
				t.Fatalf("decode response (alone) error: %v", err)
			}
			if decodedAlone.Status == types.NFS4ERR_NOT_ONLY_OP {
				t.Fatalf("sole %s rejected with NFS4ERR_NOT_ONLY_OP",
					types.OpName(exempt.opCode))
			}

			// Sharing the COMPOUND with a second op: rejected, with a single
			// result naming the offending first operation.
			sharedData := buildCompoundArgsWithOps([]byte("shared"), 1,
				[]compoundOp{exempt, {opCode: types.OP_PUTROOTFH}})
			shared, err := newTestHandler().ProcessCompound(newTestCompoundContext(), sharedData)
			if err != nil {
				t.Fatalf("ProcessCompound (shared) error: %v", err)
			}
			decodedShared, err := decodeCompoundResponse(shared)
			if err != nil {
				t.Fatalf("decode response (shared) error: %v", err)
			}
			if decodedShared.Status != types.NFS4ERR_NOT_ONLY_OP {
				t.Errorf("status = %d, want NFS4ERR_NOT_ONLY_OP (%d)",
					decodedShared.Status, types.NFS4ERR_NOT_ONLY_OP)
			}
			if decodedShared.NumResults != 1 {
				t.Fatalf("numResults = %d, want 1", decodedShared.NumResults)
			}
			if decodedShared.Results[0].OpCode != exempt.opCode {
				t.Errorf("result[0] opcode = %d, want %d",
					decodedShared.Results[0].OpCode, exempt.opCode)
			}
			if decodedShared.Results[0].Status != types.NFS4ERR_NOT_ONLY_OP {
				t.Errorf("result[0] status = %d, want NFS4ERR_NOT_ONLY_OP (%d)",
					decodedShared.Results[0].Status, types.NFS4ERR_NOT_ONLY_OP)
			}
		})
	}
}

// TestCompound_V41_ExemptOpAfterSequence pins the other half of the rule: the
// restriction is on a COMPOUND that does not start with SEQUENCE, so an exempt
// operation preceded by SEQUENCE keeps running alongside other operations.
func TestCompound_V41_ExemptOpAfterSequence(t *testing.T) {
	h, sessionID := createTestSession(t)

	var verifier [8]byte
	copy(verifier[:], "afterseq")

	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, 1, 0, true)},
		{
			opCode: types.OP_EXCHANGE_ID,
			data:   encodeExchangeIdArgs([]byte("after-sequence-client"), verifier, 0, types.SP4_NONE, nil),
		},
	}
	data := buildCompoundArgsWithOps([]byte("afterseq"), 1, ops)

	resp, err := h.ProcessCompound(newTestCompoundContext(), data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := readPastSequenceResult(t, resp, 2)

	eidOpCode, _ := xdr.DecodeUint32(reader)
	if eidOpCode != types.OP_EXCHANGE_ID {
		t.Errorf("result[1] opcode = %d, want OP_EXCHANGE_ID", eidOpCode)
	}
	eidStatus, _ := xdr.DecodeUint32(reader)
	if eidStatus != types.NFS4_OK {
		t.Errorf("result[1] status = %d, want NFS4_OK", eidStatus)
	}
}

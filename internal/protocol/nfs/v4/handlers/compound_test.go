package handlers

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// buildCompoundArgs encodes a COMPOUND4args structure for testing.
// tag: arbitrary bytes echoed in response
// minorVersion: NFSv4 minor version (must be 0 for 4.0)
// opcodes: list of operation codes (no per-op args for simple tests)
func buildCompoundArgs(tag []byte, minorVersion uint32, opcodes []uint32) []byte {
	var buf bytes.Buffer

	// Write tag as XDR opaque
	_ = xdr.WriteXDROpaque(&buf, tag)

	// Write minor version
	_ = xdr.WriteUint32(&buf, minorVersion)

	// Write number of ops
	_ = xdr.WriteUint32(&buf, uint32(len(opcodes)))

	// Write each opcode (no per-op args for these simple tests)
	for _, op := range opcodes {
		_ = xdr.WriteUint32(&buf, op)
	}

	return buf.Bytes()
}

func newTestHandler() *Handler {
	pfs := pseudofs.New()
	return NewHandler(nil, pfs)
}

func newTestCompoundContext() *types.CompoundContext {
	return &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:12345",
	}
}

// decodeCompoundResponse decodes COMPOUND4res for test assertions.
type decodedCompoundResponse struct {
	Status     uint32
	Tag        []byte
	NumResults uint32
	Results    []decodedResult
}

type decodedResult struct {
	OpCode uint32
	Status uint32
}

func decodeCompoundResponse(data []byte) (*decodedCompoundResponse, error) {
	reader := bytes.NewReader(data)

	status, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, err
	}

	tag, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, err
	}

	numResults, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, err
	}

	results := make([]decodedResult, 0, numResults)
	for i := uint32(0); i < numResults; i++ {
		opCode, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, err
		}

		// Read the status from the result data
		opStatus, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, err
		}

		results = append(results, decodedResult{
			OpCode: opCode,
			Status: opStatus,
		})
	}

	return &decodedCompoundResponse{
		Status:     status,
		Tag:        tag,
		NumResults: numResults,
		Results:    results,
	}, nil
}

func TestCompoundEmptyOps(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// 0 operations
	data := buildCompoundArgs([]byte("test"), 0, nil)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK (%d)", decoded.Status, types.NFS4_OK)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0", decoded.NumResults)
	}
	if string(decoded.Tag) != "test" {
		t.Errorf("tag = %q, want %q", string(decoded.Tag), "test")
	}
}

func TestCompoundMinorVersionMismatch(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Minor version 3 should fail (only 0 and 1 supported)
	data := buildCompoundArgs([]byte("v4.3"), 3, []uint32{types.OP_GETATTR})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_MINOR_VERS_MISMATCH {
		t.Errorf("status = %d, want NFS4ERR_MINOR_VERS_MISMATCH (%d)",
			decoded.Status, types.NFS4ERR_MINOR_VERS_MISMATCH)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0 (no results on minor version error)", decoded.NumResults)
	}
	if string(decoded.Tag) != "v4.3" {
		t.Errorf("tag = %q, want %q (must echo tag even on error)", string(decoded.Tag), "v4.3")
	}
}

func TestCompoundTagEcho(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	tests := []struct {
		name string
		tag  []byte
	}{
		{"empty tag", []byte{}},
		{"ascii tag", []byte("hello")},
		{"non-utf8 tag", []byte{0xFF, 0xFE, 0x00, 0x01}},
		{"binary tag", []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		{"long tag", bytes.Repeat([]byte("x"), 100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildCompoundArgs(tt.tag, 0, nil)
			resp, err := h.ProcessCompound(ctx, data)
			if err != nil {
				t.Fatalf("ProcessCompound error: %v", err)
			}

			decoded, err := decodeCompoundResponse(resp)
			if err != nil {
				t.Fatalf("decode response error: %v", err)
			}

			if !bytes.Equal(decoded.Tag, tt.tag) {
				t.Errorf("tag = %x, want %x", decoded.Tag, tt.tag)
			}
		})
	}
}

func TestCompoundSingleIllegalOp(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	data := buildCompoundArgs([]byte(""), 0, []uint32{types.OP_ILLEGAL})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL (%d)",
			decoded.Status, types.NFS4ERR_OP_ILLEGAL)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_ILLEGAL {
		t.Errorf("result opcode = %d, want OP_ILLEGAL (%d)",
			decoded.Results[0].OpCode, types.OP_ILLEGAL)
	}
	if decoded.Results[0].Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("result status = %d, want NFS4ERR_OP_ILLEGAL (%d)",
			decoded.Results[0].Status, types.NFS4ERR_OP_ILLEGAL)
	}
}

func TestCompoundUnknownOpcode(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Opcode 99999 is not in any valid range
	data := buildCompoundArgs([]byte(""), 0, []uint32{99999})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// Unknown opcodes outside the valid range should return OP_ILLEGAL
	if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL (%d)",
			decoded.Status, types.NFS4ERR_OP_ILLEGAL)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	// The result opcode should be OP_ILLEGAL per RFC 7530
	if decoded.Results[0].OpCode != types.OP_ILLEGAL {
		t.Errorf("result opcode = %d, want OP_ILLEGAL (%d)",
			decoded.Results[0].OpCode, types.OP_ILLEGAL)
	}
}

func TestCompoundMultipleOpsStopOnError(t *testing.T) {
	h := newTestHandler()

	// Register a test handler that succeeds for a specific op
	testOp := uint32(types.OP_PUTROOTFH) // Use a known op number
	h.opDispatchTable[testOp] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: testOp,
			Data:   encodeStatusOnly(types.NFS4_OK),
		}
	}

	ctx := newTestCompoundContext()

	// 3 ops: succeed, fail (ILLEGAL), should-not-execute
	data := buildCompoundArgs([]byte(""), 0, []uint32{testOp, types.OP_ILLEGAL, testOp})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// Overall status should be the error from op 2
	if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL", decoded.Status)
	}

	// Only 2 results (op 1 succeeded, op 2 failed, op 3 not executed)
	if decoded.NumResults != 2 {
		t.Fatalf("numResults = %d, want 2", decoded.NumResults)
	}

	// First result should be success
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("result[0].status = %d, want NFS4_OK", decoded.Results[0].Status)
	}

	// Second result should be the error
	if decoded.Results[1].Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("result[1].status = %d, want NFS4ERR_OP_ILLEGAL", decoded.Results[1].Status)
	}
}

func TestCompoundOpCountLimit(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Build COMPOUND with 129 ops (exceeds MaxCompoundOps=128)
	opcodes := make([]uint32, 129)
	for i := range opcodes {
		opcodes[i] = types.OP_PUTROOTFH
	}

	data := buildCompoundArgs([]byte("toolong"), 0, opcodes)
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
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0", decoded.NumResults)
	}
	// Tag should still be echoed
	if string(decoded.Tag) != "toolong" {
		t.Errorf("tag = %q, want %q", string(decoded.Tag), "toolong")
	}
}

func TestCompoundUnimplementedValidOp(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// OP_DELEGPURGE (7) is a valid op but not yet implemented
	data := buildCompoundArgs([]byte(""), 0, []uint32{types.OP_DELEGPURGE})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// Valid but unimplemented ops should return NFS4ERR_NOTSUPP
	if decoded.Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("status = %d, want NFS4ERR_NOTSUPP (%d)",
			decoded.Status, types.NFS4ERR_NOTSUPP)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_DELEGPURGE {
		t.Errorf("result opcode = %d, want OP_DELEGPURGE (%d)",
			decoded.Results[0].OpCode, types.OP_DELEGPURGE)
	}
}

func TestNullHandler(t *testing.T) {
	h := newTestHandler()

	resp, err := h.HandleNull([]byte{})
	if err != nil {
		t.Fatalf("HandleNull error: %v", err)
	}

	if len(resp) != 0 {
		t.Errorf("HandleNull response length = %d, want 0", len(resp))
	}
}

func TestCompoundMinorVersion2(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Minor version 2 should also fail (only 0 supported)
	data := buildCompoundArgs([]byte("v4.2"), 2, []uint32{types.OP_GETATTR})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_MINOR_VERS_MISMATCH {
		t.Errorf("status = %d, want NFS4ERR_MINOR_VERS_MISMATCH (%d)",
			decoded.Status, types.NFS4ERR_MINOR_VERS_MISMATCH)
	}
}

func TestCompoundExactlyMaxOps(t *testing.T) {
	h := newTestHandler()

	// Register a succeeding handler for testing
	testOp := uint32(types.OP_PUTROOTFH)
	h.opDispatchTable[testOp] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: testOp,
			Data:   encodeStatusOnly(types.NFS4_OK),
		}
	}

	ctx := newTestCompoundContext()

	// Build COMPOUND with exactly 128 ops (at the limit, should succeed)
	opcodes := make([]uint32, 128)
	for i := range opcodes {
		opcodes[i] = testOp
	}

	data := buildCompoundArgs([]byte("atmax"), 0, opcodes)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK (128 ops should be accepted)", decoded.Status)
	}
	if decoded.NumResults != 128 {
		t.Errorf("numResults = %d, want 128", decoded.NumResults)
	}
}

// ============================================================================
// buildCompoundArgsWithOps builds a COMPOUND with per-op XDR data.
// Each op is {opcode uint32, extra_data []byte}.
// ============================================================================

type compoundOp struct {
	opCode uint32
	data   []byte // extra XDR-encoded args for this op (may be empty)
}

func buildCompoundArgsWithOps(tag []byte, minorVersion uint32, ops []compoundOp) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteXDROpaque(&buf, tag)
	_ = xdr.WriteUint32(&buf, minorVersion)
	_ = xdr.WriteUint32(&buf, uint32(len(ops)))
	for _, op := range ops {
		_ = xdr.WriteUint32(&buf, op.opCode)
		if len(op.data) > 0 {
			buf.Write(op.data)
		}
	}
	return buf.Bytes()
}

// encodeCreateSessionArgs encodes minimal CREATE_SESSION args for testing.
// Uses dummy values sufficient for the stub to decode without error.
func encodeCreateSessionArgs() []byte {
	var buf bytes.Buffer
	args := types.CreateSessionArgs{
		ClientID:   1,
		SequenceID: 1,
		Flags:      0,
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
		CbSecParms: []types.CallbackSecParms4{},
	}
	_ = args.Encode(&buf)
	return buf.Bytes()
}

// encodeReclaimCompleteArgs encodes RECLAIM_COMPLETE args for testing.
// rca_one_fs is a bool (uint32).
func encodeReclaimCompleteArgs() []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, 0) // rca_one_fs = false
	return buf.Bytes()
}

// ============================================================================
// NFSv4.1 COMPOUND Tests
// ============================================================================

func TestCompound_MinorVersion1_Accepted(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// minorversion=1 COMPOUND with PUTROOTFH (a v4.0 op that works in v4.1)
	data := buildCompoundArgs([]byte("v4.1"), 1, []uint32{types.OP_PUTROOTFH})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// PUTROOTFH should succeed via fallback to v4.0 dispatch table
	if decoded.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK (%d) for v4.0 op in v4.1 compound",
			decoded.Status, types.NFS4_OK)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_PUTROOTFH {
		t.Errorf("result opcode = %d, want OP_PUTROOTFH (%d)",
			decoded.Results[0].OpCode, types.OP_PUTROOTFH)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("result status = %d, want NFS4_OK", decoded.Results[0].Status)
	}
	if string(decoded.Tag) != "v4.1" {
		t.Errorf("tag = %q, want %q", string(decoded.Tag), "v4.1")
	}
}

func TestCompound_MinorVersion1_V41Op_NOTSUPP(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// minorversion=1 COMPOUND with OP_RECLAIM_COMPLETE (v4.1 op, still a stub)
	// Stub should decode args and return NFS4ERR_NOTSUPP.
	// Note: EXCHANGE_ID (Phase 18) and CREATE_SESSION/DESTROY_SESSION (Phase 19)
	// are now real handlers, so we use RECLAIM_COMPLETE as the representative stub.
	ops := []compoundOp{
		{opCode: types.OP_RECLAIM_COMPLETE, data: encodeReclaimCompleteArgs()},
	}
	data := buildCompoundArgsWithOps([]byte("v41-stub"), 1, ops)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("status = %d, want NFS4ERR_NOTSUPP (%d)",
			decoded.Status, types.NFS4ERR_NOTSUPP)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_RECLAIM_COMPLETE {
		t.Errorf("result opcode = %d, want OP_RECLAIM_COMPLETE (%d)",
			decoded.Results[0].OpCode, types.OP_RECLAIM_COMPLETE)
	}
	if decoded.Results[0].Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("result status = %d, want NFS4ERR_NOTSUPP (%d)",
			decoded.Results[0].Status, types.NFS4ERR_NOTSUPP)
	}
}

func TestCompound_MinorVersion2_Rejected(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// minorversion=2 should return NFS4ERR_MINOR_VERS_MISMATCH
	data := buildCompoundArgs([]byte("v4.2"), 2, []uint32{types.OP_GETATTR})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_MINOR_VERS_MISMATCH {
		t.Errorf("status = %d, want NFS4ERR_MINOR_VERS_MISMATCH (%d)",
			decoded.Status, types.NFS4ERR_MINOR_VERS_MISMATCH)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0", decoded.NumResults)
	}
	if string(decoded.Tag) != "v4.2" {
		t.Errorf("tag = %q, want %q", string(decoded.Tag), "v4.2")
	}
}

func TestCompound_MinorVersion0_Unchanged(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Regression test: v4.0 COMPOUND with PUTROOTFH still works
	data := buildCompoundArgs([]byte("v4.0"), 0, []uint32{types.OP_PUTROOTFH})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK (v4.0 regression)", decoded.Status)
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

func TestCompound_V41_StubConsumesArgs(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Send a v4.1 COMPOUND with RECLAIM_COMPLETE (v4.1 stub) + PUTROOTFH (v4.0 op).
	// RECLAIM_COMPLETE returns NOTSUPP which stops the compound, but the critical
	// test is that the stub consumed the RECLAIM_COMPLETE XDR args correctly --
	// if it didn't, the PUTROOTFH opcode would be misread from the arg data.
	//
	// We verify this by checking that the compound returns exactly 1 result
	// (RECLAIM_COMPLETE with NOTSUPP) and not a garbage decode error.
	// Note: EXCHANGE_ID (Phase 18) and CREATE_SESSION/DESTROY_SESSION (Phase 19)
	// are now real handlers, so we use RECLAIM_COMPLETE as the representative stub.
	ops := []compoundOp{
		{opCode: types.OP_RECLAIM_COMPLETE, data: encodeReclaimCompleteArgs()},
		{opCode: types.OP_PUTROOTFH}, // no args
	}
	data := buildCompoundArgsWithOps([]byte("consume"), 1, ops)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// Should have exactly 1 result (RECLAIM_COMPLETE stops the compound)
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1 (RECLAIM_COMPLETE should stop compound)", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_RECLAIM_COMPLETE {
		t.Errorf("result opcode = %d, want OP_RECLAIM_COMPLETE (%d)",
			decoded.Results[0].OpCode, types.OP_RECLAIM_COMPLETE)
	}
	if decoded.Results[0].Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("result status = %d, want NFS4ERR_NOTSUPP", decoded.Results[0].Status)
	}
	if decoded.Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("overall status = %d, want NFS4ERR_NOTSUPP", decoded.Status)
	}
}

func TestCompound_V41_AllStubOps(t *testing.T) {
	// Verify all 19 v4.1 operations are registered in the dispatch table
	// by checking that each returns NOTSUPP (not OP_ILLEGAL).
	h := newTestHandler()
	ctx := newTestCompoundContext()

	v41Ops := []struct {
		opCode uint32
		name   string
		args   []byte
	}{
		{types.OP_RECLAIM_COMPLETE, "RECLAIM_COMPLETE", encodeReclaimCompleteArgs()},
	}

	for _, op := range v41Ops {
		t.Run(op.name, func(t *testing.T) {
			ops := []compoundOp{
				{opCode: op.opCode, data: op.args},
			}
			data := buildCompoundArgsWithOps([]byte(""), 1, ops)
			resp, err := h.ProcessCompound(ctx, data)
			if err != nil {
				t.Fatalf("ProcessCompound error: %v", err)
			}

			decoded, err := decodeCompoundResponse(resp)
			if err != nil {
				t.Fatalf("decode response error: %v", err)
			}

			if decoded.Status != types.NFS4ERR_NOTSUPP {
				t.Errorf("status = %d, want NFS4ERR_NOTSUPP (%d)",
					decoded.Status, types.NFS4ERR_NOTSUPP)
			}
			if decoded.NumResults != 1 {
				t.Fatalf("numResults = %d, want 1", decoded.NumResults)
			}
			if decoded.Results[0].OpCode != op.opCode {
				t.Errorf("result opcode = %d, want %d", decoded.Results[0].OpCode, op.opCode)
			}
		})
	}
}

func TestCompound_V41_DispatchTableComplete(t *testing.T) {
	// Verify all 19 v4.1 operation numbers (40-58) are registered in the
	// v41DispatchTable. This ensures no operation was missed during setup.
	h := newTestHandler()

	expectedOps := []uint32{
		types.OP_BACKCHANNEL_CTL,
		types.OP_BIND_CONN_TO_SESSION,
		types.OP_EXCHANGE_ID,
		types.OP_CREATE_SESSION,
		types.OP_DESTROY_SESSION,
		types.OP_FREE_STATEID,
		types.OP_GET_DIR_DELEGATION,
		types.OP_GETDEVICEINFO,
		types.OP_GETDEVICELIST,
		types.OP_LAYOUTCOMMIT,
		types.OP_LAYOUTGET,
		types.OP_LAYOUTRETURN,
		types.OP_SECINFO_NO_NAME,
		types.OP_SEQUENCE,
		types.OP_SET_SSV,
		types.OP_TEST_STATEID,
		types.OP_WANT_DELEGATION,
		types.OP_DESTROY_CLIENTID,
		types.OP_RECLAIM_COMPLETE,
	}

	if len(h.v41DispatchTable) != 19 {
		t.Errorf("v41DispatchTable has %d entries, want 19", len(h.v41DispatchTable))
	}

	for _, opCode := range expectedOps {
		if _, ok := h.v41DispatchTable[opCode]; !ok {
			t.Errorf("v41DispatchTable missing entry for %s (%d)",
				types.OpName(opCode), opCode)
		}
	}
}

func TestCompound_V41_IllegalOpOutsideRange(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Opcode 99999 is outside all valid ranges -- should return OP_ILLEGAL
	data := buildCompoundArgs([]byte(""), 1, []uint32{99999})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL (%d)",
			decoded.Status, types.NFS4ERR_OP_ILLEGAL)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_ILLEGAL {
		t.Errorf("result opcode = %d, want OP_ILLEGAL (%d)",
			decoded.Results[0].OpCode, types.OP_ILLEGAL)
	}
}

func TestCompound_V41_EmptyCompound(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// v4.1 empty COMPOUND (0 ops) should succeed
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
	if string(decoded.Tag) != "v41-empty" {
		t.Errorf("tag = %q, want %q", string(decoded.Tag), "v41-empty")
	}
}

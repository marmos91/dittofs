package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"
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

	// After Phase 20: v4.1 COMPOUND with PUTROOTFH (non-exempt, no SEQUENCE)
	// must return NFS4ERR_OP_NOT_IN_SESSION per RFC 8881.
	data := buildCompoundArgs([]byte("v4.1"), 1, []uint32{types.OP_PUTROOTFH})
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	// Non-exempt v4.0 op without SEQUENCE returns NFS4ERR_OP_NOT_IN_SESSION
	if decoded.Status != types.NFS4ERR_OP_NOT_IN_SESSION {
		t.Errorf("status = %d, want NFS4ERR_OP_NOT_IN_SESSION (%d) for v4.0 op in v4.1 compound without SEQUENCE",
			decoded.Status, types.NFS4ERR_OP_NOT_IN_SESSION)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0 (no results on missing SEQUENCE)", decoded.NumResults)
	}
	if string(decoded.Tag) != "v4.1" {
		t.Errorf("tag = %q, want %q", string(decoded.Tag), "v4.1")
	}
}

func TestCompound_MinorVersion1_V41Op_NOTSUPP(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// After Phase 20: RECLAIM_COMPLETE is not exempt from SEQUENCE.
	// Without SEQUENCE, the COMPOUND should return NFS4ERR_OP_NOT_IN_SESSION.
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

	if decoded.Status != types.NFS4ERR_OP_NOT_IN_SESSION {
		t.Errorf("status = %d, want NFS4ERR_OP_NOT_IN_SESSION (%d)",
			decoded.Status, types.NFS4ERR_OP_NOT_IN_SESSION)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0 (no results without SEQUENCE)", decoded.NumResults)
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

	// After Phase 20: RECLAIM_COMPLETE is not exempt from SEQUENCE.
	// Without SEQUENCE, the COMPOUND returns NFS4ERR_OP_NOT_IN_SESSION.
	// The stub arg consumption test is now covered by the SEQUENCE-gated tests.
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

	// Non-exempt op without SEQUENCE returns NFS4ERR_OP_NOT_IN_SESSION
	if decoded.Status != types.NFS4ERR_OP_NOT_IN_SESSION {
		t.Errorf("overall status = %d, want NFS4ERR_OP_NOT_IN_SESSION (%d)",
			decoded.Status, types.NFS4ERR_OP_NOT_IN_SESSION)
	}
	if decoded.NumResults != 0 {
		t.Errorf("numResults = %d, want 0 (no results without SEQUENCE)", decoded.NumResults)
	}
}

func TestCompound_V41_AllStubOps(t *testing.T) {
	// After Phase 20: non-exempt v4.1 ops without SEQUENCE return
	// NFS4ERR_OP_NOT_IN_SESSION. The dispatch table completeness
	// is tested in TestCompound_V41_DispatchTableComplete.
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

			// Non-exempt op without SEQUENCE returns NFS4ERR_OP_NOT_IN_SESSION
			if decoded.Status != types.NFS4ERR_OP_NOT_IN_SESSION {
				t.Errorf("status = %d, want NFS4ERR_OP_NOT_IN_SESSION (%d)",
					decoded.Status, types.NFS4ERR_OP_NOT_IN_SESSION)
			}
			if decoded.NumResults != 0 {
				t.Errorf("numResults = %d, want 0", decoded.NumResults)
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

	// After Phase 20: opcode 99999 is not exempt from SEQUENCE.
	// The first op check sees it's not exempt and not SEQUENCE, so
	// NFS4ERR_OP_NOT_IN_SESSION is returned before opcode validation.
	data := buildCompoundArgs([]byte(""), 1, []uint32{99999})
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
		t.Errorf("numResults = %d, want 0 (no results without SEQUENCE)", decoded.NumResults)
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

// ============================================================================
// v4.0 Regression Tests (Phase 20-02)
// ============================================================================
//
// These tests verify that v4.0 COMPOUND scenarios continue to work correctly
// after the v4.1 COMPOUND bifurcation. Each test exercises a specific v4.0
// code path through the dispatcher.

func TestCompound_V40_Regression(t *testing.T) {
	t.Run("PUTROOTFH_GETATTR", func(t *testing.T) {
		// v4.0 PUTFH + GETATTR still works as before
		h := newTestHandler()

		// Register succeeding handlers
		h.opDispatchTable[types.OP_PUTROOTFH] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
			return &types.CompoundResult{
				Status: types.NFS4_OK,
				OpCode: types.OP_PUTROOTFH,
				Data:   encodeStatusOnly(types.NFS4_OK),
			}
		}
		h.opDispatchTable[types.OP_GETATTR] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
			// Consume args (bitmap)
			_, _ = xdr.DecodeUint32(reader) // bitmap length
			return &types.CompoundResult{
				Status: types.NFS4_OK,
				OpCode: types.OP_GETATTR,
				Data:   encodeStatusOnly(types.NFS4_OK),
			}
		}

		ctx := newTestCompoundContext()
		data := buildCompoundArgs([]byte("v40-putfh-getattr"), 0, []uint32{types.OP_PUTROOTFH, types.OP_GETATTR})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4_OK {
			t.Errorf("status = %d, want NFS4_OK (v4.0 PUTROOTFH+GETATTR regression)", decoded.Status)
		}
		if decoded.NumResults != 2 {
			t.Fatalf("numResults = %d, want 2", decoded.NumResults)
		}
		if decoded.Results[0].OpCode != types.OP_PUTROOTFH {
			t.Errorf("result[0] opcode = %d, want OP_PUTROOTFH", decoded.Results[0].OpCode)
		}
		if decoded.Results[1].OpCode != types.OP_GETATTR {
			t.Errorf("result[1] opcode = %d, want OP_GETATTR", decoded.Results[1].OpCode)
		}
	})

	t.Run("SkipOwnerSeqid_is_false", func(t *testing.T) {
		// v4.0 COMPOUND should NOT set SkipOwnerSeqid
		h := newTestHandler()

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
		data := buildCompoundArgs([]byte("v40-seqid"), 0, []uint32{types.OP_PUTROOTFH})
		_, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		if sawSkipOwnerSeqid {
			t.Error("SkipOwnerSeqid should be false for v4.0 COMPOUND")
		}
	})

	t.Run("BlockedOperation", func(t *testing.T) {
		// v4.0 with blocked operation still returns NFS4ERR_NOTSUPP
		h := newTestHandler()
		h.SetBlockedOps([]string{"PUTROOTFH"})

		ctx := newTestCompoundContext()
		data := buildCompoundArgs([]byte("v40-blocked"), 0, []uint32{types.OP_PUTROOTFH})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_NOTSUPP {
			t.Errorf("status = %d, want NFS4ERR_NOTSUPP (blocked op regression)", decoded.Status)
		}
	})

	t.Run("UnknownOpcode", func(t *testing.T) {
		// v4.0 with unknown opcode still returns OP_ILLEGAL
		h := newTestHandler()
		ctx := newTestCompoundContext()

		data := buildCompoundArgs([]byte("v40-unknown"), 0, []uint32{99999})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
			t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL", decoded.Status)
		}
		if decoded.NumResults != 1 {
			t.Fatalf("numResults = %d, want 1", decoded.NumResults)
		}
		if decoded.Results[0].OpCode != types.OP_ILLEGAL {
			t.Errorf("result opcode = %d, want OP_ILLEGAL", decoded.Results[0].OpCode)
		}
	})

	t.Run("EmptyCompound", func(t *testing.T) {
		// v4.0 empty COMPOUND still succeeds
		h := newTestHandler()
		ctx := newTestCompoundContext()

		data := buildCompoundArgs([]byte("v40-empty"), 0, nil)
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
	})

	t.Run("IllegalOp", func(t *testing.T) {
		// v4.0 ILLEGAL op still returns NFS4ERR_OP_ILLEGAL
		h := newTestHandler()
		ctx := newTestCompoundContext()

		data := buildCompoundArgs([]byte("v40-illegal"), 0, []uint32{types.OP_ILLEGAL})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
			t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL", decoded.Status)
		}
	})

	t.Run("MultiOpsStopOnError", func(t *testing.T) {
		// v4.0 multi-op still stops on error
		h := newTestHandler()

		h.opDispatchTable[types.OP_PUTROOTFH] = func(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
			return &types.CompoundResult{
				Status: types.NFS4_OK,
				OpCode: types.OP_PUTROOTFH,
				Data:   encodeStatusOnly(types.NFS4_OK),
			}
		}

		ctx := newTestCompoundContext()
		data := buildCompoundArgs([]byte("v40-multi"), 0, []uint32{types.OP_PUTROOTFH, types.OP_ILLEGAL, types.OP_PUTROOTFH})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
			t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL (stop-on-error regression)", decoded.Status)
		}
		if decoded.NumResults != 2 {
			t.Fatalf("numResults = %d, want 2 (succeed + fail, third not executed)", decoded.NumResults)
		}
	})

	t.Run("OpCountLimit", func(t *testing.T) {
		// v4.0 op count limit still enforced
		h := newTestHandler()
		ctx := newTestCompoundContext()

		opcodes := make([]uint32, 129)
		for i := range opcodes {
			opcodes[i] = types.OP_PUTROOTFH
		}

		data := buildCompoundArgs([]byte("v40-limit"), 0, opcodes)
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if decoded.Status != types.NFS4ERR_RESOURCE {
			t.Errorf("status = %d, want NFS4ERR_RESOURCE (op count limit regression)", decoded.Status)
		}
	})

	t.Run("TagEcho", func(t *testing.T) {
		// v4.0 COMPOUND echoes tag even on error
		h := newTestHandler()
		ctx := newTestCompoundContext()

		data := buildCompoundArgs([]byte("echo-me"), 0, []uint32{types.OP_ILLEGAL})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}

		if string(decoded.Tag) != "echo-me" {
			t.Errorf("tag = %q, want %q (tag echo regression)", string(decoded.Tag), "echo-me")
		}
	})
}

// ============================================================================
// v4.0/v4.1 Coexistence Tests (Phase 20-02)
// ============================================================================
//
// These tests verify that v4.0 and v4.1 COMPOUNDs work correctly on the same
// handler instance, including mixed version sequences and shared op fallback.

func TestCompound_V40_V41_Coexistence(t *testing.T) {
	t.Run("Sequential_V40_then_V41", func(t *testing.T) {
		// Send v4.0 COMPOUND then v4.1 COMPOUND on same handler: both succeed
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// First: v4.0 COMPOUND with PUTROOTFH
		v40data := buildCompoundArgs([]byte("v40"), 0, []uint32{types.OP_PUTROOTFH})
		v40resp, err := h.ProcessCompound(ctx, v40data)
		if err != nil {
			t.Fatalf("v4.0 ProcessCompound error: %v", err)
		}

		v40decoded, err := decodeCompoundResponse(v40resp)
		if err != nil {
			t.Fatalf("decode v4.0 response error: %v", err)
		}
		if v40decoded.Status != types.NFS4_OK {
			t.Errorf("v4.0 status = %d, want NFS4_OK", v40decoded.Status)
		}

		// Second: v4.1 COMPOUND with SEQUENCE + PUTROOTFH
		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
		v41ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_PUTROOTFH},
		}
		v41data := buildCompoundArgsWithOps([]byte("v41"), 1, v41ops)
		v41resp, err := h.ProcessCompound(ctx, v41data)
		if err != nil {
			t.Fatalf("v4.1 ProcessCompound error: %v", err)
		}

		v41decoded, _ := decodeSequenceRes(t, v41resp)
		if v41decoded.Status != types.NFS4_OK {
			t.Errorf("v4.1 status = %d, want NFS4_OK", v41decoded.Status)
		}
	})

	t.Run("V41_with_V40_ops_fallback", func(t *testing.T) {
		// v4.1 COMPOUND with SEQUENCE + v4.0 ops (PUTROOTFH via fallback): works
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_PUTROOTFH},
		}
		data := buildCompoundArgsWithOps([]byte("v41-fallback"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		// Decode full response to verify both results
		reader := bytes.NewReader(resp)
		status, _ := xdr.DecodeUint32(reader)
		if status != types.NFS4_OK {
			t.Fatalf("overall status = %d, want NFS4_OK", status)
		}
		_, _ = xdr.DecodeOpaque(reader) // tag
		numResults, _ := xdr.DecodeUint32(reader)
		if numResults != 2 {
			t.Fatalf("numResults = %d, want 2 (SEQUENCE + PUTROOTFH)", numResults)
		}
	})

	t.Run("V41_SkipOwnerSeqid_set", func(t *testing.T) {
		// v4.1 COMPOUND with SEQUENCE sets SkipOwnerSeqid for v4.0 ops
		h, sessionID := createTestSession(t)

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
		data := buildCompoundArgsWithOps([]byte("seqid-bypass"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, _ := decodeSequenceRes(t, resp)
		if decoded.Status != types.NFS4_OK {
			t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
		}

		if !sawSkipOwnerSeqid {
			t.Error("SkipOwnerSeqid should be true for v4.0 ops inside v4.1 compound")
		}
	})

	t.Run("AlternatingVersions", func(t *testing.T) {
		// Mixed minor versions: alternate v4.0 and v4.1 COMPOUNDs on same handler
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		for i := 0; i < 5; i++ {
			// v4.0 COMPOUND
			v40data := buildCompoundArgs([]byte("alt-v40"), 0, []uint32{types.OP_PUTROOTFH})
			v40resp, err := h.ProcessCompound(ctx, v40data)
			if err != nil {
				t.Fatalf("iteration %d v4.0 error: %v", i, err)
			}
			v40dec, err := decodeCompoundResponse(v40resp)
			if err != nil {
				t.Fatalf("iteration %d v4.0 decode error: %v", i, err)
			}
			if v40dec.Status != types.NFS4_OK {
				t.Errorf("iteration %d v4.0 status = %d, want NFS4_OK", i, v40dec.Status)
			}

			// v4.1 COMPOUND with SEQUENCE (using incrementing seqid)
			seqid := uint32(i + 1)
			seqArgs := encodeSequenceArgs(sessionID, 0, seqid, 0, true)
			v41ops := []compoundOp{
				{opCode: types.OP_SEQUENCE, data: seqArgs},
				{opCode: types.OP_PUTROOTFH},
			}
			v41data := buildCompoundArgsWithOps([]byte("alt-v41"), 1, v41ops)
			v41resp, err := h.ProcessCompound(ctx, v41data)
			if err != nil {
				t.Fatalf("iteration %d v4.1 error: %v", i, err)
			}
			v41dec, _ := decodeSequenceRes(t, v41resp)
			if v41dec.Status != types.NFS4_OK {
				t.Errorf("iteration %d v4.1 status = %d, want NFS4_OK", i, v41dec.Status)
			}
		}
	})
}

// ============================================================================
// Concurrent Mixed Traffic Tests (Phase 20-02)
// ============================================================================

func TestCompound_ConcurrentMixedTraffic(t *testing.T) {
	// Launch N goroutines each sending COMPOUNDs with random minor version.
	// Verify: no panics, no data races (-race flag), all responses valid.
	const numGoroutines = 10
	const opsPerGoroutine = 100

	h := newTestHandler()

	// Pre-create sessions for each goroutine that will use v4.1
	type sessionInfo struct {
		sessionID types.SessionId4
	}
	sessions := make([]sessionInfo, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		clientID, seqID := registerExchangeID(t, h, fmt.Sprintf("concurrent-client-%d", i))
		ctx := newTestCompoundContext()
		secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
		csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
		ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
		data := buildCompoundArgsWithOps([]byte("cs"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("CREATE_SESSION for goroutine %d error: %v", i, err)
		}

		reader := bytes.NewReader(resp)
		status, _ := xdr.DecodeUint32(reader)
		if status != types.NFS4_OK {
			t.Fatalf("CREATE_SESSION for goroutine %d status = %d", i, status)
		}
		_, _ = xdr.DecodeOpaque(reader) // tag
		_, _ = xdr.DecodeUint32(reader) // numResults
		_, _ = xdr.DecodeUint32(reader) // opcode

		var csRes types.CreateSessionRes
		if err := csRes.Decode(reader); err != nil {
			t.Fatalf("decode CreateSessionRes for goroutine %d: %v", i, err)
		}
		sessions[i] = sessionInfo{sessionID: csRes.SessionID}
	}

	var wg sync.WaitGroup
	errCh := make(chan string, numGoroutines*opsPerGoroutine)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(goroutineID * 1000)))

			// Track per-slot seqids for v4.1
			nextSeqID := uint32(1)

			for op := 0; op < opsPerGoroutine; op++ {
				ctx := newTestCompoundContext()
				minorVersion := uint32(rng.Intn(2)) // 0 or 1

				var resp []byte
				var err error

				if minorVersion == 0 {
					// v4.0 COMPOUND
					data := buildCompoundArgs([]byte("conc-v40"), 0, []uint32{types.OP_PUTROOTFH})
					resp, err = h.ProcessCompound(ctx, data)
				} else {
					// v4.1 COMPOUND with SEQUENCE
					seqArgs := encodeSequenceArgs(sessions[goroutineID].sessionID, 0, nextSeqID, 0, true)
					ops := []compoundOp{
						{opCode: types.OP_SEQUENCE, data: seqArgs},
						{opCode: types.OP_PUTROOTFH},
					}
					data := buildCompoundArgsWithOps([]byte("conc-v41"), 1, ops)
					resp, err = h.ProcessCompound(ctx, data)
					nextSeqID++
				}

				if err != nil {
					errCh <- fmt.Sprintf("goroutine %d op %d (v4.%d): error: %v", goroutineID, op, minorVersion, err)
					continue
				}

				if resp == nil {
					errCh <- fmt.Sprintf("goroutine %d op %d (v4.%d): nil response", goroutineID, op, minorVersion)
					continue
				}

				// Verify response is decodable
				reader := bytes.NewReader(resp)
				_, decErr := xdr.DecodeUint32(reader)
				if decErr != nil {
					errCh <- fmt.Sprintf("goroutine %d op %d (v4.%d): bad response: %v", goroutineID, op, minorVersion, decErr)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	for errMsg := range errCh {
		t.Error(errMsg)
	}
}

// ============================================================================
// Version Range Gating Tests (Phase 20-02)
// ============================================================================

func TestCompound_VersionRangeGating(t *testing.T) {
	t.Run("V41Only", func(t *testing.T) {
		// Set minMinorVersion=1, maxMinorVersion=1: v4.0 COMPOUND returns MINOR_VERS_MISMATCH
		h := newTestHandler()
		h.SetMinorVersionRange(1, 1)
		ctx := newTestCompoundContext()

		// v4.0 should be rejected
		v40data := buildCompoundArgs([]byte("v40-reject"), 0, []uint32{types.OP_PUTROOTFH})
		v40resp, err := h.ProcessCompound(ctx, v40data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		v40decoded, err := decodeCompoundResponse(v40resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if v40decoded.Status != types.NFS4ERR_MINOR_VERS_MISMATCH {
			t.Errorf("v4.0 status = %d, want NFS4ERR_MINOR_VERS_MISMATCH", v40decoded.Status)
		}

		// v4.1 empty compound should work
		v41data := buildCompoundArgs([]byte("v41-accept"), 1, nil)
		v41resp, err := h.ProcessCompound(ctx, v41data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		v41decoded, err := decodeCompoundResponse(v41resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if v41decoded.Status != types.NFS4_OK {
			t.Errorf("v4.1 status = %d, want NFS4_OK", v41decoded.Status)
		}
	})

	t.Run("V40Only", func(t *testing.T) {
		// Set minMinorVersion=0, maxMinorVersion=0: v4.1 COMPOUND returns MINOR_VERS_MISMATCH
		h := newTestHandler()
		h.SetMinorVersionRange(0, 0)
		ctx := newTestCompoundContext()

		// v4.1 should be rejected
		v41data := buildCompoundArgs([]byte("v41-reject"), 1, nil)
		v41resp, err := h.ProcessCompound(ctx, v41data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		v41decoded, err := decodeCompoundResponse(v41resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if v41decoded.Status != types.NFS4ERR_MINOR_VERS_MISMATCH {
			t.Errorf("v4.1 status = %d, want NFS4ERR_MINOR_VERS_MISMATCH", v41decoded.Status)
		}

		// v4.0 should work
		v40data := buildCompoundArgs([]byte("v40-accept"), 0, []uint32{types.OP_PUTROOTFH})
		v40resp, err := h.ProcessCompound(ctx, v40data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		v40decoded, err := decodeCompoundResponse(v40resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if v40decoded.Status != types.NFS4_OK {
			t.Errorf("v4.0 status = %d, want NFS4_OK", v40decoded.Status)
		}
	})

	t.Run("DefaultBothVersions", func(t *testing.T) {
		// Default (0, 1): both versions work
		h := newTestHandler()
		ctx := newTestCompoundContext()

		// v4.0 should work
		v40data := buildCompoundArgs([]byte("v40-default"), 0, []uint32{types.OP_PUTROOTFH})
		v40resp, err := h.ProcessCompound(ctx, v40data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		v40decoded, err := decodeCompoundResponse(v40resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if v40decoded.Status != types.NFS4_OK {
			t.Errorf("v4.0 status = %d, want NFS4_OK", v40decoded.Status)
		}

		// v4.1 empty compound should work
		v41data := buildCompoundArgs([]byte("v41-default"), 1, nil)
		v41resp, err := h.ProcessCompound(ctx, v41data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		v41decoded, err := decodeCompoundResponse(v41resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if v41decoded.Status != types.NFS4_OK {
			t.Errorf("v4.1 status = %d, want NFS4_OK", v41decoded.Status)
		}
	})

	t.Run("OutOfRange_Both", func(t *testing.T) {
		// minorversion=2 should always be rejected regardless of range
		h := newTestHandler()
		ctx := newTestCompoundContext()

		data := buildCompoundArgs([]byte("v42"), 2, []uint32{types.OP_GETATTR})
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode response error: %v", err)
		}
		if decoded.Status != types.NFS4ERR_MINOR_VERS_MISMATCH {
			t.Errorf("v4.2 status = %d, want NFS4ERR_MINOR_VERS_MISMATCH", decoded.Status)
		}
		// Tag should still be echoed
		if string(decoded.Tag) != "v42" {
			t.Errorf("tag = %q, want %q", string(decoded.Tag), "v42")
		}
	})
}

// ============================================================================
// Connection Binding Integration Tests (Phase 21-01)
// ============================================================================

func TestCompound_BindConnToSession_ExemptOp(t *testing.T) {
	// BIND_CONN_TO_SESSION as first op (no SEQUENCE) should work as exempt op.
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 7001

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)
	ops := []compoundOp{
		{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs},
	}
	data := buildCompoundArgsWithOps([]byte("bcs-exempt"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK (exempt op)", status)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 1 {
		t.Fatalf("numResults = %d, want 1", numResults)
	}
	opCode, _ := xdr.DecodeUint32(reader)
	if opCode != types.OP_BIND_CONN_TO_SESSION {
		t.Errorf("result opcode = %d, want OP_BIND_CONN_TO_SESSION", opCode)
	}

	var res types.BindConnToSessionRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode BindConnToSessionRes: %v", err)
	}
	if res.Status != types.NFS4_OK {
		t.Errorf("res.Status = %d, want NFS4_OK", res.Status)
	}
	if res.Dir != types.CDFS4_FORE {
		t.Errorf("res.Dir = %d, want CDFS4_FORE", res.Dir)
	}
}

func TestCompound_CreateSession_AutoBinds(t *testing.T) {
	// CREATE_SESSION with ConnectionID set should auto-bind the connection.
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "autobind-client")

	ctx := newTestCompoundContext()
	ctx.ConnectionID = 7002 // set connection ID

	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	data := buildCompoundArgsWithOps([]byte("autobind"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Extract session ID from response
	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", status)
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

	// Verify the connection is auto-bound to the session
	bindings := h.StateManager.GetConnectionBindings(csRes.SessionID)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 auto-bound connection, got %d", len(bindings))
	}
	if bindings[0].ConnectionID != 7002 {
		t.Errorf("auto-bound connection ID = %d, want 7002", bindings[0].ConnectionID)
	}
}

func TestCompound_MultiConnection_SameSession(t *testing.T) {
	// Two connections (different ConnectionIDs) bound to the same session,
	// both sending SEQUENCE on the same session. Both should succeed.
	h, sessionID := createTestSession(t)

	// Bind two connections to the session
	ctx1 := newTestCompoundContext()
	ctx1.ConnectionID = 7003

	bcsArgs1 := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)
	ops1 := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs1}}
	data1 := buildCompoundArgsWithOps([]byte("bind1"), 1, ops1)

	resp1, err := h.ProcessCompound(ctx1, data1)
	if err != nil {
		t.Fatalf("bind conn 7003 error: %v", err)
	}
	dec1, err := decodeCompoundResponse(resp1)
	if err != nil || dec1.Status != types.NFS4_OK {
		t.Fatalf("bind conn 7003 failed: status=%d err=%v", dec1.Status, err)
	}

	ctx2 := newTestCompoundContext()
	ctx2.ConnectionID = 7004

	bcsArgs2 := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)
	ops2 := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs2}}
	data2 := buildCompoundArgsWithOps([]byte("bind2"), 1, ops2)

	resp2, err := h.ProcessCompound(ctx2, data2)
	if err != nil {
		t.Fatalf("bind conn 7004 error: %v", err)
	}
	dec2, err := decodeCompoundResponse(resp2)
	if err != nil || dec2.Status != types.NFS4_OK {
		t.Fatalf("bind conn 7004 failed: status=%d err=%v", dec2.Status, err)
	}

	// Both connections send SEQUENCE on the same session (different slots)
	seqArgs1 := encodeSequenceArgs(sessionID, 0, 1, 1, true)
	seqOps1 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs1},
		{opCode: types.OP_PUTROOTFH},
	}
	seqData1 := buildCompoundArgsWithOps([]byte("seq1"), 1, seqOps1)
	seqResp1, err := h.ProcessCompound(ctx1, seqData1)
	if err != nil {
		t.Fatalf("SEQUENCE on conn 7003 error: %v", err)
	}
	seqDec1, _ := decodeSequenceRes(t, seqResp1)
	if seqDec1.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE on conn 7003 status = %d, want NFS4_OK", seqDec1.Status)
	}

	seqArgs2 := encodeSequenceArgs(sessionID, 1, 1, 1, true)
	seqOps2 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
		{opCode: types.OP_PUTROOTFH},
	}
	seqData2 := buildCompoundArgsWithOps([]byte("seq2"), 1, seqOps2)
	seqResp2, err := h.ProcessCompound(ctx2, seqData2)
	if err != nil {
		t.Fatalf("SEQUENCE on conn 7004 error: %v", err)
	}
	seqDec2, _ := decodeSequenceRes(t, seqResp2)
	if seqDec2.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE on conn 7004 status = %d, want NFS4_OK", seqDec2.Status)
	}

	// Verify both connections are bound
	bindings := h.StateManager.GetConnectionBindings(sessionID)
	// createTestSession auto-binds one connection (ID varies), plus 7003 and 7004
	if len(bindings) < 2 {
		t.Errorf("expected at least 2 bindings, got %d", len(bindings))
	}
}

// encodeBindConnToSessionArgs builds BIND_CONN_TO_SESSION XDR args for compound tests.
func encodeBindConnToSessionArgs(sessionID types.SessionId4, dir uint32, rdma bool) []byte {
	var buf bytes.Buffer
	args := types.BindConnToSessionArgs{
		SessionID:         sessionID,
		Dir:               dir,
		UseConnInRDMAMode: rdma,
	}
	_ = args.Encode(&buf)
	return buf.Bytes()
}

// ============================================================================
// Multi-Connection Integration Tests (Phase 21-02)
// ============================================================================

// createTestSessionWithConnectionID creates a session using CREATE_SESSION
// with the given ConnectionID set on the context, causing auto-bind.
func createTestSessionWithConnectionID(t *testing.T, connID uint64) (*Handler, types.SessionId4) {
	t.Helper()
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, fmt.Sprintf("conn-test-client-%d", connID))

	ctx := newTestCompoundContext()
	ctx.ConnectionID = connID

	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
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

// bindConnection sends BIND_CONN_TO_SESSION for the given connection and direction.
func bindConnection(t *testing.T, h *Handler, sessionID types.SessionId4, connID uint64, dir uint32) {
	t.Helper()
	ctx := newTestCompoundContext()
	ctx.ConnectionID = connID

	bcsArgs := encodeBindConnToSessionArgs(sessionID, dir, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("bind"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("BIND_CONN_TO_SESSION ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("BIND_CONN_TO_SESSION status = %d, want NFS4_OK", decoded.Status)
	}
}

func TestCompound_MultiConnection_DifferentSlots(t *testing.T) {
	// Two connections (different ConnectionIDs) each send SEQUENCE on the same
	// session using different slots. Both succeed. Verify both connections are bound.
	h, sessionID := createTestSessionWithConnectionID(t, 8001)

	// Bind second connection
	bindConnection(t, h, sessionID, 8002, types.CDFC4_FORE)

	// Connection 8001 sends SEQUENCE on slot 0
	ctx1 := newTestCompoundContext()
	ctx1.ConnectionID = 8001
	seqArgs1 := encodeSequenceArgs(sessionID, 0, 1, 1, true)
	seqOps1 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs1},
		{opCode: types.OP_PUTROOTFH},
	}
	seqData1 := buildCompoundArgsWithOps([]byte("slot0"), 1, seqOps1)
	seqResp1, err := h.ProcessCompound(ctx1, seqData1)
	if err != nil {
		t.Fatalf("SEQUENCE on conn 8001 error: %v", err)
	}
	seqDec1, _ := decodeSequenceRes(t, seqResp1)
	if seqDec1.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE on conn 8001 status = %d, want NFS4_OK", seqDec1.Status)
	}

	// Connection 8002 sends SEQUENCE on slot 1
	ctx2 := newTestCompoundContext()
	ctx2.ConnectionID = 8002
	seqArgs2 := encodeSequenceArgs(sessionID, 1, 1, 1, true)
	seqOps2 := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs2},
		{opCode: types.OP_PUTROOTFH},
	}
	seqData2 := buildCompoundArgsWithOps([]byte("slot1"), 1, seqOps2)
	seqResp2, err := h.ProcessCompound(ctx2, seqData2)
	if err != nil {
		t.Fatalf("SEQUENCE on conn 8002 error: %v", err)
	}
	seqDec2, _ := decodeSequenceRes(t, seqResp2)
	if seqDec2.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE on conn 8002 status = %d, want NFS4_OK", seqDec2.Status)
	}

	// Verify both connections are bound
	bindings := h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(bindings))
	}
	connIDs := make(map[uint64]bool)
	for _, b := range bindings {
		connIDs[b.ConnectionID] = true
	}
	if !connIDs[8001] || !connIDs[8002] {
		t.Errorf("expected conn 8001 and 8002 bound, got %v", connIDs)
	}
}

func TestCompound_BindConnToSession_Rebind(t *testing.T) {
	// Bind connection with FORE, then rebind with BACK_OR_BOTH.
	// Verify direction changed to BOTH.
	h, sessionID := createTestSessionWithConnectionID(t, 8010)

	// Initial bind is fore (from auto-bind during CREATE_SESSION)
	bindings := h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 auto-bound connection, got %d", len(bindings))
	}
	if bindings[0].Direction.String() != "fore" {
		t.Errorf("initial direction = %s, want fore", bindings[0].Direction.String())
	}

	// Rebind with BACK_OR_BOTH -- generous policy should return BOTH
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 8010

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_BACK_OR_BOTH, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("rebind"), 1, ops)

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
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var res types.BindConnToSessionRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode BindConnToSessionRes: %v", err)
	}
	if res.Dir != types.CDFS4_BOTH {
		t.Errorf("rebind Dir = %d, want CDFS4_BOTH (%d)", res.Dir, types.CDFS4_BOTH)
	}

	// Verify direction changed in state
	bindings = h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding after rebind, got %d", len(bindings))
	}
	if bindings[0].Direction.String() != "both" {
		t.Errorf("rebound direction = %s, want both", bindings[0].Direction.String())
	}
}

func TestCompound_BindConnToSession_LimitExceeded(t *testing.T) {
	// Create max_connections bindings, attempt one more, verify NFS4ERR_RESOURCE.
	h, sessionID := createTestSessionWithConnectionID(t, 8020)

	// Set max connections per session to 3 (conn 8020 auto-bound = 1 already)
	h.StateManager.SetMaxConnectionsPerSession(3)

	// Bind 2 more connections to fill up the limit
	bindConnection(t, h, sessionID, 8021, types.CDFC4_FORE)
	bindConnection(t, h, sessionID, 8022, types.CDFC4_FORE)

	// Verify we have 3 connections bound
	bindings := h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings at limit, got %d", len(bindings))
	}

	// Attempt one more -- should fail with NFS4ERR_RESOURCE
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 8023

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("overflow"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_RESOURCE {
		t.Errorf("status = %d, want NFS4ERR_RESOURCE (%d)", decoded.Status, types.NFS4ERR_RESOURCE)
	}
}

func TestCompound_BindConnToSession_ForeEnforcement(t *testing.T) {
	// Bind 1 connection as BOTH (the only fore-capable connection),
	// attempt to rebind as BACK only, verify NFS4ERR_INVAL.
	h, sessionID := createTestSessionWithConnectionID(t, 8030)

	// Rebind the only connection as BOTH first (so it is the only fore-capable)
	bindConnection(t, h, sessionID, 8030, types.CDFC4_FORE_OR_BOTH)

	// Verify it is BOTH
	bindings := h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 1 || bindings[0].Direction.String() != "both" {
		t.Fatalf("expected single BOTH binding, got %v", bindings)
	}

	// Try to rebind as BACK-only -- should fail since no other fore-capable connection
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 8030

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_BACK, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("back-only"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_INVAL {
		t.Errorf("status = %d, want NFS4ERR_INVAL (%d)", decoded.Status, types.NFS4ERR_INVAL)
	}
}

func TestCompound_DisconnectCleanup(t *testing.T) {
	// Bind a connection, call UnbindConnection (simulating disconnect),
	// verify GetConnectionBindings returns empty.
	h, sessionID := createTestSessionWithConnectionID(t, 8040)

	// Verify auto-bound connection exists
	bindings := h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 auto-bound connection, got %d", len(bindings))
	}

	// Simulate disconnect
	h.StateManager.UnbindConnection(8040)

	// Verify connection is removed
	bindings = h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 0 {
		t.Errorf("expected 0 bindings after disconnect, got %d", len(bindings))
	}

	// Verify GetConnectionBinding also returns nil
	binding := h.StateManager.GetConnectionBinding(8040)
	if binding != nil {
		t.Errorf("expected nil binding after disconnect, got %v", binding)
	}
}

func TestCompound_Draining_RejectsNewRequests(t *testing.T) {
	// Set a connection as draining, send a SEQUENCE+PUTROOTFH compound,
	// verify the compound returns NFS4ERR_DELAY after SEQUENCE.
	h, sessionID := createTestSessionWithConnectionID(t, 8050)

	// Set the connection as draining
	if err := h.StateManager.SetConnectionDraining(8050, true); err != nil {
		t.Fatalf("SetConnectionDraining error: %v", err)
	}

	// Send SEQUENCE+PUTROOTFH on the draining connection
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 8050

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_PUTROOTFH},
	}
	data := buildCompoundArgsWithOps([]byte("drain"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Overall status should be NFS4ERR_DELAY (draining redirect)
	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4ERR_DELAY {
		t.Errorf("overall status = %d, want NFS4ERR_DELAY (%d)", status, types.NFS4ERR_DELAY)
	}

	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)

	// Only SEQUENCE result should be present (PUTROOTFH was not dispatched)
	if numResults != 1 {
		t.Fatalf("numResults = %d, want 1 (only SEQUENCE)", numResults)
	}
}

func TestCompound_BindConnToSession_SilentUnbind(t *testing.T) {
	// Bind connection to session A, then bind same connection to session B,
	// verify session A has 0 connections and session B has 1.
	h := newTestHandler()

	// Create session A
	clientID1, seqID1 := registerExchangeID(t, h, "silent-unbind-client-a")
	ctxA := newTestCompoundContext()
	ctxA.ConnectionID = 8060

	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs1 := encodeCreateSessionArgsWithSec(clientID1, seqID1, 0, secParms)
	ops1 := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs1}}
	data1 := buildCompoundArgsWithOps([]byte("csA"), 1, ops1)

	resp1, err := h.ProcessCompound(ctxA, data1)
	if err != nil {
		t.Fatalf("CREATE_SESSION A error: %v", err)
	}
	reader1 := bytes.NewReader(resp1)
	st1, _ := xdr.DecodeUint32(reader1)
	if st1 != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION A status = %d, want NFS4_OK", st1)
	}
	_, _ = xdr.DecodeOpaque(reader1) // tag
	_, _ = xdr.DecodeUint32(reader1) // numResults
	_, _ = xdr.DecodeUint32(reader1) // opcode
	var csResA types.CreateSessionRes
	if err := csResA.Decode(reader1); err != nil {
		t.Fatalf("decode CreateSessionRes A: %v", err)
	}
	sessionA := csResA.SessionID

	// Verify conn 8060 is auto-bound to session A
	bindingsA := h.StateManager.GetConnectionBindings(sessionA)
	if len(bindingsA) != 1 {
		t.Fatalf("expected 1 binding on session A, got %d", len(bindingsA))
	}

	// Create session B with a different client
	clientID2, seqID2 := registerExchangeID(t, h, "silent-unbind-client-b")
	ctxB := newTestCompoundContext()
	ctxB.ConnectionID = 8061

	csArgs2 := encodeCreateSessionArgsWithSec(clientID2, seqID2, 0, secParms)
	ops2 := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs2}}
	data2 := buildCompoundArgsWithOps([]byte("csB"), 1, ops2)

	resp2, err := h.ProcessCompound(ctxB, data2)
	if err != nil {
		t.Fatalf("CREATE_SESSION B error: %v", err)
	}
	reader2 := bytes.NewReader(resp2)
	st2, _ := xdr.DecodeUint32(reader2)
	if st2 != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION B status = %d, want NFS4_OK", st2)
	}
	_, _ = xdr.DecodeOpaque(reader2) // tag
	_, _ = xdr.DecodeUint32(reader2) // numResults
	_, _ = xdr.DecodeUint32(reader2) // opcode
	var csResB types.CreateSessionRes
	if err := csResB.Decode(reader2); err != nil {
		t.Fatalf("decode CreateSessionRes B: %v", err)
	}
	sessionB := csResB.SessionID

	// Now bind conn 8060 (currently on session A) to session B
	ctxRebind := newTestCompoundContext()
	ctxRebind.ConnectionID = 8060

	bcsArgs := encodeBindConnToSessionArgs(sessionB, types.CDFC4_FORE, false)
	ops3 := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data3 := buildCompoundArgsWithOps([]byte("rebind-sess"), 1, ops3)

	resp3, err := h.ProcessCompound(ctxRebind, data3)
	if err != nil {
		t.Fatalf("BIND_CONN_TO_SESSION error: %v", err)
	}
	dec3, err := decodeCompoundResponse(resp3)
	if err != nil || dec3.Status != types.NFS4_OK {
		t.Fatalf("BIND_CONN_TO_SESSION failed: status=%d err=%v", dec3.Status, err)
	}

	// Verify session A has 0 connections (silently unbound)
	bindingsA = h.StateManager.GetConnectionBindings(sessionA)
	if len(bindingsA) != 0 {
		t.Errorf("expected 0 bindings on session A after silent unbind, got %d", len(bindingsA))
	}

	// Verify session B has 2 connections (8061 auto-bound + 8060 rebound)
	bindingsB := h.StateManager.GetConnectionBindings(sessionB)
	if len(bindingsB) != 2 {
		t.Errorf("expected 2 bindings on session B, got %d", len(bindingsB))
	}
}

func TestCompound_CreateSession_AutoBind_Verify(t *testing.T) {
	// Create a session with ConnectionID set, immediately call
	// GetConnectionBindings, verify exactly 1 connection bound as fore.
	h, sessionID := createTestSessionWithConnectionID(t, 8070)

	bindings := h.StateManager.GetConnectionBindings(sessionID)
	if len(bindings) != 1 {
		t.Fatalf("expected 1 auto-bound connection, got %d", len(bindings))
	}
	if bindings[0].ConnectionID != 8070 {
		t.Errorf("auto-bound connection ID = %d, want 8070", bindings[0].ConnectionID)
	}
	if bindings[0].Direction.String() != "fore" {
		t.Errorf("auto-bound direction = %s, want fore", bindings[0].Direction.String())
	}
	if bindings[0].ConnType.String() != "TCP" {
		t.Errorf("auto-bound conn type = %s, want TCP", bindings[0].ConnType.String())
	}
}

// ============================================================================
// BACKCHANNEL_CTL Compound Integration Test (Phase 22-02)
// ============================================================================

func TestCompound_SequenceAndBackchannelCtl(t *testing.T) {
	// Create a session with backchannel support
	h := newTestHandler()
	clientID, seqID := registerExchangeID(t, h, "bctl-compound-client")

	ctx := newTestCompoundContext()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID,
		types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN, secParms)
	csOps := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	csData := buildCompoundArgsWithOps([]byte("cs"), 1, csOps)
	csResp, err := h.ProcessCompound(ctx, csData)
	if err != nil {
		t.Fatalf("CREATE_SESSION error: %v", err)
	}
	csReader := bytes.NewReader(csResp)
	csStatus, _ := xdr.DecodeUint32(csReader)
	if csStatus != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION status = %d", csStatus)
	}
	_, _ = xdr.DecodeOpaque(csReader) // tag
	_, _ = xdr.DecodeUint32(csReader) // numResults
	_, _ = xdr.DecodeUint32(csReader) // opcode
	var csRes types.CreateSessionRes
	_ = csRes.Decode(csReader)
	sessionID := csRes.SessionID

	// SEQUENCE + BACKCHANNEL_CTL in a single COMPOUND
	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
	bctlArgs := encodeBackchannelCtlArgs(0x70000000, []types.CallbackSecParms4{
		{CbSecFlavor: 0},
	})
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_BACKCHANNEL_CTL, data: bctlArgs},
	}
	data := buildCompoundArgsWithOps([]byte("seq-bctl"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Manually decode: SEQUENCE has fields beyond status, so decodeCompoundResponse
	// would desync. Parse the full response.
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

	// Result 0: SEQUENCE
	op0, _ := xdr.DecodeUint32(reader)
	if op0 != types.OP_SEQUENCE {
		t.Errorf("result[0] opcode = %d, want OP_SEQUENCE", op0)
	}
	var seqRes types.SequenceRes
	if err := seqRes.Decode(reader); err != nil {
		t.Fatalf("decode SequenceRes: %v", err)
	}
	if seqRes.Status != types.NFS4_OK {
		t.Errorf("SEQUENCE status = %d, want NFS4_OK", seqRes.Status)
	}

	// Result 1: BACKCHANNEL_CTL
	op1, _ := xdr.DecodeUint32(reader)
	if op1 != types.OP_BACKCHANNEL_CTL {
		t.Errorf("result[1] opcode = %d, want OP_BACKCHANNEL_CTL (%d)", op1, types.OP_BACKCHANNEL_CTL)
	}
	var bctlRes types.BackchannelCtlRes
	if err := bctlRes.Decode(reader); err != nil {
		t.Fatalf("decode BackchannelCtlRes: %v", err)
	}
	if bctlRes.Status != types.NFS4_OK {
		t.Errorf("BACKCHANNEL_CTL status = %d, want NFS4_OK", bctlRes.Status)
	}

	// Verify params stored
	session := h.StateManager.GetSession(sessionID)
	if session == nil {
		t.Fatal("session not found")
	}
	if session.CbProgram != 0x70000000 {
		t.Errorf("CbProgram = 0x%x, want 0x70000000", session.CbProgram)
	}
}

// ============================================================================
// v4.0-only operation rejection in v4.1 COMPOUNDs (Phase 23-02)
// ============================================================================

func TestCompound_V41_RejectsV40OnlyOps(t *testing.T) {
	// For each v4.0-only op, verify it returns NFS4ERR_NOTSUPP in a v4.1 COMPOUND
	// with SEQUENCE. The SEQUENCE must succeed, then the v4.0-only op is rejected.

	v40Ops := []struct {
		opCode uint32
		name   string
		args   []byte // XDR-encoded args to consume
	}{
		{
			types.OP_SETCLIENTID,
			"SETCLIENTID",
			encodeSetClientIDArgsForTest(),
		},
		{
			types.OP_SETCLIENTID_CONFIRM,
			"SETCLIENTID_CONFIRM",
			encodeSetClientIDConfirmArgsForTest(),
		},
		{
			types.OP_RENEW,
			"RENEW",
			encodeRenewArgsForTest(),
		},
		{
			types.OP_OPEN_CONFIRM,
			"OPEN_CONFIRM",
			encodeOpenConfirmArgsForTest(),
		},
		{
			types.OP_RELEASE_LOCKOWNER,
			"RELEASE_LOCKOWNER",
			encodeReleaseLockOwnerArgsForTest(),
		},
	}

	for _, op := range v40Ops {
		t.Run(op.name, func(t *testing.T) {
			h, sessionID := createTestSession(t)
			ctx := newTestCompoundContext()

			seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
			ops := []compoundOp{
				{opCode: types.OP_SEQUENCE, data: seqArgs},
				{opCode: op.opCode, data: op.args},
			}
			data := buildCompoundArgsWithOps([]byte("v41-reject"), 1, ops)

			resp, err := h.ProcessCompound(ctx, data)
			if err != nil {
				t.Fatalf("ProcessCompound error: %v", err)
			}

			// Decode to check SEQUENCE succeeded and v4.0-only op got NOTSUPP
			reader := bytes.NewReader(resp)
			overallStatus, _ := xdr.DecodeUint32(reader)
			_, _ = xdr.DecodeOpaque(reader)           // tag
			numResults, _ := xdr.DecodeUint32(reader) // numResults

			if numResults < 2 {
				t.Fatalf("numResults = %d, want >= 2 (SEQUENCE + %s)", numResults, op.name)
			}

			// First result: SEQUENCE should be NFS4_OK
			seqOpCode, _ := xdr.DecodeUint32(reader)
			if seqOpCode != types.OP_SEQUENCE {
				t.Errorf("result[0] opcode = %d, want OP_SEQUENCE", seqOpCode)
			}
			var seqRes types.SequenceRes
			if err := seqRes.Decode(reader); err != nil {
				t.Fatalf("decode SequenceRes: %v", err)
			}
			if seqRes.Status != types.NFS4_OK {
				t.Errorf("SEQUENCE status = %d, want NFS4_OK", seqRes.Status)
			}

			// Second result: v4.0-only op should be NOTSUPP
			v40OpCode, _ := xdr.DecodeUint32(reader)
			if v40OpCode != op.opCode {
				t.Errorf("result[1] opcode = %d, want %d (%s)", v40OpCode, op.opCode, op.name)
			}
			v40Status, _ := xdr.DecodeUint32(reader)
			if v40Status != types.NFS4ERR_NOTSUPP {
				t.Errorf("%s status = %d, want NFS4ERR_NOTSUPP (%d)",
					op.name, v40Status, types.NFS4ERR_NOTSUPP)
			}

			// Overall status should be NOTSUPP
			if overallStatus != types.NFS4ERR_NOTSUPP {
				t.Errorf("overall status = %d, want NFS4ERR_NOTSUPP (%d)",
					overallStatus, types.NFS4ERR_NOTSUPP)
			}
		})
	}
}

func TestCompound_V40_AllowsV40Ops(t *testing.T) {
	// Verify v4.0 COMPOUNDs still allow these ops (no regression).
	// Each op may fail on business logic, but it should NOT return NOTSUPP.

	v40Ops := []struct {
		opCode uint32
		name   string
		args   []byte
	}{
		{
			types.OP_SETCLIENTID,
			"SETCLIENTID",
			encodeSetClientIDArgsForTest(),
		},
		{
			types.OP_RENEW,
			"RENEW",
			encodeRenewArgsForTest(),
		},
		{
			types.OP_RELEASE_LOCKOWNER,
			"RELEASE_LOCKOWNER",
			encodeReleaseLockOwnerArgsForTest(),
		},
	}

	for _, op := range v40Ops {
		t.Run(op.name, func(t *testing.T) {
			h := newTestHandler()
			ctx := newTestCompoundContext()

			ops := []compoundOp{
				{opCode: op.opCode, data: op.args},
			}
			data := buildCompoundArgsWithOps([]byte("v40-allow"), 0, ops)

			resp, err := h.ProcessCompound(ctx, data)
			if err != nil {
				t.Fatalf("ProcessCompound error: %v", err)
			}

			decoded, err := decodeCompoundResponse(resp)
			if err != nil {
				t.Fatalf("decode response error: %v", err)
			}

			// The op should NOT return NFS4ERR_NOTSUPP -- it may fail for other
			// reasons (e.g. stale clientid for RENEW), but the dispatch must work.
			if decoded.NumResults < 1 {
				t.Fatalf("numResults = %d, want >= 1", decoded.NumResults)
			}
			if decoded.Results[0].Status == types.NFS4ERR_NOTSUPP {
				t.Errorf("%s returned NFS4ERR_NOTSUPP in v4.0 COMPOUND (regression)",
					op.name)
			}
		})
	}
}

func TestCompound_V41_DestroyClientID_SessionExempt(t *testing.T) {
	// DESTROY_CLIENTID as only op in v4.1 COMPOUND (no SEQUENCE) should work.
	h := newTestHandler()
	clientID, _ := registerExchangeID(t, h, "dc-exempt-compound")

	var buf bytes.Buffer
	args := types.DestroyClientidArgs{ClientID: clientID}
	_ = args.Encode(&buf)

	ops := []compoundOp{
		{opCode: types.OP_DESTROY_CLIENTID, data: buf.Bytes()},
	}
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

	if decoded.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK (session-exempt)", decoded.Status)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_DESTROY_CLIENTID {
		t.Errorf("result opcode = %d, want OP_DESTROY_CLIENTID", decoded.Results[0].OpCode)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("result status = %d, want NFS4_OK", decoded.Results[0].Status)
	}
}

// ============================================================================
// Test helper functions for v4.0-only op args encoding
// ============================================================================

func encodeSetClientIDConfirmArgsForTest() []byte {
	var buf bytes.Buffer
	// clientid (uint64)
	_ = xdr.WriteUint64(&buf, 0x1234567890ABCDEF)
	// confirm verifier (8 bytes raw)
	var verifier [8]byte
	copy(verifier[:], "confverf")
	buf.Write(verifier[:])
	return buf.Bytes()
}

func encodeRenewArgsForTest() []byte {
	var buf bytes.Buffer
	// clientid (uint64)
	_ = xdr.WriteUint64(&buf, 0x1234567890ABCDEF)
	return buf.Bytes()
}

func encodeOpenConfirmArgsForTest() []byte {
	var buf bytes.Buffer
	// stateid4: seqid (uint32) + other (12 bytes)
	_ = xdr.WriteUint32(&buf, 1) // seqid
	var other [12]byte
	copy(other[:], "fakestateoth")
	buf.Write(other[:])
	// seqid (uint32)
	_ = xdr.WriteUint32(&buf, 1)
	return buf.Bytes()
}

func encodeReleaseLockOwnerArgsForTest() []byte {
	var buf bytes.Buffer
	// lock_owner4: clientid (uint64) + owner (opaque)
	_ = xdr.WriteUint64(&buf, 0x1234567890ABCDEF)
	_ = xdr.WriteXDROpaque(&buf, []byte("test-lock-owner"))
	return buf.Bytes()
}

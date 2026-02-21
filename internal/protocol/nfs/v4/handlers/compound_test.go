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

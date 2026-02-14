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

	// Minor version 1 should fail
	data := buildCompoundArgs([]byte("v4.1"), 1, []uint32{types.OP_GETATTR})
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
	if string(decoded.Tag) != "v4.1" {
		t.Errorf("tag = %q, want %q (must echo tag even on error)", string(decoded.Tag), "v4.1")
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

package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

func TestBindConnToSession_BadXDR(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 100

	// Truncated args (invalid XDR)
	truncatedArgs := []byte{0x00, 0x00, 0x00, 0x01}
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: truncatedArgs}}
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
		t.Errorf("status = %d, want NFS4ERR_BADXDR (%d)", decoded.Status, types.NFS4ERR_BADXDR)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_BIND_CONN_TO_SESSION {
		t.Errorf("result opcode = %d, want OP_BIND_CONN_TO_SESSION", decoded.Results[0].OpCode)
	}
}

func TestBindConnToSession_NonExistentSession(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 200

	var fakeSID types.SessionId4
	copy(fakeSID[:], "nonexistentsess!")

	bcsArgs := encodeBindConnToSessionArgs(fakeSID, types.CDFC4_FORE, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("badsess"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("status = %d, want NFS4ERR_BADSESSION (%d)", decoded.Status, types.NFS4ERR_BADSESSION)
	}
}

func TestBindConnToSession_Success_Fore(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 300

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("fore"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Decode full response to read BindConnToSessionRes
	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader) // overall status
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", status)
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
		t.Errorf("res.Dir = %d, want CDFS4_FORE (%d)", res.Dir, types.CDFS4_FORE)
	}
	if res.SessionID != sessionID {
		t.Error("res.SessionID mismatch")
	}
	if res.UseConnInRDMAMode {
		t.Error("res.UseConnInRDMAMode should be false")
	}
}

func TestBindConnToSession_Success_Both(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 400

	// Request FORE_OR_BOTH -- generous policy should return BOTH
	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE_OR_BOTH, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("both"), 1, ops)

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
		t.Errorf("res.Dir = %d, want CDFS4_BOTH (%d) (generous policy)", res.Dir, types.CDFS4_BOTH)
	}
}

func TestBindConnToSession_RDMA_Rejected(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 500

	// Request RDMA mode -- should succeed but return UseConnInRDMAMode=false
	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, true)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("rdma"), 1, ops)

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
	if res.Status != types.NFS4_OK {
		t.Errorf("res.Status = %d, want NFS4_OK", res.Status)
	}
	if res.UseConnInRDMAMode {
		t.Error("res.UseConnInRDMAMode should be false (RDMA not supported)")
	}
}

func TestBindConnToSession_ZeroConnectionID(t *testing.T) {
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 0 // intentionally zero

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)
	ops := []compoundOp{{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs}}
	data := buildCompoundArgsWithOps([]byte("zero-conn"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_SERVERFAULT {
		t.Errorf("status = %d, want NFS4ERR_SERVERFAULT (%d)", decoded.Status, types.NFS4ERR_SERVERFAULT)
	}
}

func TestBindConnToSession_XDRDesync(t *testing.T) {
	// Verify BIND_CONN_TO_SESSION properly consumes its args without desyncing
	// the XDR reader -- PUTROOTFH after BIND_CONN_TO_SESSION should succeed.
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 600

	bcsArgs := encodeBindConnToSessionArgs(sessionID, types.CDFC4_FORE, false)

	ops := []compoundOp{
		{opCode: types.OP_BIND_CONN_TO_SESSION, data: bcsArgs},
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

	// First op: BIND_CONN_TO_SESSION
	op1Code, _ := xdr.DecodeUint32(reader)
	if op1Code != types.OP_BIND_CONN_TO_SESSION {
		t.Errorf("result[0] opcode = %d, want OP_BIND_CONN_TO_SESSION", op1Code)
	}
	var bcsRes types.BindConnToSessionRes
	_ = bcsRes.Decode(reader)
	if bcsRes.Status != types.NFS4_OK {
		t.Errorf("BIND_CONN_TO_SESSION status = %d, want NFS4_OK", bcsRes.Status)
	}

	// Second op: PUTROOTFH (only valid if XDR reader was not desynced)
	op2Code, _ := xdr.DecodeUint32(reader)
	if op2Code != types.OP_PUTROOTFH {
		t.Errorf("result[1] opcode = %d, want OP_PUTROOTFH", op2Code)
	}
	op2Status, _ := xdr.DecodeUint32(reader)
	if op2Status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", op2Status)
	}
}

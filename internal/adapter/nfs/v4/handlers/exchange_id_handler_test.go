package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// encodeExchangeIdArgs builds XDR-encoded EXCHANGE_ID args.
func encodeExchangeIdArgs(ownerID []byte, verifier [8]byte, flags uint32, sp4How uint32, implId []types.NfsImplId4) []byte {
	var buf bytes.Buffer

	// ClientOwner4: co_verifier (8 bytes) + co_ownerid (opaque)
	buf.Write(verifier[:])
	_ = xdr.WriteXDROpaque(&buf, ownerID)

	_ = xdr.WriteUint32(&buf, flags)

	spa := types.StateProtect4A{How: sp4How}
	_ = spa.Encode(&buf)

	// eia_client_impl_id<1>
	_ = xdr.WriteUint32(&buf, uint32(len(implId)))
	for i := range implId {
		_ = implId[i].Encode(&buf)
	}

	return buf.Bytes()
}

func TestHandleExchangeID_Success(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	ownerID := []byte("test-exchange-id-client")
	var verifier [8]byte
	copy(verifier[:], "testverf")

	args := encodeExchangeIdArgs(ownerID, verifier, 0, types.SP4_NONE, nil)
	ops := []compoundOp{
		{opCode: types.OP_EXCHANGE_ID, data: args},
	}
	data := buildCompoundArgsWithOps([]byte("exchid"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)

	status, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}

	_, _ = xdr.DecodeOpaque(reader) // tag

	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 1 {
		t.Fatalf("numResults = %d, want 1", numResults)
	}

	opCode, _ := xdr.DecodeUint32(reader)
	if opCode != types.OP_EXCHANGE_ID {
		t.Errorf("result opcode = %d, want OP_EXCHANGE_ID (%d)", opCode, types.OP_EXCHANGE_ID)
	}

	var res types.ExchangeIdRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode ExchangeIdRes: %v", err)
	}

	if res.Status != types.NFS4_OK {
		t.Errorf("ExchangeIdRes status = %d, want NFS4_OK", res.Status)
	}
	if res.ClientID == 0 {
		t.Error("ClientID should not be zero")
	}
	if res.SequenceID != 1 {
		t.Errorf("SequenceID = %d, want 1", res.SequenceID)
	}
	if res.Flags&types.EXCHGID4_FLAG_USE_NON_PNFS == 0 {
		t.Error("Flags should include EXCHGID4_FLAG_USE_NON_PNFS")
	}
	if res.Flags&types.EXCHGID4_FLAG_CONFIRMED_R != 0 {
		t.Error("Flags should NOT include CONFIRMED_R for new client")
	}
	if res.StateProtect.How != types.SP4_NONE {
		t.Errorf("StateProtect.How = %d, want SP4_NONE (%d)", res.StateProtect.How, types.SP4_NONE)
	}
	if len(res.ServerOwner.MajorID) == 0 {
		t.Error("ServerOwner.MajorID should not be empty")
	}
	if len(res.ServerScope) == 0 {
		t.Error("ServerScope should not be empty")
	}
	if len(res.ServerImplId) != 1 {
		t.Fatalf("ServerImplId length = %d, want 1", len(res.ServerImplId))
	}
	if res.ServerImplId[0].Name != "dittofs" {
		t.Errorf("ServerImplId Name = %q, want %q", res.ServerImplId[0].Name, "dittofs")
	}
}

func TestHandleExchangeID_SP4Rejected(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	tests := []struct {
		name string
		how  uint32
	}{
		{"SP4_MACH_CRED", types.SP4_MACH_CRED},
		{"SP4_SSV", types.SP4_SSV},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ownerID := []byte("sp4-test-client")
			var verifier [8]byte

			args := encodeExchangeIdArgs(ownerID, verifier, 0, tt.how, nil)
			ops := []compoundOp{
				{opCode: types.OP_EXCHANGE_ID, data: args},
			}
			data := buildCompoundArgsWithOps([]byte("sp4"), 1, ops)

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
			if decoded.NumResults != 1 {
				t.Fatalf("numResults = %d, want 1", decoded.NumResults)
			}
			if decoded.Results[0].OpCode != types.OP_EXCHANGE_ID {
				t.Errorf("result opcode = %d, want OP_EXCHANGE_ID", decoded.Results[0].OpCode)
			}
		})
	}
}

func TestHandleExchangeID_BadXDR(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Truncated args: not enough data for a valid EXCHANGE_ID
	truncatedArgs := []byte{0x00, 0x00, 0x00, 0x01}
	ops := []compoundOp{
		{opCode: types.OP_EXCHANGE_ID, data: truncatedArgs},
	}
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
	if decoded.Results[0].OpCode != types.OP_EXCHANGE_ID {
		t.Errorf("result opcode = %d, want OP_EXCHANGE_ID", decoded.Results[0].OpCode)
	}
}

func TestHandleExchangeID_Idempotent(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	ownerID := []byte("idempotent-handler-client")
	var verifier [8]byte
	copy(verifier[:], "testverf")

	args := encodeExchangeIdArgs(ownerID, verifier, 0, types.SP4_NONE, nil)

	// First call
	ops1 := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: args}}
	data1 := buildCompoundArgsWithOps([]byte("idem1"), 1, ops1)
	resp1, err := h.ProcessCompound(ctx, data1)
	if err != nil {
		t.Fatalf("ProcessCompound #1 error: %v", err)
	}

	// Second call with same args
	ops2 := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: args}}
	data2 := buildCompoundArgsWithOps([]byte("idem2"), 1, ops2)
	resp2, err := h.ProcessCompound(ctx, data2)
	if err != nil {
		t.Fatalf("ProcessCompound #2 error: %v", err)
	}

	clientID1 := extractClientIDFromResponse(t, resp1)
	clientID2 := extractClientIDFromResponse(t, resp2)

	if clientID1 != clientID2 {
		t.Errorf("ClientIDs differ (%d vs %d) for idempotent call", clientID1, clientID2)
	}
}

func TestHandleExchangeID_WithImplId(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	ownerID := []byte("impl-id-client")
	var verifier [8]byte
	implId := []types.NfsImplId4{
		{
			Domain: "kernel.org",
			Name:   "Linux NFS client",
			Date:   types.NFS4Time{Seconds: 1000, Nseconds: 0},
		},
	}

	args := encodeExchangeIdArgs(ownerID, verifier, 0, types.SP4_NONE, implId)
	ops := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: args}}
	data := buildCompoundArgsWithOps([]byte("impl"), 1, ops)

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
}

func TestHandleExchangeID_FollowedByPutRootFH(t *testing.T) {
	// Verify EXCHANGE_ID properly consumes its args without desyncing the
	// XDR reader -- PUTROOTFH after EXCHANGE_ID should succeed.
	h := newTestHandler()
	ctx := newTestCompoundContext()

	ownerID := []byte("desync-test")
	var verifier [8]byte
	args := encodeExchangeIdArgs(ownerID, verifier, 0, types.SP4_NONE, nil)

	ops := []compoundOp{
		{opCode: types.OP_EXCHANGE_ID, data: args},
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

	// First op: EXCHANGE_ID
	op1Code, _ := xdr.DecodeUint32(reader)
	if op1Code != types.OP_EXCHANGE_ID {
		t.Errorf("result[0] opcode = %d, want OP_EXCHANGE_ID", op1Code)
	}
	var res types.ExchangeIdRes
	_ = res.Decode(reader)
	if res.Status != types.NFS4_OK {
		t.Errorf("EXCHANGE_ID status = %d, want NFS4_OK", res.Status)
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

// extractClientIDFromResponse extracts the client ID from a single-op
// EXCHANGE_ID compound response.
func extractClientIDFromResponse(t *testing.T, resp []byte) uint64 {
	t.Helper()
	reader := bytes.NewReader(resp)

	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", status)
	}

	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults
	_, _ = xdr.DecodeUint32(reader) // opcode

	var res types.ExchangeIdRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode ExchangeIdRes: %v", err)
	}

	return res.ClientID
}

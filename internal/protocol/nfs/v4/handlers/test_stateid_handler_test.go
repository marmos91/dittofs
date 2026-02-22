package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

func TestHandleTestStateid(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		// Empty stateid array -- should return NFS4_OK with empty results
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		var tsBuf bytes.Buffer
		tsArgs := types.TestStateidArgs{Stateids: nil}
		_ = tsArgs.Encode(&tsBuf)

		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_TEST_STATEID, data: tsBuf.Bytes()},
		}
		data := buildCompoundArgsWithOps([]byte("ts-empty"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader) // overall status
		if overallStatus != types.NFS4_OK {
			t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
		}
		_, _ = xdr.DecodeOpaque(reader)          // tag
		numResults, _ := xdr.DecodeUint32(reader) // numResults
		if numResults != 2 {
			t.Fatalf("numResults = %d, want 2", numResults)
		}

		// Skip SEQUENCE result
		_, _ = xdr.DecodeUint32(reader) // SEQUENCE opcode
		var seqRes types.SequenceRes
		_ = seqRes.Decode(reader)

		// Decode TEST_STATEID result
		tsOpCode, _ := xdr.DecodeUint32(reader)
		if tsOpCode != types.OP_TEST_STATEID {
			t.Errorf("result opcode = %d, want OP_TEST_STATEID", tsOpCode)
		}
		var tsRes types.TestStateidRes
		if err := tsRes.Decode(reader); err != nil {
			t.Fatalf("decode TestStateidRes: %v", err)
		}
		if tsRes.Status != types.NFS4_OK {
			t.Errorf("TEST_STATEID status = %d, want NFS4_OK", tsRes.Status)
		}
		if len(tsRes.StatusCodes) != 0 {
			t.Errorf("StatusCodes length = %d, want 0", len(tsRes.StatusCodes))
		}
	})

	t.Run("mixed", func(t *testing.T) {
		// Mix of invalid stateids -- should return per-stateid error codes
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// Create two fake stateids (both invalid)
		sid1 := types.Stateid4{Seqid: 1}
		copy(sid1.Other[:], []byte{0x01, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06})
		sid2 := types.Stateid4{Seqid: 2}
		copy(sid2.Other[:], []byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB})

		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		var tsBuf bytes.Buffer
		tsArgs := types.TestStateidArgs{Stateids: []types.Stateid4{sid1, sid2}}
		_ = tsArgs.Encode(&tsBuf)

		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_TEST_STATEID, data: tsBuf.Bytes()},
		}
		data := buildCompoundArgsWithOps([]byte("ts-mixed"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader)
		if overallStatus != types.NFS4_OK {
			t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
		}
		_, _ = xdr.DecodeOpaque(reader)
		numResults, _ := xdr.DecodeUint32(reader)
		if numResults != 2 {
			t.Fatalf("numResults = %d, want 2", numResults)
		}

		// Skip SEQUENCE result
		_, _ = xdr.DecodeUint32(reader)
		var seqRes types.SequenceRes
		_ = seqRes.Decode(reader)

		// Decode TEST_STATEID result
		_, _ = xdr.DecodeUint32(reader) // opcode
		var tsRes types.TestStateidRes
		if err := tsRes.Decode(reader); err != nil {
			t.Fatalf("decode TestStateidRes: %v", err)
		}

		// Overall TEST_STATEID status is always NFS4_OK
		if tsRes.Status != types.NFS4_OK {
			t.Errorf("TEST_STATEID status = %d, want NFS4_OK", tsRes.Status)
		}
		// Should have 2 per-stateid status codes
		if len(tsRes.StatusCodes) != 2 {
			t.Fatalf("StatusCodes length = %d, want 2", len(tsRes.StatusCodes))
		}
		// Both should be error codes (not NFS4_OK) since stateids are invalid
		for i, code := range tsRes.StatusCodes {
			if code == types.NFS4_OK {
				t.Errorf("StatusCodes[%d] = NFS4_OK, want error for invalid stateid", i)
			}
		}
	})

	t.Run("bad_xdr", func(t *testing.T) {
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// SEQUENCE + truncated TEST_STATEID args
		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_TEST_STATEID, data: []byte{0xFF}}, // truncated
		}
		data := buildCompoundArgsWithOps([]byte("ts-badxdr"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader)
		if overallStatus != types.NFS4ERR_BADXDR {
			t.Errorf("overall status = %d, want NFS4ERR_BADXDR (%d)",
				overallStatus, types.NFS4ERR_BADXDR)
		}
	})
}

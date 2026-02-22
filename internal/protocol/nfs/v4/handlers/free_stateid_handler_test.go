package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

func TestHandleFreeStateid(t *testing.T) {
	t.Run("bad_stateid", func(t *testing.T) {
		// Free a stateid that doesn't exist
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// Build an invalid stateid
		invalidStateid := types.Stateid4{
			Seqid: 1,
		}
		copy(invalidStateid.Other[:], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		var fsBuf bytes.Buffer
		fsArgs := types.FreeStateidArgs{Stateid: invalidStateid}
		_ = fsArgs.Encode(&fsBuf)

		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_FREE_STATEID, data: fsBuf.Bytes()},
		}
		data := buildCompoundArgsWithOps([]byte("fs-bad"), 1, ops)

		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		// Decode response -- should have SEQUENCE OK + FREE_STATEID error
		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader)
		_, _ = xdr.DecodeOpaque(reader)           // tag
		numResults, _ := xdr.DecodeUint32(reader) // numResults

		if numResults < 2 {
			t.Logf("overall status = %d, numResults = %d", overallStatus, numResults)
		}

		// The status should be NFS4ERR_BAD_STATEID
		if overallStatus != types.NFS4ERR_BAD_STATEID {
			t.Errorf("overall status = %d, want NFS4ERR_BAD_STATEID (%d)",
				overallStatus, types.NFS4ERR_BAD_STATEID)
		}
	})

	t.Run("bad_xdr", func(t *testing.T) {
		h, sessionID := createTestSession(t)
		ctx := newTestCompoundContext()

		// SEQUENCE + truncated FREE_STATEID args (stateid needs 16 bytes)
		seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, false)
		ops := []compoundOp{
			{opCode: types.OP_SEQUENCE, data: seqArgs},
			{opCode: types.OP_FREE_STATEID, data: []byte{0x00, 0x01}}, // truncated
		}
		data := buildCompoundArgsWithOps([]byte("fs-badxdr"), 1, ops)

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

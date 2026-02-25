// Package handlers -- TEST_STATEID operation handler (op 55).
//
// TEST_STATEID tests a set of stateids for validity, returning per-stateid
// status codes. Per RFC 8881 Section 18.48.
// TEST_STATEID requires SEQUENCE (not session-exempt).
package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// handleTestStateid implements the TEST_STATEID operation
// (RFC 8881 Section 18.48).
//
// TEST_STATEID tests each stateid in the array and returns per-stateid
// status codes (not fail-on-first). The overall operation status is always
// NFS4_OK; individual stateid validity is reported in the status codes array.
//
// Uses RLock only in StateManager -- no lease renewal side effects per RFC 8881.
func (h *Handler) handleTestStateid(
	ctx *types.CompoundContext,
	_ *types.V41RequestContext,
	reader io.Reader,
) *types.CompoundResult {
	var args types.TestStateidArgs
	if err := args.Decode(reader); err != nil {
		logger.Debug("TEST_STATEID: decode error", "error", err, "client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_TEST_STATEID,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Delegate to StateManager for per-stateid validation
	statusCodes := h.StateManager.TestStateids(args.Stateids)

	// Encode response with per-stateid status codes
	res := &types.TestStateidRes{
		Status:      types.NFS4_OK,
		StatusCodes: statusCodes,
	}
	var buf bytes.Buffer
	if err := res.Encode(&buf); err != nil {
		logger.Error("TEST_STATEID: encode response error", "error", err)
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_TEST_STATEID,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	logger.Debug("TEST_STATEID: tested stateids",
		"count", len(args.Stateids),
		"client", ctx.ClientAddr)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_TEST_STATEID,
		Data:   buf.Bytes(),
	}
}

// Package handlers -- TEST_STATEID operation handler (op 55).
//
// TEST_STATEID tests a set of stateids for validity, returning per-stateid
// status codes. Per RFC 8881 Section 18.48.
// TEST_STATEID requires SEQUENCE (not session-exempt).
package v41handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// HandleTestStateid implements the TEST_STATEID operation (RFC 8881 Section 18.48).
// Tests each stateid in an array for validity, returning per-stateid status codes.
// Delegates to StateManager.TestStateid with RLock only (no lease renewal side effects).
// No side effects; read-only stateid probe (overall status always NFS4_OK; individual results vary).
// Errors: NFS4ERR_BADXDR (decode failure); per-stateid: NFS4ERR_BAD_STATEID, NFS4ERR_EXPIRED, etc.
func HandleTestStateid(
	d *Deps,
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
			Data:   EncodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Delegate to StateManager for per-stateid validation
	statusCodes := d.StateManager.TestStateids(args.Stateids)

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
			Data:   EncodeStatusOnly(types.NFS4ERR_SERVERFAULT),
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

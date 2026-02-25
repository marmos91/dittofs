package handlers

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nsm/xdr"
)

// HandlerResult contains the XDR-encoded response for NSM procedures.
// This type is used by all NSM handlers and the dispatch table.
type HandlerResult struct {
	// Data contains the XDR-encoded response
	Data []byte

	// NSMStatus is the NSM protocol status code
	NSMStatus uint32
}

// Null handles the SM_NULL procedure (procedure 0).
//
// SM_NULL is a ping/health check procedure that takes no arguments
// and returns an empty response. It is used to verify the NSM service
// is running and reachable.
//
// Parameters:
//   - ctx: The NSM handler context
//
// Returns:
//   - *HandlerResult: Empty response data with STAT_SUCC status
//   - error: Always nil for NULL procedure
func (h *Handler) Null(ctx *NSMHandlerContext) (*HandlerResult, error) {
	logger.Debug("NSM NULL request",
		"client", ctx.ClientAddr)

	// SM_NULL returns void - just an empty success response
	// Some implementations return sm_stat_res, but void is per-spec
	return &HandlerResult{
		Data:      []byte{},
		NSMStatus: types.StatSucc,
	}, nil
}

// Stat handles the SM_STAT procedure (procedure 1).
//
// SM_STAT queries the current state of the NSM without establishing
// monitoring. It returns the server's current state counter.
//
// The state counter follows these conventions:
//   - Odd values: Server is up
//   - Even values: Server went down
//
// Parameters:
//   - ctx: The NSM handler context
//   - data: XDR-encoded sm_name (host to query)
//
// Returns:
//   - *HandlerResult: sm_stat_res with current state
//   - error: XDR decoding error if input is malformed
func (h *Handler) Stat(ctx *NSMHandlerContext, data []byte) (*HandlerResult, error) {
	// Decode sm_name argument
	r := newBytesReader(data)
	smName, err := xdr.DecodeSmName(r)
	if err != nil {
		logger.Warn("NSM STAT decode error",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatFailure(h.GetServerState())
	}

	logger.Debug("NSM STAT request",
		"client", ctx.ClientAddr,
		"mon_name", smName.Name)

	// Return current state without registering
	state := h.GetServerState()

	response := &types.SMStatRes{
		Result: types.StatSucc,
		State:  state,
	}

	encoded, err := xdr.EncodeSMStatRes(response)
	if err != nil {
		logger.Error("NSM STAT encode error",
			"client", ctx.ClientAddr,
			"error", err)
		return encodeStatFailure(state)
	}

	logger.Debug("NSM STAT response",
		"client", ctx.ClientAddr,
		"state", state)

	return &HandlerResult{
		Data:      encoded,
		NSMStatus: types.StatSucc,
	}, nil
}

// encodeStatFailure returns a STAT_FAIL response with the current state.
func encodeStatFailure(state int32) (*HandlerResult, error) {
	response := &types.SMStatRes{
		Result: types.StatFail,
		State:  state,
	}

	encoded, err := xdr.EncodeSMStatRes(response)
	if err != nil {
		// If we can't encode a failure response, return empty data
		return &HandlerResult{
			Data:      []byte{},
			NSMStatus: types.StatFail,
		}, nil
	}

	return &HandlerResult{
		Data:      encoded,
		NSMStatus: types.StatFail,
	}, nil
}

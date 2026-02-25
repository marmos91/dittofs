package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
)

// HandlerResult contains the XDR-encoded response for NSM procedures.
// This type is used by all NSM handlers and the dispatch table.
type HandlerResult struct {
	// Data contains the XDR-encoded response
	Data []byte

	// NSMStatus is the NSM protocol status code
	NSMStatus uint32
}

// Null handles NSM NULL (RFC 1813, SM procedure 0).
// No-op ping/health check verifying the NSM service is running and reachable.
// No delegation; returns immediately with empty response.
// No side effects; stateless operation.
// Errors: none (NULL always succeeds).
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

// Stat handles NSM STAT (RFC 1813, SM procedure 1).
// Queries the current NSM state counter without establishing monitoring (read-only).
// No delegation; reads server state counter directly from Handler.
// No side effects; does not register any monitoring callback.
// Errors: STAT_FAIL (XDR decode error or internal failure).
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

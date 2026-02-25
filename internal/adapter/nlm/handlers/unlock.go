package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/adapter/nlm/xdr"
)

// UnlockRequest represents an NLM_UNLOCK request.
type UnlockRequest struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Lock contains the lock parameters to release.
	Lock types.NLM4Lock
}

// UnlockResponse represents an NLM_UNLOCK response.
type UnlockResponse struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is the result of the operation.
	// Per NLM specification: always NLM4Granted, even for non-existent locks.
	Status uint32
}

// DecodeUnlockRequest decodes an NLM_UNLOCK request from XDR format.
func DecodeUnlockRequest(data []byte) (*UnlockRequest, error) {
	r := bytes.NewReader(data)
	args, err := nlm_xdr.DecodeNLM4UnlockArgs(r)
	if err != nil {
		return nil, fmt.Errorf("decode NLM4UnlockArgs: %w", err)
	}

	return &UnlockRequest{
		Cookie: args.Cookie,
		Lock:   args.Lock,
	}, nil
}

// EncodeUnlockResponse encodes an NLM_UNLOCK response to XDR format.
func EncodeUnlockResponse(resp *UnlockResponse) ([]byte, error) {
	buf := new(bytes.Buffer)

	res := &types.NLM4Res{
		Cookie: resp.Cookie,
		Status: resp.Status,
	}

	if err := nlm_xdr.EncodeNLM4Res(buf, res); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Unlock handles the NLM_UNLOCK procedure (procedure 4).
//
// NLM_UNLOCK releases a previously acquired lock.
//
// Per NLM specification and CONTEXT.md:
//   - Unlock of non-existent lock silently succeeds (returns NLM4Granted)
//   - This ensures idempotency for retried unlock requests
//   - The Exclusive flag in the lock is ignored on unlock
//
// Parameters:
//   - ctx: The NLM handler context with auth and client info
//   - req: The UNLOCK request containing lock parameters
//
// Returns:
//   - *UnlockResponse: Always NLM4Granted unless system error
//   - error: System-level errors only
func (h *Handler) Unlock(ctx *NLMHandlerContext, req *UnlockRequest) (*UnlockResponse, error) {
	// Build owner ID
	ownerID := buildOwnerID(req.Lock.CallerName, req.Lock.Svid, req.Lock.OH)

	logger.Debug("NLM UNLOCK",
		"client", ctx.ClientAddr,
		"caller", req.Lock.CallerName,
		"owner", ownerID,
		"offset", req.Lock.Offset,
		"length", req.Lock.Length)

	// Convert file handle
	handle := req.Lock.FH

	// Call NLMService to release lock
	// Per NLM spec: unlock of non-existent lock silently succeeds
	err := h.nlmService.UnlockFileNLM(
		ctx.Context,
		handle,
		ownerID,
		req.Lock.Offset,
		req.Lock.Length,
	)

	if err != nil {
		// System error - but still return granted for idempotency
		logger.Warn("NLM UNLOCK error (returning granted)",
			"client", ctx.ClientAddr,
			"error", err)
	}

	logger.Debug("NLM UNLOCK granted",
		"client", ctx.ClientAddr,
		"owner", ownerID)

	return &UnlockResponse{
		Cookie: req.Cookie,
		Status: types.NLM4Granted,
	}, nil
}

package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/protocol/nlm/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CancelRequest represents an NLM_CANCEL request.
type CancelRequest struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Block must match the Block value from the original LOCK request.
	Block bool

	// Exclusive must match the Exclusive value from the original LOCK request.
	Exclusive bool

	// Lock contains the lock parameters to cancel.
	Lock types.NLM4Lock
}

// CancelResponse represents an NLM_CANCEL response.
type CancelResponse struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is the result of the operation.
	Status uint32
}

// DecodeCancelRequest decodes an NLM_CANCEL request from XDR format.
func DecodeCancelRequest(data []byte) (*CancelRequest, error) {
	r := bytes.NewReader(data)
	args, err := nlm_xdr.DecodeNLM4CancelArgs(r)
	if err != nil {
		return nil, fmt.Errorf("decode NLM4CancelArgs: %w", err)
	}

	return &CancelRequest{
		Cookie:    args.Cookie,
		Block:     args.Block,
		Exclusive: args.Exclusive,
		Lock:      args.Lock,
	}, nil
}

// EncodeCancelResponse encodes an NLM_CANCEL response to XDR format.
func EncodeCancelResponse(resp *CancelResponse) ([]byte, error) {
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

// Cancel handles the NLM_CANCEL procedure (procedure 3).
//
// NLM_CANCEL removes a pending (blocked) lock request from the server's wait queue.
// This is used when a client times out waiting for a lock or wants to abort
// a blocking lock request.
//
// Per NLM specification and CONTEXT.md:
//   - Returns NLM4Granted if cancellation is processed (whether or not
//     the lock request existed in the queue)
//   - The Block and Exclusive flags should match the original LOCK request
//
// Parameters:
//   - ctx: The NLM handler context with auth and client info
//   - req: The CANCEL request containing lock parameters
//
// Returns:
//   - *CancelResponse: Status indicating result (always NLM4Granted)
//   - error: System-level errors only
func (h *Handler) Cancel(ctx *NLMHandlerContext, req *CancelRequest) (*CancelResponse, error) {
	// Build owner ID
	ownerID := buildOwnerID(req.Lock.CallerName, req.Lock.Svid, req.Lock.OH)

	logger.Debug("NLM CANCEL",
		"client", ctx.ClientAddr,
		"caller", req.Lock.CallerName,
		"owner", ownerID,
		"block", req.Block,
		"exclusive", req.Exclusive,
		"offset", req.Lock.Offset,
		"length", req.Lock.Length)

	// Convert file handle
	handle := metadata.FileHandle(req.Lock.FH)
	handleKey := string(handle)

	// Try to cancel from blocking queue
	cancelled := h.blockingQueue.Cancel(handleKey, ownerID, req.Lock.Offset, req.Lock.Length)
	if cancelled {
		logger.Debug("NLM CANCEL found and cancelled waiter",
			"client", ctx.ClientAddr,
			"owner", ownerID)
	} else {
		logger.Debug("NLM CANCEL no waiter found (already processed or never queued)",
			"client", ctx.ClientAddr,
			"owner", ownerID)
	}

	// Per NLM specification: always return NLM4Granted for idempotency
	return &CancelResponse{
		Cookie: req.Cookie,
		Status: types.NLM4Granted,
	}, nil
}

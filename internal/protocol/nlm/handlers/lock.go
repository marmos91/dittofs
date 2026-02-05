package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/protocol/nlm/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// LockRequest represents an NLM_LOCK request.
type LockRequest struct {
	// Cookie is an opaque value echoed back in the response.
	Cookie []byte

	// Block indicates whether to block waiting for the lock.
	// If true and lock conflicts, server queues request and calls back via GRANTED.
	// If false and lock conflicts, server returns NLM4Denied immediately.
	Block bool

	// Exclusive indicates the lock type.
	// true = exclusive (write) lock
	// false = shared (read) lock
	Exclusive bool

	// Lock contains the lock parameters.
	Lock types.NLM4Lock

	// Reclaim indicates this is a lock reclaim during grace period.
	Reclaim bool

	// State is the NSM state counter for crash recovery.
	State int32
}

// LockResponse represents an NLM_LOCK response.
type LockResponse struct {
	// Cookie is echoed from the request.
	Cookie []byte

	// Status is the result of the operation.
	Status uint32
}

// DecodeLockRequest decodes an NLM_LOCK request from XDR format.
func DecodeLockRequest(data []byte) (*LockRequest, error) {
	r := bytes.NewReader(data)
	args, err := nlm_xdr.DecodeNLM4LockArgs(r)
	if err != nil {
		return nil, fmt.Errorf("decode NLM4LockArgs: %w", err)
	}

	return &LockRequest{
		Cookie:    args.Cookie,
		Block:     args.Block,
		Exclusive: args.Exclusive,
		Lock:      args.Lock,
		Reclaim:   args.Reclaim,
		State:     args.State,
	}, nil
}

// EncodeLockResponse encodes an NLM_LOCK response to XDR format.
func EncodeLockResponse(resp *LockResponse) ([]byte, error) {
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

// Lock handles the NLM_LOCK procedure (procedure 2).
//
// NLM_LOCK acquires an advisory lock on a byte range of a file.
//
// Behavior:
//   - Non-blocking (Block=false): Returns NLM4Granted on success, NLM4Denied on conflict
//   - Blocking (Block=true): Returns NLM4Blocked if conflict exists (actual blocking
//     handled in Plan 02-03 with blocking queue)
//   - Reclaim (Reclaim=true): Uses reclaim path during grace period
//   - During grace period with Reclaim=false: Returns NLM4DeniedGrace
//
// Parameters:
//   - ctx: The NLM handler context with auth and client info
//   - req: The LOCK request containing lock parameters
//
// Returns:
//   - *LockResponse: Status indicating result
//   - error: System-level errors only
func (h *Handler) Lock(ctx *NLMHandlerContext, req *LockRequest) (*LockResponse, error) {
	// Build owner ID following format: nlm:{caller_name}:{svid}:{oh_hex}
	ownerID := buildOwnerID(req.Lock.CallerName, req.Lock.Svid, req.Lock.OH)

	logger.Debug("NLM LOCK",
		"client", ctx.ClientAddr,
		"caller", req.Lock.CallerName,
		"owner", ownerID,
		"exclusive", req.Exclusive,
		"block", req.Block,
		"reclaim", req.Reclaim,
		"offset", req.Lock.Offset,
		"length", req.Lock.Length)

	// Convert file handle
	handle := metadata.FileHandle(req.Lock.FH)

	// Build lock owner
	owner := lock.LockOwner{
		OwnerID:  ownerID,
		ClientID: ctx.ClientAddr,
	}

	// Call MetadataService to acquire lock
	result, err := h.metadataService.LockFileNLM(
		ctx.Context,
		handle,
		owner,
		req.Lock.Offset,
		req.Lock.Length,
		req.Exclusive,
		req.Reclaim,
	)

	if err != nil {
		// System error
		logger.Warn("NLM LOCK failed",
			"client", ctx.ClientAddr,
			"error", err)
		return &LockResponse{
			Cookie: req.Cookie,
			Status: types.NLM4Failed,
		}, nil
	}

	if result.Success {
		logger.Debug("NLM LOCK granted",
			"client", ctx.ClientAddr,
			"owner", ownerID)
		return &LockResponse{
			Cookie: req.Cookie,
			Status: types.NLM4Granted,
		}, nil
	}

	// Lock conflict
	if req.Block {
		// Blocking request - return NLM4Blocked
		// Actual blocking queue handling is in Plan 02-03
		logger.Debug("NLM LOCK blocked",
			"client", ctx.ClientAddr,
			"owner", ownerID)
		return &LockResponse{
			Cookie: req.Cookie,
			Status: types.NLM4Blocked,
		}, nil
	}

	// Non-blocking request with conflict - return denied
	logger.Debug("NLM LOCK denied",
		"client", ctx.ClientAddr,
		"owner", ownerID)
	return &LockResponse{
		Cookie: req.Cookie,
		Status: types.NLM4Denied,
	}, nil
}

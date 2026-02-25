package handlers

import (
	"bytes"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/xdr"
	"github.com/marmos91/dittofs/internal/logger"
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

// Lock handles NLM LOCK (RFC 1813, NLM procedure 2).
// Acquires an advisory byte-range lock; supports blocking, non-blocking, and reclaim modes.
// Delegates to NLMLockService.Lock and checks SMB lease conflicts via cross-protocol handler.
// Modifies lock state in LockManager; queues blocked requests in BlockingQueue.
// Errors: NLM4Denied (conflict), NLM4Blocked (queued), NLM4DeniedGrace (grace period).
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
	handle := req.Lock.FH

	// Build lock owner
	owner := lock.LockOwner{
		OwnerID:  ownerID,
		ClientID: ctx.ClientAddr,
	}

	// Call NLMService to acquire lock (cross-protocol lease checks happen at lock manager level)
	result, err := h.nlmService.LockFileNLM(
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
		// Blocking request - queue the waiter
		waiter := &blocking.Waiter{
			Lock: &lock.UnifiedLock{
				Owner:      owner,
				FileHandle: lock.FileHandle(handle),
				Offset:     req.Lock.Offset,
				Length:     req.Lock.Length,
			},
			Cookie:       req.Cookie,
			Exclusive:    req.Exclusive,
			CallbackAddr: extractCallbackAddr(ctx.ClientAddr),
			CallbackProg: types.ProgramNLM,
			CallbackVers: types.NLMVersion4,
			CallerName:   req.Lock.CallerName,
			Svid:         req.Lock.Svid,
			OH:           req.Lock.OH,
			FileHandle:   req.Lock.FH,
		}

		// Try to queue the waiter
		handleKey := string(handle)
		if err := h.blockingQueue.Enqueue(handleKey, waiter); err != nil {
			if err == blocking.ErrQueueFull {
				// Per CONTEXT.md: queue full returns NLM4_DENIED_NOLOCKS
				logger.Warn("NLM LOCK queue full",
					"client", ctx.ClientAddr,
					"owner", ownerID)
				return &LockResponse{
					Cookie: req.Cookie,
					Status: types.NLM4DeniedNoLocks,
				}, nil
			}
			// Other queue error
			logger.Warn("NLM LOCK queue error",
				"client", ctx.ClientAddr,
				"error", err)
			return &LockResponse{
				Cookie: req.Cookie,
				Status: types.NLM4Failed,
			}, nil
		}

		logger.Debug("NLM LOCK queued",
			"client", ctx.ClientAddr,
			"owner", ownerID)
		return &LockResponse{
			Cookie: req.Cookie,
			Status: types.NLM4Blocked,
		}, nil
	}

	// Non-blocking request with conflict - check if it's SMB lease or byte-range lock
	// Per CONTEXT.md: Return holder info for cross-protocol conflicts
	if result.Conflict != nil && result.Conflict.Lock != nil {
		if result.Conflict.Lock.IsLease() {
			// Conflict is with SMB lease - build response with SMB holder info
			logger.Info("NLM LOCK denied by SMB lease",
				"client", ctx.ClientAddr,
				"owner", ownerID,
				"lease_state", result.Conflict.Lock.Lease.StateString())
			// Record cross-protocol conflict metric
			lock.RecordCrossProtocolConflict(lock.InitiatorNFS, lock.ConflictingSMBLease, lock.ResolutionDenied)
			return buildDeniedResponseFromSMBLease(req.Cookie, result.Conflict.Lock), nil
		}
		// Conflict is with byte-range lock - build response with lock holder info
		logger.Debug("NLM LOCK denied by byte-range lock",
			"client", ctx.ClientAddr,
			"owner", ownerID)
		return buildDeniedResponseFromByteRangeLock(req.Cookie, result.Conflict.Lock), nil
	}

	// Generic conflict without detailed info
	logger.Debug("NLM LOCK denied",
		"client", ctx.ClientAddr,
		"owner", ownerID)
	return &LockResponse{
		Cookie: req.Cookie,
		Status: types.NLM4Denied,
	}, nil
}

// extractCallbackAddr constructs the callback address from the client address.
//
// Per NLM protocol, the callback is sent to the client's IP with the standard
// NLM port (same as the main NLM port). Some implementations use a separate
// callback port, but most use the same port.
//
// Parameters:
//   - clientAddr: Client address in "host:port" format
//
// Returns the callback address in "host:port" format using standard NLM port.
func extractCallbackAddr(clientAddr string) string {
	host, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		// If we can't parse, use the original address
		return clientAddr
	}
	// Use the standard NLM port (same as NFS port typically)
	// NLM callbacks go to the same port the client is listening on
	// which is typically a dynamic port chosen by the client
	// For now, use the standard approach of connecting back to standard NLM port
	return net.JoinHostPort(host, "12049")
}

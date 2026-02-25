// Package nlm provides Network Lock Manager (NLM) protocol dispatch.
package nlm

import (
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nlm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nlm/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ============================================================================
// Handler Result Structure
// ============================================================================

// HandlerResult contains the XDR-encoded response and metadata about the operation.
type HandlerResult struct {
	// Data contains the XDR-encoded response to send to the client.
	Data []byte

	// NLMStatus is the NLM protocol status code for this operation.
	NLMStatus uint32
}

// ============================================================================
// Procedure Dispatch Types
// ============================================================================

// NLMProcedureHandler defines the signature for NLM procedure handlers.
type NLMProcedureHandler func(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error)

// NLMProcedure contains metadata about an NLM procedure for dispatch.
type NLMProcedure struct {
	// Name is the procedure name for logging (e.g., "NULL", "LOCK")
	Name string

	// Handler is the function that processes this procedure
	Handler NLMProcedureHandler

	// NeedsAuth indicates whether this procedure requires authentication.
	NeedsAuth bool
}

// NLMDispatchTable maps NLM procedure numbers to their handlers.
var NLMDispatchTable map[uint32]*NLMProcedure

// init initializes the NLM procedure dispatch table.
func init() {
	initNLMDispatchTable()
}

// ============================================================================
// NLM Dispatch Table Initialization
// ============================================================================

func initNLMDispatchTable() {
	NLMDispatchTable = map[uint32]*NLMProcedure{
		types.NLMProcNull: {
			Name:      "NULL",
			Handler:   handleNLMNull,
			NeedsAuth: false,
		},
		types.NLMProcTest: {
			Name:      "TEST",
			Handler:   handleNLMTest,
			NeedsAuth: true,
		},
		types.NLMProcLock: {
			Name:      "LOCK",
			Handler:   handleNLMLock,
			NeedsAuth: true,
		},
		types.NLMProcCancel: {
			Name:      "CANCEL",
			Handler:   handleNLMCancel,
			NeedsAuth: true,
		},
		types.NLMProcUnlock: {
			Name:      "UNLOCK",
			Handler:   handleNLMUnlock,
			NeedsAuth: true,
		},
		types.NLMProcShare: {
			Name:      "SHARE",
			Handler:   handleNLMShare,
			NeedsAuth: true,
		},
		types.NLMProcUnshare: {
			Name:      "UNSHARE",
			Handler:   handleNLMUnshare,
			NeedsAuth: true,
		},
		types.NLMProcFreeAll: {
			Name:      "FREE_ALL",
			Handler:   handleNLMFreeAll,
			NeedsAuth: false, // FREE_ALL is called by rpc.statd, uses AUTH_NULL
		},
	}
}

// ============================================================================
// NLM Procedure Handler Wrappers
// ============================================================================

func handleNLMNull(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeNullRequest(data)
	if err != nil {
		logger.Debug("NLM NULL decode error", "error", err)
		// NULL procedure has no failure response - return empty success
		encoded, _ := handlers.EncodeNullResponse(&handlers.NullResponse{Status: types.NLM4Granted})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Granted}, nil
	}

	resp, err := handler.Null(ctx, req)
	if err != nil {
		logger.Debug("NLM NULL handler error", "error", err)
		encoded, _ := handlers.EncodeNullResponse(&handlers.NullResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeNullResponse(resp)
	if err != nil {
		logger.Debug("NLM NULL encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

func handleNLMTest(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeTestRequest(data)
	if err != nil {
		logger.Debug("NLM TEST decode error", "error", err)
		encoded, _ := handlers.EncodeTestResponse(&handlers.TestResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Test(ctx, req)
	if err != nil {
		logger.Debug("NLM TEST handler error", "error", err)
		encoded, _ := handlers.EncodeTestResponse(&handlers.TestResponse{Cookie: req.Cookie, Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeTestResponse(resp)
	if err != nil {
		logger.Debug("NLM TEST encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

func handleNLMLock(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeLockRequest(data)
	if err != nil {
		logger.Debug("NLM LOCK decode error", "error", err)
		encoded, _ := handlers.EncodeLockResponse(&handlers.LockResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Lock(ctx, req)
	if err != nil {
		logger.Debug("NLM LOCK handler error", "error", err)
		encoded, _ := handlers.EncodeLockResponse(&handlers.LockResponse{Cookie: req.Cookie, Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeLockResponse(resp)
	if err != nil {
		logger.Debug("NLM LOCK encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

func handleNLMCancel(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeCancelRequest(data)
	if err != nil {
		logger.Debug("NLM CANCEL decode error", "error", err)
		encoded, _ := handlers.EncodeCancelResponse(&handlers.CancelResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Cancel(ctx, req)
	if err != nil {
		logger.Debug("NLM CANCEL handler error", "error", err)
		encoded, _ := handlers.EncodeCancelResponse(&handlers.CancelResponse{Cookie: req.Cookie, Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeCancelResponse(resp)
	if err != nil {
		logger.Debug("NLM CANCEL encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

func handleNLMUnlock(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeUnlockRequest(data)
	if err != nil {
		logger.Debug("NLM UNLOCK decode error", "error", err)
		encoded, _ := handlers.EncodeUnlockResponse(&handlers.UnlockResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Unlock(ctx, req)
	if err != nil {
		logger.Debug("NLM UNLOCK handler error", "error", err)
		encoded, _ := handlers.EncodeUnlockResponse(&handlers.UnlockResponse{Cookie: req.Cookie, Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeUnlockResponse(resp)
	if err != nil {
		logger.Debug("NLM UNLOCK encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

func handleNLMFreeAll(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	// FREE_ALL needs the raw data bytes since it decodes directly
	ctx.Data = data

	encoded, err := handler.FreeAll(ctx)
	if err != nil {
		logger.Debug("NLM FREE_ALL handler error", "error", err)
		// FREE_ALL returns void, so we still return empty response
		return &HandlerResult{Data: []byte{}, NLMStatus: types.NLM4Granted}, err
	}

	// FREE_ALL returns void per NLM spec
	return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Granted}, nil
}

func handleNLMShare(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeShareRequest(data)
	if err != nil {
		logger.Debug("NLM SHARE decode error", "error", err)
		encoded, _ := handlers.EncodeShareResponse(&handlers.ShareResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Share(ctx, req)
	if err != nil {
		logger.Debug("NLM SHARE handler error", "error", err)
		encoded, _ := handlers.EncodeShareResponse(&handlers.ShareResponse{Cookie: req.Cookie, Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeShareResponse(resp)
	if err != nil {
		logger.Debug("NLM SHARE encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

func handleNLMUnshare(
	ctx *handlers.NLMHandlerContext,
	handler *handlers.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	req, err := handlers.DecodeShareRequest(data)
	if err != nil {
		logger.Debug("NLM UNSHARE decode error", "error", err)
		encoded, _ := handlers.EncodeShareResponse(&handlers.ShareResponse{Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Unshare(ctx, req)
	if err != nil {
		logger.Debug("NLM UNSHARE handler error", "error", err)
		encoded, _ := handlers.EncodeShareResponse(&handlers.ShareResponse{Cookie: req.Cookie, Status: types.NLM4Failed})
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeShareResponse(resp)
	if err != nil {
		logger.Debug("NLM UNSHARE encode error", "error", err)
		return &HandlerResult{Data: nil, NLMStatus: types.NLM4Failed}, err
	}

	return &HandlerResult{Data: encoded, NLMStatus: resp.Status}, nil
}

// ============================================================================
// Status Code Helpers
// ============================================================================

// NLMStatusToString returns a human-readable string for an NLM status code.
func NLMStatusToString(status uint32) string {
	return types.StatusString(status)
}

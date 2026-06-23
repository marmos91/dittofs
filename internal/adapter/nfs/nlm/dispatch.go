// Package nlm provides Network Lock Manager (NLM) protocol dispatch.
package nlm

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ============================================================================
// Handler Result Structure
// ============================================================================

// HandlerResult contains the XDR-encoded response and metadata about the operation.
type HandlerResult struct {
	// Data contains the XDR-encoded response to send to the client as the inline
	// RPC reply. For asynchronous (_MSG) procedures this is empty (the reply is
	// void) and the result is delivered via AsyncRes instead.
	Data []byte

	// NLMStatus is the NLM protocol status code for this operation.
	NLMStatus uint32

	// AsyncRes, when non-nil, marks this as an asynchronous (_MSG) operation:
	// the transport must reply to the call with a void body and instead send an
	// NLM *_RES callback (AsyncRes.Proc) carrying AsyncRes.Body to the client.
	// macOS/BSD lockd use the async procedures (TEST_MSG/LOCK_MSG/...).
	AsyncRes *AsyncResult
}

// AsyncResult carries the NLM *_RES callback the transport must deliver for an
// asynchronous (_MSG) request: the *_RES procedure number and its XDR body
// (identical to the corresponding synchronous reply body).
type AsyncResult struct {
	Proc uint32
	Body []byte
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
			Name:    "NULL",
			Handler: handleNLMNull,
		},
		types.NLMProcTest: {
			Name:    "TEST",
			Handler: handleNLMTest,
		},
		types.NLMProcLock: {
			Name:    "LOCK",
			Handler: handleNLMLock,
		},
		types.NLMProcCancel: {
			Name:    "CANCEL",
			Handler: handleNLMCancel,
		},
		types.NLMProcUnlock: {
			Name:    "UNLOCK",
			Handler: handleNLMUnlock,
		},
		types.NLMProcShare: {
			Name:    "SHARE",
			Handler: handleNLMShare,
		},
		types.NLMProcUnshare: {
			Name:    "UNSHARE",
			Handler: handleNLMUnshare,
		},
		types.NLMProcFreeAll: {
			Name:    "FREE_ALL",
			Handler: handleNLMFreeAll,
		},
		// Asynchronous (callback-style) procedures. macOS/BSD lockd use these:
		// the client sends a *_MSG, the server replies void, and the result is
		// delivered as a *_RES callback to the client's NLM address.
		types.NLMProcTestMsg: {
			Name:    "TEST_MSG",
			Handler: handleNLMTestMsg,
		},
		types.NLMProcLockMsg: {
			Name:    "LOCK_MSG",
			Handler: handleNLMLockMsg,
		},
		types.NLMProcCancelMsg: {
			Name:    "CANCEL_MSG",
			Handler: handleNLMCancelMsg,
		},
		types.NLMProcUnlockMsg: {
			Name:    "UNLOCK_MSG",
			Handler: handleNLMUnlockMsg,
		},
	}
}

// asAsync converts a synchronous handler result into an asynchronous (_MSG)
// result: the inline RPC reply becomes void and the encoded result is instead
// delivered as the given NLM *_RES callback (TEST_RES/LOCK_RES/...). The _RES
// body is byte-for-byte the synchronous reply, so the async procedures reuse
// the sync decode, processing, and encoding unchanged.
func asAsync(res *HandlerResult, err error, resProc uint32) (*HandlerResult, error) {
	if res == nil {
		return res, err
	}
	return &HandlerResult{
		NLMStatus: res.NLMStatus,
		AsyncRes:  &AsyncResult{Proc: resProc, Body: res.Data},
	}, err
}

func handleNLMTestMsg(ctx *handlers.NLMHandlerContext, h *handlers.Handler, reg *runtime.Runtime, data []byte) (*HandlerResult, error) {
	res, err := handleNLMTest(ctx, h, reg, data)
	return asAsync(res, err, types.NLMProcTestRes)
}

func handleNLMLockMsg(ctx *handlers.NLMHandlerContext, h *handlers.Handler, reg *runtime.Runtime, data []byte) (*HandlerResult, error) {
	res, err := handleNLMLock(ctx, h, reg, data)
	return asAsync(res, err, types.NLMProcLockRes)
}

func handleNLMCancelMsg(ctx *handlers.NLMHandlerContext, h *handlers.Handler, reg *runtime.Runtime, data []byte) (*HandlerResult, error) {
	res, err := handleNLMCancel(ctx, h, reg, data)
	return asAsync(res, err, types.NLMProcCancelRes)
}

func handleNLMUnlockMsg(ctx *handlers.NLMHandlerContext, h *handlers.Handler, reg *runtime.Runtime, data []byte) (*HandlerResult, error) {
	res, err := handleNLMUnlock(ctx, h, reg, data)
	return asAsync(res, err, types.NLMProcUnlockRes)
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
	req, err := handlers.DecodeTestRequest(data, ctx.Version)
	if err != nil {
		logger.Debug("NLM TEST decode error", "error", err)
		encoded, _ := handlers.EncodeTestResponse(&handlers.TestResponse{Status: types.NLM4Failed}, ctx.Version)
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	resp, err := handler.Test(ctx, req)
	if err != nil {
		logger.Debug("NLM TEST handler error", "error", err)
		encoded, _ := handlers.EncodeTestResponse(&handlers.TestResponse{Cookie: req.Cookie, Status: types.NLM4Failed}, ctx.Version)
		return &HandlerResult{Data: encoded, NLMStatus: types.NLM4Failed}, err
	}

	encoded, err := handlers.EncodeTestResponse(resp, ctx.Version)
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
	req, err := handlers.DecodeLockRequest(data, ctx.Version)
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
	req, err := handlers.DecodeCancelRequest(data, ctx.Version)
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
	req, err := handlers.DecodeUnlockRequest(data, ctx.Version)
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

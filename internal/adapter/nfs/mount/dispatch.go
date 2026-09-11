// Package mount provides Mount protocol (program 100005) dispatch: the
// procedure table, the six procedure handlers, and the XDR encode/dispatch
// seam shared with the NFSv3 handlers.
package mount

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ============================================================================
// Handler Result Structure
// ============================================================================

// MountResult contains the XDR-encoded response for a Mount procedure call.
// Mount handlers return only response bytes: every Mount procedure embeds
// MountResponseBase, so the status code is already inside Data and no
// separate status field is needed for metrics.
type MountResult struct {
	// Data contains the XDR-encoded response to send to the client.
	Data []byte
}

// ============================================================================
// Procedure Dispatch Types
// ============================================================================

// MountProcedureHandler defines the signature for Mount procedure handlers.
//
// Handlers return (*MountResult, error) where error carries system-level
// failures only (context cancelled, I/O errors); protocol errors are encoded
// in the response.
type MountProcedureHandler func(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error)

// MountProcedure contains metadata about a Mount procedure for dispatch.
type MountProcedure struct {
	// Name is the procedure name for logging (e.g., "NULL", "MNT")
	Name string

	// Handler is the function that processes this procedure
	Handler MountProcedureHandler
}

// MountDispatchTable maps Mount procedure numbers to their handlers.
var MountDispatchTable map[uint32]*MountProcedure

// init initializes the Mount procedure dispatch table.
func init() {
	initMountDispatchTable()
}

// ============================================================================
// Mount Dispatch Table Initialization
// ============================================================================

func initMountDispatchTable() {
	MountDispatchTable = map[uint32]*MountProcedure{
		mount.MountProcNull: {
			Name:    "NULL",
			Handler: handleMountNull,
		},
		mount.MountProcMnt: {
			Name:    "MNT",
			Handler: handleMountMnt,
		},
		mount.MountProcDump: {
			Name:    "DUMP",
			Handler: handleMountDump,
		},
		mount.MountProcUmnt: {
			Name:    "UMNT",
			Handler: handleMountUmnt,
		},
		mount.MountProcUmntAll: {
			Name:    "UMNTALL",
			Handler: handleMountUmntAll,
		},
		mount.MountProcExport: {
			Name:    "EXPORT",
			Handler: handleMountExport,
		},
	}
}

// ============================================================================
// Mount Procedure Handlers
// ============================================================================
//
// Each Mount handler decodes the request, calls the handler, and encodes the
// response; a decode or handler error is answered with the Mount error
// response carrying the fallback status.

// mountResponse is the constraint every Mount response type satisfies. Mount
// request types carry no methods — only the response is encoded.
type mountResponse interface {
	*mount.MountResponse |
		*mount.NullResponse |
		*mount.UmountResponse |
		*mount.DumpResponse |
		*mount.UmountAllResponse |
		*mount.ExportResponse
	Encode() ([]byte, error)
	GetStatus() uint32
}

// handleRequest decodes data, runs handle, and encodes the response. Decode
// and handler errors are answered with makeErrorResp(fallbackStatus) — the
// Mount error reply — while the response's own status is used on success.
func handleRequest[Req any, Resp mountResponse](
	data []byte,
	decode func([]byte) (Req, error),
	handle func(Req) (Resp, error),
	fallbackStatus uint32,
	makeErrorResp func(uint32) Resp,
) (*MountResult, error) {
	req, err := decode(data)
	if err != nil {
		logger.Debug("Error decoding request", "error", err)
		errorResp := makeErrorResp(fallbackStatus)
		encoded, encErr := errorResp.Encode()
		if encErr != nil {
			return &MountResult{Data: nil}, encErr
		}
		return &MountResult{Data: encoded}, err
	}

	resp, err := handle(req)
	if err != nil {
		logger.Debug("Handler error", "error", err)
		errorResp := makeErrorResp(fallbackStatus)
		encoded, encErr := errorResp.Encode()
		if encErr != nil {
			return &MountResult{Data: nil}, encErr
		}
		return &MountResult{Data: encoded}, err
	}

	status := resp.GetStatus()
	encoded, err := resp.Encode()
	if err != nil {
		logger.Debug("Error encoding response", "error", err)
		errorResp := makeErrorResp(fallbackStatus)
		encodedErr, encErr := errorResp.Encode()
		if encErr != nil {
			return &MountResult{Data: nil}, encErr
		}
		return &MountResult{Data: encodedErr}, err
	}

	_ = status
	return &MountResult{Data: encoded}, nil
}

func handleMountNull(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error) {
	return handleRequest(
		data,
		mount.DecodeNullRequest,
		func(req *mount.NullRequest) (*mount.NullResponse, error) {
			return handler.MountNull(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.NullResponse {
			return &mount.NullResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountMnt(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error) {
	return handleRequest(
		data,
		mount.DecodeMountRequest,
		func(req *mount.MountRequest) (*mount.MountResponse, error) {
			return handler.Mount(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.MountResponse {
			return &mount.MountResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountDump(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error) {
	return handleRequest(
		data,
		mount.DecodeDumpRequest,
		func(req *mount.DumpRequest) (*mount.DumpResponse, error) {
			return handler.Dump(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.DumpResponse {
			return &mount.DumpResponse{MountResponseBase: mount.MountResponseBase{Status: status}, Entries: []mount.DumpEntry{}}
		},
	)
}

func handleMountUmnt(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error) {
	return handleRequest(
		data,
		mount.DecodeUmountRequest,
		func(req *mount.UmountRequest) (*mount.UmountResponse, error) {
			return handler.Umnt(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.UmountResponse {
			return &mount.UmountResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountUmntAll(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error) {
	return handleRequest(
		data,
		mount.DecodeUmountAllRequest,
		func(req *mount.UmountAllRequest) (*mount.UmountAllResponse, error) {
			return handler.UmntAll(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.UmountAllResponse {
			return &mount.UmountAllResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountExport(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*MountResult, error) {
	return handleRequest(
		data,
		mount.DecodeExportRequest,
		func(req *mount.ExportRequest) (*mount.ExportResponse, error) {
			return handler.Export(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.ExportResponse {
			return &mount.ExportResponse{MountResponseBase: mount.MountResponseBase{Status: status}, Entries: []mount.ExportEntry{}}
		},
	)
}

// ============================================================================
// Mount Dispatch Entry Point
// ============================================================================

// DispatchMount routes a Mount procedure call: MNT requires v3 (returns v3
// file handle format); the other procedures are version-agnostic (macOS
// umount uses mount v1 for UMNT). Unknown procedures return an empty response.
func DispatchMount(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	data []byte,
	clientAddr string,
	handler *mount.Handler,
	reg *runtime.Runtime,
) ([]byte, error) {
	if call.Procedure == mount.MountProcMnt && call.Version != rpc.MountVersion3 {
		logger.Warn("Unsupported Mount version for MNT",
			"requested", call.Version,
			"supported", rpc.MountVersion3,
			"xid", fmt.Sprintf("0x%x", call.XID),
			"client", clientAddr)

		mismatchReply, makeErr := rpc.MakeProgMismatchReply(call.XID, rpc.MountVersion3, rpc.MountVersion3)
		if makeErr != nil {
			return nil, fmt.Errorf("make version mismatch reply: %w", makeErr)
		}
		return mismatchReply, nil
	}

	procedure, ok := MountDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown Mount procedure", "procedure", call.Procedure)
		return []byte{}, nil
	}

	handlerCtx := middleware.ExtractMountHandlerContext(ctx, call, clientAddr, false)

	result, err := procedure.Handler(handlerCtx, handler, reg, data)
	if result == nil {
		return nil, err
	}
	return result.Data, err
}

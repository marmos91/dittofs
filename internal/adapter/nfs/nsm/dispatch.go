// Package nsm provides Network Status Monitor (NSM) protocol dispatch.
//
// NSM is the crash recovery protocol used by NLM (Network Lock Manager)
// clients to detect server crashes and reclaim locks. This package provides
// the dispatch table that routes NSM procedure calls to their handlers.
package nsm

import (
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
)

// ============================================================================
// Procedure Dispatch Types
// ============================================================================

// NSMProcedureHandler defines the signature for NSM procedure handlers.
//
// Parameters:
//   - ctx: Handler context with client info and Go context
//   - handler: The NSM handler instance
//   - data: Raw XDR-encoded request data
//
// Returns:
//   - *handlers.HandlerResult: XDR-encoded response and status
//   - error: Processing error (separate from NSM status)
type NSMProcedureHandler func(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error)

// NSMProcedure contains metadata about an NSM procedure for dispatch.
type NSMProcedure struct {
	// Name is the procedure name for logging (e.g., "NULL", "MON")
	Name string

	// Handler is the function that processes this procedure
	Handler NSMProcedureHandler
}

// NSMDispatchTable maps NSM procedure numbers to their handlers.
var NSMDispatchTable map[uint32]*NSMProcedure

// init initializes the NSM procedure dispatch table.
func init() {
	initNSMDispatchTable()
}

// ============================================================================
// NSM Dispatch Table Initialization
// ============================================================================

func initNSMDispatchTable() {
	NSMDispatchTable = map[uint32]*NSMProcedure{
		types.SMProcNull: {
			Name:    "NULL",
			Handler: handleNSMNull,
		},
		types.SMProcStat: {
			Name:    "STAT",
			Handler: handleNSMStat,
		},
		types.SMProcMon: {
			Name:    "MON",
			Handler: handleNSMMon,
		},
		types.SMProcUnmon: {
			Name:    "UNMON",
			Handler: handleNSMUnmon,
		},
		types.SMProcUnmonAll: {
			Name:    "UNMON_ALL",
			Handler: handleNSMUnmonAll,
		},
		types.SMProcNotify: {
			Name:    "NOTIFY",
			Handler: handleNSMNotify,
		},
	}
}

// ============================================================================
// NSM Procedure Handler Wrappers
// ============================================================================

func handleNSMNull(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error) {
	return handler.Null(ctx)
}

func handleNSMStat(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error) {
	return handler.Stat(ctx, data)
}

func handleNSMMon(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error) {
	return handler.Mon(ctx, data)
}

func handleNSMUnmon(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error) {
	return handler.Unmon(ctx, data)
}

func handleNSMUnmonAll(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error) {
	return handler.UnmonAll(ctx, data)
}

func handleNSMNotify(
	ctx *handlers.NSMHandlerContext,
	handler *handlers.Handler,
	data []byte,
) (*handlers.HandlerResult, error) {
	return handler.Notify(ctx, data)
}

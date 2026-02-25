package nfs

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ============================================================================
// Handler Result Structure
// ============================================================================

// HandlerResult contains both the XDR-encoded response and metadata about the operation.
//
// This structure separates the response bytes (which are sent to the client) from
// metadata about the operation (which is used for metrics, logging, etc.).
//
// By returning the NFS status code explicitly, we enable:
//   - Accurate metrics tracking of success/error rates by NFS error type
//   - Clean separation of protocol-level errors from system-level errors
//   - Type-safe handler contracts
type HandlerResult struct {
	// Data contains the XDR-encoded response to send to the client.
	// This includes the NFS status code embedded in the response structure.
	Data []byte

	// NFSStatus is the NFS protocol status code for this operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File not found
	//   - types.NFS3ErrAcces (13): Permission denied
	//   - types.NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	//
	// This is duplicated from the response Data for observability purposes.
	NFSStatus uint32

	// BytesRead contains the number of bytes read for READ operations.
	// Optional: Only populated by READ handlers for metrics tracking.
	// Zero value indicates not a read operation or no data read.
	BytesRead uint64

	// BytesWritten contains the number of bytes written for WRITE operations.
	// Optional: Only populated by WRITE handlers for metrics tracking.
	// Zero value indicates not a write operation or no data written.
	BytesWritten uint64
}

// ============================================================================
// Handler Context Creation (delegates to middleware)
// ============================================================================

// ExtractHandlerContext creates an NFSHandlerContext from an RPC call message.
// This delegates to middleware.ExtractHandlerContext for the actual extraction.
//
// Parameters:
//   - ctx: The Go context for cancellation and timeout control
//   - call: The RPC call message containing authentication data
//   - clientAddr: The remote address of the client connection
//   - share: The share name extracted from file handle (empty if not available)
//   - procedure: Name of the procedure (for logging purposes)
//
// Returns:
//   - *nfs.NFSHandlerContext with extracted authentication information and propagated context
func ExtractHandlerContext(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	clientAddr string,
	share string,
	procedure string,
) *nfs.NFSHandlerContext {
	return middleware.ExtractHandlerContext(ctx, call, clientAddr, share, procedure)
}

// ============================================================================
// Consolidated Dispatch Entry Point
// ============================================================================

// DispatchResult contains the result of dispatching an RPC call.
type DispatchResult struct {
	// Data contains the XDR-encoded response to send to the client.
	Data []byte

	// ProgramName identifies which program handled the request (e.g., "NFS", "Mount").
	ProgramName string

	// ProcedureName is the human-readable name of the dispatched procedure.
	ProcedureName string

	// HandlerResult is the structured handler result for NFS/Mount procedures.
	// Nil for v4, NLM, NSM, and portmap (which use different result types).
	HandlerResult *HandlerResult

	// Err is the error returned by the handler, if any.
	Err error
}

// DispatchDeps contains the dependencies required by Dispatch to route and
// execute RPC procedure calls. This struct avoids circular imports between
// pkg/adapter/nfs and internal/adapter/nfs by accepting handlers as interfaces.
type DispatchDeps struct {
	// V3Handler is the NFSv3 procedure handler.
	V3Handler *nfs.Handler

	// V4Handler dispatches NFSv4 COMPOUND operations.
	// This is an interface to avoid importing pkg/adapter/nfs from internal/adapter/nfs.
	V4Handler V4Dispatcher

	// MountHandler is the Mount protocol procedure handler.
	MountHandler *mount.Handler

	// NLMHandler dispatches NLM (Network Lock Manager) procedure calls.
	NLMHandler NLMDispatcher

	// NSMHandler dispatches NSM (Network Status Monitor) procedure calls.
	NSMHandler NSMDispatcher

	// PortmapHandler dispatches portmapper procedure calls.
	PortmapHandler PortmapDispatcher

	// Registry provides access to runtime state for procedure handlers.
	Registry *runtime.Runtime
}

// V4Dispatcher is the interface for NFSv4 procedure dispatch.
// This avoids a circular import from internal/adapter/nfs to pkg/adapter/nfs.
type V4Dispatcher interface {
	// DispatchV4 dispatches an NFSv4 procedure call.
	// Returns the reply data and any error.
	DispatchV4(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error)
}

// NLMDispatcher is the interface for NLM procedure dispatch.
type NLMDispatcher interface {
	// DispatchNLM dispatches an NLM procedure call.
	DispatchNLM(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error)
}

// NSMDispatcher is the interface for NSM procedure dispatch.
type NSMDispatcher interface {
	// DispatchNSM dispatches an NSM procedure call.
	DispatchNSM(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error)
}

// PortmapDispatcher is the interface for portmapper procedure dispatch.
type PortmapDispatcher interface {
	// DispatchPortmap dispatches a portmapper procedure call.
	DispatchPortmap(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error)
}

// Dispatch is the consolidated entry point for all NFS ecosystem RPC dispatch.
// It routes calls by program number -> version -> procedure in a hierarchical chain.
//
// Program routing:
//   - 100003 (NFS) -> dispatchNFS() -> v3 or v4 based on version
//   - 100005 (Mount) -> dispatchMount() -> mount procedure dispatch
//   - 100021 (NLM) -> deps.NLMHandler.DispatchNLM()
//   - 100024 (NSM) -> deps.NSMHandler.DispatchNSM()
//   - 100000 (Portmap) -> deps.PortmapHandler.DispatchPortmap()
//   - default -> PROG_UNAVAIL error
//
// Error handling at each level:
//   - Unknown program -> RPC PROG_UNAVAIL
//   - Unsupported version -> RPC PROG_MISMATCH with supported range
//   - Unknown procedure -> empty response (per existing behavior)
//
// Parameters:
//   - ctx: Context for cancellation and timeout control
//   - call: The parsed RPC call message with program/version/procedure
//   - data: The raw procedure data (XDR-encoded arguments)
//   - clientAddr: Remote client address for logging
//   - deps: Handler dependencies for dispatching to protocol-specific handlers
//
// Returns:
//   - replyData: XDR-encoded reply data (nil if error reply was generated)
//   - rpcReply: Pre-formatted RPC error reply (non-nil for PROG_UNAVAIL/PROG_MISMATCH)
//   - err: System-level error (context cancelled, I/O errors)
func Dispatch(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) (replyData []byte, rpcReply []byte, err error) {
	switch call.Program {
	case rpc.ProgramNFS:
		return dispatchNFS(ctx, call, data, clientAddr, deps)

	case rpc.ProgramMount:
		return dispatchMount(ctx, call, data, clientAddr, deps)

	case rpc.ProgramNLM:
		return dispatchNLM(ctx, call, data, clientAddr, deps)

	case rpc.ProgramNSM:
		return dispatchNSM(ctx, call, data, clientAddr, deps)

	case rpc.ProgramPortmap:
		return dispatchPortmap(ctx, call, data, clientAddr, deps)

	default:
		logger.Debug("Unknown program", "program", call.Program)
		errorReply, makeErr := rpc.MakeErrorReply(call.XID, rpc.RPCProgUnavail)
		if makeErr != nil {
			return nil, nil, fmt.Errorf("make error reply: %w", makeErr)
		}
		return nil, errorReply, nil
	}
}

// dispatchNFS routes NFS calls by version: v3 -> v3.Dispatch, v4 -> v4.Dispatch.
// Returns PROG_MISMATCH with range [3,4] for unsupported versions.
func dispatchNFS(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) ([]byte, []byte, error) {
	switch call.Version {
	case rpc.NFSVersion3:
		result, err := dispatchNFSv3Procedure(ctx, call, data, clientAddr, deps)
		return result, nil, err

	case rpc.NFSVersion4:
		if deps.V4Handler == nil {
			logger.Debug("NFSv4 not available", "client", clientAddr)
			mismatch, makeErr := rpc.MakeProgMismatchReply(call.XID, rpc.NFSVersion3, rpc.NFSVersion3)
			if makeErr != nil {
				return nil, nil, fmt.Errorf("make prog mismatch reply: %w", makeErr)
			}
			return nil, mismatch, nil
		}
		result, err := deps.V4Handler.DispatchV4(ctx, call, data, clientAddr)
		return result, nil, err

	default:
		logger.Warn("Unsupported NFS version",
			"requested", call.Version,
			"supported_low", rpc.NFSVersion3,
			"supported_high", rpc.NFSVersion4,
			"xid", fmt.Sprintf("0x%x", call.XID),
			"client", clientAddr)

		mismatchReply, makeErr := rpc.MakeProgMismatchReply(call.XID, rpc.NFSVersion3, rpc.NFSVersion4)
		if makeErr != nil {
			return nil, nil, fmt.Errorf("make version mismatch reply: %w", makeErr)
		}
		return nil, mismatchReply, nil
	}
}

// dispatchNFSv3Procedure dispatches an NFSv3 procedure call using the dispatch table.
func dispatchNFSv3Procedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) ([]byte, error) {
	procedure, ok := NfsDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown NFS procedure", "procedure", call.Procedure)
		return []byte{}, nil
	}

	handlerCtx := middleware.ExtractHandlerContext(ctx, call, clientAddr, "", procedure.Name)

	result, err := procedure.Handler(handlerCtx, deps.V3Handler, deps.Registry, data)
	if result == nil {
		return nil, err
	}
	return result.Data, err
}

// dispatchMount routes Mount protocol calls.
// MNT procedure requires v3; other procedures accept v1/v2/v3 (macOS uses v1 for UMNT).
// Returns PROG_MISMATCH for MNT with non-v3 version.
func dispatchMount(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) ([]byte, []byte, error) {
	// Mount protocol version handling:
	// - MNT requires v3 (returns v3 file handle format)
	// - Other procedures (NULL, DUMP, UMNT, UMNTALL, EXPORT) are version-agnostic
	// macOS umount uses mount v1 for UMNT, so we accept v1/v2/v3 for those procedures
	if call.Procedure == mount.MountProcMnt && call.Version != rpc.MountVersion3 {
		logger.Warn("Unsupported Mount version for MNT",
			"requested", call.Version,
			"supported", rpc.MountVersion3,
			"xid", fmt.Sprintf("0x%x", call.XID),
			"client", clientAddr)

		mismatchReply, makeErr := rpc.MakeProgMismatchReply(call.XID, rpc.MountVersion3, rpc.MountVersion3)
		if makeErr != nil {
			return nil, nil, fmt.Errorf("make version mismatch reply: %w", makeErr)
		}
		return nil, mismatchReply, nil
	}

	procedure, ok := MountDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown Mount procedure", "procedure", call.Procedure)
		return []byte{}, nil, nil
	}

	handlerCtx := middleware.ExtractMountHandlerContext(ctx, call, clientAddr, false)

	result, err := procedure.Handler(handlerCtx, deps.MountHandler, deps.Registry, data)
	if result == nil {
		return nil, nil, err
	}
	return result.Data, nil, err
}

// dispatchNLM routes NLM (Network Lock Manager) calls. NLM v4 only.
func dispatchNLM(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) ([]byte, []byte, error) {
	if call.Version != rpc.NLMVersion4 {
		logger.Warn("Unsupported NLM version",
			"requested", call.Version,
			"supported", rpc.NLMVersion4,
			"xid", fmt.Sprintf("0x%x", call.XID),
			"client", clientAddr)

		mismatchReply, makeErr := rpc.MakeProgMismatchReply(call.XID, rpc.NLMVersion4, rpc.NLMVersion4)
		if makeErr != nil {
			return nil, nil, fmt.Errorf("make version mismatch reply: %w", makeErr)
		}
		return nil, mismatchReply, nil
	}

	if deps.NLMHandler == nil {
		logger.Debug("NLM handler not available", "client", clientAddr)
		return []byte{}, nil, nil
	}

	result, err := deps.NLMHandler.DispatchNLM(ctx, call, data, clientAddr)
	return result, nil, err
}

// dispatchNSM routes NSM (Network Status Monitor) calls. NSM v1 only.
func dispatchNSM(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) ([]byte, []byte, error) {
	if call.Version != rpc.NSMVersion1 {
		logger.Warn("Unsupported NSM version",
			"requested", call.Version,
			"supported", rpc.NSMVersion1,
			"xid", fmt.Sprintf("0x%x", call.XID),
			"client", clientAddr)

		mismatchReply, makeErr := rpc.MakeProgMismatchReply(call.XID, rpc.NSMVersion1, rpc.NSMVersion1)
		if makeErr != nil {
			return nil, nil, fmt.Errorf("make version mismatch reply: %w", makeErr)
		}
		return nil, mismatchReply, nil
	}

	if deps.NSMHandler == nil {
		logger.Debug("NSM handler not available", "client", clientAddr)
		return []byte{}, nil, nil
	}

	result, err := deps.NSMHandler.DispatchNSM(ctx, call, data, clientAddr)
	return result, nil, err
}

// dispatchPortmap routes portmapper calls.
func dispatchPortmap(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string, deps *DispatchDeps) ([]byte, []byte, error) {
	if deps.PortmapHandler == nil {
		logger.Debug("Portmap handler not available", "client", clientAddr)
		return []byte{}, nil, nil
	}

	result, err := deps.PortmapHandler.DispatchPortmap(ctx, call, data, clientAddr)
	return result, nil, err
}

// ============================================================================
// Procedure Dispatch Tables
// ============================================================================

// nfsProcedureHandler defines the signature for NFS procedure handlers.
// Each handler receives the necessary stores, request data, and
// handler context, and returns a structured result with NFS status.
//
// **Return Values:**
//
// Handlers return (*HandlerResult, error) where:
//   - HandlerResult: Contains XDR-encoded response and NFS status code
//   - error: System-level failures only (context cancelled, I/O errors)
//
// **Context Handling:**
//
// The NFSHandlerContext parameter includes a Go context that handlers should check
// for cancellation before expensive operations. This enables:
//   - Graceful server shutdown without waiting for in-flight requests
//   - Cancellation of orphaned requests from disconnected clients
//   - Request timeout enforcement
//   - Efficient resource cleanup
type nfsProcedureHandler func(
	ctx *nfs.NFSHandlerContext,
	handler *nfs.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error)

// nfsProcedure contains metadata about an NFS procedure for dispatch.
type nfsProcedure struct {
	// Name is the procedure name for logging (e.g., "NULL", "GETATTR")
	Name string

	// Handler is the function that processes this procedure
	Handler nfsProcedureHandler

	// NeedsAuth indicates whether this procedure requires authentication.
	// If true and AUTH_UNIX parsing fails, the procedure may still execute
	// but with nil credentials.
	NeedsAuth bool
}

// NfsDispatchTable maps NFSv3 procedure numbers to their handlers.
// This replaces the large switch statement in handleNFSProcedure.
//
// The table is initialized once at package init time for efficiency.
// Each entry contains the procedure name, handler function, and metadata
// about authentication requirements.
//
// Note: NFSv4 uses its own COMPOUND internal dispatch (v4/handlers/compound.go)
// and does not use this table. ProgramNFS handles both v3 and v4, with
// version routing in Dispatch().
var NfsDispatchTable map[uint32]*nfsProcedure

// mountProcedureHandler defines the signature for Mount procedure handlers.
//
// **Return Values:**
//
// Handlers return (*HandlerResult, error) where:
//   - HandlerResult: Contains XDR-encoded response and status code
//   - error: System-level failures only
//
// **Context Handling:**
//
// Like NFS handlers, Mount handlers receive a MountHandlerContext with a Go context
// for cancellation support.
type mountProcedureHandler func(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error)

// mountProcedure contains metadata about a Mount procedure for dispatch.
type mountProcedure struct {
	Name      string
	Handler   mountProcedureHandler
	NeedsAuth bool
}

// MountDispatchTable maps Mount procedure numbers to their nfs.
var MountDispatchTable map[uint32]*mountProcedure

// init initializes the procedure dispatch tables.
// This is called once at package initialization time.
func init() {
	initNFSDispatchTable()
	initMountDispatchTable()
}

package v3

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
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
}

// NfsDispatchTable maps NFSv3 procedure numbers to their handlers.
// This replaces the large switch statement in handleNFSProcedure.
//
// The table is initialized once at package init time for efficiency.
// Each entry contains the procedure name and handler function.
//
// Note: NFSv4 uses its own COMPOUND internal dispatch (v4/handlers/compound.go)
// and does not use this table. Program number 100003 carries both v3 and v4;
// the connection layer routes on the RPC version before reaching either.
var NfsDispatchTable map[uint32]*nfsProcedure

// init initializes the NFSv3 procedure dispatch table.
// This is called once at package initialization time.
func init() {
	initNFSDispatchTable()
}

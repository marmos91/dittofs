package nfs

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	mount "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc/gss"
	nfs "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
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
// Handler Context Creation
// ============================================================================

// ExtractHandlerContext creates an NFSHandlerContext from an RPC call message.
// This centralizes authentication extraction logic and ensures consistent
// handling across all procedures.
//
// For AUTH_UNIX credentials, this parses the Unix auth body and extracts
// the UID, GID, and supplementary GIDs. For other auth flavors (like AUTH_NULL),
// the Unix credential fields are left as nil.
//
// Parsing failures are logged but do not cause the procedure to fail -
// the procedure receives a context with nil credentials and can decide
// how to handle unauthenticated requests.
//
// **Context Propagation:**
//
// The Go context passed to this function is embedded in the returned NFSHandlerContext.
// This context will be passed through to all procedure handlers, enabling them
// to respect cancellation signals from the server or client disconnect events.
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
	handlerCtx := &nfs.NFSHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		Share:      share,
		AuthFlavor: call.GetAuthFlavor(),
	}

	// Check for GSS identity from context.Value (set by handleRPCCall GSS interception)
	if handlerCtx.AuthFlavor == rpc.AuthRPCSECGSS {
		if gssIdentity := gss.IdentityFromContext(ctx); gssIdentity != nil {
			handlerCtx.UID = gssIdentity.UID
			handlerCtx.GID = gssIdentity.GID
			handlerCtx.GIDs = gssIdentity.GIDs

			logger.Debug("Using GSS identity",
				"procedure", procedure,
				"uid", gssIdentity.UID,
				"gid", gssIdentity.GID,
				"ngids", len(gssIdentity.GIDs))

			return handlerCtx
		}
		// GSS auth flavor but no identity in context - this should not happen
		// for DATA requests, but can happen if GSS interception was bypassed
		logger.Warn("RPCSEC_GSS auth flavor but no GSS identity in context",
			"procedure", procedure)
		return handlerCtx
	}

	// Only attempt to parse Unix credentials if AUTH_UNIX is specified
	if handlerCtx.AuthFlavor != rpc.AuthUnix {
		return handlerCtx
	}

	// Get auth body
	authBody := call.GetAuthBody()
	if len(authBody) == 0 {
		logger.Warn("AUTH_UNIX specified but auth body is empty", "procedure", procedure)
		return handlerCtx
	}

	// Parse Unix auth credentials
	unixAuth, err := rpc.ParseUnixAuth(authBody)
	if err != nil {
		// Log the parsing failure - this is unexpected and may indicate
		// a protocol issue or malicious client
		logger.Warn("Failed to parse AUTH_UNIX credentials",
			"procedure", procedure,
			"error", err)
		return handlerCtx
	}

	// Log successful auth parsing at debug level
	logger.Debug("Parsed Unix auth",
		"procedure", procedure,
		"uid", unixAuth.UID,
		"gid", unixAuth.GID,
		"ngids", len(unixAuth.GIDs))

	handlerCtx.UID = &unixAuth.UID
	handlerCtx.GID = &unixAuth.GID
	handlerCtx.GIDs = unixAuth.GIDs

	return handlerCtx
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
// version routing in nfs_connection.go.
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

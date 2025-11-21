package nfs

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	mount "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	nfs "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/pkg/registry"
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
}

// ============================================================================
// Authentication Context Creation
// ============================================================================

// NFSAuthContext holds the RPC-level authentication information extracted from
// an NFS/Mount RPC call. This contains raw wire-format authentication data
// that is passed to all NFS and Mount procedure handlers.
//
// This is distinct from pkg/metadata/AuthContext, which contains the effective
// identity after share-level identity mapping rules are applied. NFSAuthContext
// represents the raw RPC credentials before any mapping.
//
// The context includes both the raw authentication flavor (AUTH_UNIX, AUTH_NULL)
// and parsed Unix credentials when available. Procedures can use this to make
// authorization decisions.
//
// **Context Cancellation:**
//
// The Context field enables proper cancellation propagation throughout the
// request processing pipeline:
//   - Server shutdown triggers context cancellation
//   - Client disconnects can trigger cancellation
//   - Request timeouts are enforced via context deadlines
//   - Long-running operations (READ, WRITE, READDIR) can be interrupted
//
// All procedure handlers receive this context and should check for cancellation
// before expensive operations.
type NFSAuthContext struct {
	// Context is the Go context for cancellation and timeout control.
	// This context is derived from the connection's context and may be
	// cancelled when:
	//   - The server is shutting down
	//   - The client connection is closed
	//   - A request timeout occurs
	//   - The operation exceeds configured limits
	Context context.Context

	// ClientAddr is the remote address of the client connection.
	// Format: "IP:port" for TCP connections.
	ClientAddr string

	// Share is the name of the share being accessed (e.g., "/export").
	// Empty string for requests that don't include file handles (e.g., NULL, MOUNT).
	// Extracted at the connection layer to avoid re-parsing in each handler.
	Share string

	// AuthFlavor indicates the RPC authentication type (AUTH_UNIX, AUTH_NULL, etc.)
	AuthFlavor uint32

	// Unix credentials (nil if not AUTH_UNIX or parsing failed)
	UID  *uint32  // User ID
	GID  *uint32  // Primary group ID
	GIDs []uint32 // Supplementary group IDs
}

// extractAuthContext creates an NFSAuthContext from an RPC call message.
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
// The Go context passed to this function is embedded in the returned NFSAuthContext.
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
//   - NFSAuthContext with extracted authentication information and propagated context
func ExtractAuthContext(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	clientAddr string,
	share string,
	procedure string,
) *NFSAuthContext {
	authCtx := &NFSAuthContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		Share:      share,
		AuthFlavor: call.GetAuthFlavor(),
	}

	// Only attempt to parse Unix credentials if AUTH_UNIX is specified
	if authCtx.AuthFlavor != rpc.AuthUnix {
		return authCtx
	}

	// Get auth body
	authBody := call.GetAuthBody()
	if len(authBody) == 0 {
		logger.Warn("%s: AUTH_UNIX specified but auth body is empty", procedure)
		return authCtx
	}

	// Parse Unix auth credentials
	unixAuth, err := rpc.ParseUnixAuth(authBody)
	if err != nil {
		// Log the parsing failure - this is unexpected and may indicate
		// a protocol issue or malicious client
		logger.Warn("%s: Failed to parse AUTH_UNIX credentials: %v", procedure, err)
		return authCtx
	}

	// Log successful auth parsing at debug level
	logger.Debug("%s: Parsed Unix auth: uid=%d gid=%d ngids=%d",
		procedure, unixAuth.UID, unixAuth.GID, len(unixAuth.GIDs))

	authCtx.UID = &unixAuth.UID
	authCtx.GID = &unixAuth.GID
	authCtx.GIDs = unixAuth.GIDs

	return authCtx
}

// ============================================================================
// Procedure Dispatch Tables
// ============================================================================

// nfsProcedureHandler defines the signature for NFS procedure handlers.
// Each handler receives the necessary stores, request data, and
// authentication context, and returns a structured result with NFS status.
//
// **Return Values:**
//
// Handlers return (*HandlerResult, error) where:
//   - HandlerResult: Contains XDR-encoded response and NFS status code
//   - error: System-level failures only (context cancelled, I/O errors)
//
// **Context Handling:**
//
// The NFSAuthContext parameter includes a Go context that handlers should check
// for cancellation before expensive operations. This enables:
//   - Graceful server shutdown without waiting for in-flight requests
//   - Cancellation of orphaned requests from disconnected clients
//   - Request timeout enforcement
//   - Efficient resource cleanup
type nfsProcedureHandler func(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error)

// nfsProcedureInfo contains metadata about an NFS procedure for dispatch.
type nfsProcedureInfo struct {
	// Name is the procedure name for logging (e.g., "NULL", "GETATTR")
	Name string

	// Handler is the function that processes this procedure
	Handler nfsProcedureHandler

	// NeedsAuth indicates whether this procedure requires authentication.
	// If true and AUTH_UNIX parsing fails, the procedure may still execute
	// but with nil credentials.
	NeedsAuth bool
}

// nfsDispatchTable maps NFS procedure numbers to their nfs.
// This replaces the large switch statement in handleNFSProcedure.
//
// The table is initialized once at package init time for efficiency.
// Each entry contains the procedure name, handler function, and metadata
// about authentication requirements.
var NfsDispatchTable map[uint32]*nfsProcedureInfo

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
// Like NFS handlers, Mount handlers receive an NFSAuthContext with a Go context
// for cancellation support.
type mountProcedureHandler func(
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error)

// mountProcedureInfo contains metadata about a Mount procedure for dispatch.
type mountProcedureInfo struct {
	Name      string
	Handler   mountProcedureHandler
	NeedsAuth bool
}

// MountDispatchTable maps Mount procedure numbers to their nfs.
var MountDispatchTable map[uint32]*mountProcedureInfo

// init initializes the procedure dispatch tables.
// This is called once at package initialization time.
func init() {
	initNFSDispatchTable()
	initMountDispatchTable()
}

// ============================================================================
// Status Extraction Helpers
// ============================================================================

// extractNFSStatus extracts the NFS status code from an XDR-encoded response.
//
// NFS v3 responses always start with a 4-byte status field (big-endian uint32).
// This function reads that field without fully parsing the response.
//
// Parameters:
//   - data: XDR-encoded response bytes
//
// Returns:
//   - uint32: NFS status code, or types.NFS3ErrIO if data is malformed
func extractNFSStatus(data []byte) uint32 {
	// NFS responses always start with status (4 bytes, big-endian)
	if len(data) < 4 {
		logger.Warn("Response too short to extract status: %d bytes", len(data))
		return types.NFS3ErrIO
	}

	// Read big-endian uint32
	status := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	return status
}

// extractMountStatus extracts the Mount protocol status from an XDR-encoded response.
//
// Mount protocol responses (for MNT procedure) start with a 4-byte status field.
// For other mount procedures (DUMP, EXPORT, etc.), we return 0 (success).
//
// Parameters:
//   - data: XDR-encoded response bytes
//
// Returns:
//   - uint32: Mount status code, or 0 if cannot extract
func extractMountStatus(data []byte) uint32 {
	// Mount responses vary by procedure, but MNT starts with status
	if len(data) < 4 {
		return 0 // Non-MNT procedures or empty responses
	}

	// Read big-endian uint32
	status := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	return status
}

// ============================================================================
// NFS Dispatch Table Initialization
// ============================================================================

func initNFSDispatchTable() {
	NfsDispatchTable = map[uint32]*nfsProcedureInfo{
		types.NFSProcNull: {
			Name:      "NULL",
			Handler:   handleNFSNull,
			NeedsAuth: false,
		},
		types.NFSProcGetAttr: {
			Name:      "GETATTR",
			Handler:   handleNFSGetAttr,
			NeedsAuth: false,
		},
		types.NFSProcSetAttr: {
			Name:      "SETATTR",
			Handler:   handleNFSSetAttr,
			NeedsAuth: true,
		},
		types.NFSProcLookup: {
			Name:      "LOOKUP",
			Handler:   handleNFSLookup,
			NeedsAuth: true,
		},
		types.NFSProcAccess: {
			Name:      "ACCESS",
			Handler:   handleNFSAccess,
			NeedsAuth: true,
		},
		types.NFSProcReadLink: {
			Name:      "READLINK",
			Handler:   handleNFSReadLink,
			NeedsAuth: true,
		},
		types.NFSProcRead: {
			Name:      "READ",
			Handler:   handleNFSRead,
			NeedsAuth: true,
		},
		types.NFSProcWrite: {
			Name:      "WRITE",
			Handler:   handleNFSWrite,
			NeedsAuth: true,
		},
		types.NFSProcCreate: {
			Name:      "CREATE",
			Handler:   handleNFSCreate,
			NeedsAuth: true,
		},
		types.NFSProcMkdir: {
			Name:      "MKDIR",
			Handler:   handleNFSMkdir,
			NeedsAuth: true,
		},
		types.NFSProcSymlink: {
			Name:      "SYMLINK",
			Handler:   handleNFSSymlink,
			NeedsAuth: true,
		},
		types.NFSProcMknod: {
			Name:      "MKNOD",
			Handler:   handleNFSMknod,
			NeedsAuth: false,
		},
		types.NFSProcRemove: {
			Name:      "REMOVE",
			Handler:   handleNFSRemove,
			NeedsAuth: true,
		},
		types.NFSProcRmdir: {
			Name:      "RMDIR",
			Handler:   handleNFSRmdir,
			NeedsAuth: true,
		},
		types.NFSProcRename: {
			Name:      "RENAME",
			Handler:   handleNFSRename,
			NeedsAuth: true,
		},
		types.NFSProcLink: {
			Name:      "LINK",
			Handler:   handleNFSLink,
			NeedsAuth: true,
		},
		types.NFSProcReadDir: {
			Name:      "READDIR",
			Handler:   handleNFSReadDir,
			NeedsAuth: true,
		},
		types.NFSProcReadDirPlus: {
			Name:      "READDIRPLUS",
			Handler:   handleNFSReadDirPlus,
			NeedsAuth: true,
		},
		types.NFSProcFsStat: {
			Name:      "FSSTAT",
			Handler:   handleNFSFsStat,
			NeedsAuth: false,
		},
		types.NFSProcFsInfo: {
			Name:      "FSINFO",
			Handler:   handleNFSFsInfo,
			NeedsAuth: false,
		},
		types.NFSProcPathConf: {
			Name:      "PATHCONF",
			Handler:   handleNFSPathConf,
			NeedsAuth: false,
		},
		types.NFSProcCommit: {
			Name:      "COMMIT",
			Handler:   handleNFSCommit,
			NeedsAuth: true,
		},
	}
}

// ============================================================================
// NFS Procedure Handlers
// ============================================================================
//
// Each handler function below follows the same pattern:
//  1. Create a procedure-specific context from the AuthContext
//  2. Propagate the Go context for cancellation support
//  3. Call the handler via handleRequest helper
//  4. Return the encoded response
//
// The context propagation ensures that all procedure handlers can:
//  - Check for cancellation before expensive operations
//  - Respond to server shutdown signals
//  - Implement request timeouts
//  - Clean up resources properly on cancellation

func handleNFSNull(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeNullRequest,
		func(req *nfs.NullRequest) (*nfs.NullResponse, error) {
			return handler.Null(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.NullResponse {
			return &nfs.NullResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSGetAttr(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
	}

	return handleRequest(
		data,
		nfs.DecodeGetAttrRequest,
		func(req *nfs.GetAttrRequest) (*nfs.GetAttrResponse, error) {
			return handler.GetAttr(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.GetAttrResponse {
			return &nfs.GetAttrResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSSetAttr(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeSetAttrRequest,
		func(req *nfs.SetAttrRequest) (*nfs.SetAttrResponse, error) {
			return handler.SetAttr(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.SetAttrResponse {
			return &nfs.SetAttrResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSLookup(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeLookupRequest,
		func(req *nfs.LookupRequest) (*nfs.LookupResponse, error) {
			return handler.Lookup(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.LookupResponse {
			return &nfs.LookupResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSAccess(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeAccessRequest,
		func(req *nfs.AccessRequest) (*nfs.AccessResponse, error) {
			return handler.Access(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.AccessResponse {
			return &nfs.AccessResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSReadLink(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeReadLinkRequest,
		func(req *nfs.ReadLinkRequest) (*nfs.ReadLinkResponse, error) {
			return handler.ReadLink(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.ReadLinkResponse {
			return &nfs.ReadLinkResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRead(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeReadRequest,
		func(req *nfs.ReadRequest) (*nfs.ReadResponse, error) {
			return handler.Read(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.ReadResponse {
			return &nfs.ReadResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSWrite(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeWriteRequest,
		func(req *nfs.WriteRequest) (*nfs.WriteResponse, error) {
			return handler.Write(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.WriteResponse {
			return &nfs.WriteResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSCreate(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
	}

	return handleRequest(
		data,
		nfs.DecodeCreateRequest,
		func(req *nfs.CreateRequest) (*nfs.CreateResponse, error) {
			return handler.Create(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.CreateResponse {
			return &nfs.CreateResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSMkdir(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeMkdirRequest,
		func(req *nfs.MkdirRequest) (*nfs.MkdirResponse, error) {
			return handler.Mkdir(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.MkdirResponse {
			return &nfs.MkdirResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSSymlink(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeSymlinkRequest,
		func(req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error) {
			return handler.Symlink(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.SymlinkResponse {
			return &nfs.SymlinkResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSMknod(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeMknodRequest,
		func(req *nfs.MknodRequest) (*nfs.MknodResponse, error) {
			return handler.Mknod(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.MknodResponse {
			return &nfs.MknodResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRemove(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeRemoveRequest,
		func(req *nfs.RemoveRequest) (*nfs.RemoveResponse, error) {
			return handler.Remove(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RemoveResponse {
			return &nfs.RemoveResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRmdir(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeRmdirRequest,
		func(req *nfs.RmdirRequest) (*nfs.RmdirResponse, error) {
			return handler.Rmdir(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RmdirResponse {
			return &nfs.RmdirResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSRename(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeRenameRequest,
		func(req *nfs.RenameRequest) (*nfs.RenameResponse, error) {
			return handler.Rename(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RenameResponse {
			return &nfs.RenameResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSLink(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeLinkRequest,
		func(req *nfs.LinkRequest) (*nfs.LinkResponse, error) {
			return handler.Link(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.LinkResponse {
			return &nfs.LinkResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSReadDir(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeReadDirRequest,
		func(req *nfs.ReadDirRequest) (*nfs.ReadDirResponse, error) {
			return handler.ReadDir(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.ReadDirResponse {
			return &nfs.ReadDirResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSReadDirPlus(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeReadDirPlusRequest,
		func(req *nfs.ReadDirPlusRequest) (*nfs.ReadDirPlusResponse, error) {
			return handler.ReadDirPlus(ctx, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.ReadDirPlusResponse {
			return &nfs.ReadDirPlusResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSFsStat(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeFsStatRequest,
		func(req *nfs.FsStatRequest) (*nfs.FsStatResponse, error) {
			return handler.FsStat(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.FsStatResponse {
			return &nfs.FsStatResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSFsInfo(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeFsInfoRequest,
		func(req *nfs.FsInfoRequest) (*nfs.FsInfoResponse, error) {
			return handler.FsInfo(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.FsInfoResponse {
			return &nfs.FsInfoResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSPathConf(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodePathConfRequest,
		func(req *nfs.PathConfRequest) (*nfs.PathConfResponse, error) {
			return handler.PathConf(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.PathConfResponse {
			return &nfs.PathConfResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

func handleNFSCommit(
	authCtx *NFSAuthContext,
	handler *nfs.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &nfs.NFSHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
		Share:      authCtx.Share,
	}

	return handleRequest(
		data,
		nfs.DecodeCommitRequest,
		func(req *nfs.CommitRequest) (*nfs.CommitResponse, error) {
			return handler.Commit(ctx, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.CommitResponse {
			return &nfs.CommitResponse{NFSResponseBase: nfs.NFSResponseBase{Status: status}}
		},
	)
}

// ============================================================================
// Mount Dispatch Table Initialization
// ============================================================================

func initMountDispatchTable() {
	MountDispatchTable = map[uint32]*mountProcedureInfo{
		mount.MountProcNull: {
			Name:      "NULL",
			Handler:   handleMountNull,
			NeedsAuth: true,
		},
		mount.MountProcMnt: {
			Name:      "MNT",
			Handler:   handleMountMnt,
			NeedsAuth: true,
		},
		mount.MountProcDump: {
			Name:      "DUMP",
			Handler:   handleMountDump,
			NeedsAuth: false,
		},
		mount.MountProcUmnt: {
			Name:      "UMNT",
			Handler:   handleMountUmnt,
			NeedsAuth: false,
		},
		mount.MountProcUmntAll: {
			Name:      "UMNTALL",
			Handler:   handleMountUmntAll,
			NeedsAuth: false,
		},
		mount.MountProcExport: {
			Name:      "EXPORT",
			Handler:   handleMountExport,
			NeedsAuth: false,
		},
	}
}

// ============================================================================
// Mount Procedure Handlers
// ============================================================================
//
// Each Mount handler follows the same pattern as NFS handlers:
// context propagation for cancellation support.

func handleMountNull(
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	nullCtx := &mount.MountHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		mount.DecodeNullRequest,
		func(req *mount.NullRequest) (*mount.NullResponse, error) {
			return handler.MountNull(nullCtx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.NullResponse {
			return &mount.NullResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountMnt(
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &mount.MountHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

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
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &mount.MountHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

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
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &mount.MountHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

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
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	ctx := &mount.MountHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

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
	authCtx *NFSAuthContext,
	handler *mount.Handler,
	reg *registry.Registry,
	data []byte,
) (*HandlerResult, error) {
	exportCtx := &mount.MountHandlerContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		mount.DecodeExportRequest,
		func(req *mount.ExportRequest) (*mount.ExportResponse, error) {
			return handler.Export(exportCtx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.ExportResponse {
			return &mount.ExportResponse{MountResponseBase: mount.MountResponseBase{Status: status}, Entries: []mount.ExportEntry{}}
		},
	)
}

package nfs

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	mount "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	nfs "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Authentication Context Creation
// ============================================================================

// AuthContext holds the common authentication information extracted from
// an RPC call. This provides a unified view of authentication data that
// is passed to all NFS and Mount procedures.
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
type AuthContext struct {
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

	// AuthFlavor indicates the RPC authentication type (AUTH_UNIX, AUTH_NULL, etc.)
	AuthFlavor uint32

	// Unix credentials (nil if not AUTH_UNIX or parsing failed)
	UID  *uint32  // User ID
	GID  *uint32  // Primary group ID
	GIDs []uint32 // Supplementary group IDs
}

// extractAuthContext creates an AuthContext from an RPC call message.
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
// The Go context passed to this function is embedded in the returned AuthContext.
// This context will be passed through to all procedure handlers, enabling them
// to respect cancellation signals from the server or client disconnect events.
//
// Parameters:
//   - ctx: The Go context for cancellation and timeout control
//   - call: The RPC call message containing authentication data
//   - clientAddr: The remote address of the client connection
//   - procedure: Name of the procedure (for logging purposes)
//
// Returns:
//   - AuthContext with extracted authentication information and propagated context
func ExtractAuthContext(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	clientAddr string,
	procedure string,
) *AuthContext {
	authCtx := &AuthContext{
		Context:    ctx,
		ClientAddr: clientAddr,
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

// nfsProcedureHandler defines the signature for NFS procedure nfs.
// Each handler receives the necessary stores, request data, and
// authentication context, and returns encoded response data or an error.
//
// **Context Handling:**
//
// The AuthContext parameter includes a Go context that handlers should check
// for cancellation before expensive operations. This enables:
//   - Graceful server shutdown without waiting for in-flight requests
//   - Cancellation of orphaned requests from disconnected clients
//   - Request timeout enforcement
//   - Efficient resource cleanup
type nfsProcedureHandler func(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error)

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

// mountProcedureHandler defines the signature for Mount procedure nfs.
//
// **Context Handling:**
//
// Like NFS handlers, Mount handlers receive an AuthContext with a Go context
// for cancellation support.
type mountProcedureHandler func(
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error)

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
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.NullContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeNullRequest,
		func(req *nfs.NullRequest) (*nfs.NullResponse, error) {
			return handler.Null(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.NullResponse {
			return &nfs.NullResponse{}
		},
	)
}

func handleNFSGetAttr(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.GetAttrContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
	}

	return handleRequest(
		data,
		nfs.DecodeGetAttrRequest,
		func(req *nfs.GetAttrRequest) (*nfs.GetAttrResponse, error) {
			return handler.GetAttr(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.GetAttrResponse {
			return &nfs.GetAttrResponse{Status: status}
		},
	)
}

func handleNFSSetAttr(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.SetAttrContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeSetAttrRequest,
		func(req *nfs.SetAttrRequest) (*nfs.SetAttrResponse, error) {
			return handler.SetAttr(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.SetAttrResponse {
			return &nfs.SetAttrResponse{Status: status}
		},
	)
}

func handleNFSLookup(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.LookupContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeLookupRequest,
		func(req *nfs.LookupRequest) (*nfs.LookupResponse, error) {
			return handler.Lookup(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.LookupResponse {
			return &nfs.LookupResponse{Status: status}
		},
	)
}

func handleNFSAccess(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.AccessContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeAccessRequest,
		func(req *nfs.AccessRequest) (*nfs.AccessResponse, error) {
			return handler.Access(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.AccessResponse {
			return &nfs.AccessResponse{Status: status}
		},
	)
}

func handleNFSReadLink(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.ReadLinkContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeReadLinkRequest,
		func(req *nfs.ReadLinkRequest) (*nfs.ReadLinkResponse, error) {
			return handler.ReadLink(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.ReadLinkResponse {
			return &nfs.ReadLinkResponse{Status: status}
		},
	)
}

func handleNFSRead(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.ReadContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeReadRequest,
		func(req *nfs.ReadRequest) (*nfs.ReadResponse, error) {
			return handler.Read(ctx, content, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.ReadResponse {
			return &nfs.ReadResponse{Status: status}
		},
	)
}

func handleNFSWrite(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.WriteContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeWriteRequest,
		func(req *nfs.WriteRequest) (*nfs.WriteResponse, error) {
			return handler.Write(ctx, content, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.WriteResponse {
			return &nfs.WriteResponse{Status: status}
		},
	)
}

func handleNFSCreate(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.CreateContext{
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
			return handler.Create(ctx, content, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.CreateResponse {
			return &nfs.CreateResponse{Status: status}
		},
	)
}

func handleNFSMkdir(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.MkdirContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeMkdirRequest,
		func(req *nfs.MkdirRequest) (*nfs.MkdirResponse, error) {
			return handler.Mkdir(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.MkdirResponse {
			return &nfs.MkdirResponse{Status: status}
		},
	)
}

func handleNFSSymlink(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.SymlinkContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeSymlinkRequest,
		func(req *nfs.SymlinkRequest) (*nfs.SymlinkResponse, error) {
			return handler.Symlink(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.SymlinkResponse {
			return &nfs.SymlinkResponse{Status: status}
		},
	)
}

func handleNFSMknod(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.MknodContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
	}

	return handleRequest(
		data,
		nfs.DecodeMknodRequest,
		func(req *nfs.MknodRequest) (*nfs.MknodResponse, error) {
			return handler.Mknod(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.MknodResponse {
			return &nfs.MknodResponse{Status: status}
		},
	)
}

func handleNFSRemove(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.RemoveContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeRemoveRequest,
		func(req *nfs.RemoveRequest) (*nfs.RemoveResponse, error) {
			return handler.Remove(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RemoveResponse {
			return &nfs.RemoveResponse{Status: status}
		},
	)
}

func handleNFSRmdir(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.RmdirContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeRmdirRequest,
		func(req *nfs.RmdirRequest) (*nfs.RmdirResponse, error) {
			return handler.Rmdir(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RmdirResponse {
			return &nfs.RmdirResponse{Status: status}
		},
	)
}

func handleNFSRename(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.RenameContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeRenameRequest,
		func(req *nfs.RenameRequest) (*nfs.RenameResponse, error) {
			return handler.Rename(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.RenameResponse {
			return &nfs.RenameResponse{Status: status}
		},
	)
}

func handleNFSLink(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.LinkContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeLinkRequest,
		func(req *nfs.LinkRequest) (*nfs.LinkResponse, error) {
			return handler.Link(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.LinkResponse {
			return &nfs.LinkResponse{Status: status}
		},
	)
}

func handleNFSReadDir(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.ReadDirContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeReadDirRequest,
		func(req *nfs.ReadDirRequest) (*nfs.ReadDirResponse, error) {
			return handler.ReadDir(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.ReadDirResponse {
			return &nfs.ReadDirResponse{Status: status}
		},
	)
}

func handleNFSReadDirPlus(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.ReadDirPlusContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeReadDirPlusRequest,
		func(req *nfs.ReadDirPlusRequest) (*nfs.ReadDirPlusResponse, error) {
			return handler.ReadDirPlus(ctx, store, req)
		},
		types.NFS3ErrAcces,
		func(status uint32) *nfs.ReadDirPlusResponse {
			return &nfs.ReadDirPlusResponse{Status: status}
		},
	)
}

func handleNFSFsStat(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.FsStatContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
	}

	return handleRequest(
		data,
		nfs.DecodeFsStatRequest,
		func(req *nfs.FsStatRequest) (*nfs.FsStatResponse, error) {
			return handler.FsStat(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.FsStatResponse {
			return &nfs.FsStatResponse{Status: status}
		},
	)
}

func handleNFSFsInfo(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.FsInfoContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
	}

	return handleRequest(
		data,
		nfs.DecodeFsInfoRequest,
		func(req *nfs.FsInfoRequest) (*nfs.FsInfoResponse, error) {
			return handler.FsInfo(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.FsInfoResponse {
			return &nfs.FsInfoResponse{Status: status}
		},
	)
}

func handleNFSPathConf(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.PathConfContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
	}

	return handleRequest(
		data,
		nfs.DecodePathConfRequest,
		func(req *nfs.PathConfRequest) (*nfs.PathConfResponse, error) {
			return handler.PathConf(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.PathConfResponse {
			return &nfs.PathConfResponse{Status: status}
		},
	)
}

func handleNFSCommit(
	authCtx *AuthContext,
	handler nfs.NFSHandler,
	store metadata.MetadataStore,
	content content.Repository,
	data []byte,
) ([]byte, error) {
	ctx := &nfs.CommitContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UID:        authCtx.UID,
		GID:        authCtx.GID,
		GIDs:       authCtx.GIDs,
	}

	return handleRequest(
		data,
		nfs.DecodeCommitRequest,
		func(req *nfs.CommitRequest) (*nfs.CommitResponse, error) {
			return handler.Commit(ctx, store, req)
		},
		types.NFS3ErrIO,
		func(status uint32) *nfs.CommitResponse {
			return &nfs.CommitResponse{Status: status}
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
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error) {
	nullCtx := &mount.NullContext{
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
			return handler.MountNull(nullCtx, store, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.NullResponse {
			return &mount.NullResponse{}
		},
	)

}

func handleMountMnt(
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error) {
	// Create mount context with Unix auth if available
	var unixAuth *rpc.UnixAuth
	if authCtx.UID != nil && authCtx.GID != nil {
		unixAuth = &rpc.UnixAuth{
			UID:  *authCtx.UID,
			GID:  *authCtx.GID,
			GIDs: authCtx.GIDs,
		}
	}

	ctx := &mount.MountContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
		AuthFlavor: authCtx.AuthFlavor,
		UnixAuth:   unixAuth,
	}

	return handleRequest(
		data,
		mount.DecodeMountRequest,
		func(req *mount.MountRequest) (*mount.MountResponse, error) {
			return handler.Mount(ctx, store, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.MountResponse {
			return &mount.MountResponse{Status: status}
		},
	)
}

func handleMountDump(
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error) {
	ctx := &mount.DumpContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
	}

	return handleRequest(
		data,
		mount.DecodeDumpRequest,
		func(req *mount.DumpRequest) (*mount.DumpResponse, error) {
			return handler.Dump(ctx, store, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.DumpResponse {
			return &mount.DumpResponse{Entries: []mount.DumpEntry{}}
		},
	)
}

func handleMountUmnt(
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error) {
	ctx := &mount.UmountContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
	}

	return handleRequest(
		data,
		mount.DecodeUmountRequest,
		func(req *mount.UmountRequest) (*mount.UmountResponse, error) {
			return handler.Umnt(ctx, store, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.UmountResponse {
			return &mount.UmountResponse{}
		},
	)
}

func handleMountUmntAll(
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error) {
	ctx := &mount.UmountAllContext{
		Context:    authCtx.Context,
		ClientAddr: authCtx.ClientAddr,
	}

	return handleRequest(
		data,
		mount.DecodeUmountAllRequest,
		func(req *mount.UmountAllRequest) (*mount.UmountAllResponse, error) {
			return handler.UmntAll(ctx, store, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.UmountAllResponse {
			return &mount.UmountAllResponse{}
		},
	)
}

func handleMountExport(
	authCtx *AuthContext,
	handler mount.MountHandler,
	store metadata.MetadataStore,
	data []byte,
) ([]byte, error) {
	exportCtx := &mount.ExportContext{
		Context: authCtx.Context,
	}

	return handleRequest(
		data,
		mount.DecodeExportRequest,
		func(req *mount.ExportRequest) (*mount.ExportResponse, error) {
			return handler.Export(exportCtx, store, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.ExportResponse {
			return &mount.ExportResponse{Entries: []mount.ExportEntry{}}
		},
	)
}

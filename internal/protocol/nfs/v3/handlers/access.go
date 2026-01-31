package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// AccessRequest represents an ACCESS request from an NFS client.
// The client wants to verify if it has specific permissions on a file or directory.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.4 specifies the ACCESS procedure as:
//
//	ACCESS3res NFSPROC3_ACCESS(ACCESS3args) = 4;
//
// The ACCESS procedure is used by clients to check permissions before
// attempting operations, avoiding the overhead of failed operations.
type AccessRequest struct {
	// Handle is the file handle of the object to check permissions for.
	// Must be a valid file handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Access is a bitmap of requested access permissions.
	// The client specifies which permissions it wants to check using the
	// Access* constants (AccessRead, AccessLookup, AccessModify, etc.).
	// Multiple permissions can be combined with bitwise OR.
	// Example: AccessRead | AccessExecute checks for read and execute.
	Access uint32
}

// AccessResponse represents the response to an ACCESS request.
// It contains the status of the check and, if successful, which permissions
// were granted and optional post-operation attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type AccessResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains the post-operation attributes for the file handle.
	// This is optional and may be nil.
	// Including attributes helps clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Access is a bitmap of granted access permissions.
	// Only present when Status == types.NFS3OK.
	// This is a subset (or equal to) the requested permissions.
	// A bit set to 1 means that permission is granted.
	// A bit set to 0 means that permission is denied or unknown.
	// The client should check each bit individually.
	Access uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Access checks access permissions for a file system object.
//
// This implements the NFS ACCESS procedure as defined in RFC 1813 Section 3.3.4.
//
// **Purpose:**
//
// ACCESS allows clients to check permissions efficiently before attempting
// operations. This is more efficient than trying an operation and handling errors.
// It's particularly useful for implementing UI elements (graying out unavailable
// actions) and for optimizing batch operations.
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format and length)
//  3. Extract client IP and authentication credentials from context
//  4. Verify file handle exists via metaSvc.GetFile()
//  5. Check for cancellation before expensive permission check
//  6. Delegate permission checking to metaSvc.CheckPermissions()
//  7. Retrieve file attributes for cache consistency
//  8. Return granted permissions bitmap to client
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after GetFile, before the potentially expensive CheckPermissions
//   - No check after CheckPermissions since response building is fast
//   - Returns NFS3ErrIO status with context error for cancellation
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (permission checking) is delegated to store
//   - File handle validation is performed by metaSvc.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer extracts and passes these to the store, which
// implements the actual permission checking logic based on:
//   - File ownership (UID/GID)
//   - File mode bits (rwx permissions)
//   - ACLs or other access control mechanisms (implementation-specific)
//
// **Permission Bitmap:**
//
// The Access field uses these bits (can be combined with bitwise OR):
//   - AccessRead (0x0001): Read file data or list directory
//   - AccessLookup (0x0002): Look up names in directory
//   - AccessModify (0x0004): Modify file data
//   - AccessExtend (0x0008): Extend file (write beyond EOF)
//   - AccessDelete (0x0010): Delete file or directory
//   - AccessExecute (0x0020): Execute file or search directory
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// The procedure rarely returns errors - instead, it returns success
// with Access=0 (no permissions granted) when access would be denied.
//
// Store errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Permission denied → types.NFS3OK with Access=0 (or NFS3ErrAcces)
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Store layer enforces actual access control
//   - Client context enables audit logging
//   - No sensitive information leaked in error messages
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - store: The metadata store for file access and permission checks
//   - req: The access request containing handle and requested permissions
//
// **Returns:**
//   - *AccessResponse: Response with status and granted permissions (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.4: ACCESS Procedure**
//
// Example:
//
//	handler := &Handler{}
//	req := &AccessRequest{
//	    Handle: fileHandle,
//	    Access: AccessRead | AccessExecute,
//	}
//	ctx := &NFSHandlerContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    Share:      "/export",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Access(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    if resp.Access & AccessRead != 0 {
//	        // Client has read permission
//	    }
//	}
func (h *Handler) Access(
	ctx *NFSHandlerContext,
	req *AccessRequest,
) (*AccessResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "ACCESS cancelled before processing",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", ctx.ClientAddr,
			"error", ctx.Context.Err())
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "ACCESS",
		"handle", fmt.Sprintf("%x", req.Handle),
		"requested", fmt.Sprintf("0x%x", req.Access),
		"client", clientIP,
		"auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateAccessRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "ACCESS validation failed",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP,
			"error", err)
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata service
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "ACCESS failed: metadata service not initialized", "client", clientIP, "error", err)
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	logger.DebugCtx(ctx.Context, "ACCESS", "share", ctx.Share)

	// ========================================================================
	// Step 3: Verify file handle exists and is valid
	// ========================================================================

	file, err := metaSvc.GetFile(ctx.Context, fileHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "ACCESS cancelled during file lookup",
				"handle", fmt.Sprintf("%x", req.Handle),
				"client", clientIP,
				"error", ctx.Context.Err())
			return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		logger.DebugCtx(ctx.Context, "ACCESS failed: handle not found",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP,
			"error", err)
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	// Check for cancellation before the permission check
	// CheckPermissions may involve complex ACL evaluation, so it's worth checking here
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "ACCESS cancelled before permission check",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP,
			"error", ctx.Context.Err())
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "ACCESS cancelled during auth context building",
				"handle", fmt.Sprintf("%x", req.Handle),
				"client", clientIP,
				"error", ctx.Context.Err())
			return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "ACCESS failed: failed to build auth context",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP)
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 4: Translate NFS access bits to generic permissions
	// ========================================================================

	requestedPerms := nfsAccessToPermissions(req.Access, file.Type)

	logger.DebugCtx(ctx.Context, "ACCESS translation",
		"nfs_access", fmt.Sprintf("0x%x", req.Access),
		"generic_perms", fmt.Sprintf("0x%x", requestedPerms),
		"type", file.Type)

	// ========================================================================
	// Step 5: Check permissions via store
	// ========================================================================

	grantedPerms, err := metaSvc.CheckPermissions(authCtx, fileHandle, requestedPerms)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "ACCESS cancelled during permission check",
				"handle", fmt.Sprintf("%x", req.Handle),
				"client", clientIP,
				"error", ctx.Context.Err())
			return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "ACCESS failed: permission check error",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP)
		return &AccessResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 6: Translate granted permissions back to NFS access bits
	// ========================================================================

	grantedAccess := permissionsToNFSAccess(grantedPerms, file.Type)

	logger.DebugCtx(ctx.Context, "ACCESS translation",
		"generic_perms", fmt.Sprintf("0x%x", grantedPerms),
		"nfs_access", fmt.Sprintf("0x%x", grantedAccess))

	// ========================================================================
	// Step 7: Build response with granted permissions and attributes
	// ========================================================================

	// Generate file ID from handle for NFS attributes
	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	logger.InfoCtx(ctx.Context, "ACCESS successful",
		"handle", fmt.Sprintf("%x", req.Handle),
		"granted", fmt.Sprintf("0x%x", grantedAccess),
		"requested", fmt.Sprintf("0x%x", req.Access),
		"client", clientIP)

	logger.DebugCtx(ctx.Context, "ACCESS details",
		"type", nfsAttr.Type,
		"mode", fmt.Sprintf("%o", file.Mode),
		"uid", file.UID,
		"gid", file.GID,
		"client_uid", ctx.UID,
		"client_gid", ctx.GID)

	return &AccessResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Access:          grantedAccess,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// nfsAccessToPermissions translates NFS ACCESS bits to generic Permission flags.
//
// NFS ACCESS bits (RFC 1813 Section 3.3.4):
//   - AccessRead (0x0001): Read file data or list directory
//   - AccessLookup (0x0002): Look up names in directory
//   - AccessModify (0x0004): Modify file data
//   - AccessExtend (0x0008): Extend file (write beyond EOF)
//   - AccessDelete (0x0010): Delete file or directory
//   - AccessExecute (0x0020): Execute file or search directory
//
// Generic Permission flags:
//   - PermissionRead: Read file data
//   - PermissionWrite: Modify file data
//   - PermissionExecute: Execute files
//   - PermissionDelete: Delete files/directories
//   - PermissionListDirectory: List directory contents (read for directories)
//   - PermissionTraverse: Search/traverse directories (execute for directories)
//
// The translation is context-sensitive based on file type:
//   - For files: AccessRead -> PermissionRead, AccessModify/AccessExtend -> PermissionWrite
//   - For directories: AccessRead -> PermissionListDirectory, AccessLookup -> PermissionTraverse
func nfsAccessToPermissions(nfsAccess uint32, fileType metadata.FileType) metadata.Permission {
	var perms metadata.Permission

	// Handle directories specially
	if fileType == metadata.FileTypeDirectory {
		// AccessRead for directories means list contents
		if nfsAccess&types.AccessRead != 0 {
			perms |= metadata.PermissionListDirectory
		}
		// AccessLookup means search/traverse
		if nfsAccess&types.AccessLookup != 0 {
			perms |= metadata.PermissionTraverse
		}
		// AccessExecute also means traverse for directories
		if nfsAccess&types.AccessExecute != 0 {
			perms |= metadata.PermissionTraverse
		}
		// AccessModify and AccessExtend mean write (add/remove entries)
		if nfsAccess&(types.AccessModify|types.AccessExtend) != 0 {
			perms |= metadata.PermissionWrite
		}
	} else {
		// For files: straightforward mapping
		if nfsAccess&types.AccessRead != 0 {
			perms |= metadata.PermissionRead
		}
		if nfsAccess&(types.AccessModify|types.AccessExtend) != 0 {
			perms |= metadata.PermissionWrite
		}
		if nfsAccess&types.AccessExecute != 0 {
			perms |= metadata.PermissionExecute
		}
	}

	// AccessDelete maps directly
	if nfsAccess&types.AccessDelete != 0 {
		perms |= metadata.PermissionDelete
	}

	return perms
}

// permissionsToNFSAccess translates generic Permission flags back to NFS ACCESS bits.
//
// This is the inverse of nfsAccessToPermissions and must maintain consistency.
func permissionsToNFSAccess(perms metadata.Permission, fileType metadata.FileType) uint32 {
	var nfsAccess uint32

	// Handle directories specially
	if fileType == metadata.FileTypeDirectory {
		// PermissionListDirectory -> AccessRead
		if perms&metadata.PermissionListDirectory != 0 {
			nfsAccess |= types.AccessRead
		}
		// PermissionTraverse -> AccessLookup and AccessExecute
		if perms&metadata.PermissionTraverse != 0 {
			nfsAccess |= types.AccessLookup | types.AccessExecute
		}
		// PermissionWrite -> AccessModify and AccessExtend
		if perms&metadata.PermissionWrite != 0 {
			nfsAccess |= types.AccessModify | types.AccessExtend
		}
	} else {
		// For files: straightforward mapping
		if perms&metadata.PermissionRead != 0 {
			nfsAccess |= types.AccessRead
		}
		if perms&metadata.PermissionWrite != 0 {
			nfsAccess |= types.AccessModify | types.AccessExtend
		}
		if perms&metadata.PermissionExecute != 0 {
			nfsAccess |= types.AccessExecute
		}
	}

	// PermissionDelete maps directly
	if perms&metadata.PermissionDelete != 0 {
		nfsAccess |= types.AccessDelete
	}

	return nfsAccess
}

// ============================================================================
// Request Validation
// ============================================================================

// accessValidationError represents an ACCESS request validation error.
type accessValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *accessValidationError) Error() string {
	return e.message
}

// validateAccessRequest validates ACCESS request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//   - Access bitmap is valid (only uses defined bits)
//
// Returns:
//   - nil if valid
//   - *accessValidationError with NFS status if invalid
func validateAccessRequest(req *AccessRequest) *accessValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &accessValidationError{
			message:   "file handle is empty",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &accessValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &accessValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 Section 3.3.4 states:
	// "The client may request access rights for additional operations not
	// defined in this protocol by setting bits in the access argument other
	// than those defined in this section."
	//
	// Therefore, we do NOT reject unknown access bits. Instead, we accept
	// the request and simply don't grant those unknown permissions in the
	// response. This allows for forward compatibility with future protocol
	// extensions.
	//
	// The valid bits we understand: AccessRead | AccessLookup | AccessModify |
	// AccessExtend | AccessDelete | AccessExecute (bits 0-5, 0x003F)

	return nil
}

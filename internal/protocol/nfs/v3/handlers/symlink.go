package handlers

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// SymlinkRequest represents a SYMLINK request from an NFS client.
// The client provides a parent directory handle, a name for the new symlink,
// the target path, and optional attributes for the symlink.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.10 specifies the SYMLINK procedure as:
//
//	SYMLINK3res NFSPROC3_SYMLINK(SYMLINK3args) = 10;
//
// The SYMLINK procedure creates a symbolic link. A symbolic link is a special
// file that contains a pathname to another file or directory. When a client
// accesses a symbolic link, it reads the target path and performs a new lookup
// using that path.
type SymlinkRequest struct {
	// DirHandle is the file handle of the parent directory where the symlink
	// will be created. Must be a valid directory handle obtained from MOUNT
	// or LOOKUP. Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name for the new symbolic link.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	Name string

	// Target is the pathname that the symbolic link will point to.
	// This can be:
	//   - Absolute path: /usr/bin/python3
	//   - Relative path: ../lib/config.txt
	//   - Any valid pathname string
	// Maximum length is typically 1024 bytes (POSIX PATH_MAX).
	// The server does not validate or resolve the target path.
	Target string

	// Attr contains attributes for the new symbolic link
	Attr metadata.SetAttrs
}

// SymlinkResponse represents the response to a SYMLINK request.
// It contains the status, optional file handle and attributes for the new symlink,
// and WCC data for the parent directory.
//
// The response is encoded in XDR format before being sent back to the client.
type SymlinkResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// FileHandle is the file handle of the newly created symlink.
	// Only present when Status == types.NFS3OK.
	// Clients can use this handle for subsequent operations (GETATTR, READLINK, etc.).
	FileHandle []byte

	// Attr contains post-operation attributes of the newly created symlink.
	// Optional, may be nil. Includes type (symlink), permissions, owner, size, etc.
	// Only present when Status == types.NFS3OK.
	Attr *types.NFSFileAttr

	// DirAttrBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect changes.
	// May be nil if attributes could not be captured.
	DirAttrBefore *types.WccAttr

	// DirAttrAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide updated directory state.
	// Should be present for both success and failure cases when possible.
	DirAttrAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Symlink creates a symbolic link in a directory.
//
// This implements the NFS SYMLINK procedure as defined in RFC 1813 Section 3.3.10.
//
// **Purpose:**
//
// SYMLINK creates a special file that contains a pathname to another file or
// directory. Symbolic links provide:
//   - Flexible file system organization (links across filesystems)
//   - Indirection for file access (change target without changing link)
//   - Multiple paths to the same file
//   - Links to directories (unlike hard links)
//
// Common use cases:
//   - Creating shortcuts: ln -s /usr/bin/python3 /usr/local/bin/python
//   - Version management: ln -s app-v2.0 app-current
//   - Compatibility paths: ln -s /new/path /old/path
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format, name syntax, target length)
//  3. Extract client IP and authentication credentials from context
//  4. Verify parent directory exists and is a directory (via store)
//  5. Capture pre-operation directory state (for WCC)
//  6. Check for cancellation before create operation
//  7. Convert client attributes to metadata format with proper defaults
//  8. Delegate symlink creation to store.CreateSymlink()
//  9. Check for cancellation after create
//  10. Retrieve symlink attributes
//  11. Return symlink handle, attributes, and updated directory WCC data
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after directory lookup (before create operation)
//   - Checks after CreateSymlink (before attribute retrieval)
//   - No check during attribute conversion (pure computation, fast)
//   - Returns NFS3ErrIO status with context error for cancellation
//   - Always includes WCC data for cache consistency
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (creation, validation, access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which can implement:
//   - Write permission checking on the parent directory
//   - Access control based on UID/GID
//   - Ownership assignment for the new symlink
//
// **Symlink Semantics:**
//
// Per RFC 1813 and POSIX semantics:
//   - Target path is stored as-is without validation or resolution
//   - Target can be absolute or relative path
//   - Target can point to non-existent files (dangling symlinks are allowed)
//   - Symlink permissions are typically 0777 (lrwxrwxrwx)
//   - Actual access control is applied when following the symlink
//   - Symlink size is typically the length of the target pathname
//
// **Attribute Handling:**
//
// The client can provide optional attributes in the request:
//   - Mode: Usually ignored, symlinks default to 0777
//   - UID/GID: Used for ownership (or defaults to authenticated user)
//   - Size/Times: Ignored, set by server
//
// The protocol layer uses `convertSetAttrsToMetadata` to:
//   - Apply client-provided attributes (mode, uid, gid)
//   - Set defaults from authentication context when not provided
//   - Ensure consistent behavior with other file creation operations
//
// The store completes the attributes with:
//   - File type (symlink)
//   - Creation/modification times
//   - Size (length of target path)
//   - Target path storage
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - Name already exists → NFS3ErrExist
//   - Permission denied → NFS3ErrAcces
//   - No space left → NFS3ErrNoSpc
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients maintain cache coherency for the parent directory:
//  1. Capture directory attributes before the operation (WccBefore)
//  2. Perform the symlink creation
//  3. Capture directory attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect if directory changed during the operation
//   - Update their cached directory attributes
//   - Invalidate stale cached data
//
// **Performance Considerations:**
//
// SYMLINK is a metadata-only operation:
//   - No data blocks need to be allocated
//   - Target path is stored in metadata (or small inline data)
//   - Operation should be relatively fast
//   - May require directory block updates
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces write permission on parent directory
//   - Name validation prevents directory traversal attacks
//   - Target path is not validated (allows flexibility but requires care)
//   - Client context enables audit logging
//   - Symlinks can create security issues (symlink attacks, race conditions)
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for symlink operations
//   - req: The symlink request containing directory handle, name, target, and attributes
//
// **Returns:**
//   - *SymlinkResponse: Response with status, symlink handle/attributes, and directory WCC
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.10: SYMLINK Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &SymlinkRequest{
//	    DirHandle: dirHandle,
//	    Name:      "mylink",
//	    Target:    "/usr/bin/python3",
//	    Attr:      SetAttrs{Mode: &mode},
//	}
//	ctx := &SymlinkContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Symlink(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    // Symlink created successfully
//	    // Use resp.FileHandle for subsequent operations
//	}
func (h *Handler) Symlink(
	ctx *NFSHandlerContext,
	req *SymlinkRequest,
) (*SymlinkResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "SYMLINK cancelled before processing", "name", req.Name, "target", req.Target, "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &SymlinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "SYMLINK", "name", req.Name, "target", req.Target, "dir", fmt.Sprintf("0x%x", req.DirHandle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateSymlinkRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "SYMLINK validation failed", "name", req.Name, "target", req.Target, "client", clientIP, "error", err)
		return &SymlinkResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store from context
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		logger.WarnCtx(ctx.Context, "SYMLINK failed", "error", err, "dir", fmt.Sprintf("0x%x", req.DirHandle), "client", clientIP)
		return &SymlinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	dirHandle := metadata.FileHandle(req.DirHandle)

	logger.DebugCtx(ctx.Context, "SYMLINK", "share", ctx.Share, "name", req.Name, "target", req.Target)

	// ========================================================================
	// Step 3: Verify parent directory exists and capture pre-op attributes
	// ========================================================================

	dirFile, status, err := h.getFileOrError(ctx, metadataStore, dirHandle, "SYMLINK", req.DirHandle)
	if dirFile == nil {
		return &SymlinkResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&dirFile.FileAttr)

	// Verify parent is a directory
	if dirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "SYMLINK failed: handle not a directory", "dir", fmt.Sprintf("0x%x", req.DirHandle), "type", dirFile.Type, "client", clientIP)

		// Include directory attributes even on error for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &SymlinkResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    nfsDirAttr,
		}, nil
	}

	// Check for cancellation before the create operation
	// This is an important check since we're about to modify the filesystem
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "SYMLINK cancelled before create operation", "name", req.Name, "target", req.Target, "client", clientIP, "error", ctx.Context.Err())

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &SymlinkResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    nfsDirAttr,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Build authentication context for store
	// ========================================================================

	authCtx, nfsDirAttr, err := h.buildAuthContextWithWCCError(ctx, dirHandle, &dirFile.FileAttr, "SYMLINK", req.Name, req.DirHandle)
	if authCtx == nil {
		return &SymlinkResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    nfsDirAttr,
		}, err
	}

	// ========================================================================
	// Step 4: Convert client attributes to metadata format
	// ========================================================================
	// Use convertSetAttrsToMetadata to ensure consistent attribute handling
	// across all file creation operations (MKDIR, MKNOD, SYMLINK, CREATE).
	// This properly applies:
	// - Client-provided attributes (mode, uid, gid)
	// - Defaults from authentication context when not provided by client
	// - Default permissions (0777 for symlinks)
	//
	// The store will complete the attributes with:
	// - Timestamps (atime, mtime, ctime)
	// - Size (length of target path)
	// - Target path (stored in SymlinkTarget field)
	//
	// No cancellation check here - this is fast pure computation

	symlinkAttr := xdr.ConvertSetAttrsToMetadata(metadata.FileTypeSymlink, &req.Attr, authCtx)

	// ========================================================================
	// Step 5: Create symlink via store
	// ========================================================================
	// The store is responsible for:
	// - Checking write permission on parent directory
	// - Verifying name doesn't already exist
	// - Completing symlink attributes (timestamps, size, target path)
	// - Creating the symlink metadata
	// - Linking it to the parent directory
	// - Updating parent directory timestamps
	// - Respecting context cancellation internally

	createdSymlink, err := metadataStore.CreateSymlink(authCtx, dirHandle, req.Name, req.Target, symlinkAttr)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "SYMLINK cancelled during create operation", "name", req.Name, "target", req.Target, "client", clientIP, "error", ctx.Context.Err())

			// Get updated directory attributes for WCC data (best effort)
			var wccAfter *types.NFSFileAttr
			if updatedDirFile, getErr := metadataStore.GetFile(ctx.Context, dirHandle); getErr == nil {
				wccAfter = h.convertFileAttrToNFS(dirHandle, &updatedDirFile.FileAttr)
			}

			return &SymlinkResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				DirAttrBefore:   wccBefore,
				DirAttrAfter:    wccAfter,
			}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "SYMLINK failed: store error", "name", req.Name, "target", req.Target, "client", clientIP)

		// Get updated directory attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if updatedDirFile, getErr := metadataStore.GetFile(ctx.Context, dirHandle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(dirHandle, &updatedDirFile.FileAttr)
		}

		// Map store errors to NFS status codes
		status := xdr.MapStoreErrorToNFSStatus(err, clientIP, "SYMLINK")

		return &SymlinkResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirAttrBefore:   wccBefore,
			DirAttrAfter:    wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Encode file handle and prepare response
	// ========================================================================

	// Encode the file handle for the symlink
	symlinkHandle, err := metadata.EncodeFileHandle(createdSymlink)
	if err != nil {
		traceError(ctx.Context, err, "SYMLINK: failed to encode symlink handle")
		return &SymlinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Generate symlink attributes for response
	nfsSymlinkAttr := h.convertFileAttrToNFS(symlinkHandle, &createdSymlink.FileAttr)

	// Get updated directory attributes for WCC data
	updatedDirFile, err := metadataStore.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "SYMLINK: successful but cannot get updated directory attributes", "dir", fmt.Sprintf("0x%x", req.DirHandle), "error", err)
		// Continue with nil WccAfter rather than failing
	}

	var wccAfter *types.NFSFileAttr
	if updatedDirFile != nil {
		wccAfter = h.convertFileAttrToNFS(dirHandle, &updatedDirFile.FileAttr)
	}

	logger.InfoCtx(ctx.Context, "SYMLINK successful", "name", req.Name, "target", req.Target, "dir", fmt.Sprintf("0x%x", req.DirHandle), "handle", fmt.Sprintf("0x%x", symlinkHandle), "client", clientIP)

	logger.DebugCtx(ctx.Context, "SYMLINK details", "symlink_handle", fmt.Sprintf("0x%x", symlinkHandle), "mode", fmt.Sprintf("%o", createdSymlink.Mode), "uid", createdSymlink.UID, "gid", createdSymlink.GID, "size", createdSymlink.Size, "target_len", len(req.Target))

	return &SymlinkResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		FileHandle:      symlinkHandle,
		Attr:            nfsSymlinkAttr,
		DirAttrBefore:   wccBefore,
		DirAttrAfter:    wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// symlinkValidationError represents a SYMLINK request validation error.
type symlinkValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *symlinkValidationError) Error() string {
	return e.message
}

// validateSymlinkRequest validates SYMLINK request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Symlink name is valid (not empty, not "." or "..", length, characters)
//   - Target path is not empty and within reasonable length limits
//
// Returns:
//   - nil if valid
//   - *symlinkValidationError with NFS status if invalid
func validateSymlinkRequest(req *SymlinkRequest) *symlinkValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &symlinkValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate symlink name
	if req.Name == "" {
		return &symlinkValidationError{
			message:   "empty symlink name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &symlinkValidationError{
			message:   fmt.Sprintf("cannot create symlink named '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check symlink name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("symlink name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if strings.ContainsAny(req.Name, "\x00") {
		return &symlinkValidationError{
			message:   "symlink name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if strings.ContainsAny(req.Name, "/") {
		return &symlinkValidationError{
			message:   "symlink name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &symlinkValidationError{
				message:   fmt.Sprintf("symlink name contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	// Validate target path
	if req.Target == "" {
		return &symlinkValidationError{
			message:   "empty target path",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check target path length (using POSIX PATH_MAX = 4096 bytes)
	// While RFC 1813 specifies NFS3_MAXPATHLEN as 1024, we use PATH_MAX
	// for POSIX compliance - symlink targets can be up to 4095 bytes
	if len(req.Target) > types.NFS3MaxPathLen {
		return &symlinkValidationError{
			message:   fmt.Sprintf("target path too long: %d bytes (max %d)", len(req.Target), types.NFS3MaxPathLen),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes in target path
	if strings.ContainsAny(req.Target, "\x00") {
		return &symlinkValidationError{
			message:   "target path contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

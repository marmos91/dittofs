package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// LookupRequest represents a LOOKUP request from an NFS client.
// The client provides a directory handle and a filename to search for.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.3 specifies the LOOKUP procedure as:
//
//	LOOKUP3res NFSPROC3_LOOKUP(LOOKUP3args) = 3;
//
// The LOOKUP procedure is fundamental to NFS path resolution. It's used to:
//   - Navigate directory hierarchies
//   - Resolve pathnames component by component
//   - Obtain file handles for files and subdirectories
//   - Build complete paths from the root
type LookupRequest struct {
	// DirHandle is the file handle of the directory to search in.
	// Must be a valid directory handle obtained from MOUNT or a previous LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Filename is the name to search for within the directory.
	// Must follow NFS naming conventions:
	//   - Maximum 255 bytes
	//   - Cannot contain null bytes or path separators
	//   - Case-sensitive (unless filesystem is case-insensitive)
	Filename string
}

// LookupResponse represents the response to a LOOKUP request.
// It contains the status and, if successful, the file handle and attributes
// of the found file, plus optional post-operation directory attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type LookupResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// FileHandle is the handle of the found file or directory.
	// Only present when Status == types.NFS3OK.
	// This handle can be used in subsequent NFS operations.
	FileHandle []byte

	// Attr contains the attributes of the found file or directory.
	// Only present when Status == types.NFS3OK.
	// Includes type, permissions, size, timestamps, etc.
	Attr *types.NFSFileAttr

	// DirAttr contains post-operation attributes of the directory.
	// Optional, may be nil even on success.
	// Helps clients maintain cache consistency for the directory.
	DirAttr *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Lookup searches a directory for a specific name and returns its file handle.
//
// This implements the NFS LOOKUP procedure as defined in RFC 1813 Section 3.3.3.
//
// **Purpose:**
//
// LOOKUP is the fundamental building block for pathname resolution in NFS.
// Clients use it to traverse directory hierarchies one component at a time:
//   - Start with root handle from MOUNT
//   - LOOKUP "usr" in root → get handle for /usr
//   - LOOKUP "local" in /usr → get handle for /usr/local
//   - LOOKUP "bin" in /usr/local → get handle for /usr/local/bin
//
// **Process:**
//
//  1. Check for context cancellation before starting
//  2. Validate request parameters (handles, filename)
//  3. Build AuthContext for permission checking
//  4. Verify directory handle exists and is a directory (via GetFile)
//  5. Delegate lookup to store.Lookup() which atomically:
//     - Checks search/execute permission on directory
//     - Finds the child by name
//     - Returns handle AND attributes
//  6. Optionally retrieve directory attributes for cache consistency
//  7. Return file handle and attributes to client
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (child lookup, access control) is delegated to store
//   - store.Lookup() is atomic - combines search and attribute retrieval
//   - File handle validation is performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer builds AuthContext and passes it to store.Lookup(),
// which implements:
//   - Execute permission checking on the directory (search permission)
//   - Access control based on UID/GID
//   - Hiding files based on permissions
//
// **Path Resolution:**
//
// LOOKUP operates on single filename components only. For path resolution:
//   - Client splits "/usr/local/bin" into ["usr", "local", "bin"]
//   - Client performs separate LOOKUP for each component
//   - Server never sees or processes full paths
//   - This enables proper permission checking at each level
//
// **Special Names:**
//
//   - "." (current directory): Returns the directory's own handle
//   - ".." (parent directory): Returns the parent's handle
//   - Regular names: Search for child in directory
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - Child not found → types.NFS3ErrNoEnt
//   - Access denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO
//
// **Performance Considerations:**
//
// LOOKUP is one of the most frequently called NFS procedures. The new design:
//   - Combines search and attribute retrieval in one store call
//   - Reduces round trips to storage
//   - Built-in caching opportunities in store
//   - Minimal context cancellation overhead (check only at operation boundaries)
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store layer enforces directory search permission
//   - Filename validation prevents directory traversal
//   - Client context enables audit logging
//
// **Context Cancellation:**
//
// This operation respects context cancellation at key boundaries:
//   - Before operation starts
//   - Before GetFile for directory verification
//   - Before Lookup call
//
// Since LOOKUP is a high-frequency operation, cancellation checks are placed
// strategically to balance responsiveness with performance.
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for file and directory operations
//   - req: The lookup request containing directory handle and filename
//
// **Returns:**
//   - *LookupResponse: Response with status and file handle (if successful)
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.3: LOOKUP Procedure**
//
// Example:
//
//	handler := &Handler{}
//	req := &LookupRequest{
//	    DirHandle: dirHandle,
//	    Filename:  "myfile.txt",
//	}
//	ctx := &NFSHandlerContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    Share:      "/export",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Lookup(ctx, store, req)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.FileHandle for subsequent operations
//	}
func (h *Handler) Lookup(
	ctx *NFSHandlerContext,
	req *LookupRequest,
) (*LookupResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "LOOKUP",
		"name", req.Filename,
		"handle", fmt.Sprintf("%x", req.DirHandle),
		"client", clientIP,
		"auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "LOOKUP cancelled",
			"name", req.Filename,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP,
			"error", ctx.Context.Err())
		return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateLookupRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "LOOKUP validation failed",
			"name", req.Filename,
			"client", clientIP,
			"error", err)
		return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Get metadata store from context
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		logger.WarnCtx(ctx.Context, "LOOKUP failed",
			"error", err,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP)
		return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	dirHandle := metadata.FileHandle(req.DirHandle)
	logger.DebugCtx(ctx.Context, "LOOKUP",
		"share", ctx.Share,
		"name", req.Filename)

	// ========================================================================
	// Step 4: Verify directory handle exists and is valid
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "LOOKUP cancelled before GetFile (dir)",
			"name", req.Filename,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP,
			"error", ctx.Context.Err())
		return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	dirFile, err := metadataStore.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "LOOKUP failed: directory not found",
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP,
			"error", err)
		return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Verify parent is actually a directory
	if dirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "LOOKUP failed: handle not a directory",
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"type", dirFile.Type,
			"client", clientIP)

		// Include directory attributes even on error for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &LookupResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 4: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "LOOKUP cancelled during auth context building",
				"name", req.Filename,
				"handle", fmt.Sprintf("%x", req.DirHandle),
				"client", clientIP,
				"error", ctx.Context.Err())
			return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
		}

		traceError(ctx.Context, err, "LOOKUP failed: failed to build auth context",
			"name", req.Filename,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP)

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &LookupResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 5: Look up the child via store
	// ========================================================================
	// The store.Lookup() method atomically:
	// - Checks search/execute permission on the directory
	// - Finds the child by name (including "." and "..")
	// - Returns both handle AND attributes in one operation
	// - Enforces any access control policies

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "LOOKUP cancelled before Lookup",
			"name", req.Filename,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP,
			"error", ctx.Context.Err())

		// Include directory post-op attributes for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &LookupResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	childFile, err := metadataStore.Lookup(authCtx, dirHandle, req.Filename)
	if err != nil {
		logger.DebugCtx(ctx.Context, "LOOKUP failed: child not found or access denied",
			"name", req.Filename,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP,
			"error", err)

		// Map store errors to NFS status codes
		status := mapMetadataErrorToNFS(err)

		// Include directory post-op attributes for cache consistency
		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &LookupResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with file handle and attributes
	// ========================================================================
	// store.Lookup() already returned the File object with attributes,
	// so we don't need a separate GetFile() call!

	// Encode child file handle
	childHandle, err := metadata.EncodeFileHandle(childFile)
	if err != nil {
		traceError(ctx.Context, err, "LOOKUP failed: cannot encode child handle",
			"name", req.Filename,
			"handle", fmt.Sprintf("%x", req.DirHandle),
			"client", clientIP)

		nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &LookupResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirAttr:         nfsDirAttr,
		}, nil
	}

	// Generate file IDs from handles for NFS attributes
	nfsChildAttr := h.convertFileAttrToNFS(childHandle, &childFile.FileAttr)
	nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

	logger.InfoCtx(ctx.Context, "LOOKUP successful",
		"name", req.Filename,
		"handle", fmt.Sprintf("%x", childHandle),
		"type", nfsChildAttr.Type,
		"size", childFile.Size,
		"client", clientIP)

	logger.DebugCtx(ctx.Context, "LOOKUP details",
		"child_handle", fmt.Sprintf("%x", childHandle),
		"child_mode", fmt.Sprintf("%o", childFile.Mode),
		"dir_handle", fmt.Sprintf("%x", dirHandle))

	return &LookupResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		FileHandle:      childHandle,
		Attr:            nfsChildAttr,
		DirAttr:         nfsDirAttr,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// lookupValidationError represents a LOOKUP request validation error.
type lookupValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *lookupValidationError) Error() string {
	return e.message
}

// validateLookupRequest validates LOOKUP request parameters.
//
// Checks performed:
//   - Directory handle is not nil or empty
//   - Directory handle length is within RFC 1813 limits (max 64 bytes)
//   - Directory handle is long enough for file ID extraction (min 8 bytes)
//   - Filename is not empty
//   - Filename length doesn't exceed 255 bytes
//   - Filename doesn't contain invalid characters (null bytes)
//   - Filename doesn't contain path separators (prevents traversal)
//
// Returns:
//   - nil if valid
//   - *lookupValidationError with NFS status if invalid
func validateLookupRequest(req *LookupRequest) *lookupValidationError {
	// Validate directory handle
	if len(req.DirHandle) == 0 {
		return &lookupValidationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.DirHandle) > 64 {
		return &lookupValidationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &lookupValidationError{
			message:   fmt.Sprintf("directory handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate filename
	if req.Filename == "" {
		return &lookupValidationError{
			message:   "empty filename",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// NFS filename limit is typically 255 bytes
	if len(req.Filename) > 255 {
		return &lookupValidationError{
			message:   fmt.Sprintf("filename too long: %d bytes (max 255)", len(req.Filename)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.ContainsAny([]byte(req.Filename), "\x00") {
		return &lookupValidationError{
			message:   "filename contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	// Note: "." and ".." are allowed (handled specially by store)
	if bytes.ContainsAny([]byte(req.Filename), "/") {
		return &lookupValidationError{
			message:   "filename contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

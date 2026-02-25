package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
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

// Lookup handles NFS LOOKUP (RFC 1813 Section 3.3.3).
// Resolves a filename in a directory to a file handle and attributes.
// Delegates to MetadataService.Lookup which atomically checks permissions and finds the child.
// No side effects; read-only, high-frequency path resolution operation.
// Errors: NFS3ErrNoEnt (not found), NFS3ErrNotDir, NFS3ErrAcces, NFS3ErrIO.
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
	// Step 3: Get metadata from registry
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "LOOKUP failed: metadata service not initialized", "client", clientIP, "error", err)
		return &LookupResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
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

	dirFile, err := metaSvc.GetFile(ctx.Context, dirHandle)
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

		logError(ctx.Context, err, "LOOKUP failed: failed to build auth context",
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

	childFile, err := metaSvc.Lookup(authCtx, dirHandle, req.Filename)
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
		logError(ctx.Context, err, "LOOKUP failed: cannot encode child handle",
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
//   - *validationError with NFS status if invalid
func validateLookupRequest(req *LookupRequest) *validationError {
	// Validate directory handle
	if len(req.DirHandle) == 0 {
		return &validationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.DirHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("directory handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate filename
	if req.Filename == "" {
		return &validationError{
			message:   "empty filename",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// NFS filename limit is typically 255 bytes
	if len(req.Filename) > 255 {
		return &validationError{
			message:   fmt.Sprintf("filename too long: %d bytes (max 255)", len(req.Filename)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.ContainsAny([]byte(req.Filename), "\x00") {
		return &validationError{
			message:   "filename contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	// Note: "." and ".." are allowed (handled specially by store)
	if bytes.ContainsAny([]byte(req.Filename), "/") {
		return &validationError{
			message:   "filename contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

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

// LinkRequest represents a LINK request from an NFS client.
// The LINK procedure creates a hard link to an existing file.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.15 specifies the LINK procedure as:
//
//	LINK3res NFSPROC3_LINK(LINK3args) = 15;
//
// Hard links create additional directory entries that reference the same
// underlying file. All hard links to a file are equivalent - there is no
// "original" and modifications through any link affect all links.
type LinkRequest struct {
	// FileHandle is the file handle of the existing file to link to.
	// This must be a valid file handle for a regular file, not a directory.
	// Maximum length is 64 bytes per RFC 1813.
	FileHandle []byte

	// DirHandle is the file handle of the directory where the new link will be created.
	// This must be a valid directory handle.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name for the new link within the target directory.
	// Must follow NFS naming conventions (max 255 bytes, no null bytes or slashes).
	// Must not already exist in the target directory.
	Name string
}

// LinkResponse represents the response to a LINK request.
// It contains the status of the operation and, if successful, post-operation
// attributes for both the linked file and the target directory.
//
// The response is encoded in XDR format before being sent back to the client.
type LinkResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// FileAttr contains post-operation attributes of the linked file.
	// Only present when Status == types.NFS3OK or for cache consistency on errors.
	// The nlink count will be incremented to reflect the new hard link.
	FileAttr *types.NFSFileAttr

	// DirWccBefore contains pre-operation attributes of the target directory.
	// Used for weak cache consistency to help clients detect changes.
	DirWccBefore *types.WccAttr

	// DirWccAfter contains post-operation attributes of the target directory.
	// Used for weak cache consistency. Present for both success and failure.
	DirWccAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Link handles NFS LINK (RFC 1813 Section 3.3.15).
// Creates a hard link to an existing file in a target directory.
// Delegates to MetadataService.CreateHardLink after cross-share validation.
// Adds directory entry, increments nlink; returns file attrs and dir WCC data.
// Errors: NFS3ErrNoEnt, NFS3ErrExist, NFS3ErrIsDir, NFS3ErrNotDir, NFS3ErrAcces.
func (h *Handler) Link(
	ctx *NFSHandlerContext,
	req *LinkRequest,
) (*LinkResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "LINK", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "dir_handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation early
	// ========================================================================
	// LINK involves multiple operations, so respect cancellation to avoid
	// wasting resources on abandoned requests

	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "LINK cancelled", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateLinkRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "LINK validation failed", "name", req.Name, "client", clientIP, "error", err)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Get metadata store from context and verify cross-share restriction
	// ========================================================================

	// Decode file handle to verify it's from the same share
	fileHandle := metadata.FileHandle(req.FileHandle)
	fileShareName, _, err := metadata.DecodeFileHandle(fileHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "LINK failed: invalid file handle", "file_handle", fmt.Sprintf("%x", req.FileHandle), "client", clientIP, "error", err)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// Verify both handles are from the same share (cross-share linking not allowed)
	if ctx.Share != fileShareName {
		logger.WarnCtx(ctx.Context, "LINK failed: cross-share link attempted", "file_share", fileShareName, "dir_share", ctx.Share, "client", clientIP)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrInval}}, nil
	}

	metaSvc, svcErr := getMetadataService(h.Registry)
	if svcErr != nil {
		logger.ErrorCtx(ctx.Context, "LINK failed: metadata service not initialized", "client", clientIP, "error", svcErr)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	dirHandle := metadata.FileHandle(req.DirHandle)
	logger.DebugCtx(ctx.Context, "LINK", "share", ctx.Share, "name", req.Name)

	// ========================================================================
	// Step 4: Build AuthContext for permission checking
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "LINK cancelled during auth context building", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP, "error", ctx.Context.Err())
			return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "LINK failed: failed to build auth context", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 4: Check cancellation before first store operation
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "LINK cancelled before GetFile", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 5: Verify source file exists and is a regular file
	// ========================================================================

	fileAttr, err := metaSvc.GetFile(ctx.Context, fileHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "LINK failed: source file not found", "file_handle", fmt.Sprintf("%x", req.FileHandle), "client", clientIP, "error", err)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Hard links to directories are not allowed (prevents filesystem cycles)
	if fileAttr.Type == metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "LINK failed: cannot link directory", "file_handle", fmt.Sprintf("%x", req.FileHandle), "client", clientIP)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIsDir}}, nil
	}

	// ========================================================================
	// Step 6: Check cancellation before target directory lookup
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "LINK cancelled before directory lookup", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 7: Verify target directory exists and is a directory
	// ========================================================================

	dirFile, err := metaSvc.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "LINK failed: target directory not found", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", err)
		return &LinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Capture pre-operation directory attributes for WCC
	dirWccBefore := xdr.CaptureWccAttr(&dirFile.FileAttr)

	// Verify target is a directory
	if dirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "LINK failed: target not a directory", "handle", fmt.Sprintf("%x", req.DirHandle), "type", dirFile.Type, "client", clientIP)

		// Get current directory state for WCC
		dirWccAfter := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &LinkResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirWccBefore:    dirWccBefore,
			DirWccAfter:     dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 8: Check cancellation before name conflict check
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "LINK cancelled before name check", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 9: Check if name already exists in target directory using Lookup
	// ========================================================================

	_, err = metaSvc.Lookup(authCtx, dirHandle, req.Name)
	if err == nil {
		// No error means file exists
		logger.DebugCtx(ctx.Context, "LINK failed: name already exists", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		// Get updated directory attributes for WCC
		updatedDirFile, _ := metaSvc.GetFile(ctx.Context, dirHandle)
		dirWccAfter := h.convertFileAttrToNFS(dirHandle, &updatedDirFile.FileAttr)

		return &LinkResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrExist},
			DirWccBefore:    dirWccBefore,
			DirWccAfter:     dirWccAfter,
		}, nil
	}
	// If error, file doesn't exist (good) - continue with link creation

	// ========================================================================
	// Step 10: Check cancellation before write operation
	// ========================================================================
	// This is the most critical check as CreateHardLink modifies filesystem state

	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "LINK cancelled before CreateHardLink", "file_handle", fmt.Sprintf("%x", req.FileHandle), "name", req.Name, "client", clientIP, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 11: Create the hard link via store
	// ========================================================================
	// The store is responsible for:
	// - Verifying write access to the target directory
	// - Adding the new directory entry
	// - Incrementing the link count (nlink) on the file
	// - Updating directory timestamps

	err = metaSvc.CreateHardLink(authCtx, dirHandle, req.Name, fileHandle)
	if err != nil {
		traceError(ctx.Context, err, "LINK failed: store error", "name", req.Name, "client", clientIP)

		// Get updated directory attributes for WCC
		updatedDirFile, _ := metaSvc.GetFile(ctx.Context, dirHandle)
		dirWccAfter := h.convertFileAttrToNFS(dirHandle, &updatedDirFile.FileAttr)

		// Map store errors to NFS status codes
		status := mapMetadataErrorToNFS(err)

		return &LinkResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirWccBefore:    dirWccBefore,
			DirWccAfter:     dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 12: Build success response with updated attributes
	// ========================================================================
	// No cancellation check here - operation succeeded, fetching attributes
	// is best-effort for cache consistency

	// Get updated file attributes (nlink should be incremented)
	updatedFile, err := metaSvc.GetFile(ctx.Context, fileHandle)
	if err != nil {
		traceError(ctx.Context, err, "LINK: failed to get file attributes after link", "file_handle", fmt.Sprintf("%x", req.FileHandle))
		// Continue with cached attributes - this shouldn't happen but handle gracefully
	}

	nfsFileAttr := h.convertFileAttrToNFS(fileHandle, &updatedFile.FileAttr)

	// Get updated directory attributes
	updatedDirFile, _ := metaSvc.GetFile(ctx.Context, dirHandle)
	nfsDirAttr := h.convertFileAttrToNFS(dirHandle, &updatedDirFile.FileAttr)

	logger.InfoCtx(ctx.Context, "LINK successful", "name", req.Name, "file_handle", fmt.Sprintf("%x", req.FileHandle), "nlink", nfsFileAttr.Nlink, "client", clientIP)

	return &LinkResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		FileAttr:        nfsFileAttr,
		DirWccBefore:    dirWccBefore,
		DirWccAfter:     nfsDirAttr,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// ============================================================================
// Request Validation
// ============================================================================

// validateLinkRequest validates LINK request parameters.
//
// Checks performed:
//   - Source file handle is not empty and within limits
//   - Target directory handle is not empty and within limits
//   - Link name is valid (not empty, length, characters)
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateLinkRequest(req *LinkRequest) *validationError {
	// Validate source file handle
	if len(req.FileHandle) == 0 {
		return &validationError{
			message:   "empty source file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.FileHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("source file handle too long: %d bytes (max 64)", len(req.FileHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate target directory handle
	if len(req.DirHandle) == 0 {
		return &validationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate link name
	if req.Name == "" {
		return &validationError{
			message:   "empty link name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.Name) > 255 {
		return &validationError{
			message:   fmt.Sprintf("link name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for invalid characters
	if bytes.ContainsAny([]byte(req.Name), "/\x00") {
		return &validationError{
			message:   "link name contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &validationError{
			message:   fmt.Sprintf("link name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

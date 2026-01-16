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

// RemoveRequest represents a REMOVE request from an NFS client.
// The client provides a directory handle and filename to delete.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.12 specifies the REMOVE procedure as:
//
//	REMOVE3res NFSPROC3_REMOVE(REMOVE3args) = 12;
//
// The REMOVE procedure deletes a file from a directory. It cannot be used
// to remove directories (use RMDIR for that). The operation is atomic from
// the client's perspective.
type RemoveRequest struct {
	// DirHandle is the file handle of the parent directory containing the file.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Filename is the name of the file to remove from the directory.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	Filename string
}

// RemoveResponse represents the response to a REMOVE request.
// It contains the status of the operation and WCC (Weak Cache Consistency)
// data for the parent directory.
//
// The response is encoded in XDR format before being sent back to the client.
type RemoveResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// DirWccBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect changes.
	// May be nil if attributes could not be captured.
	DirWccBefore *types.WccAttr

	// DirWccAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide updated directory state.
	// May be nil on error, but should be present on success.
	DirWccAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Remove deletes a file from a directory.
//
// This implements the NFS REMOVE procedure as defined in RFC 1813 Section 3.3.12.
//
// **Purpose:**
//
// REMOVE deletes a regular file (not a directory) from a parent directory.
// It is one of the fundamental file system operations. Common use cases:
//   - Deleting temporary files
//   - Removing old or unused files
//   - Cleaning up workspace
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format, filename syntax)
//  3. Extract client IP and authentication credentials from context
//  4. Verify parent directory exists (via store)
//  5. Capture pre-operation directory state (for WCC)
//  6. Check for cancellation before remove operation
//  7. Delegate file removal to store.RemoveFile()
//  8. Return updated directory WCC data
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after directory lookup (before atomic remove operation)
//   - No check during RemoveFile to maintain atomicity
//   - Returns NFS3ErrIO status with context error for cancellation
//   - Always includes WCC data for cache consistency
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (deletion, validation, access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which can implement:
//   - Write permission checking on the parent directory
//   - Access control based on UID/GID
//   - Ownership verification (can only delete own files, or root can delete any)
//
// **REMOVE vs RMDIR:**
//
// REMOVE is for files only:
//   - Regular files: Success
//   - Directories: Returns types.NFS3ErrIsDir (must use RMDIR)
//   - Symbolic links: Success (removes the link, not the target)
//   - Special files: Success (device files, sockets, FIFOs)
//
// **Atomicity:**
//
// From the client's perspective, REMOVE is atomic. Either:
//   - The file is completely removed (success)
//   - The file remains unchanged (failure)
//
// There should be no intermediate state visible to clients.
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - File not found → types.NFS3ErrNoEnt
//   - File is directory → types.NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients maintain cache coherency:
//  1. Capture directory attributes before the operation (WccBefore)
//  2. Perform the file removal
//  3. Capture directory attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect if directory changed during the operation
//   - Update their cached directory attributes
//   - Invalidate stale cached data
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces write permission on parent directory
//   - Filename validation prevents directory traversal attacks
//   - Client context enables audit logging
//   - Cannot delete directories (prevents accidental data loss)
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for file and directory operations
//   - req: The remove request containing directory handle and filename
//
// **Returns:**
//   - *RemoveResponse: Response with status and directory WCC data
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.12: REMOVE Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &RemoveRequest{
//	    DirHandle: dirHandle,
//	    Filename:  "oldfile.txt",
//	}
//	ctx := &RemoveContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Remove(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    // File removed successfully
//	}
func (h *Handler) Remove(
	ctx *NFSHandlerContext,
	req *RemoveRequest,
) (*RemoveResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "REMOVE cancelled before processing", "name", req.Filename, "handle", fmt.Sprintf("%x", req.DirHandle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &RemoveResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "REMOVE", "name", req.Filename, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateRemoveRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "REMOVE validation failed", "name", req.Filename, "client", clientIP, "error", err)
		return &RemoveResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata and content services from registry
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()

	// Get content service for this share
	contentSvc := h.Registry.GetBlockService()

	dirHandle := metadata.FileHandle(req.DirHandle)

	logger.DebugCtx(ctx.Context, "REMOVE", "share", ctx.Share, "name", req.Filename)

	// ========================================================================
	// Step 3: Capture pre-operation directory attributes for WCC
	// ========================================================================

	dirFile, status, err := h.getFileOrError(ctx, dirHandle, "REMOVE", req.DirHandle)
	if dirFile == nil {
		return &RemoveResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&dirFile.FileAttr)

	// Check for cancellation before the remove operation
	// This is the most critical check - we don't want to start removing
	// the file if the client has already disconnected
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "REMOVE cancelled before remove operation", "name", req.Filename, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

		wccAfter := h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)

		return &RemoveResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Build authentication context with share-level identity mapping
	// ========================================================================

	authCtx, wccAfter, err := h.buildAuthContextWithWCCError(ctx, dirHandle, &dirFile.FileAttr, "REMOVE", req.Filename, req.DirHandle)
	if authCtx == nil {
		return &RemoveResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, err
	}

	// ========================================================================
	// Step 4: Remove file via store
	// ========================================================================
	// The store handles:
	// - Verifying parent is a directory
	// - Verifying the file exists
	// - Checking it's not a directory (must use RMDIR for directories)
	// - Verifying write permission on the parent directory
	// - Removing the file from the directory
	// - Deleting the file metadata
	// - Updating parent directory timestamps
	//
	// We don't check for cancellation inside RemoveFile to maintain atomicity.
	// The store should respect context internally for its operations.

	removedFileAttr, err := metaSvc.RemoveFile(authCtx, dirHandle, req.Filename)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "REMOVE cancelled during remove operation", "name", req.Filename, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

			// Get updated directory attributes for WCC data (best effort)
			var wccAfter *types.NFSFileAttr
			if dirFile, getErr := metaSvc.GetFile(ctx.Context, dirHandle); getErr == nil {
				wccAfter = h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)
			}

			return &RemoveResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				DirWccBefore:    wccBefore,
				DirWccAfter:     wccAfter,
			}, ctx.Context.Err()
		}

		// Map store errors to NFS status codes
		nfsStatus := xdr.MapStoreErrorToNFSStatus(err, clientIP, "REMOVE")

		// Get updated directory attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if dirFile, getErr := metaSvc.GetFile(ctx.Context, dirHandle); getErr == nil {
			wccAfter = h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)
		}

		return &RemoveResponse{
			NFSResponseBase: NFSResponseBase{Status: nfsStatus},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4.5: Delete content if file has content
	// ========================================================================
	// After successfully removing the metadata, attempt to delete the actual
	// file content. This is done after metadata removal to ensure consistency:
	// if metadata is removed but content deletion fails, the content becomes
	// orphaned but the file is still properly deleted from the client's view.
	//
	// Note: With async write mode, cached writes are flushed during COMMIT.
	// REMOVE should only delete what's already in the content store.
	// Any unflushed cache data will be cleaned up by cache eviction.

	if removedFileAttr.PayloadID != "" {
		if err := contentSvc.Delete(ctx.Context, ctx.Share, removedFileAttr.PayloadID); err != nil {
			// Log but don't fail the operation - metadata is already removed
			logger.WarnCtx(ctx.Context, "REMOVE: failed to delete content", "name", req.Filename, "content_id", removedFileAttr.PayloadID, "error", err)
			// This is non-fatal - the file is successfully removed from metadata
			// The orphaned content can be cleaned up later via garbage collection
		} else {
			logger.DebugCtx(ctx.Context, "REMOVE: deleted content", "name", req.Filename, "content_id", removedFileAttr.PayloadID)
		}
	}

	// ========================================================================
	// Step 5: Build success response with updated directory attributes
	// ========================================================================

	// Get updated directory attributes for WCC data
	dirFile, err = metaSvc.GetFile(ctx.Context, dirHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "REMOVE: file removed but cannot get updated directory attributes", "handle", fmt.Sprintf("%x", req.DirHandle), "error", err)
		// Continue with nil WccAfter rather than failing the entire operation
		wccAfter = nil
	} else {
		wccAfter = h.convertFileAttrToNFS(dirHandle, &dirFile.FileAttr)
	}

	logger.InfoCtx(ctx.Context, "REMOVE successful", "name", req.Filename, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

	// Convert internal type to NFS type for logging
	nfsType := uint32(removedFileAttr.Type) + 1 // Internal types are 0-based, NFS types are 1-based
	logger.DebugCtx(ctx.Context, "REMOVE details", "file_type", nfsType, "file_size", removedFileAttr.Size)

	return &RemoveResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		DirWccBefore:    wccBefore,
		DirWccAfter:     wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// removeValidationError represents a REMOVE request validation error.
type removeValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *removeValidationError) Error() string {
	return e.message
}

// validateRemoveRequest validates REMOVE request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Filename is valid (not empty, not "." or "..", length, characters)
//
// Returns:
//   - nil if valid
//   - *removeValidationError with NFS status if invalid
func validateRemoveRequest(req *RemoveRequest) *removeValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &removeValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &removeValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &removeValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate filename
	if req.Filename == "" {
		return &removeValidationError{
			message:   "empty filename",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Filename == "." || req.Filename == ".." {
		return &removeValidationError{
			message:   fmt.Sprintf("cannot remove '%s'", req.Filename),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check filename length (NFS limit is typically 255 bytes)
	if len(req.Filename) > 255 {
		return &removeValidationError{
			message:   fmt.Sprintf("filename too long: %d bytes (max 255)", len(req.Filename)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if strings.ContainsAny(req.Filename, "\x00") {
		return &removeValidationError{
			message:   "filename contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if strings.ContainsAny(req.Filename, "/") {
		return &removeValidationError{
			message:   "filename contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	for i, r := range req.Filename {
		if r < 0x20 || r == 0x7F {
			return &removeValidationError{
				message:   fmt.Sprintf("filename contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

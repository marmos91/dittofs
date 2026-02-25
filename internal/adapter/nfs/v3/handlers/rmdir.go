package handlers

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// RmdirRequest represents a RMDIR request from an NFS client.
// The client provides a parent directory handle and the name of the
// directory to remove.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.13 specifies the RMDIR procedure as:
//
//	RMDIR3res NFSPROC3_RMDIR(RMDIR3args) = 13;
//
// The RMDIR procedure removes (deletes) a subdirectory from a directory.
// The directory must be empty (contain no entries other than "." and "..").
type RmdirRequest struct {
	// DirHandle is the file handle of the parent directory containing
	// the directory to be removed.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name of the directory to remove within the parent directory.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	//   - Should not contain control characters
	Name string
}

// RmdirResponse represents the response to a RMDIR request.
// It contains the status of the operation and WCC (Weak Cache Consistency)
// data for the parent directory to help clients maintain cache coherency.
//
// The response is encoded in XDR format before being sent back to the client.
type RmdirResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// DirWccBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect if the parent
	// directory changed during the operation. May be nil.
	DirWccBefore *types.WccAttr

	// DirWccAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide the updated parent state.
	// May be nil on error, but should be present on success.
	DirWccAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Rmdir handles NFS RMDIR (RFC 1813 Section 3.3.13).
// Removes an empty directory from a parent directory (must contain only "." and "..").
// Delegates to MetadataService.RemoveDirectory after verifying parent is a directory.
// Removes directory entry and metadata from parent; returns parent WCC data.
// Errors: NFS3ErrNoEnt, NFS3ErrNotDir, NFS3ErrNotEmpty, NFS3ErrAcces, NFS3ErrIO.
func (h *Handler) Rmdir(
	ctx *NFSHandlerContext,
	req *RmdirRequest,
) (*RmdirResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "RMDIR", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "RMDIR cancelled", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())
		return &RmdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateRmdirRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "RMDIR validation failed", "name", req.Name, "client", clientIP, "error", err)
		return &RmdirResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 3: Get metadata store from context
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "RMDIR failed: metadata service not initialized", "client", clientIP, "error", err)
		return &RmdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	parentHandle := metadata.FileHandle(req.DirHandle)

	logger.DebugCtx(ctx.Context, "RMDIR", "share", ctx.Share, "name", req.Name)

	// ========================================================================
	// Step 4: Verify parent directory exists and is valid
	// ========================================================================

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "RMDIR cancelled before GetFile", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())
		return &RmdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	parentFile, err := metaSvc.GetFile(ctx.Context, parentHandle)
	if err != nil {
		logger.WarnCtx(ctx.Context, "RMDIR failed: parent not found", "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", err)
		return &RmdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&parentFile.FileAttr)

	// Verify parent is actually a directory
	if parentFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "RMDIR failed: parent not a directory", "handle", fmt.Sprintf("%x", req.DirHandle), "type", parentFile.Type, "client", clientIP)

		// Get current parent state for WCC
		wccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &RmdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Build authentication context with share-level identity mapping
	// ========================================================================

	authCtx, wccAfter, err := h.buildAuthContextWithWCCError(ctx, parentHandle, &parentFile.FileAttr, "RMDIR", req.Name, req.DirHandle)
	if authCtx == nil {
		return &RmdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, err
	}

	// ========================================================================
	// Step 5: Remove directory via store
	// ========================================================================
	// The store is responsible for:
	// - Verifying the directory exists
	// - Verifying it's actually a directory
	// - Checking it's empty (no entries except "." and "..")
	// - Checking write permission on parent directory
	// - Removing the directory entry from parent
	// - Deleting the directory metadata
	// - Updating parent directory timestamps

	// Check context before store call
	if ctx.isContextCancelled() {
		logger.WarnCtx(ctx.Context, "RMDIR cancelled before RemoveDirectory", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

		// Get updated parent attributes for WCC data
		parentFile, _ = metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &RmdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, nil
	}

	// Delegate to metaSvc for directory removal
	err = metaSvc.RemoveDirectory(authCtx, parentHandle, req.Name)
	if err != nil {
		logger.DebugCtx(ctx.Context, "RMDIR failed: store error", "name", req.Name, "client", clientIP, "error", err)

		// Get updated parent attributes for WCC data
		parentFile, _ = metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		// Map store errors to NFS status codes
		status := mapRmdirErrorToNFSStatus(err)

		return &RmdirResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			DirWccBefore:    wccBefore,
			DirWccAfter:     wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 5: Build success response with updated parent attributes
	// ========================================================================

	// Get updated parent directory attributes
	parentFile, _ = metaSvc.GetFile(ctx.Context, parentHandle)
	wccAfter = h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

	logger.InfoCtx(ctx.Context, "RMDIR successful", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

	logger.DebugCtx(ctx.Context, "RMDIR details", "parent_handle", fmt.Sprintf("%x", parentHandle))

	return &RmdirResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		DirWccBefore:    wccBefore,
		DirWccAfter:     wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// rmdirValidationError represents a RMDIR request validation error.
type rmdirValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *rmdirValidationError) Error() string {
	return e.message
}

// validateRmdirRequest validates RMDIR request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Directory name is valid (not empty, not "." or "..", length, characters)
//
// Returns:
//   - nil if valid
//   - *rmdirValidationError with NFS status if invalid
func validateRmdirRequest(req *RmdirRequest) *rmdirValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &rmdirValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &rmdirValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &rmdirValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate directory name
	if req.Name == "" {
		return &rmdirValidationError{
			message:   "empty directory name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &rmdirValidationError{
			message:   fmt.Sprintf("directory name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &rmdirValidationError{
			message:   fmt.Sprintf("directory name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if strings.ContainsAny(req.Name, "\x00") {
		return &rmdirValidationError{
			message:   "directory name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if strings.ContainsAny(req.Name, "/") {
		return &rmdirValidationError{
			message:   "directory name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	// This prevents potential issues with terminal output and logs
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &rmdirValidationError{
				message:   fmt.Sprintf("directory name contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

// ============================================================================
// Error Mapping
// ============================================================================

// mapRmdirErrorToNFSStatus maps store errors to NFS status codes.
// This provides consistent error mapping for RMDIR operations.
func mapRmdirErrorToNFSStatus(err error) uint32 {
	// Use the common metadata error mapper
	return mapMetadataErrorToNFS(err)
}

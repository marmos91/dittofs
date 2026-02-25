package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// GetAttrRequest represents a GETATTR request from an NFS client.
// The client provides a file handle to retrieve attributes for.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.1 specifies the GETATTR procedure as:
//
//	GETATTR3res NFSPROC3_GETATTR(GETATTR3args) = 1;
//
// The GETATTR procedure is used to obtain attributes for a file system object.
// This is one of the most frequently called NFS procedures.
type GetAttrRequest struct {
	// Handle is the file handle of the object to get attributes for.
	// Must be a valid file handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// GetAttrResponse represents the response to a GETATTR request.
// It contains the status of the operation and, if successful, the file attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type GetAttrResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains the file attributes.
	// Only present when Status == types.NFS3OK.
	// Includes file type, permissions, ownership, size, timestamps, etc.
	Attr *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// GetAttr handles NFS GETATTR (RFC 1813 Section 3.3.1).
// Returns file attributes (type, mode, size, timestamps, ownership) for a file handle.
// Delegates to MetadataService.GetFile for attribute retrieval.
// No side effects; read-only, high-frequency operation optimized for minimal overhead.
// Errors: NFS3ErrBadHandle (invalid handle), NFS3ErrStale (not found), NFS3ErrIO.
func (h *Handler) GetAttr(
	ctx *NFSHandlerContext,
	req *GetAttrRequest,
) (*GetAttrResponse, error) {
	// Check for cancellation before starting any work
	// This is the only pre-operation check for GETATTR to minimize overhead
	// GETATTR is one of the most frequently called procedures, so we optimize
	// for the common case of no cancellation
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "GETATTR cancelled before processing",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", ctx.ClientAddr,
			"error", ctx.Context.Err())
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.DebugCtx(ctx.Context, "GETATTR",
		"handle", fmt.Sprintf("%x", req.Handle),
		"client", clientIP,
		"auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateGetAttrRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "GETATTR validation failed",
			"handle", fmt.Sprintf("%x", req.Handle),
			"client", clientIP,
			"error", err)
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata from registry
	// ========================================================================

	if _, err := getMetadataService(h.Registry); err != nil {
		logger.ErrorCtx(ctx.Context, "GETATTR failed: metadata service not initialized", "client", clientIP, "error", err)
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 3: Verify file handle exists and retrieve attributes
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)

	file, status, err := h.getFileOrError(ctx, fileHandle, "GETATTR", req.Handle)
	if file == nil {
		return &GetAttrResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	logger.DebugCtx(ctx.Context, "GETATTR",
		"share", ctx.Share,
		"path", file.Path)

	// ========================================================================
	// Step 4: Generate file attributes with proper file ID
	// ========================================================================
	// The file ID is extracted from the handle for NFS protocol purposes.
	// This is a protocol-layer concern for creating the wire format.
	// No cancellation check here - this operation is extremely fast (pure computation)

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	logger.DebugCtx(ctx.Context, "GETATTR successful",
		"handle", fmt.Sprintf("%x", req.Handle),
		"type", nfsAttr.Type,
		"mode", fmt.Sprintf("%o", nfsAttr.Mode),
		"size", nfsAttr.Size,
		"nlink", nfsAttr.Nlink,
		"client", clientIP)

	logger.DebugCtx(ctx.Context, "GETATTR details",
		"handle", fmt.Sprintf("%x", fileHandle),
		"uid", nfsAttr.UID,
		"gid", nfsAttr.GID,
		"mtime", fmt.Sprintf("%d.%d", nfsAttr.Mtime.Seconds, nfsAttr.Mtime.Nseconds))

	return &GetAttrResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// validateGetAttrRequest validates GETATTR request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateGetAttrRequest(req *GetAttrRequest) *validationError {
	// Validate file handle presence
	if len(req.Handle) == 0 {
		return &validationError{
			message:   "file handle is empty",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	// This is a protocol-specific requirement for generating the fileid field
	if len(req.Handle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}

package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// ReadLinkRequest represents a READLINK request from an NFS client.
// The client provides a file handle for a symbolic link and requests
// the target path that the symlink points to.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.5 specifies the READLINK procedure as:
//
//	READLINK3res NFSPROC3_READLINK(READLINK3args) = 5;
//
// The READLINK procedure reads the data associated with a symbolic link.
// Symbolic links are special files that contain a pathname to another file
// or directory. Following a symbolic link involves reading the link contents
// and using that path for the next lookup operation.
type ReadLinkRequest struct {
	// Handle is the file handle of the symbolic link to read.
	// Must be a valid symlink handle obtained from MOUNT, LOOKUP, or CREATE.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// ReadLinkResponse represents the response to a READLINK request.
// It contains the status of the operation and, if successful, the target
// path and optional post-operation attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type ReadLinkResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains the post-operation attributes of the symbolic link.
	// Optional, may be nil. These attributes help clients maintain cache
	// consistency for the symlink itself (not the target).
	Attr *types.NFSFileAttr

	// Target is the symbolic link target path.
	// Only present when Status == types.NFS3OK.
	// This is the path string stored in the symlink file.
	// May be absolute (/usr/bin/python) or relative (../lib/file.so).
	// Maximum length is 1024 bytes per POSIX PATH_MAX.
	Target string
}

// ============================================================================
// Protocol Handler
// ============================================================================

// ReadLink handles NFS READLINK (RFC 1813 Section 3.3.5).
// Reads the target pathname stored in a symbolic link for client-side path resolution.
// Delegates to MetadataService.ReadSymlink; falls back to MFsymlink detection for SMB-created symlinks.
// No side effects; read-only metadata operation returning target path and post-op attributes.
// Errors: NFS3ErrNoEnt (not found), NFS3ErrInval (not a symlink), NFS3ErrAcces, NFS3ErrIO.
func (h *Handler) ReadLink(
	ctx *NFSHandlerContext,
	req *ReadLinkRequest,
) (*ReadLinkResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start processing. While READLINK is fast, we should still
	// respect cancellation to avoid wasted work on abandoned requests.
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READLINK: request cancelled at entry", "handle", fmt.Sprintf("%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "READLINK", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadLinkRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "READLINK validation failed", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "error", err)
		return &ReadLinkResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	// Symlink file resolved in store

	// ========================================================================
	// Step 3: Build authentication context for store
	// ========================================================================
	// The store needs authentication details to enforce access control
	// on the symbolic link (read permission checking)

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "READLINK cancelled during auth context building", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "error", ctx.Context.Err())
			return &ReadLinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
		}

		logError(ctx.Context, err, "READLINK failed: failed to build auth context", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP)
		return &ReadLinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// ========================================================================
	// Step 3: Read symlink target via store
	// ========================================================================
	// The store is responsible for:
	// - Verifying the handle is a valid symlink
	// - Checking read permission on the symlink
	// - Retrieving the target path
	// - Handling any I/O errors
	// - Respecting context cancellation

	metaSvc, svcErr := getMetadataService(h.Registry)
	if svcErr != nil {
		logger.ErrorCtx(ctx.Context, "READLINK failed: metadata service not initialized", "client", clientIP, "error", svcErr)
		return &ReadLinkResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	target, file, err := metaSvc.ReadSymlink(authCtx, fileHandle)
	if err != nil {
		// Check if error is due to context cancellation
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.DebugCtx(ctx.Context, "READLINK: store operation cancelled", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP)
			return nil, err
		}

		// ReadSymlink failed - check if this is an unconverted MFsymlink
		// (SMB-created symlink not yet converted on CLOSE)
		mfsResult := h.checkMFsymlinkByHandle(ctx, fileHandle)
		if mfsResult.IsMFsymlink {
			logger.InfoCtx(ctx.Context, "READLINK successful (MFsymlink)",
				"handle", fmt.Sprintf("%x", req.Handle),
				"target", mfsResult.Target,
				"client", clientIP)

			nfsAttr := h.convertFileAttrToNFS(fileHandle, mfsResult.ModifiedAttr)
			return &ReadLinkResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
				Attr:            nfsAttr,
				Target:          mfsResult.Target,
			}, nil
		}

		logger.WarnCtx(ctx.Context, "READLINK failed", "handle", fmt.Sprintf("%x", req.Handle), "client", clientIP, "error", err)

		// Map store errors to NFS status codes
		status := mapReadLinkErrorToNFSStatus(err)

		return &ReadLinkResponse{NFSResponseBase: NFSResponseBase{Status: status}}, nil
	}

	// ========================================================================
	// Step 4: Generate file attributes for cache consistency
	// ========================================================================

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	logger.InfoCtx(ctx.Context, "READLINK successful", "handle", fmt.Sprintf("%x", req.Handle), "target", target, "target_len", len(target), "client", clientIP)

	logger.DebugCtx(ctx.Context, "READLINK details", "handle", fmt.Sprintf("%x", fileHandle), "mode", fmt.Sprintf("%o", file.Mode), "uid", file.UID, "gid", file.GID, "size", file.Size)

	return &ReadLinkResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Target:          target,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// validateReadLinkRequest validates READLINK request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateReadLinkRequest(req *ReadLinkRequest) *validationError {
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
	if len(req.Handle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}

// ============================================================================
// Error Mapping
// ============================================================================

// mapReadLinkErrorToNFSStatus maps store errors to NFS status codes.
// This provides consistent error handling across the READLINK operation.
func mapReadLinkErrorToNFSStatus(err error) uint32 {
	// Use the common metadata error mapper
	return mapMetadataErrorToNFS(err)
}

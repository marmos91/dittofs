package handlers

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// RenameRequest represents a RENAME request from an NFS client.
// The client provides source and destination directory handles and names
// to move or rename a file or directory.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.14 specifies the RENAME procedure as:
//
//	RENAME3res NFSPROC3_RENAME(RENAME3args) = 14;
//
// The RENAME procedure changes the name of a file or directory, and can also
// move it to a different directory (if supported by the filesystem).
type RenameRequest struct {
	// FromDirHandle is the file handle of the source directory.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	FromDirHandle []byte

	// FromName is the current name of the file or directory to rename.
	// Must follow NFS naming conventions (max 255 bytes, no null bytes or slashes).
	FromName string

	// ToDirHandle is the file handle of the destination directory.
	// Must be a valid directory handle.
	// Can be the same as FromDirHandle for a simple rename.
	// Maximum length is 64 bytes per RFC 1813.
	ToDirHandle []byte

	// ToName is the new name for the file or directory.
	// Must follow NFS naming conventions (max 255 bytes, no null bytes or slashes).
	// If a file with this name already exists in the destination, it will be
	// replaced (atomically, if the filesystem supports it).
	ToName string
}

// RenameResponse represents the response to a RENAME request.
// It contains the status of the operation and WCC data for both
// source and destination directories.
//
// The response is encoded in XDR format before being sent back to the client.
type RenameResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// FromDirWccBefore contains pre-operation attributes of the source directory.
	// Used for weak cache consistency.
	FromDirWccBefore *types.WccAttr

	// FromDirWccAfter contains post-operation attributes of the source directory.
	// Used for weak cache consistency.
	FromDirWccAfter *types.NFSFileAttr

	// ToDirWccBefore contains pre-operation attributes of the destination directory.
	// Used for weak cache consistency.
	ToDirWccBefore *types.WccAttr

	// ToDirWccAfter contains post-operation attributes of the destination directory.
	// Used for weak cache consistency.
	ToDirWccAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Rename handles NFS RENAME (RFC 1813 Section 3.3.14).
// Moves/renames a file or directory between directories, atomically replacing target if it exists.
// Delegates to MetadataService.Move after cross-share validation; handles .nfs* silly-rename orphaning.
// Updates source and dest directory entries and timestamps; returns WCC data for both directories.
// Errors: NFS3ErrNoEnt, NFS3ErrNotDir, NFS3ErrNotEmpty, NFS3ErrAcces, NFS3ErrInval, NFS3ErrIO.
func (h *Handler) Rename(
	ctx *NFSHandlerContext,
	req *RenameRequest,
) (*RenameResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "RENAME cancelled before processing", "from", req.FromName, "to", req.ToName, "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &RenameResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "RENAME", "from", req.FromName, "from_dir", fmt.Sprintf("0x%x", req.FromDirHandle), "to", req.ToName, "to_dir", fmt.Sprintf("0x%x", req.ToDirHandle), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateRenameRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "RENAME validation failed", "from", req.FromName, "to", req.ToName, "client", clientIP, "error", err)
		return &RenameResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store from context and validate handles
	// ========================================================================

	metaSvc, svcErr := getMetadataService(h.Registry)
	if svcErr != nil {
		logger.ErrorCtx(ctx.Context, "RENAME failed: metadata service not initialized", "client", clientIP, "error", svcErr)
		return &RenameResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fromDirHandle := metadata.FileHandle(req.FromDirHandle)
	toDirHandle := metadata.FileHandle(req.ToDirHandle)

	// Verify both handles are from the same share (cross-share rename not allowed)
	// This is validated by extracting share from both handles
	fromShareName, _, fromErr := metadata.DecodeFileHandle(fromDirHandle)
	toShareName, _, toErr := metadata.DecodeFileHandle(toDirHandle)
	if fromErr != nil || toErr != nil {
		logger.WarnCtx(ctx.Context, "RENAME failed: invalid file handle", "from_dir", fmt.Sprintf("0x%x", req.FromDirHandle), "to_dir", fmt.Sprintf("0x%x", req.ToDirHandle), "client", clientIP)
		return &RenameResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	if fromShareName != toShareName {
		logger.WarnCtx(ctx.Context, "RENAME failed: cross-share rename attempted", "from_share", fromShareName, "to_share", toShareName, "client", clientIP)
		return &RenameResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrInval}}, nil
	}

	logger.DebugCtx(ctx.Context, "RENAME", "share", ctx.Share, "from", req.FromName, "to", req.ToName)

	// ========================================================================
	// Step 3: Verify source directory exists and is valid
	// ========================================================================

	fromDirFile, status, err := h.getFileOrError(ctx, fromDirHandle, "RENAME", req.FromDirHandle)
	if fromDirFile == nil {
		return &RenameResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation attributes for source directory
	fromDirWccBefore := xdr.CaptureWccAttr(&fromDirFile.FileAttr)

	// Verify source is a directory
	if fromDirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "RENAME failed: source handle not a directory", "dir", fmt.Sprintf("0x%x", req.FromDirHandle), "type", fromDirFile.Type, "client", clientIP)

		fromDirWccAfter := h.convertFileAttrToNFS(fromDirHandle, &fromDirFile.FileAttr)

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: types.NFS3ErrNotDir},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
		}, nil
	}

	// Check for cancellation before destination directory lookup
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "RENAME cancelled before destination lookup", "from", req.FromName, "to", req.ToName, "client", clientIP, "error", ctx.Context.Err())

		fromDirWccAfter := h.convertFileAttrToNFS(fromDirHandle, &fromDirFile.FileAttr)

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: types.NFS3ErrIO},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Verify destination directory exists and is valid
	// ========================================================================

	toDirFile, status, err := h.getFileOrError(ctx, toDirHandle, "RENAME", req.ToDirHandle)
	if toDirFile == nil {
		// Return WCC for source directory
		fromDirWccAfter := h.convertFileAttrToNFS(fromDirHandle, &fromDirFile.FileAttr)

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: status},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
		}, err
	}

	// Capture pre-operation attributes for destination directory
	toDirWccBefore := xdr.CaptureWccAttr(&toDirFile.FileAttr)

	// Verify destination is a directory
	if toDirFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "RENAME failed: destination handle not a directory", "dir", fmt.Sprintf("0x%x", req.ToDirHandle), "type", toDirFile.Type, "client", clientIP)

		fromDirWccAfter := h.convertFileAttrToNFS(fromDirHandle, &fromDirFile.FileAttr)

		toDirWccAfter := h.convertFileAttrToNFS(toDirHandle, &toDirFile.FileAttr)

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: types.NFS3ErrNotDir},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, nil
	}

	// Check for cancellation before the atomic rename operation
	// This is the most critical check - we don't want to start the rename
	// if the client has already disconnected
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "RENAME cancelled before rename operation", "from", req.FromName, "to", req.ToName, "client", clientIP, "error", ctx.Context.Err())

		fromDirWccAfter := h.convertFileAttrToNFS(fromDirHandle, &fromDirFile.FileAttr)

		toDirWccAfter := h.convertFileAttrToNFS(toDirHandle, &toDirFile.FileAttr)

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: types.NFS3ErrIO},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 4: Build authentication context for store
	// ========================================================================

	authCtx, fromDirWccAfter, err := h.buildAuthContextWithWCCError(ctx, fromDirHandle, &fromDirFile.FileAttr, "RENAME", req.FromName, req.FromDirHandle)
	if authCtx == nil {
		toDirWccAfter := h.convertFileAttrToNFS(toDirHandle, &toDirFile.FileAttr)

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: types.NFS3ErrIO},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, err
	}

	// ========================================================================
	// Step 4.5: Cross-protocol oplock break on source and destination (placeholder)
	// ========================================================================
	// TODO(plan-03): Wire to LockManager.CheckAndBreakOpLocksForDelete() once
	// centralized break methods are available (Phase 26 Plan 03).
	// Previously: metaSvc.CheckAndBreakLeasesForDelete(ctx.Context, sourceHandle)
	//             metaSvc.CheckAndBreakLeasesForDelete(ctx.Context, destHandle)

	// ========================================================================
	// Step 5: Perform rename via store
	// ========================================================================
	// The store is responsible for:
	// - Verifying source file exists
	// - Checking write permissions on both directories
	// - Handling atomic replacement of destination if it exists
	// - Ensuring destination is not a non-empty directory
	// - Updating parent relationships
	// - Updating directory timestamps
	// - Ensuring atomicity or proper rollback
	//
	// We don't check for cancellation inside RenameFile to maintain atomicity.
	// The store should respect context internally for its operations.

	err = metaSvc.Move(authCtx, fromDirHandle, req.FromName, toDirHandle, req.ToName)
	if err == nil {
		// ====================================================================
		// NFS-specific: Handle silly rename (.nfs* pattern)
		// ====================================================================
		// When an NFS client deletes a file that's still open, it renames the
		// file to a temporary name starting with ".nfs". We mark such files as
		// orphaned (nlink=0) so that fstat() returns the correct link count.
		// This is NFS protocol behavior, not general POSIX semantics.
		if strings.HasPrefix(req.ToName, ".nfs") {
			if renamedHandle, childErr := metaSvc.GetChild(ctx.Context, toDirHandle, req.ToName); childErr == nil {
				// Use a minimal auth context for the orphan operation
				orphanCtx := &metadata.AuthContext{
					Context:  ctx.Context,
					Identity: authCtx.Identity,
				}
				if markErr := metaSvc.MarkFileAsOrphaned(orphanCtx, renamedHandle); markErr != nil {
					// Log but don't fail the rename - the rename itself succeeded
					logger.DebugCtx(ctx.Context, "RENAME: failed to mark silly-renamed file as orphaned", "name", req.ToName, "error", markErr)
				}
			}
		}
	}
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "RENAME cancelled during rename operation", "from", req.FromName, "to", req.ToName, "client", clientIP, "error", ctx.Context.Err())

			// Get updated directory attributes for WCC data (best effort)
			var fromDirWccAfter *types.NFSFileAttr
			if updatedFromDirFile, getErr := metaSvc.GetFile(ctx.Context, fromDirHandle); getErr == nil {
				fromDirWccAfter = h.convertFileAttrToNFS(fromDirHandle, &updatedFromDirFile.FileAttr)
			}

			var toDirWccAfter *types.NFSFileAttr
			if updatedToDirFile, getErr := metaSvc.GetFile(ctx.Context, toDirHandle); getErr == nil {
				toDirWccAfter = h.convertFileAttrToNFS(toDirHandle, &updatedToDirFile.FileAttr)
			}

			return &RenameResponse{
				NFSResponseBase:  NFSResponseBase{Status: types.NFS3ErrIO},
				FromDirWccBefore: fromDirWccBefore,
				FromDirWccAfter:  fromDirWccAfter,
				ToDirWccBefore:   toDirWccBefore,
				ToDirWccAfter:    toDirWccAfter,
			}, ctx.Context.Err()
		}

		logError(ctx.Context, err, "RENAME failed: store error", "from", req.FromName, "to", req.ToName, "client", clientIP)

		// Get updated directory attributes for WCC data
		var fromDirWccAfter *types.NFSFileAttr
		if updatedFromDirFile, getErr := metaSvc.GetFile(ctx.Context, fromDirHandle); getErr == nil {
			fromDirWccAfter = h.convertFileAttrToNFS(fromDirHandle, &updatedFromDirFile.FileAttr)
		}

		var toDirWccAfter *types.NFSFileAttr
		if updatedToDirFile, getErr := metaSvc.GetFile(ctx.Context, toDirHandle); getErr == nil {
			toDirWccAfter = h.convertFileAttrToNFS(toDirHandle, &updatedToDirFile.FileAttr)
		}

		// Map store errors to NFS status codes
		status := xdr.MapStoreErrorToNFSStatus(err, clientIP, "rename")

		return &RenameResponse{
			NFSResponseBase:  NFSResponseBase{Status: status},
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with updated WCC data
	// ========================================================================

	// Get updated source directory attributes
	if updatedFromDirFile, getErr := metaSvc.GetFile(ctx.Context, fromDirHandle); getErr != nil {
		logger.WarnCtx(ctx.Context, "RENAME: successful but cannot get updated source directory attributes", "dir", fmt.Sprintf("0x%x", req.FromDirHandle), "error", getErr)
		fromDirWccAfter = nil
	} else {
		fromDirWccAfter = h.convertFileAttrToNFS(fromDirHandle, &updatedFromDirFile.FileAttr)
	}

	// Get updated destination directory attributes
	var toDirWccAfter *types.NFSFileAttr
	if updatedToDirFile, getErr := metaSvc.GetFile(ctx.Context, toDirHandle); getErr != nil {
		logger.WarnCtx(ctx.Context, "RENAME: successful but cannot get updated destination directory attributes", "dir", fmt.Sprintf("0x%x", req.ToDirHandle), "error", getErr)
		// toDirWccAfter will be nil
	} else {
		toDirWccAfter = h.convertFileAttrToNFS(toDirHandle, &updatedToDirFile.FileAttr)
	}

	logger.InfoCtx(ctx.Context, "RENAME successful", "from", req.FromName, "to", req.ToName, "client", clientIP)

	// Extract IDs for debug logging
	fromDirID := xdr.ExtractFileID(fromDirHandle)
	toDirID := xdr.ExtractFileID(toDirHandle)
	logger.DebugCtx(ctx.Context, "RENAME details", "from_dir", fromDirID, "to_dir", toDirID, "same_dir", bytes.Equal(req.FromDirHandle, req.ToDirHandle))

	return &RenameResponse{
		NFSResponseBase:  NFSResponseBase{Status: types.NFS3OK},
		FromDirWccBefore: fromDirWccBefore,
		FromDirWccAfter:  fromDirWccAfter,
		ToDirWccBefore:   toDirWccBefore,
		ToDirWccAfter:    toDirWccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// validateRenameRequest validates RENAME request parameters.
//
// Checks performed:
//   - Source directory handle is not empty and within limits
//   - Destination directory handle is not empty and within limits
//   - Source and destination names are valid
//   - Names are not "." or ".."
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateRenameRequest(req *RenameRequest) *validationError {
	// Validate source directory handle
	if len(req.FromDirHandle) == 0 {
		return &validationError{
			message:   "empty source directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.FromDirHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("source directory handle too long: %d bytes (max 64)", len(req.FromDirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate destination directory handle
	if len(req.ToDirHandle) == 0 {
		return &validationError{
			message:   "empty destination directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.ToDirHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("destination directory handle too long: %d bytes (max 64)", len(req.ToDirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate source name
	if req.FromName == "" {
		return &validationError{
			message:   "empty source name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.FromName) > 255 {
		return &validationError{
			message:   fmt.Sprintf("source name too long: %d bytes (max 255)", len(req.FromName)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for reserved names
	if req.FromName == "." || req.FromName == ".." {
		return &validationError{
			message:   fmt.Sprintf("cannot rename '%s'", req.FromName),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for invalid characters in source name
	if strings.ContainsAny(req.FromName, "/\x00") {
		return &validationError{
			message:   "source name contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate destination name
	if req.ToName == "" {
		return &validationError{
			message:   "empty destination name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.ToName) > 255 {
		return &validationError{
			message:   fmt.Sprintf("destination name too long: %d bytes (max 255)", len(req.ToName)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for reserved names
	if req.ToName == "." || req.ToName == ".." {
		return &validationError{
			message:   fmt.Sprintf("cannot rename to '%s'", req.ToName),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for invalid characters in destination name
	if strings.ContainsAny(req.ToName, "/\x00") {
		return &validationError{
			message:   "destination name contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

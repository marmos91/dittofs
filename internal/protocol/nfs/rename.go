package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
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
	// Status indicates the result of the rename operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): Source file or directory not found
	//   - types.NFS3ErrNotDir (20): From or To handle is not a directory
	//   - NFS3ErrInval (22): Invalid argument (e.g., renaming to "." or "..")
	//   - NFS3ErrExist (17): Destination exists and cannot be replaced
	//   - NFS3ErrNotEmpty (66): Attempt to rename directory over non-empty directory
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrXDev (18): Cross-device rename attempted (not supported)
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

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

// RenameContext contains the context information needed to process a RENAME request.
// This includes client identification, authentication details, and cancellation handling
// for access control.
type RenameContext struct {
	// Context carries cancellation signals and deadlines
	// The Rename handler checks this context to abort operations if the client
	// disconnects or the request times out
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the authenticated user ID (from AUTH_UNIX).
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Rename changes the name of a file or directory, optionally moving it.
//
// This implements the NFS RENAME procedure as defined in RFC 1813 Section 3.3.14.
//
// **Purpose:**
//
// RENAME is used to:
//   - Change a file's name within the same directory
//   - Move a file to a different directory
//   - Atomically replace an existing file with a new one
//
// Common use cases:
//   - File renaming: mv oldname.txt newname.txt
//   - File moving: mv file.txt /other/directory/
//   - Atomic replacement: mv newfile.txt existingfile.txt
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handles, names)
//  3. Extract client IP and authentication credentials from context
//  4. Verify source directory exists and is valid
//  5. Capture pre-operation WCC data for source directory
//  6. Check for cancellation before destination lookup
//  7. Verify destination directory exists and is valid
//  8. Capture pre-operation WCC data for destination directory
//  9. Check for cancellation before atomic rename operation
//  10. Delegate rename operation to repository.RenameFile()
//  11. Return updated WCC data for both directories
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after source directory lookup
//   - Checks after destination directory lookup (before atomic rename)
//   - No check during RenameFile to maintain atomicity
//   - Returns NFS3ErrIO status with context error for cancellation
//   - Always includes WCC data for both directories for cache consistency
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (rename, replace, validation) delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which implements:
//   - Write permission checking on source directory (to remove entry)
//   - Write permission checking on destination directory (to add entry)
//   - Ownership checks for replacing existing files
//   - Access control based on UID/GID
//
// **Atomicity:**
//
// Per RFC 1813, RENAME should be atomic if possible:
//   - If destination exists, it should be replaced atomically
//   - The source should not disappear before destination is updated
//   - Failures should leave filesystem in consistent state
//
// The repository implementation should ensure atomicity or handle
// failure recovery appropriately.
//
// **Special Cases:**
//
//   - Renaming to same name in same directory: Success (no-op)
//   - Renaming over existing file: Replaces atomically if allowed
//   - Renaming over existing directory: Only if empty (RFC 1813 requirement)
//   - Renaming directory over file: Not allowed (NFS3ErrExist or types.NFS3ErrNotDir)
//   - Renaming file over directory: Not allowed (NFS3ErrExist or types.NFS3ErrIsDir)
//   - Renaming "." or "..": Not allowed (NFS3ErrInval)
//   - Cross-filesystem rename: May not be supported (NFS3ErrXDev)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Source not found → types.NFS3ErrNoEnt
//   - Source/dest not directory → types.NFS3ErrNotDir
//   - Invalid names → NFS3ErrInval
//   - Permission denied → NFS3ErrAcces
//   - Cross-device → NFS3ErrXDev
//   - Destination is non-empty directory → NFS3ErrNotEmpty
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Weak Cache Consistency (WCC):**
//
// WCC data is provided for both source and destination directories:
//  1. Capture pre-operation attributes for both directories
//  2. Perform the rename operation
//  3. Capture post-operation attributes for both directories
//  4. Return both sets of WCC data to client
//
// This helps clients detect concurrent modifications and maintain
// cache consistency for both affected directories.
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces write permission on both directories
//   - Name validation prevents directory traversal
//   - Cannot rename "." or ".." (prevents filesystem corruption)
//   - Client context enables audit logging
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - repository: The metadata repository for file operations
//   - req: The rename request containing source and destination info
//
// **Returns:**
//   - *RenameResponse: Response with status and WCC data
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.14: RENAME Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &RenameRequest{
//	    FromDirHandle: sourceDirHandle,
//	    FromName:      "oldname.txt",
//	    ToDirHandle:   destDirHandle,
//	    ToName:        "newname.txt",
//	}
//	ctx := &RenameContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Rename(ctx, repository, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    // Rename successful
//	}
func (h *DefaultNFSHandler) Rename(
	ctx *RenameContext,
	repository metadata.Repository,
	req *RenameRequest,
) (*RenameResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("RENAME cancelled before processing: from='%s' to='%s' client=%s error=%v",
			req.FromName, req.ToName, ctx.ClientAddr, ctx.Context.Err())
		return &RenameResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("RENAME: from='%s' in dir=%x to='%s' in dir=%x client=%s auth=%d",
		req.FromName, req.FromDirHandle, req.ToName, req.ToDirHandle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateRenameRequest(req); err != nil {
		logger.Warn("RENAME validation failed: from='%s' to='%s' client=%s error=%v",
			req.FromName, req.ToName, clientIP, err)
		return &RenameResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify source directory exists and is valid
	// ========================================================================

	fromDirHandle := metadata.FileHandle(req.FromDirHandle)
	fromDirAttr, err := repository.GetFile(ctx.Context, fromDirHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("RENAME cancelled during source directory lookup: from='%s' to='%s' client=%s error=%v",
				req.FromName, req.ToName, clientIP, ctx.Context.Err())
			return &RenameResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Warn("RENAME failed: source directory not found: dir=%x client=%s error=%v",
			req.FromDirHandle, clientIP, err)
		return &RenameResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for source directory
	fromDirWccBefore := xdr.CaptureWccAttr(fromDirAttr)

	// Verify source is a directory
	if fromDirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("RENAME failed: source handle not a directory: dir=%x type=%d client=%s",
			req.FromDirHandle, fromDirAttr.Type, clientIP)

		fromDirID := xdr.ExtractFileID(fromDirHandle)
		fromDirWccAfter := xdr.MetadataToNFS(fromDirAttr, fromDirID)

		return &RenameResponse{
			Status:           types.NFS3ErrNotDir,
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
		}, nil
	}

	// Check for cancellation before destination directory lookup
	select {
	case <-ctx.Context.Done():
		logger.Debug("RENAME cancelled before destination lookup: from='%s' to='%s' client=%s error=%v",
			req.FromName, req.ToName, clientIP, ctx.Context.Err())

		fromDirID := xdr.ExtractFileID(fromDirHandle)
		fromDirWccAfter := xdr.MetadataToNFS(fromDirAttr, fromDirID)

		return &RenameResponse{
			Status:           types.NFS3ErrIO,
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
		}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 3: Verify destination directory exists and is valid
	// ========================================================================

	toDirHandle := metadata.FileHandle(req.ToDirHandle)
	toDirAttr, err := repository.GetFile(ctx.Context, toDirHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("RENAME cancelled during destination directory lookup: from='%s' to='%s' client=%s error=%v",
				req.FromName, req.ToName, clientIP, ctx.Context.Err())

			// Return WCC for source directory
			fromDirID := xdr.ExtractFileID(fromDirHandle)
			fromDirWccAfter := xdr.MetadataToNFS(fromDirAttr, fromDirID)

			return &RenameResponse{
				Status:           types.NFS3ErrIO,
				FromDirWccBefore: fromDirWccBefore,
				FromDirWccAfter:  fromDirWccAfter,
			}, ctx.Context.Err()
		}

		logger.Warn("RENAME failed: destination directory not found: dir=%x client=%s error=%v",
			req.ToDirHandle, clientIP, err)

		// Return WCC for source directory
		fromDirID := xdr.ExtractFileID(fromDirHandle)
		fromDirWccAfter := xdr.MetadataToNFS(fromDirAttr, fromDirID)

		return &RenameResponse{
			Status:           types.NFS3ErrNoEnt,
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
		}, nil
	}

	// Capture pre-operation attributes for destination directory
	toDirWccBefore := xdr.CaptureWccAttr(toDirAttr)

	// Verify destination is a directory
	if toDirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("RENAME failed: destination handle not a directory: dir=%x type=%d client=%s",
			req.ToDirHandle, toDirAttr.Type, clientIP)

		fromDirID := xdr.ExtractFileID(fromDirHandle)
		fromDirWccAfter := xdr.MetadataToNFS(fromDirAttr, fromDirID)

		toDirID := xdr.ExtractFileID(toDirHandle)
		toDirWccAfter := xdr.MetadataToNFS(toDirAttr, toDirID)

		return &RenameResponse{
			Status:           types.NFS3ErrNotDir,
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, nil
	}

	// Check for cancellation before the atomic rename operation
	// This is the most critical check - we don't want to start the rename
	// if the client has already disconnected
	select {
	case <-ctx.Context.Done():
		logger.Debug("RENAME cancelled before rename operation: from='%s' to='%s' client=%s error=%v",
			req.FromName, req.ToName, clientIP, ctx.Context.Err())

		fromDirID := xdr.ExtractFileID(fromDirHandle)
		fromDirWccAfter := xdr.MetadataToNFS(fromDirAttr, fromDirID)

		toDirID := xdr.ExtractFileID(toDirHandle)
		toDirWccAfter := xdr.MetadataToNFS(toDirAttr, toDirID)

		return &RenameResponse{
			Status:           types.NFS3ErrIO,
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 4: Perform rename via repository
	// ========================================================================
	// The repository is responsible for:
	// - Verifying source file exists
	// - Checking write permissions on both directories
	// - Handling atomic replacement of destination if it exists
	// - Ensuring destination is not a non-empty directory
	// - Updating parent relationships
	// - Updating directory timestamps
	// - Ensuring atomicity or proper rollback
	//
	// We don't check for cancellation inside RenameFile to maintain atomicity.
	// The repository should respect context internally for its operations.

	// Build authentication context for repository
	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	err = repository.RenameFile(authCtx, fromDirHandle, req.FromName, toDirHandle, req.ToName)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("RENAME cancelled during rename operation: from='%s' to='%s' client=%s error=%v",
				req.FromName, req.ToName, clientIP, ctx.Context.Err())

			// Get updated directory attributes for WCC data (best effort)
			var fromDirWccAfter *types.NFSFileAttr
			if updatedFromDirAttr, getErr := repository.GetFile(ctx.Context, fromDirHandle); getErr == nil {
				fromDirID := xdr.ExtractFileID(fromDirHandle)
				fromDirWccAfter = xdr.MetadataToNFS(updatedFromDirAttr, fromDirID)
			}

			var toDirWccAfter *types.NFSFileAttr
			if updatedToDirAttr, getErr := repository.GetFile(ctx.Context, toDirHandle); getErr == nil {
				toDirID := xdr.ExtractFileID(toDirHandle)
				toDirWccAfter = xdr.MetadataToNFS(updatedToDirAttr, toDirID)
			}

			return &RenameResponse{
				Status:           types.NFS3ErrIO,
				FromDirWccBefore: fromDirWccBefore,
				FromDirWccAfter:  fromDirWccAfter,
				ToDirWccBefore:   toDirWccBefore,
				ToDirWccAfter:    toDirWccAfter,
			}, ctx.Context.Err()
		}

		logger.Error("RENAME failed: repository error: from='%s' to='%s' client=%s error=%v",
			req.FromName, req.ToName, clientIP, err)

		// Get updated directory attributes for WCC data
		var fromDirWccAfter *types.NFSFileAttr
		if updatedFromDirAttr, getErr := repository.GetFile(ctx.Context, fromDirHandle); getErr == nil {
			fromDirID := xdr.ExtractFileID(fromDirHandle)
			fromDirWccAfter = xdr.MetadataToNFS(updatedFromDirAttr, fromDirID)
		}

		var toDirWccAfter *types.NFSFileAttr
		if updatedToDirAttr, getErr := repository.GetFile(ctx.Context, toDirHandle); getErr == nil {
			toDirID := xdr.ExtractFileID(toDirHandle)
			toDirWccAfter = xdr.MetadataToNFS(updatedToDirAttr, toDirID)
		}

		// Map repository errors to NFS status codes
		status := xdr.MapRepositoryErrorToNFSStatus(err, clientIP, "rename")

		return &RenameResponse{
			Status:           status,
			FromDirWccBefore: fromDirWccBefore,
			FromDirWccAfter:  fromDirWccAfter,
			ToDirWccBefore:   toDirWccBefore,
			ToDirWccAfter:    toDirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 5: Build success response with updated WCC data
	// ========================================================================

	// Get updated source directory attributes
	var fromDirWccAfter *types.NFSFileAttr
	if updatedFromDirAttr, getErr := repository.GetFile(ctx.Context, fromDirHandle); getErr != nil {
		logger.Warn("RENAME: successful but cannot get updated source directory attributes: dir=%x error=%v",
			req.FromDirHandle, getErr)
		// fromDirWccAfter will be nil
	} else {
		fromDirID := xdr.ExtractFileID(fromDirHandle)
		fromDirWccAfter = xdr.MetadataToNFS(updatedFromDirAttr, fromDirID)
	}

	// Get updated destination directory attributes
	var toDirWccAfter *types.NFSFileAttr
	if updatedToDirAttr, getErr := repository.GetFile(ctx.Context, toDirHandle); getErr != nil {
		logger.Warn("RENAME: successful but cannot get updated destination directory attributes: dir=%x error=%v",
			req.ToDirHandle, getErr)
		// toDirWccAfter will be nil
	} else {
		toDirID := xdr.ExtractFileID(toDirHandle)
		toDirWccAfter = xdr.MetadataToNFS(updatedToDirAttr, toDirID)
	}

	logger.Info("RENAME successful: from='%s' to='%s' client=%s",
		req.FromName, req.ToName, clientIP)

	// Extract IDs for debug logging
	fromDirID := xdr.ExtractFileID(fromDirHandle)
	toDirID := xdr.ExtractFileID(toDirHandle)
	logger.Debug("RENAME details: from_dir=%d to_dir=%d same_dir=%v",
		fromDirID, toDirID, bytes.Equal(req.FromDirHandle, req.ToDirHandle))

	return &RenameResponse{
		Status:           types.NFS3OK,
		FromDirWccBefore: fromDirWccBefore,
		FromDirWccAfter:  fromDirWccAfter,
		ToDirWccBefore:   toDirWccBefore,
		ToDirWccAfter:    toDirWccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// renameValidationError represents a RENAME request validation error.
type renameValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *renameValidationError) Error() string {
	return e.message
}

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
//   - *renameValidationError with NFS status if invalid
func validateRenameRequest(req *RenameRequest) *renameValidationError {
	// Validate source directory handle
	if len(req.FromDirHandle) == 0 {
		return &renameValidationError{
			message:   "empty source directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.FromDirHandle) > 64 {
		return &renameValidationError{
			message:   fmt.Sprintf("source directory handle too long: %d bytes (max 64)", len(req.FromDirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate destination directory handle
	if len(req.ToDirHandle) == 0 {
		return &renameValidationError{
			message:   "empty destination directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.ToDirHandle) > 64 {
		return &renameValidationError{
			message:   fmt.Sprintf("destination directory handle too long: %d bytes (max 64)", len(req.ToDirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate source name
	if req.FromName == "" {
		return &renameValidationError{
			message:   "empty source name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.FromName) > 255 {
		return &renameValidationError{
			message:   fmt.Sprintf("source name too long: %d bytes (max 255)", len(req.FromName)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for reserved names
	if req.FromName == "." || req.FromName == ".." {
		return &renameValidationError{
			message:   fmt.Sprintf("cannot rename '%s'", req.FromName),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for invalid characters in source name
	if strings.ContainsAny(req.FromName, "/\x00") {
		return &renameValidationError{
			message:   "source name contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate destination name
	if req.ToName == "" {
		return &renameValidationError{
			message:   "empty destination name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.ToName) > 255 {
		return &renameValidationError{
			message:   fmt.Sprintf("destination name too long: %d bytes (max 255)", len(req.ToName)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for reserved names
	if req.ToName == "." || req.ToName == ".." {
		return &renameValidationError{
			message:   fmt.Sprintf("cannot rename to '%s'", req.ToName),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for invalid characters in destination name
	if strings.ContainsAny(req.ToName, "/\x00") {
		return &renameValidationError{
			message:   "destination name contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeRenameRequest decodes a RENAME request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.14 specifications:
//  1. Source directory handle length (4 bytes, big-endian uint32)
//  2. Source directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Source name length (4 bytes, big-endian uint32)
//  5. Source name data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//  7. Destination directory handle length (4 bytes, big-endian uint32)
//  8. Destination directory handle data (variable length, up to 64 bytes)
//  9. Padding to 4-byte boundary (0-3 bytes)
//  10. Destination name length (4 bytes, big-endian uint32)
//  11. Destination name data (variable length, up to 255 bytes)
//  12. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the RENAME request
//
// Returns:
//   - *RenameRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded RENAME request from network
//	req, err := DecodeRenameRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req in RENAME procedure
func DecodeRenameRequest(data []byte) (*RenameRequest, error) {
	// Validate minimum data length
	if len(data) < 16 {
		return nil, fmt.Errorf("data too short: need at least 16 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode source directory handle
	// ========================================================================

	fromDirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode source directory handle: %w", err)
	}

	// ========================================================================
	// Decode source name
	// ========================================================================

	fromName, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode source name: %w", err)
	}

	// ========================================================================
	// Decode destination directory handle
	// ========================================================================

	toDirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode destination directory handle: %w", err)
	}

	// ========================================================================
	// Decode destination name
	// ========================================================================

	toName, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode destination name: %w", err)
	}

	logger.Debug("Decoded RENAME request: from='%s' in dir_len=%d to='%s' in dir_len=%d",
		fromName, len(fromDirHandle), toName, len(toDirHandle))

	return &RenameRequest{
		FromDirHandle: fromDirHandle,
		FromName:      fromName,
		ToDirHandle:   toDirHandle,
		ToName:        toName,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the RenameResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.14 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Source directory WCC data (pre-op and post-op attributes)
//  3. Destination directory WCC data (pre-op and post-op attributes)
//
// WCC data is included for both success and failure cases to help
// clients maintain cache consistency for both directories.
//
// XDR encoding requires all data to be in big-endian format and aligned
// to 4-byte boundaries.
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
//
// Example:
//
//	resp := &RenameResponse{
//	    Status:           types.NFS3OK,
//	    FromDirWccBefore: fromWccBefore,
//	    FromDirWccAfter:  fromWccAfter,
//	    ToDirWccBefore:   toWccBefore,
//	    ToDirWccAfter:    toWccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *RenameResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write WCC data for source directory (both success and failure)
	// ========================================================================

	if err := xdr.EncodeWccData(&buf, resp.FromDirWccBefore, resp.FromDirWccAfter); err != nil {
		return nil, fmt.Errorf("encode source directory wcc data: %w", err)
	}

	// ========================================================================
	// Write WCC data for destination directory (both success and failure)
	// ========================================================================

	if err := xdr.EncodeWccData(&buf, resp.ToDirWccBefore, resp.ToDirWccAfter); err != nil {
		return nil, fmt.Errorf("encode destination directory wcc data: %w", err)
	}

	logger.Debug("Encoded RENAME response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

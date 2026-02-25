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

// MkdirRequest represents a MKDIR request from an NFS client.
// The client provides the parent directory handle, the name for the new directory,
// and optional attributes to set on the newly created directory.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.9 specifies the MKDIR procedure as:
//
//	MKDIR3res NFSPROC3_MKDIR(MKDIR3args) = 9;
//
// The MKDIR procedure creates a new subdirectory within an existing directory.
// It is one of the fundamental operations for building directory hierarchies.
type MkdirRequest struct {
	// DirHandle is the file handle of the parent directory where the new directory
	// will be created. Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name of the directory to create within the parent directory.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	//   - Should not contain control characters
	Name string

	// Attr contains the attributes to set on the new directory.
	// Only certain fields are meaningful for MKDIR:
	//   - Mode: Directory permissions (e.g., 0755)
	//   - UID: Owner user ID
	//   - GID: Owner group ID
	// Other fields (size, times) are ignored and set by the server.
	// If not specified, the server applies defaults (typically 0755, uid=0, gid=0).
	Attr *metadata.SetAttrs
}

// MkdirResponse represents the response to a MKDIR request.
// On success, it returns the new directory's file handle and attributes,
// plus WCC (Weak Cache Consistency) data for the parent directory.
//
// The response is encoded in XDR format before being sent back to the client.
//
// The WCC data helps clients maintain cache coherency by providing
// before-and-after snapshots of the parent directory.
type MkdirResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Handle is the file handle of the newly created directory.
	// Only present when Status == types.NFS3OK.
	// The handle can be used in subsequent NFS operations to access the directory.
	Handle []byte

	// Attr contains the attributes of the newly created directory.
	// Only present when Status == types.NFS3OK.
	// Includes mode, ownership, timestamps, etc.
	Attr *types.NFSFileAttr

	// WccBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect if the parent
	// directory changed during the operation. May be nil.
	WccBefore *types.WccAttr

	// WccAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide the updated parent state.
	// May be nil on error, but should be present on success.
	WccAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Mkdir handles NFS MKDIR (RFC 1813 Section 3.3.9).
// Creates a new subdirectory within a parent directory with specified attributes.
// Delegates to MetadataService.CreateDirectory after permission and existence checks.
// Creates directory metadata and parent entry; returns new handle and parent WCC data.
// Errors: NFS3ErrExist, NFS3ErrNotDir, NFS3ErrAcces, NFS3ErrIO.
func (h *Handler) Mkdir(
	ctx *NFSHandlerContext,
	req *MkdirRequest,
) (*MkdirResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "MKDIR cancelled before processing", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &MkdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	var mode uint32 = 0755 // Default
	if req.Attr != nil && req.Attr.Mode != nil {
		mode = *req.Attr.Mode
	}

	logger.InfoCtx(ctx.Context, "MKDIR", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "mode", fmt.Sprintf("%o", mode), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateMkdirRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "MKDIR validation failed", "name", req.Name, "client", clientIP, "error", err)
		return &MkdirResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store from context
	// ========================================================================

	metaSvc, err := getMetadataService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "MKDIR failed: metadata service not initialized", "client", clientIP, "error", err)
		return &MkdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	parentHandle := metadata.FileHandle(req.DirHandle)
	logger.DebugCtx(ctx.Context, "MKDIR", "share", ctx.Share, "name", req.Name)

	// ========================================================================
	// Step 3: Verify parent directory exists and is valid
	// ========================================================================

	parentFile, status, err := h.getFileOrError(ctx, parentHandle, "MKDIR", req.DirHandle)
	if parentFile == nil {
		return &MkdirResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(&parentFile.FileAttr)

	// ========================================================================
	// Step 3: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, wccAfter, err := h.buildAuthContextWithWCCError(ctx, parentHandle, &parentFile.FileAttr, "MKDIR", req.Name, req.DirHandle)
	if authCtx == nil {
		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, err
	}

	// Verify parent is actually a directory
	if parentFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "MKDIR failed: parent not a directory", "handle", fmt.Sprintf("%x", req.DirHandle), "type", parentFile.Type, "client", clientIP)

		// Get current parent state for WCC
		wccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, nil
	}

	// Check for cancellation before the existence check
	// Lookup may involve directory scanning which can be expensive
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "MKDIR cancelled before existence check", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

		wccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 4: Check if directory name already exists using Lookup
	// ========================================================================

	_, err = metaSvc.Lookup(authCtx, parentHandle, req.Name)
	if err != nil && ctx.Context.Err() != nil {
		// Context was cancelled during Lookup
		logger.DebugCtx(ctx.Context, "MKDIR cancelled during existence check", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

		// Get updated parent attributes for WCC data
		updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, ctx.Context.Err()
	}

	if err == nil {
		// Child exists (no error from Lookup)
		logger.DebugCtx(ctx.Context, "MKDIR failed: directory already exists", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP)

		// Get updated parent attributes for WCC data
		updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrExist},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, nil
	}
	// If error from Lookup, directory doesn't exist (good) - continue

	// Check for cancellation before the create operation
	// This is the most critical check - we don't want to start creating
	// the directory if the client has already disconnected
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "MKDIR cancelled before create operation", "name", req.Name, "handle", fmt.Sprintf("%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())

		// Get updated parent attributes for WCC data
		updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 5: Create directory via store.Create()
	// ========================================================================
	// The store.Create() method handles both regular files and directories
	// based on the Type field in FileAttr.
	//
	// The store is responsible for:
	// - Checking write permission on parent directory
	// - Creating the directory metadata
	// - Linking it to the parent
	// - Updating parent directory timestamps
	// - Setting default attributes (size, nlink, timestamps)

	// Build directory attributes
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755, // Default: rwxr-xr-x
		UID:  0,
		GID:  0,
	}

	// Apply context defaults (authenticated user's UID/GID)
	if authCtx.Identity.UID != nil {
		dirAttr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		dirAttr.GID = *authCtx.Identity.GID
	}

	// Apply explicit attributes from request
	if req.Attr != nil {
		if req.Attr.Mode != nil {
			dirAttr.Mode = *req.Attr.Mode
		}
		if req.Attr.UID != nil {
			dirAttr.UID = *req.Attr.UID
		}
		if req.Attr.GID != nil {
			dirAttr.GID = *req.Attr.GID
		}
	}

	// Call store.Create() with Type = FileTypeDirectory
	// The store will complete the attributes with timestamps, size, etc.
	newDirFile, err := metaSvc.CreateDirectory(authCtx, parentHandle, req.Name, dirAttr)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "MKDIR cancelled during create operation", "name", req.Name, "client", clientIP, "error", ctx.Context.Err())

			// Get updated parent attributes for WCC data
			updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
			wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

			return &MkdirResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				WccBefore:       wccBefore,
				WccAfter:        wccAfter,
			}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "MKDIR failed: store error", "name", req.Name, "client", clientIP)

		// Get updated parent attributes for WCC data
		updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
		wccAfter := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

		// Map store errors to NFS status codes
		status := mapMetadataErrorToNFS(err)

		return &MkdirResponse{
			NFSResponseBase: NFSResponseBase{Status: status},
			WccBefore:       wccBefore,
			WccAfter:        wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with new directory attributes
	// ========================================================================

	// Encode the file handle for the new directory
	newHandle, err := metadata.EncodeFileHandle(newDirFile)
	if err != nil {
		traceError(ctx.Context, err, "MKDIR: failed to encode directory handle")
		return &MkdirResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Generate file ID from handle for NFS attributes
	nfsAttr := h.convertFileAttrToNFS(newHandle, &newDirFile.FileAttr)

	// Get updated parent attributes for WCC data
	updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
	wccAfter = h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

	logger.InfoCtx(ctx.Context, "MKDIR successful", "name", req.Name, "handle", fmt.Sprintf("%x", newHandle), "mode", fmt.Sprintf("%o", newDirFile.Mode), "size", newDirFile.Size, "client", clientIP)

	logger.DebugCtx(ctx.Context, "MKDIR details", "handle", fmt.Sprintf("%x", newHandle), "uid", newDirFile.UID, "gid", newDirFile.GID, "parent_handle", fmt.Sprintf("%x", parentHandle))

	return &MkdirResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Handle:          newHandle,
		Attr:            nfsAttr,
		WccBefore:       wccBefore,
		WccAfter:        wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// validateMkdirRequest validates MKDIR request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Directory name is valid (not empty, not "." or "..", length, characters)
//
// Returns:
//   - nil if valid
//   - *validationError with NFS status if invalid
func validateMkdirRequest(req *MkdirRequest) *validationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &validationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &validationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &validationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate directory name
	if req.Name == "" {
		return &validationError{
			message:   "empty directory name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &validationError{
			message:   fmt.Sprintf("directory name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &validationError{
			message:   fmt.Sprintf("directory name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.ContainsAny([]byte(req.Name), "\x00") {
		return &validationError{
			message:   "directory name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if bytes.ContainsAny([]byte(req.Name), "/") {
		return &validationError{
			message:   "directory name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	// This prevents potential issues with terminal output and logs
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &validationError{
				message:   fmt.Sprintf("directory name contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

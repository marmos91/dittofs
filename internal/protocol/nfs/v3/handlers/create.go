package handlers

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/content"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// CreateRequest represents an NFS CREATE request (RFC 1813 Section 3.3.8).
//
// The CREATE procedure creates a new regular file in a specified directory.
// It supports three creation modes:
//   - UNCHECKED: Create file or truncate if exists
//   - GUARDED: Create only if file doesn't exist
//   - EXCLUSIVE: Create with verifier for idempotent retry
//
// RFC 1813 Section 3.3.8 specifies the CREATE procedure as:
//
//	CREATE3res NFSPROC3_CREATE(CREATE3args) = 8;
type CreateRequest struct {
	// DirHandle is the file handle of the parent directory where the file will be created.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	DirHandle []byte

	// Filename is the name of the file to create within the parent directory.
	// Maximum length is 255 bytes per NFS specification.
	Filename string

	// Mode specifies the creation mode.
	// Valid values:
	//   - CreateUnchecked (0): Create or truncate existing file
	//   - CreateGuarded (1): Fail if file exists
	//   - CreateExclusive (2): Use verifier for idempotent creation
	Mode uint32

	// Attr contains optional attributes to set on the new file.
	// Only mode, uid, gid are meaningful for CREATE.
	Attr *metadata.SetAttrs

	// Verf is the creation verifier for EXCLUSIVE mode (8 bytes).
	// Only used when Mode == CreateExclusive.
	Verf uint64
}

// CreateResponse represents an NFS CREATE response (RFC 1813 Section 3.3.8).
type CreateResponse struct {
	NFSResponseBase // Embeds Status field and GetStatus() method

	// FileHandle is the handle of the newly created file.
	// Only present when Status == types.NFS3OK.
	FileHandle []byte

	// Attr contains post-operation attributes of the created file.
	// Only present when Status == types.NFS3OK.
	Attr *types.NFSFileAttr

	// DirBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency.
	DirBefore *types.WccAttr

	// DirAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency.
	DirAfter *types.NFSFileAttr
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Create handles the CREATE procedure, which creates a new regular file.
//
// This implements the NFS CREATE procedure as defined in RFC 1813 Section 3.3.8.
//
// **Creation Modes:**
//
//  1. UNCHECKED (0): Create or truncate existing file
//  2. GUARDED (1): Create only if doesn't exist (fail with NFS3ErrExist)
//  3. EXCLUSIVE (2): Create with verifier for idempotent retry
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (filename, mode, handle)
//  3. Build AuthContext for permission checking
//  4. Verify parent directory exists and is a directory
//  5. Capture pre-operation directory state (for WCC)
//  6. Check for cancellation before existence check
//  7. Check if file already exists using Lookup
//  8. Check for cancellation before create/truncate operations
//  9. Based on mode: create new file or truncate existing
//  10. Return file handle and attributes
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after parent lookup, before existence check
//   - Checks before actual create/truncate operations
//   - Repository operations respect context internally
//   - Returns NFS3ErrIO status with context error for cancellation
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer uses these for:
//   - Building AuthContext for permission checking
//   - Setting default file ownership (UID/GID)
//   - Logging and audit trails
//
// Access control enforcement is implemented by the repository layer.
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Access denied → NFS3ErrAcces
//   - Not found → types.NFS3ErrNoEnt
//   - Already exists → NFS3ErrExist
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Parameters:**
//   - ctx: Handler context with cancellation, client address and authentication credentials
//   - req: Create request with parent handle, filename, mode, attributes
//
// **Returns:**
//   - *CreateResponse: Response with status and file handle (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures
//
// **RFC 1813 Section 3.3.8: CREATE Procedure**
func (h *Handler) Create(
	ctx *NFSHandlerContext,
	req *CreateRequest,
) (*CreateResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "CREATE cancelled before processing", "file", req.Filename, "dir", fmt.Sprintf("0x%x", req.DirHandle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return &CreateResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.InfoCtx(ctx.Context, "CREATE", "file", req.Filename, "dir", fmt.Sprintf("0x%x", req.DirHandle), "mode", createModeName(req.Mode), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateCreateRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "CREATE validation failed", "file", req.Filename, "client", clientIP, "error", err)
		return &CreateResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Get content service from registry
	// ========================================================================

	// Get content service for this share
	contentSvc := h.Registry.GetContentService()

	parentHandle := metadata.FileHandle(req.DirHandle)
	logger.DebugCtx(ctx.Context, "CREATE", "share", ctx.Share, "file", req.Filename)

	// ========================================================================
	// Step 3: Verify parent directory exists and is valid
	// ========================================================================

	parentFile, status, err := h.getFileOrError(ctx, parentHandle, "CREATE", req.DirHandle)
	if parentFile == nil {
		return &CreateResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Capture pre-operation directory state for WCC
	dirWccBefore := xdr.CaptureWccAttr(&parentFile.FileAttr)

	// Verify parent is a directory
	if parentFile.Type != metadata.FileTypeDirectory {
		logger.WarnCtx(ctx.Context, "CREATE failed: parent not a directory", "file", req.Filename, "dir", fmt.Sprintf("0x%x", req.DirHandle), "type", parentFile.Type, "client", clientIP)

		// Get current parent state for WCC
		dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &CreateResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNotDir},
			DirBefore:       dirWccBefore,
			DirAfter:        dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 3: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, dirWccAfter, err := h.buildAuthContextWithWCCError(ctx, parentHandle, &parentFile.FileAttr, "CREATE", req.Filename, req.DirHandle)
	if authCtx == nil {
		return &CreateResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			DirBefore:       dirWccBefore,
			DirAfter:        dirWccAfter,
		}, err
	}

	// Check for cancellation before the existence check
	// This is important because Lookup may involve directory scanning
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "CREATE cancelled before existence check", "file", req.Filename, "dir", fmt.Sprintf("0x%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())
		return &CreateResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 4: Check if file already exists using Lookup
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	existingFile, err := metaSvc.Lookup(authCtx, parentHandle, req.Filename)
	if err != nil && ctx.Context.Err() != nil {
		// Context was cancelled during Lookup
		logger.DebugCtx(ctx.Context, "CREATE cancelled during existence check", "file", req.Filename, "dir", fmt.Sprintf("0x%x", req.DirHandle), "client", clientIP, "error", ctx.Context.Err())
		return &CreateResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// Determine if file exists (no error from Lookup means it exists)
	fileExists := (err == nil)

	// Check for cancellation before the potentially expensive create/truncate operations
	// This is critical because these operations modify state
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "CREATE cancelled before file operation", "file", req.Filename, "dir", fmt.Sprintf("0x%x", req.DirHandle), "exists", fileExists, "client", clientIP, "error", ctx.Context.Err())
		return &CreateResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, ctx.Context.Err()
	}

	// ========================================================================
	// Step 5: Handle creation based on mode
	// ========================================================================

	var fileHandle metadata.FileHandle
	var fileAttr *metadata.FileAttr

	switch req.Mode {
	case types.CreateGuarded:
		// GUARDED: Fail if file exists
		if fileExists {
			logger.DebugCtx(ctx.Context, "CREATE failed: file exists (guarded)", "file", req.Filename, "client", clientIP)

			// Get current parent state for WCC
			dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

			return &CreateResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrExist},
				DirBefore:       dirWccBefore,
				DirAfter:        dirWccAfter,
			}, nil
		}

		// Create new file
		fileHandle, fileAttr, err = createNewFile(authCtx, metaSvc, parentHandle, req)

	case types.CreateExclusive:
		// EXCLUSIVE: Check idempotency token if file exists
		// RFC 1813 Section 3.3.8: If the file exists, compare the stored token
		// with the client's token. If they match, this is a retry - return success.
		if fileExists {
			// Check if idempotency token matches - this indicates a retry
			if existingFile.IdempotencyToken == req.Verf && req.Verf != 0 {
				// Token matches - this is a retry of a successful create
				logger.InfoCtx(ctx.Context, "CREATE EXCLUSIVE retry detected",
					"file", req.Filename, "token", fmt.Sprintf("0x%016x", req.Verf), "client", clientIP)

				existingHandle, err := metadata.EncodeFileHandle(existingFile)
				if err != nil {
					logger.ErrorCtx(ctx.Context, "failed to encode file handle for CREATE EXCLUSIVE retry",
						"file", req.Filename, "client", clientIP, "error", err)

					dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)
					return &CreateResponse{
						NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
						DirBefore:       dirWccBefore,
						DirAfter:        dirWccAfter,
					}, nil
				}
				dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)
				nfsFileAttr := h.convertFileAttrToNFS(existingHandle, &existingFile.FileAttr)

				return &CreateResponse{
					NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
					FileHandle:      existingHandle,
					Attr:            nfsFileAttr,
					DirBefore:       dirWccBefore,
					DirAfter:        dirWccAfter,
				}, nil
			}

			// Different token - genuine conflict (different client or different request)
			logger.DebugCtx(ctx.Context, "CREATE EXCLUSIVE failed: file exists with different token",
				"file", req.Filename, "stored", fmt.Sprintf("0x%016x", existingFile.IdempotencyToken),
				"requested", fmt.Sprintf("0x%016x", req.Verf), "client", clientIP)

			dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

			return &CreateResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrExist},
				DirBefore:       dirWccBefore,
				DirAfter:        dirWccAfter,
			}, nil
		}

		// Create new file with idempotency token
		fileHandle, fileAttr, err = createNewFile(authCtx, metaSvc, parentHandle, req)

	case types.CreateUnchecked:
		// UNCHECKED: Create or truncate existing
		if fileExists {
			// Truncate existing file
			existingHandle, _ := metadata.EncodeFileHandle(existingFile)
			fileHandle = existingHandle
			fileAttr, err = truncateExistingFile(authCtx, contentSvc, ctx.Share, metaSvc, existingFile, req)
		} else {
			// Create new file
			fileHandle, fileAttr, err = createNewFile(authCtx, metaSvc, parentHandle, req)
		}

	default:
		logger.WarnCtx(ctx.Context, "CREATE failed: invalid mode", "file", req.Filename, "mode", req.Mode, "client", clientIP)

		dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &CreateResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrInval},
			DirBefore:       dirWccBefore,
			DirAfter:        dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Handle errors from file creation/truncation
	// ========================================================================

	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, "CREATE cancelled during file operation", "file", req.Filename, "client", clientIP, "error", ctx.Context.Err())

			dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

			return &CreateResponse{
				NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
				DirBefore:       dirWccBefore,
				DirAfter:        dirWccAfter,
			}, ctx.Context.Err()
		}

		traceError(ctx.Context, err, "CREATE failed: repository error", "file", req.Filename, "client", clientIP)

		// Map repository errors to NFS status codes
		nfsStatus := mapMetadataErrorToNFS(err)

		dirWccAfter := h.convertFileAttrToNFS(parentHandle, &parentFile.FileAttr)

		return &CreateResponse{
			NFSResponseBase: NFSResponseBase{Status: nfsStatus},
			DirBefore:       dirWccBefore,
			DirAfter:        dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 7: Build success response
	// ========================================================================

	// Convert metadata to NFS attributes
	nfsFileAttr := h.convertFileAttrToNFS(fileHandle, fileAttr)

	// Get updated parent directory attributes
	updatedParentFile, _ := metaSvc.GetFile(ctx.Context, parentHandle)
	nfsDirAttr := h.convertFileAttrToNFS(parentHandle, &updatedParentFile.FileAttr)

	logger.InfoCtx(ctx.Context, "CREATE successful", "file", req.Filename, "handle", fmt.Sprintf("0x%x", fileHandle), "mode", fmt.Sprintf("0%o", fileAttr.Mode), "size", fileAttr.Size, "client", clientIP)

	return &CreateResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		FileHandle:      fileHandle,
		Attr:            nfsFileAttr,
		DirBefore:       dirWccBefore,
		DirAfter:        nfsDirAttr,
	}, nil
}

// ============================================================================
// Helper Functions for File Operations
// ============================================================================

// createNewFile creates a new file using the metadata store's Create method.
//
// This function:
//  1. Builds file attributes with defaults from context
//  2. Calls store.Create() which atomically:
//     - Creates file metadata
//     - Links file to parent directory
//     - Updates parent timestamps
//     - Performs permission checking
//
// The new metadata interface handles all of this atomically, including rollback
// on failure, so we don't need manual cleanup.
//
// Parameters:
//   - authCtx: Authentication context for permission checking
//   - metaSvc: Metadata service for file operations
//   - parentHandle: Parent directory handle
//   - req: Create request with filename and attributes
//
// Returns:
//   - File handle, file attributes, and error
func createNewFile(
	authCtx *metadata.AuthContext,
	metaSvc *metadata.MetadataService,
	parentHandle metadata.FileHandle,
	req *CreateRequest,
) (metadata.FileHandle, *metadata.FileAttr, error) {
	// Build file attributes for the new file
	// The repository will complete these with timestamps and ContentID
	fileAttr := &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644, // Default: rw-r--r--
		UID:  0,
		GID:  0,
		Size: 0,
	}

	// Apply context defaults (authenticated user's UID/GID)
	if authCtx.Identity.UID != nil {
		fileAttr.UID = *authCtx.Identity.UID
	}
	if authCtx.Identity.GID != nil {
		fileAttr.GID = *authCtx.Identity.GID
	}

	// Store idempotency token for EXCLUSIVE mode
	// This enables clients to safely retry file creation after network failures.
	//
	// DESIGN NOTE: Intentional deviation from Linux kernel implementation
	// ====================================================================
	// Linux's fs/nfsd stores the verifier in atime/mtime fields:
	//   attrs->ia_atime.tv_sec = verf[0];
	//   attrs->ia_mtime.tv_sec = verf[1];
	//
	// We use a dedicated IdempotencyToken field instead because:
	//
	// 1. Cleaner semantics: The verifier is stored explicitly rather than
	//    overloading timestamp fields with non-timestamp data.
	//
	// 2. Better timestamp preservation: RFC 1813 allows clients to set
	//    atime/mtime in CREATE EXCLUSIVE mode. Our approach allows this
	//    without conflict.
	//
	// 3. Easier debugging: The verifier is visible in metadata stores as
	//    a separate field rather than being hidden in timestamps.
	//
	// 4. No ext2 compatibility hacks: Linux clears high bits for ext2
	//    compatibility; we don't need this complexity.
	//
	// Trade-off: Tools expecting Linux-style verifier storage in timestamps
	// won't find it there. This is acceptable since:
	//   - The verifier is only used for idempotency during CREATE retries
	//   - NFS clients interact via protocol, not by inspecting timestamps
	//   - Recovery tools should use our IdempotencyToken field instead
	//
	// Per RFC 1813, the verifier comparison is opaque - how it's stored
	// is implementation-defined as long as the same verifier can be
	// retrieved on CREATE retry to detect duplicate requests.
	if req.Mode == types.CreateExclusive && req.Verf != 0 {
		fileAttr.IdempotencyToken = req.Verf
	}

	// Apply explicit attributes from request
	if req.Attr != nil {
		applySetAttrsToFileAttr(fileAttr, req.Attr)
	}

	// Call metaSvc's atomic Create operation
	// This handles file creation, parent linking, and permission checking
	createdFile, err := metaSvc.CreateFile(authCtx, parentHandle, req.Filename, fileAttr)
	if err != nil {
		return nil, nil, fmt.Errorf("create file: %w", err)
	}

	// Debug logging to trace file creation
	logger.Debug("NFS CREATE file created",
		"filename", req.Filename,
		"fileType", int(createdFile.Type),
		"fileID", createdFile.ID.String(),
		"filePath", createdFile.Path,
		"inputType", int(fileAttr.Type))

	// Encode the file handle for return
	fileHandle, err := metadata.EncodeFileHandle(createdFile)
	if err != nil {
		return nil, nil, fmt.Errorf("encode file handle: %w", err)
	}

	return fileHandle, &createdFile.FileAttr, nil
}

// truncateExistingFile truncates an existing file and updates attributes.
//
// For UNCHECKED mode when file exists, this:
//  1. Determines target size (from Attr.Size or 0)
//  2. Updates file metadata using SetFileAttributes
//  3. Truncates content (if content store is available)
//
// Parameters:
//   - authCtx: Authentication context for permission checking
//   - contentSvc: Content service for truncation
//   - shareName: Share name for content service routing
//   - metaSvc: Metadata service for file operations
//   - existingFile: Existing file to truncate
//   - req: Create request with attributes
//
// Returns:
//   - Updated file attributes and error
func truncateExistingFile(
	authCtx *metadata.AuthContext,
	contentSvc *content.ContentService,
	shareName string,
	metaSvc *metadata.MetadataService,
	existingFile *metadata.File,
	req *CreateRequest,
) (*metadata.FileAttr, error) {
	fileHandle, _ := metadata.EncodeFileHandle(existingFile)
	// Build SetAttrs for the update
	setAttrs := &metadata.SetAttrs{}

	// Determine target size
	targetSize := uint64(0) // Default: truncate to empty
	if req.Attr != nil && req.Attr.Size != nil {
		targetSize = *req.Attr.Size
	}
	setAttrs.Size = &targetSize

	// Apply other requested attributes from request
	if req.Attr != nil {
		if req.Attr.Mode != nil {
			setAttrs.Mode = req.Attr.Mode
		}
		if req.Attr.UID != nil {
			setAttrs.UID = req.Attr.UID
		}
		if req.Attr.GID != nil {
			setAttrs.GID = req.Attr.GID
		}
		if req.Attr.Atime != nil {
			setAttrs.Atime = req.Attr.Atime
		}
		if req.Attr.Mtime != nil {
			setAttrs.Mtime = req.Attr.Mtime
		}
	}

	// Update file metadata using metaSvc
	// This includes permission checking
	if err := metaSvc.SetFileAttributes(authCtx, fileHandle, setAttrs); err != nil {
		return nil, fmt.Errorf("update file metadata: %w", err)
	}

	// Truncate content if file has content
	if existingFile.ContentID != "" {
		if err := contentSvc.Truncate(authCtx.Context, shareName, existingFile.ContentID, targetSize); err != nil {
			logger.Warn("Failed to truncate content", "size", targetSize, "error", err)
			// Non-fatal: metadata is already updated
		}
	}

	// Get updated attributes
	updatedFile, err := metaSvc.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		return nil, fmt.Errorf("get updated attributes: %w", err)
	}

	return &updatedFile.FileAttr, nil
}

// applySetAttrsToFileAttr applies SetAttrs to FileAttr for initial file creation.
//
// This is used when creating a new file with explicit attributes.
// Note: This only applies the attributes that make sense at creation time.
func applySetAttrsToFileAttr(fileAttr *metadata.FileAttr, setAttrs *metadata.SetAttrs) {
	if setAttrs.Mode != nil {
		fileAttr.Mode = *setAttrs.Mode
	}
	if setAttrs.UID != nil {
		fileAttr.UID = *setAttrs.UID
	}
	if setAttrs.GID != nil {
		fileAttr.GID = *setAttrs.GID
	}
	// Size is always 0 for new files, ignore setAttrs.Size
	// Atime/Mtime will be set by repository to current time
}

// mapMetadataErrorToNFS maps metadata repository errors to NFS status codes.
// Uses errors.As to handle wrapped errors (e.g., from fmt.Errorf).
func mapMetadataErrorToNFS(err error) uint32 {
	var storeErr *metadata.StoreError
	if errors.As(err, &storeErr) {
		switch storeErr.Code {
		case metadata.ErrNotFound:
			return types.NFS3ErrNoEnt
		case metadata.ErrAccessDenied, metadata.ErrAuthRequired:
			return types.NFS3ErrAccess
		case metadata.ErrPermissionDenied:
			return types.NFS3ErrAccess
		case metadata.ErrPrivilegeRequired:
			// RFC 1813: EPERM for privilege violations (e.g., creating device files as non-root)
			return types.NFS3ErrPerm
		case metadata.ErrAlreadyExists:
			return types.NFS3ErrExist
		case metadata.ErrNotEmpty:
			return types.NFS3ErrNotEmpty
		case metadata.ErrIsDirectory:
			return types.NFS3ErrIsDir
		case metadata.ErrNotDirectory:
			return types.NFS3ErrNotDir
		case metadata.ErrInvalidArgument:
			return types.NFS3ErrInval
		case metadata.ErrNoSpace:
			return types.NFS3ErrNoSpc
		case metadata.ErrQuotaExceeded:
			return types.NFS3ErrDquot
		case metadata.ErrReadOnly:
			return types.NFS3ErrRofs
		case metadata.ErrNotSupported:
			return types.NFS3ErrNotSupp
		case metadata.ErrInvalidHandle, metadata.ErrStaleHandle:
			return types.NFS3ErrStale
		case metadata.ErrNameTooLong:
			return types.NFS3ErrNameTooLong
		case metadata.ErrIOError:
			return types.NFS3ErrIO
		default:
			return types.NFS3ErrIO
		}
	}
	return types.NFS3ErrIO
}

// ============================================================================
// Request Validation
// ============================================================================

// createValidationError represents a CREATE request validation error.
type createValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *createValidationError) Error() string {
	return e.message
}

// validateCreateRequest validates CREATE request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and not too long
//   - Filename is not empty and doesn't exceed 255 bytes
//   - Filename doesn't contain invalid characters
//   - Filename is not "." or ".."
//   - Creation mode is valid (0-2)
//
// Returns:
//   - nil if valid
//   - *createValidationError with NFS status if invalid
func validateCreateRequest(req *CreateRequest) *createValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &createValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.DirHandle) > 64 {
		return &createValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate filename
	if req.Filename == "" {
		return &createValidationError{
			message:   "empty filename",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	if len(req.Filename) > 255 {
		return &createValidationError{
			message:   fmt.Sprintf("filename too long: %d bytes (max 255)", len(req.Filename)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for invalid characters
	if bytes.ContainsAny([]byte(req.Filename), "/\x00") {
		return &createValidationError{
			message:   "filename contains invalid characters (null or path separator)",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Filename == "." || req.Filename == ".." {
		return &createValidationError{
			message:   fmt.Sprintf("filename cannot be '%s'", req.Filename),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Validate creation mode
	if req.Mode > types.CreateExclusive {
		return &createValidationError{
			message:   fmt.Sprintf("invalid creation mode: %d", req.Mode),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// createModeName returns a human-readable name for a creation mode.
func createModeName(mode uint32) string {
	switch mode {
	case types.CreateUnchecked:
		return "UNCHECKED"
	case types.CreateGuarded:
		return "GUARDED"
	case types.CreateExclusive:
		return "EXCLUSIVE"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", mode)
	}
}

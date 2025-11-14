package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
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
	// Status indicates the result of the create operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - NFS3ErrExist (17): File exists (GUARDED/EXCLUSIVE)
	//   - types.NFS3ErrNoEnt (2): Parent directory not found
	//   - types.NFS3ErrNotDir (20): Parent handle is not a directory
	//   - NFS3ErrInval (22): Invalid argument
	//   - types.NFS3ErrIO (5): I/O error
	Status uint32

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

// CreateContext contains the context information for processing a CREATE request.
//
// This includes client identification, authentication details, and cancellation
// handling used for:
//   - Access control enforcement (by repository)
//   - Audit logging
//   - Default ownership assignment
//   - Graceful cancellation of long-running operations
type CreateContext struct {
	// Context carries cancellation signals and deadlines
	// The Create handler checks this context to abort operations if the client
	// disconnects or the request times out
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor indicates the authentication method used.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the authenticated user ID (from AUTH_UNIX).
	// Used for default file ownership if not specified in Attr.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for default file ownership if not specified in Attr.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for permission checking.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// Implement NFSAuthContext interface for CreateContext
func (c *CreateContext) GetContext() context.Context { return c.Context }
func (c *CreateContext) GetClientAddr() string       { return c.ClientAddr }
func (c *CreateContext) GetAuthFlavor() uint32       { return c.AuthFlavor }
func (c *CreateContext) GetUID() *uint32             { return c.UID }
func (c *CreateContext) GetGID() *uint32             { return c.GID }
func (c *CreateContext) GetGIDs() []uint32           { return c.GIDs }

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
//   - ctx: Context with cancellation, client address and authentication credentials
//   - contentRepo: Content repository for file data operations
//   - metadataRepo: Metadata repository for file system structure
//   - req: Create request with parent handle, filename, mode, attributes
//
// **Returns:**
//   - *CreateResponse: Response with status and file handle (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures
//
// **RFC 1813 Section 3.3.8: CREATE Procedure**
func (h *DefaultNFSHandler) Create(
	ctx *CreateContext,
	contentRepo content.ContentStore,
	metadataRepo metadata.MetadataStore,
	req *CreateRequest,
) (*CreateResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("CREATE cancelled before processing: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, ctx.ClientAddr, ctx.Context.Err())
		return &CreateResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("CREATE: file='%s' dir=%x mode=%s client=%s auth=%d",
		req.Filename, req.DirHandle, createModeName(req.Mode), clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateCreateRequest(req); err != nil {
		logger.Warn("CREATE validation failed: file='%s' client=%s error=%v",
			req.Filename, clientIP, err)
		return &CreateResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify parent directory exists and is valid
	// ========================================================================

	parentHandle := metadata.FileHandle(req.DirHandle)
	parentAttr, err := metadataRepo.GetFile(ctx.Context, parentHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("CREATE cancelled during parent lookup: file='%s' dir=%x client=%s error=%v",
				req.Filename, req.DirHandle, clientIP, ctx.Context.Err())
			return &CreateResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Warn("CREATE failed: parent not found: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, err)
		return &CreateResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation directory state for WCC
	dirWccBefore := xdr.CaptureWccAttr(parentAttr)

	// Verify parent is a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("CREATE failed: parent not a directory: file='%s' dir=%x type=%d client=%s",
			req.Filename, req.DirHandle, parentAttr.Type, clientIP)

		// Get current parent state for WCC
		dirID := xdr.ExtractFileID(parentHandle)
		dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &CreateResponse{
			Status:    types.NFS3ErrNotDir,
			DirBefore: dirWccBefore,
			DirAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 3: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, metadataRepo, parentHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("CREATE cancelled during auth context building: file='%s' dir=%x client=%s error=%v",
				req.Filename, req.DirHandle, clientIP, ctx.Context.Err())
			return &CreateResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Error("CREATE failed: failed to build auth context: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, err)

		// Get current parent state for WCC
		dirID := xdr.ExtractFileID(parentHandle)
		dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &CreateResponse{
			Status:    types.NFS3ErrIO,
			DirBefore: dirWccBefore,
			DirAfter:  dirWccAfter,
		}, nil
	}

	// Check for cancellation before the existence check
	// This is important because Lookup may involve directory scanning
	select {
	case <-ctx.Context.Done():
		logger.Debug("CREATE cancelled before existence check: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, ctx.Context.Err())
		return &CreateResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 4: Check if file already exists using Lookup
	// ========================================================================

	existingHandle, existingAttr, err := metadataRepo.Lookup(authCtx, parentHandle, req.Filename)
	if err != nil && ctx.Context.Err() != nil {
		// Context was cancelled during Lookup
		logger.Debug("CREATE cancelled during existence check: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, ctx.Context.Err())
		return &CreateResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	}

	// Determine if file exists (no error from Lookup means it exists)
	fileExists := (err == nil)

	// Check for cancellation before the potentially expensive create/truncate operations
	// This is critical because these operations modify state
	select {
	case <-ctx.Context.Done():
		logger.Debug("CREATE cancelled before file operation: file='%s' dir=%x exists=%v client=%s error=%v",
			req.Filename, req.DirHandle, fileExists, clientIP, ctx.Context.Err())
		return &CreateResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
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
			logger.Debug("CREATE failed: file exists (guarded): file='%s' client=%s",
				req.Filename, clientIP)

			// Get current parent state for WCC
			parentAttr, _ = metadataRepo.GetFile(ctx.Context, parentHandle)
			dirID := xdr.ExtractFileID(parentHandle)
			dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &CreateResponse{
				Status:    types.NFS3ErrExist,
				DirBefore: dirWccBefore,
				DirAfter:  dirWccAfter,
			}, nil
		}

		// Create new file
		fileHandle, fileAttr, err = createNewFile(authCtx, metadataRepo, parentHandle, req)

	case types.CreateExclusive:
		// EXCLUSIVE: Check verifier if file exists
		if fileExists {
			// TODO: Implement verifier checking for idempotency
			// For now, treat like GUARDED
			logger.Debug("CREATE failed: file exists (exclusive): file='%s' client=%s verifier=%016x",
				req.Filename, clientIP, req.Verf)

			parentAttr, _ = metadataRepo.GetFile(ctx.Context, parentHandle)
			dirID := xdr.ExtractFileID(parentHandle)
			dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &CreateResponse{
				Status:    types.NFS3ErrExist,
				DirBefore: dirWccBefore,
				DirAfter:  dirWccAfter,
			}, nil
		}

		// Create new file with verifier
		fileHandle, fileAttr, err = createNewFile(authCtx, metadataRepo, parentHandle, req)

	case types.CreateUnchecked:
		// UNCHECKED: Create or truncate existing
		if fileExists {
			// Truncate existing file
			fileHandle = existingHandle
			fileAttr, err = truncateExistingFile(authCtx, contentRepo, metadataRepo, existingHandle, existingAttr, req)
		} else {
			// Create new file
			fileHandle, fileAttr, err = createNewFile(authCtx, metadataRepo, parentHandle, req)
		}

	default:
		logger.Warn("CREATE failed: invalid mode: file='%s' mode=%d client=%s",
			req.Filename, req.Mode, clientIP)

		parentAttr, _ = metadataRepo.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &CreateResponse{
			Status:    types.NFS3ErrInval,
			DirBefore: dirWccBefore,
			DirAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Handle errors from file creation/truncation
	// ========================================================================

	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("CREATE cancelled during file operation: file='%s' client=%s error=%v",
				req.Filename, clientIP, ctx.Context.Err())

			parentAttr, _ = metadataRepo.GetFile(ctx.Context, parentHandle)
			dirID := xdr.ExtractFileID(parentHandle)
			dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &CreateResponse{
				Status:    types.NFS3ErrIO,
				DirBefore: dirWccBefore,
				DirAfter:  dirWccAfter,
			}, ctx.Context.Err()
		}

		logger.Error("CREATE failed: repository error: file='%s' client=%s error=%v",
			req.Filename, clientIP, err)

		// Map repository errors to NFS status codes
		nfsStatus := mapMetadataErrorToNFS(err)

		parentAttr, _ = metadataRepo.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &CreateResponse{
			Status:    nfsStatus,
			DirBefore: dirWccBefore,
			DirAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 7: Build success response
	// ========================================================================

	// Convert metadata to NFS attributes
	fileID := xdr.ExtractFileID(fileHandle)
	nfsFileAttr := xdr.MetadataToNFS(fileAttr, fileID)

	// Get updated parent directory attributes
	parentAttr, _ = metadataRepo.GetFile(ctx.Context, parentHandle)
	dirID := xdr.ExtractFileID(parentHandle)
	nfsDirAttr := xdr.MetadataToNFS(parentAttr, dirID)

	logger.Info("CREATE successful: file='%s' handle=%x mode=%o size=%d client=%s",
		req.Filename, fileHandle, fileAttr.Mode, fileAttr.Size, clientIP)

	return &CreateResponse{
		Status:     types.NFS3OK,
		FileHandle: fileHandle,
		Attr:       nfsFileAttr,
		DirBefore:  dirWccBefore,
		DirAfter:   nfsDirAttr,
	}, nil
}

// ============================================================================
// Helper Functions for File Operations
// ============================================================================

// createNewFile creates a new file using the metadata repository's Create method.
//
// This function:
//  1. Builds file attributes with defaults from context
//  2. Calls repository.Create() which atomically:
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
//   - metadataRepo: Metadata repository
//   - parentHandle: Parent directory handle
//   - req: Create request with filename and attributes
//
// Returns:
//   - File handle, file attributes, and error
func createNewFile(
	authCtx *metadata.AuthContext,
	metadataRepo metadata.MetadataStore,
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

	// Apply explicit attributes from request
	if req.Attr != nil {
		applySetAttrsToFileAttr(fileAttr, req.Attr)
	}

	// Call repository's atomic Create operation
	// This handles file creation, parent linking, and permission checking
	fileHandle, err := metadataRepo.Create(authCtx, parentHandle, req.Filename, fileAttr)
	if err != nil {
		return nil, nil, fmt.Errorf("create file: %w", err)
	}

	// Get the completed attributes (with timestamps and ContentID)
	createdAttr, err := metadataRepo.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		return nil, nil, fmt.Errorf("get created file attributes: %w", err)
	}

	return fileHandle, createdAttr, nil
}

// truncateExistingFile truncates an existing file and updates attributes.
//
// For UNCHECKED mode when file exists, this:
//  1. Determines target size (from Attr.Size or 0)
//  2. Updates file metadata using SetFileAttributes
//  3. Truncates content (if content repository is available)
//
// Parameters:
//   - authCtx: Authentication context for permission checking
//   - contentRepo: Content repository for truncation
//   - metadataRepo: Metadata repository
//   - fileHandle: Handle of existing file
//   - existingAttr: Current file attributes
//   - req: Create request with attributes
//
// Returns:
//   - Updated file attributes and error
func truncateExistingFile(
	authCtx *metadata.AuthContext,
	contentRepo content.ContentStore,
	metadataRepo metadata.MetadataStore,
	fileHandle metadata.FileHandle,
	existingAttr *metadata.FileAttr,
	req *CreateRequest,
) (*metadata.FileAttr, error) {
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

	// Update file metadata using repository
	// This includes permission checking
	if err := metadataRepo.SetFileAttributes(authCtx, fileHandle, setAttrs); err != nil {
		return nil, fmt.Errorf("update file metadata: %w", err)
	}

	// Truncate content if repository supports writes and file has content
	if existingAttr.ContentID != "" {
		if writeRepo, ok := contentRepo.(content.WritableContentStore); ok {
			if err := writeRepo.Truncate(authCtx.Context, existingAttr.ContentID, targetSize); err != nil {
				logger.Warn("Failed to truncate content to %d bytes: %v", targetSize, err)
				// Non-fatal: metadata is already updated
			}
		}
	}

	// Get updated attributes
	updatedAttr, err := metadataRepo.GetFile(authCtx.Context, fileHandle)
	if err != nil {
		return nil, fmt.Errorf("get updated attributes: %w", err)
	}

	return updatedAttr, nil
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
func mapMetadataErrorToNFS(err error) uint32 {
	if storeErr, ok := err.(*metadata.StoreError); ok {
		switch storeErr.Code {
		case metadata.ErrNotFound:
			return types.NFS3ErrNoEnt
		case metadata.ErrAccessDenied, metadata.ErrAuthRequired:
			return types.NFS3ErrAcces
		case metadata.ErrPermissionDenied:
			return types.NFS3ErrAcces
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
		case metadata.ErrReadOnly:
			return types.NFS3ErrRofs
		case metadata.ErrNotSupported:
			return types.NFS3ErrNotSupp
		case metadata.ErrInvalidHandle, metadata.ErrStaleHandle:
			return types.NFS3ErrStale
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

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeCreateRequest decodes an XDR-encoded CREATE request.
//
// The request format (RFC 1813 Section 3.3.8):
//
//	struct CREATE3args {
//	    diropargs3   where;
//	    createhow3   how;
//	};
//
// Decoding process:
//  1. Decode directory handle (opaque)
//  2. Decode filename (string)
//  3. Decode creation mode (uint32)
//  4. Based on mode:
//     - UNCHECKED/GUARDED: Decode sattr3
//     - EXCLUSIVE: Decode verifier (8 bytes)
//
// Parameters:
//   - data: XDR-encoded bytes
//
// Returns:
//   - *CreateRequest: Decoded request
//   - error: Decoding error if data is malformed
func DecodeCreateRequest(data []byte) (*CreateRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short for CREATE request: %d bytes", len(data))
	}

	reader := bytes.NewReader(data)

	// Decode directory handle
	dirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// Decode filename
	filename, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode filename: %w", err)
	}

	// Decode creation mode
	var mode uint32
	if err := binary.Read(reader, binary.BigEndian, &mode); err != nil {
		return nil, fmt.Errorf("decode creation mode: %w", err)
	}

	req := &CreateRequest{
		DirHandle: dirHandle,
		Filename:  filename,
		Mode:      mode,
	}

	// Decode mode-specific data
	switch mode {
	case types.CreateExclusive:
		// Decode verifier (8 bytes)
		var verf uint64
		if err := binary.Read(reader, binary.BigEndian, &verf); err != nil {
			return nil, fmt.Errorf("decode creation verifier: %w", err)
		}
		req.Verf = verf

	case types.CreateUnchecked, types.CreateGuarded:
		// Decode sattr3 (set attributes)
		attr, err := xdr.DecodeSetAttrs(reader)
		if err != nil {
			return nil, fmt.Errorf("decode attributes: %w", err)
		}
		req.Attr = attr

	default:
		return nil, fmt.Errorf("invalid creation mode: %d", mode)
	}

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the CreateResponse into XDR-encoded bytes.
//
// The response format (RFC 1813 Section 3.3.8):
//  1. Status code (4 bytes)
//  2. If success:
//     - Optional file handle
//     - Optional file attributes
//     - Directory WCC data
//  3. If failure:
//     - Directory WCC data
//
// Returns:
//   - []byte: XDR-encoded response
//   - error: Encoding error
func (resp *CreateResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Success case: Write file handle and attributes
	if resp.Status == types.NFS3OK {
		// Write optional file handle
		if err := xdr.EncodeOptionalOpaque(&buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Write optional file attributes
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode file attributes: %w", err)
		}
	}

	// Write directory WCC data (both success and failure)
	if err := xdr.EncodeWccData(&buf, resp.DirBefore, resp.DirAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	return buf.Bytes(), nil
}

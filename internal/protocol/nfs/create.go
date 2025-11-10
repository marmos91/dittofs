package nfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
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
// This includes client identification and authentication details used for:
//   - Access control enforcement (by repository)
//   - Audit logging
//   - Default ownership assignment
type CreateContext struct {
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
//  1. Validate request parameters (filename, mode, handle)
//  2. Verify parent directory exists and is a directory
//  3. Capture pre-operation directory state (for WCC)
//  4. Check if file already exists
//  5. Based on mode: create new file or truncate existing
//  6. Return file handle and attributes
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer uses these for:
//   - Setting default file ownership (UID/GID)
//   - Logging and audit trails
//
// Access control enforcement should be implemented by the repository layer
// based on the credentials passed in the context.
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Access denied → NFS3ErrAcces
//   - Not found → types.NFS3ErrNoEnt
//   - Already exists → NFS3ErrExist
//   - I/O error → types.NFS3ErrIO
//
// **Parameters:**
//   - contentRepo: Content repository for file data operations
//   - metadataRepo: Metadata repository for file system structure
//   - req: Create request with parent handle, filename, mode, attributes
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *CreateResponse: Response with status and file handle (if successful)
//   - error: Returns error only for catastrophic internal failures
//
// **RFC 1813 Section 3.3.8: CREATE Procedure**
func (h *DefaultNFSHandler) Create(
	contentRepo content.Repository,
	metadataRepo metadata.Repository,
	req *CreateRequest,
	ctx *CreateContext,
) (*CreateResponse, error) {
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
	parentAttr, err := metadataRepo.GetFile(parentHandle)
	if err != nil {
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
	// Step 3: Check if file already exists
	// ========================================================================

	existingHandle, err := metadataRepo.GetChild(parentHandle, req.Filename)
	fileExists := (err == nil)

	// ========================================================================
	// Step 4: Handle creation based on mode
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
			parentAttr, _ = metadataRepo.GetFile(parentHandle)
			dirID := xdr.ExtractFileID(parentHandle)
			dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &CreateResponse{
				Status:    types.NFS3ErrExist,
				DirBefore: dirWccBefore,
				DirAfter:  dirWccAfter,
			}, nil
		}

		// Create new file
		fileHandle, fileAttr, err = createNewFile(metadataRepo, parentHandle, req, ctx)

	case types.CreateExclusive:
		// EXCLUSIVE: Check verifier if file exists
		if fileExists {
			// TODO: Implement verifier checking for idempotency
			// For now, treat like GUARDED
			logger.Debug("CREATE failed: file exists (exclusive): file='%s' client=%s verifier=%016x",
				req.Filename, clientIP, req.Verf)

			parentAttr, _ = metadataRepo.GetFile(parentHandle)
			dirID := xdr.ExtractFileID(parentHandle)
			dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &CreateResponse{
				Status:    types.NFS3ErrExist,
				DirBefore: dirWccBefore,
				DirAfter:  dirWccAfter,
			}, nil
		}

		// Create new file with verifier
		fileHandle, fileAttr, err = createNewFile(metadataRepo, parentHandle, req, ctx)

	case types.CreateUnchecked:
		// UNCHECKED: Create or truncate existing
		if fileExists {
			// Truncate existing file
			fileHandle = existingHandle
			fileAttr, err = truncateExistingFile(contentRepo, metadataRepo, existingHandle, req)
		} else {
			// Create new file
			fileHandle, fileAttr, err = createNewFile(metadataRepo, parentHandle, req, ctx)
		}

	default:
		logger.Warn("CREATE failed: invalid mode: file='%s' mode=%d client=%s",
			req.Filename, req.Mode, clientIP)

		parentAttr, _ = metadataRepo.GetFile(parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &CreateResponse{
			Status:    types.NFS3ErrInval,
			DirBefore: dirWccBefore,
			DirAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 5: Handle errors from file creation/truncation
	// ========================================================================

	if err != nil {
		logger.Error("CREATE failed: repository error: file='%s' client=%s error=%v",
			req.Filename, clientIP, err)

		parentAttr, _ = metadataRepo.GetFile(parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		dirWccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &CreateResponse{
			Status:    types.NFS3ErrIO,
			DirBefore: dirWccBefore,
			DirAfter:  dirWccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response
	// ========================================================================

	// Convert metadata to NFS attributes
	fileID := xdr.ExtractFileID(fileHandle)
	nfsFileAttr := xdr.MetadataToNFS(fileAttr, fileID)

	// Get updated parent directory attributes
	parentAttr, _ = metadataRepo.GetFile(parentHandle)
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

// createNewFile creates a new file with the specified attributes.
//
// This function:
//  1. Builds file attributes with defaults from context
//  2. Generates a unique file handle
//  3. Creates file in metadata repository
//  4. Links file to parent directory
//  5. Updates parent directory timestamps
//
// Parameters:
//   - metadataRepo: Metadata repository
//   - parentHandle: Parent directory handle
//   - req: Create request with filename and attributes
//   - ctx: Context with authentication credentials
//
// Returns:
//   - File handle, file attributes, and error
func createNewFile(
	metadataRepo metadata.Repository,
	parentHandle metadata.FileHandle,
	req *CreateRequest,
	ctx *CreateContext,
) (metadata.FileHandle, *metadata.FileAttr, error) {
	now := time.Now()

	// Build file attributes with defaults from context
	fileAttr := &metadata.FileAttr{
		Type:      metadata.FileTypeRegular,
		Mode:      0644, // Default: rw-r--r--
		UID:       0,
		GID:       0,
		Size:      0,
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Will be set on first write
	}

	// Apply context defaults (authenticated user's UID/GID)
	if ctx.UID != nil {
		fileAttr.UID = *ctx.UID
	}
	if ctx.GID != nil {
		fileAttr.GID = *ctx.GID
	}

	// Apply explicit attributes from request
	if req.Attr != nil {
		xdr.ApplySetAttrs(fileAttr, req.Attr)
	}

	// Generate unique file handle
	fileHandle := generateFileHandle(req.DirHandle, req.Filename, now)

	// Create file in metadata repository
	if err := metadataRepo.CreateFile(fileHandle, fileAttr); err != nil {
		return nil, nil, fmt.Errorf("create file metadata: %w", err)
	}

	// Link file to parent directory
	if err := metadataRepo.AddChild(parentHandle, req.Filename, fileHandle); err != nil {
		// Cleanup: delete the file we just created
		metadataRepo.DeleteFile(fileHandle)
		return nil, nil, fmt.Errorf("link to parent: %w", err)
	}

	// Set parent relationship
	if err := metadataRepo.SetParent(fileHandle, parentHandle); err != nil {
		// Cleanup: remove from parent and delete file
		metadataRepo.DeleteChild(parentHandle, req.Filename)
		metadataRepo.DeleteFile(fileHandle)
		return nil, nil, fmt.Errorf("set parent: %w", err)
	}

	// Update parent directory timestamps
	parentAttr, err := metadataRepo.GetFile(parentHandle)
	if err == nil {
		parentAttr.Mtime = now
		parentAttr.Ctime = now
		metadataRepo.UpdateFile(parentHandle, parentAttr)
	}

	return fileHandle, fileAttr, nil
}

// truncateExistingFile truncates an existing file and updates attributes.
//
// For UNCHECKED mode when file exists, this:
//  1. Gets current file attributes
//  2. Determines target size (from Attr.Size or 0)
//  3. Updates file metadata
//  4. Truncates content (if WriteRepository is available)
//
// Parameters:
//   - contentRepo: Content repository for truncation
//   - metadataRepo: Metadata repository
//   - fileHandle: Handle of existing file
//   - req: Create request with attributes
//   - ctx: Context with authentication credentials
//
// Returns:
//   - Updated file attributes and error
func truncateExistingFile(
	contentRepo content.Repository,
	metadataRepo metadata.Repository,
	fileHandle metadata.FileHandle,
	req *CreateRequest,
) (*metadata.FileAttr, error) {
	// Get current file attributes
	fileAttr, err := metadataRepo.GetFile(fileHandle)
	if err != nil {
		return nil, fmt.Errorf("get file for truncation: %w", err)
	}

	now := time.Now()

	// Determine target size
	targetSize := uint64(0) // Default: truncate to empty
	if req.Attr != nil && req.Attr.SetSize {
		targetSize = req.Attr.Size
	}

	// Update file metadata
	fileAttr.Size = targetSize
	fileAttr.Mtime = now
	fileAttr.Ctime = now

	// Apply other requested attributes
	if req.Attr != nil {
		xdr.ApplySetAttrs(fileAttr, req.Attr)
	}

	// Update metadata
	if err := metadataRepo.UpdateFile(fileHandle, fileAttr); err != nil {
		return nil, fmt.Errorf("update file metadata: %w", err)
	}

	// Truncate content if repository supports writes
	if fileAttr.ContentID != "" {
		if writeRepo, ok := contentRepo.(content.WriteRepository); ok {
			if err := writeRepo.Truncate(fileAttr.ContentID, targetSize); err != nil {
				logger.Warn("Failed to truncate content to %d bytes: %v", targetSize, err)
				// Non-fatal: metadata is already updated
			}
		}
	}

	return fileAttr, nil
}

// generateFileHandle creates a deterministic file handle.
//
// The handle is generated using SHA-256 hash of:
//   - Parent directory handle
//   - Filename
//   - Current timestamp
//
// This ensures handles are:
//   - Unique (timestamp + filename prevents collisions)
//   - Stable (same parameters → same handle)
//   - Opaque (clients can't predict or manipulate)
//
// Parameters:
//   - parentHandle: Parent directory handle
//   - filename: Name of file
//   - timestamp: Current time for uniqueness
//
// Returns:
//   - File handle (32 bytes from SHA-256)
func generateFileHandle(parentHandle []byte, filename string, timestamp time.Time) metadata.FileHandle {
	data := fmt.Sprintf("%x-%s-%d", parentHandle, filename, timestamp.UnixNano())
	hash := sha256.Sum256([]byte(data))
	return metadata.FileHandle(hash[:])
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

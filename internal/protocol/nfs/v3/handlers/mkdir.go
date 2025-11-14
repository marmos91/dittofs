package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
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
	// Status indicates the result of the mkdir operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - NFS3ErrExist (17): Directory already exists
	//   - types.NFS3ErrNoEnt (2): Parent directory not found
	//   - types.NFS3ErrNotDir (20): Parent handle is not a directory
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrNoSpc (28): No space left on device
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrInval (22): Invalid argument (e.g., bad name)
	//   - NFS3ErrNameTooLong (63): Directory name too long
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

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

// MkdirContext contains the context information needed to process a MKDIR request.
// This includes client identification, authentication details, and cancellation
// handling for access control and auditing purposes.
type MkdirContext struct {
	// Context carries cancellation signals and deadlines
	// The Mkdir handler checks this context to abort operations if the client
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
	// Used for default directory ownership if not specified in Attr.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for default directory ownership if not specified in Attr.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for access control checks by the store.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// Implement NFSAuthContext interface for MkdirContext
func (c *MkdirContext) GetContext() context.Context { return c.Context }
func (c *MkdirContext) GetClientAddr() string       { return c.ClientAddr }
func (c *MkdirContext) GetAuthFlavor() uint32       { return c.AuthFlavor }
func (c *MkdirContext) GetUID() *uint32             { return c.UID }
func (c *MkdirContext) GetGID() *uint32             { return c.GID }
func (c *MkdirContext) GetGIDs() []uint32           { return c.GIDs }


// ============================================================================
// Protocol Handler
// ============================================================================

// Mkdir creates a new directory within a parent directory.
//
// This implements the NFS MKDIR procedure as defined in RFC 1813 Section 3.3.9.
//
// **Purpose:**
//
// MKDIR is used to create new subdirectories in the NFS filesystem. It's essential
// for building directory hierarchies and organizing files. Common use cases include:
//   - Creating project directories
//   - Building nested folder structures
//   - Setting up workspace directories with specific permissions
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format, name syntax)
//  3. Build AuthContext for permission checking
//  4. Verify parent directory exists and is a directory (via store)
//  5. Capture pre-operation parent state (for WCC)
//  6. Check for cancellation before existence check
//  7. Check if directory name already exists using Lookup
//  8. Check for cancellation before create operation
//  9. Delegate directory creation to store.Create() with FileTypeDirectory
//  10. Return new directory handle and attributes with WCC data
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after parent lookup (before potentially expensive existence check)
//   - Checks before the create operation (the most critical check point)
//   - store.Create() respects context internally
//   - Returns NFS3ErrIO status with context error for cancellation
//   - Always includes WCC data in responses for cache consistency
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (creation, validation, access control) delegated to store
//   - File handle validation performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer builds AuthContext and passes it to store.Create(),
// which implements:
//   - Write permission checking on the parent directory
//   - Access control based on UID/GID
//   - Default ownership assignment for new directories
//   - Quota enforcement
//
// **Directory Attributes:**
//
// The client can specify:
//   - Mode: Directory permissions (e.g., 0755 for rwxr-xr-x)
//   - UID: Owner user ID
//   - GID: Owner group ID
//
// The server/store sets:
//   - Type: Always FileTypeDirectory
//   - Size: Implementation-dependent (typically 4096 bytes)
//   - Timestamps: Current time for atime, mtime, ctime
//   - Nlink: Initially 2 (. and ..)
//   - ContentID: Empty for directories
//
// **Naming Restrictions:**
//
// Per RFC 1813 and POSIX conventions, directory names:
//   - Must not be empty
//   - Must not be "." (current directory)
//   - Must not be ".." (parent directory)
//   - Must not exceed 255 bytes
//   - Must not contain null bytes (string terminator)
//   - Must not contain '/' (path separator)
//   - Should not contain control characters (0x00-0x1F, 0x7F)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes via mapMetadataErrorToNFS().
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients detect if the parent directory changed between
// operations and maintain cache consistency. The server:
//  1. Captures parent attributes before the operation (WccBefore)
//  2. Performs the directory creation
//  3. Captures parent attributes after the operation (WccAfter)
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces write permission on parent directory
//   - Name validation prevents directory traversal attacks
//   - Client context enables audit logging
//   - Access control prevents unauthorized directory creation
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - metadataStore: The metadata store for directory operations
//   - req: The mkdir request containing parent handle, name, and attributes
//
// **Returns:**
//   - *MkdirResponse: Response with status, new directory handle (if successful),
//     and WCC data for the parent directory
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.9: MKDIR Procedure**
func (h *DefaultNFSHandler) Mkdir(
	ctx *MkdirContext,
	metadataStore metadata.MetadataStore,
	req *MkdirRequest,
) (*MkdirResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("MKDIR cancelled before processing: name='%s' dir=%x client=%s error=%v",
			req.Name, req.DirHandle, ctx.ClientAddr, ctx.Context.Err())
		return &MkdirResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	var mode uint32 = 0755 // Default
	if req.Attr != nil && req.Attr.Mode != nil {
		mode = *req.Attr.Mode
	}

	logger.Info("MKDIR: name='%s' dir=%x mode=%o client=%s auth=%d",
		req.Name, req.DirHandle, mode, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateMkdirRequest(req); err != nil {
		logger.Warn("MKDIR validation failed: name='%s' client=%s error=%v",
			req.Name, clientIP, err)
		return &MkdirResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify parent directory exists and is valid
	// ========================================================================

	parentHandle := metadata.FileHandle(req.DirHandle)
	parentAttr, err := metadataStore.GetFile(ctx.Context, parentHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("MKDIR cancelled during parent lookup: name='%s' dir=%x client=%s error=%v",
				req.Name, req.DirHandle, clientIP, ctx.Context.Err())
			return &MkdirResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Warn("MKDIR failed: parent not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &MkdirResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(parentAttr)

	// ========================================================================
	// Step 3: Build AuthContext with share-level identity mapping
	// ========================================================================

	authCtx, err := BuildAuthContextWithMapping(ctx, metadataStore, parentHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("MKDIR cancelled during auth context building: name='%s' dir=%x client=%s error=%v",
				req.Name, req.DirHandle, clientIP, ctx.Context.Err())

			dirID := xdr.ExtractFileID(parentHandle)
			wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &MkdirResponse{
				Status:    types.NFS3ErrIO,
				WccBefore: wccBefore,
				WccAfter:  wccAfter,
			}, ctx.Context.Err()
		}

		logger.Error("MKDIR failed: failed to build auth context: name='%s' dir=%x client=%s error=%v",
			req.Name, req.DirHandle, clientIP, err)

		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrIO,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// Verify parent is actually a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("MKDIR failed: parent not a directory: dir=%x type=%d client=%s",
			req.DirHandle, parentAttr.Type, clientIP)

		// Get current parent state for WCC
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrNotDir,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// Check for cancellation before the existence check
	// Lookup may involve directory scanning which can be expensive
	select {
	case <-ctx.Context.Done():
		logger.Debug("MKDIR cancelled before existence check: name='%s' dir=%x client=%s error=%v",
			req.Name, req.DirHandle, clientIP, ctx.Context.Err())

		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrIO,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 4: Check if directory name already exists using Lookup
	// ========================================================================

	_, _, err = metadataStore.Lookup(authCtx, parentHandle, req.Name)
	if err != nil && ctx.Context.Err() != nil {
		// Context was cancelled during Lookup
		logger.Debug("MKDIR cancelled during existence check: name='%s' dir=%x client=%s error=%v",
			req.Name, req.DirHandle, clientIP, ctx.Context.Err())

		// Get updated parent attributes for WCC data
		parentAttr, _ = metadataStore.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrIO,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, ctx.Context.Err()
	}

	if err == nil {
		// Child exists (no error from Lookup)
		logger.Debug("MKDIR failed: directory '%s' already exists: dir=%x client=%s",
			req.Name, req.DirHandle, clientIP)

		// Get updated parent attributes for WCC data
		parentAttr, _ = metadataStore.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrExist,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}
	// If error from Lookup, directory doesn't exist (good) - continue

	// Check for cancellation before the create operation
	// This is the most critical check - we don't want to start creating
	// the directory if the client has already disconnected
	select {
	case <-ctx.Context.Done():
		logger.Debug("MKDIR cancelled before create operation: name='%s' dir=%x client=%s error=%v",
			req.Name, req.DirHandle, clientIP, ctx.Context.Err())

		// Get updated parent attributes for WCC data
		parentAttr, _ = metadataStore.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrIO,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, ctx.Context.Err()
	default:
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
	newHandle, err := metadataStore.Create(authCtx, parentHandle, req.Name, dirAttr)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("MKDIR cancelled during create operation: name='%s' client=%s error=%v",
				req.Name, clientIP, ctx.Context.Err())

			// Get updated parent attributes for WCC data
			parentAttr, _ = metadataStore.GetFile(ctx.Context, parentHandle)
			dirID := xdr.ExtractFileID(parentHandle)
			wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

			return &MkdirResponse{
				Status:    types.NFS3ErrIO,
				WccBefore: wccBefore,
				WccAfter:  wccAfter,
			}, ctx.Context.Err()
		}

		logger.Error("MKDIR failed: store error: name='%s' client=%s error=%v",
			req.Name, clientIP, err)

		// Get updated parent attributes for WCC data
		parentAttr, _ = metadataStore.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		// Map store errors to NFS status codes
		status := mapMetadataErrorToNFS(err)

		return &MkdirResponse{
			Status:    status,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with new directory attributes
	// ========================================================================

	// Get the newly created directory's attributes
	newDirAttr, err := metadataStore.GetFile(ctx.Context, newHandle)
	if err != nil {
		logger.Error("MKDIR: failed to get new directory attributes: handle=%x error=%v",
			newHandle, err)
		// This shouldn't happen, but handle gracefully
		return &MkdirResponse{Status: types.NFS3ErrIO}, nil
	}

	// Generate file ID from handle for NFS attributes
	fileid := xdr.ExtractFileID(newHandle)
	nfsAttr := xdr.MetadataToNFS(newDirAttr, fileid)

	// Get updated parent attributes for WCC data
	parentAttr, _ = metadataStore.GetFile(ctx.Context, parentHandle)
	parentFileid := xdr.ExtractFileID(parentHandle)
	wccAfter := xdr.MetadataToNFS(parentAttr, parentFileid)

	logger.Info("MKDIR successful: name='%s' handle=%x mode=%o size=%d client=%s",
		req.Name, newHandle, newDirAttr.Mode, newDirAttr.Size, clientIP)

	logger.Debug("MKDIR details: fileid=%d uid=%d gid=%d parent=%d",
		fileid, newDirAttr.UID, newDirAttr.GID, parentFileid)

	return &MkdirResponse{
		Status:    types.NFS3OK,
		Handle:    newHandle,
		Attr:      nfsAttr,
		WccBefore: wccBefore,
		WccAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// mkdirValidationError represents a MKDIR request validation error.
type mkdirValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *mkdirValidationError) Error() string {
	return e.message
}

// validateMkdirRequest validates MKDIR request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Directory name is valid (not empty, not "." or "..", length, characters)
//
// Returns:
//   - nil if valid
//   - *mkdirValidationError with NFS status if invalid
func validateMkdirRequest(req *MkdirRequest) *mkdirValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &mkdirValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &mkdirValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &mkdirValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate directory name
	if req.Name == "" {
		return &mkdirValidationError{
			message:   "empty directory name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &mkdirValidationError{
			message:   fmt.Sprintf("directory name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &mkdirValidationError{
			message:   fmt.Sprintf("directory name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.ContainsAny([]byte(req.Name), "\x00") {
		return &mkdirValidationError{
			message:   "directory name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if bytes.ContainsAny([]byte(req.Name), "/") {
		return &mkdirValidationError{
			message:   "directory name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	// This prevents potential issues with terminal output and logs
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &mkdirValidationError{
				message:   fmt.Sprintf("directory name contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeMkdirRequest decodes a MKDIR request from XDR-encoded bytes.
//
// The MKDIR request has the following XDR structure (RFC 1813 Section 3.3.9):
//
//	struct MKDIR3args {
//	    diropargs3   where;     // Parent dir handle + name
//	    sattr3       attributes; // Directory attributes to set
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read directory name (variable length string with padding)
//  3. Read attributes structure (sattr3)
//
// Parameters:
//   - data: XDR-encoded bytes containing the mkdir request
//
// Returns:
//   - *MkdirRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
func DecodeMkdirRequest(data []byte) (*MkdirRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)
	req := &MkdirRequest{}

	// Decode parent directory handle
	handle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}
	req.DirHandle = handle

	// Decode directory name
	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	req.Name = name

	// Decode sattr3 attributes structure
	attr, err := xdr.DecodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}
	req.Attr = attr

	var mode uint32
	if attr != nil && attr.Mode != nil {
		mode = *attr.Mode
	}

	logger.Debug("Decoded MKDIR request: handle_len=%d name='%s' mode=%o",
		len(handle), name, mode)

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the MkdirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.9 specifications.
//
// Returns:
//   - []byte: The XDR-encoded response ready to send to the client
//   - error: Any error encountered during encoding
func (resp *MkdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// Success case: Write handle and attributes
	if resp.Status == types.NFS3OK {
		// Write new directory handle (post_op_fh3 - optional)
		if err := xdr.EncodeOptionalOpaque(&buf, resp.Handle); err != nil {
			return nil, fmt.Errorf("encode handle: %w", err)
		}

		// Write new directory attributes (post_op_attr - optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode attributes: %w", err)
		}
	}

	// Write WCC data for parent directory (both success and failure)
	if err := xdr.EncodeWccData(&buf, resp.WccBefore, resp.WccAfter); err != nil {
		return nil, fmt.Errorf("encode wcc data: %w", err)
	}

	logger.Debug("Encoded MKDIR response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
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
	Attr metadata.SetAttrs
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
// This includes client identification and authentication details for access control
// and auditing purposes.
type MkdirContext struct {
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
	// Used for access control checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

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
//  1. Validate request parameters (handle format, name syntax)
//  2. Extract client IP and authentication credentials from context
//  3. Verify parent directory exists and is a directory (via repository)
//  4. Capture pre-operation parent state (for WCC)
//  5. Delegate directory creation to repository.CreateDirectory()
//  6. Return new directory handle and attributes with WCC data
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (creation, validation, access control) delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which can implement:
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
// The server sets:
//   - Type: Always FileTypeDirectory
//   - Size: Implementation-dependent (typically 4096 bytes)
//   - Timestamps: Current time for atime, mtime, ctime
//   - Nlink: Initially 2 (. and ..)
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
// Repository errors are mapped to NFS status codes:
//   - Parent not found → types.NFS3ErrNoEnt
//   - Parent not directory → types.NFS3ErrNotDir
//   - Name already exists → NFS3ErrExist
//   - Invalid name → NFS3ErrInval
//   - Name too long → NFS3ErrNameTooLong
//   - Permission denied → NFS3ErrAcces
//   - No space → NFS3ErrNoSpc
//   - I/O error → types.NFS3ErrIO
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients detect if the parent directory changed between
// operations and maintain cache consistency. The server:
//  1. Captures parent attributes before the operation (WccBefore)
//  2. Performs the directory creation
//  3. Captures parent attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect concurrent modifications to the parent directory
//   - Update their cached parent directory attributes
//   - Invalidate stale cached data
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces write permission on parent directory
//   - Name validation prevents directory traversal attacks
//   - Client context enables audit logging
//   - Access control prevents unauthorized directory creation
//
// **Parameters:**
//   - repository: The metadata repository for directory operations
//   - req: The mkdir request containing parent handle, name, and attributes
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *MkdirResponse: Response with status, new directory handle (if successful),
//     and WCC data for the parent directory
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.9: MKDIR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &MkdirRequest{
//	    DirHandle: parentHandle,
//	    Name:      "documents",
//	    Attr:      SetAttrs{SetMode: true, Mode: 0755},
//	}
//	ctx := &MkdirContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Mkdir(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Directory created successfully, use resp.Handle
//	}
func (h *DefaultNFSHandler) Mkdir(
	repository metadata.Repository,
	req *MkdirRequest,
	ctx *MkdirContext,
) (*MkdirResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("MKDIR: name='%s' dir=%x mode=%o client=%s auth=%d",
		req.Name, req.DirHandle, req.Attr.Mode, clientIP, ctx.AuthFlavor)

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
	parentAttr, err := repository.GetFile(parentHandle)
	if err != nil {
		logger.Warn("MKDIR failed: parent not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &MkdirResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(parentAttr)

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

	// ========================================================================
	// Step 3: Check if directory name already exists
	// ========================================================================

	_, err = repository.GetChild(parentHandle, req.Name)
	if err == nil {
		// Child exists
		logger.Debug("MKDIR failed: directory '%s' already exists: dir=%x client=%s",
			req.Name, req.DirHandle, clientIP)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &MkdirResponse{
			Status:    types.NFS3ErrExist,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Create directory via repository
	// ========================================================================
	// The repository is responsible for:
	// - Building complete directory attributes with defaults
	// - Checking write permission on parent directory
	// - Creating the directory metadata
	// - Linking it to the parent
	// - Updating parent directory timestamps

	// Build authentication context for repository
	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	// Convert SetAttrs to metadata format for repository
	dirAttr := xdr.ConvertSetAttrsToMetadata(metadata.FileTypeDirectory, &req.Attr, authCtx)

	newHandle, err := repository.CreateDirectory(parentHandle, req.Name, dirAttr, authCtx)
	if err != nil {
		logger.Error("MKDIR failed: repository error: name='%s' client=%s error=%v",
			req.Name, clientIP, err)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		// Map repository errors to NFS status codes
		status := xdr.MapRepositoryErrorToNFSStatus(err, clientIP, "mkdir")

		return &MkdirResponse{
			Status:    status,
			WccBefore: wccBefore,
			WccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 5: Build success response with new directory attributes
	// ========================================================================

	// Get the newly created directory's attributes
	newDirAttr, err := repository.GetFile(newHandle)
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
	parentAttr, _ = repository.GetFile(parentHandle)
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
// XDR encoding details:
//   - All integers are 4-byte aligned (32-bit)
//   - Variable-length data (handles, strings) are length-prefixed
//   - Padding is added to maintain 4-byte alignment
//
// Parameters:
//   - data: XDR-encoded bytes containing the mkdir request
//
// Returns:
//   - *MkdirRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
//
// Example:
//
//	data := []byte{...} // XDR-encoded MKDIR request from network
//	req, err := DecodeMkdirRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Name, req.Attr in MKDIR procedure
func DecodeMkdirRequest(data []byte) (*MkdirRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)
	req := &MkdirRequest{}

	// ========================================================================
	// Decode parent directory handle
	// ========================================================================

	handle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}
	req.DirHandle = handle

	// ========================================================================
	// Decode directory name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	req.Name = name

	// ========================================================================
	// Decode sattr3 attributes structure
	// ========================================================================

	attr, err := xdr.DecodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}
	req.Attr = *attr

	logger.Debug("Decoded MKDIR request: handle_len=%d name='%s' set_mode=%v mode=%o",
		len(handle), name, attr.SetMode, attr.Mode)

	return req, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the MkdirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The MKDIR response has the following XDR structure (RFC 1813 Section 3.3.9):
//
//	struct MKDIR3res {
//	    nfsstat3    status;
//	    union switch (status) {
//	    case NFS3_OK:
//	        struct {
//	            post_op_fh3   obj;        // New directory handle
//	            post_op_attr  obj_attributes;
//	            wcc_data      dir_wcc;    // Parent directory WCC
//	        } resok;
//	    default:
//	        wcc_data      dir_wcc;
//	    } resfail;
//	};
//
// Encoding process:
//  1. Write status code (4 bytes)
//  2. If success (NFS3OK):
//     a. Write optional new directory handle
//     b. Write optional new directory attributes
//     c. Write WCC data for parent directory
//  3. If failure:
//     a. Write WCC data for parent directory (best effort)
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
//	resp := &MkdirResponse{
//	    Status:    types.NFS3OK,
//	    Handle:    newDirHandle,
//	    Attr:      dirAttr,
//	    WccBefore: wccBefore,
//	    WccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *MkdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Success case: Write handle and attributes
	// ========================================================================

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

	// ========================================================================
	// Write WCC data for parent directory (both success and failure)
	// ========================================================================

	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.
	if err := xdr.EncodeWccData(&buf, resp.WccBefore, resp.WccAfter); err != nil {
		return nil, fmt.Errorf("encode wcc data: %w", err)
	}

	logger.Debug("Encoded MKDIR response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

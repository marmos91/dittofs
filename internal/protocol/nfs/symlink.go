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

// SymlinkRequest represents a SYMLINK request from an NFS client.
// The client provides a parent directory handle, a name for the new symlink,
// the target path, and optional attributes for the symlink.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.10 specifies the SYMLINK procedure as:
//
//	SYMLINK3res NFSPROC3_SYMLINK(SYMLINK3args) = 10;
//
// The SYMLINK procedure creates a symbolic link. A symbolic link is a special
// file that contains a pathname to another file or directory. When a client
// accesses a symbolic link, it reads the target path and performs a new lookup
// using that path.
type SymlinkRequest struct {
	// DirHandle is the file handle of the parent directory where the symlink
	// will be created. Must be a valid directory handle obtained from MOUNT
	// or LOOKUP. Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name for the new symbolic link.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	Name string

	// Target is the pathname that the symbolic link will point to.
	// This can be:
	//   - Absolute path: /usr/bin/python3
	//   - Relative path: ../lib/config.txt
	//   - Any valid pathname string
	// Maximum length is typically 1024 bytes (POSIX PATH_MAX).
	// The server does not validate or resolve the target path.
	Target string

	// Attr contains attributes for the new symbolic link
	Attr metadata.SetAttrs
}

// SymlinkResponse represents the response to a SYMLINK request.
// It contains the status, optional file handle and attributes for the new symlink,
// and WCC data for the parent directory.
//
// The response is encoded in XDR format before being sent back to the client.
type SymlinkResponse struct {
	// Status indicates the result of the symlink creation.
	// Common values:
	//   - types.NFS3OK (0): Success - symlink created
	//   - types.NFS3ErrNoEnt (2): Parent directory not found
	//   - types.NFS3ErrNotDir (20): DirHandle is not a directory
	//   - NFS3ErrExist (17): A file with the same name already exists
	//   - NFS3ErrAcces (13): Permission denied
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	//   - NFS3ErrNameTooLong (63): Name or target path too long
	//   - NFS3ErrNoSpc (28): No space left on device
	Status uint32

	// FileHandle is the file handle of the newly created symlink.
	// Only present when Status == types.NFS3OK.
	// Clients can use this handle for subsequent operations (GETATTR, READLINK, etc.).
	FileHandle []byte

	// Attr contains post-operation attributes of the newly created symlink.
	// Optional, may be nil. Includes type (symlink), permissions, owner, size, etc.
	// Only present when Status == types.NFS3OK.
	Attr *types.NFSFileAttr

	// DirAttrBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect changes.
	// May be nil if attributes could not be captured.
	DirAttrBefore *types.WccAttr

	// DirAttrAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide updated directory state.
	// Should be present for both success and failure cases when possible.
	DirAttrAfter *types.NFSFileAttr
}

// SymlinkContext contains the context information needed to process a SYMLINK request.
// This includes client identification and authentication details for access control.
type SymlinkContext struct {
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
	// Used for access control checks and setting symlink ownership.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for access control checks and setting symlink ownership.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for checking if user belongs to directory's group.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Symlink creates a symbolic link in a directory.
//
// This implements the NFS SYMLINK procedure as defined in RFC 1813 Section 3.3.10.
//
// **Purpose:**
//
// SYMLINK creates a special file that contains a pathname to another file or
// directory. Symbolic links provide:
//   - Flexible file system organization (links across filesystems)
//   - Indirection for file access (change target without changing link)
//   - Multiple paths to the same file
//   - Links to directories (unlike hard links)
//
// Common use cases:
//   - Creating shortcuts: ln -s /usr/bin/python3 /usr/local/bin/python
//   - Version management: ln -s app-v2.0 app-current
//   - Compatibility paths: ln -s /new/path /old/path
//
// **Process:**
//
//  1. Validate request parameters (handle format, name syntax, target length)
//  2. Extract client IP and authentication credentials from context
//  3. Verify parent directory exists and is a directory (via repository)
//  4. Capture pre-operation directory state (for WCC)
//  5. Convert client attributes to metadata format with proper defaults
//  6. Delegate symlink creation to repository.CreateSymlink()
//  7. Return symlink handle, attributes, and updated directory WCC data
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
//   - Ownership assignment for the new symlink
//
// **Symlink Semantics:**
//
// Per RFC 1813 and POSIX semantics:
//   - Target path is stored as-is without validation or resolution
//   - Target can be absolute or relative path
//   - Target can point to non-existent files (dangling symlinks are allowed)
//   - Symlink permissions are typically 0777 (lrwxrwxrwx)
//   - Actual access control is applied when following the symlink
//   - Symlink size is typically the length of the target pathname
//
// **Attribute Handling:**
//
// The client can provide optional attributes in the request:
//   - Mode: Usually ignored, symlinks default to 0777
//   - UID/GID: Used for ownership (or defaults to authenticated user)
//   - Size/Times: Ignored, set by server
//
// The protocol layer uses `convertSetAttrsToMetadata` to:
//   - Apply client-provided attributes (mode, uid, gid)
//   - Set defaults from authentication context when not provided
//   - Ensure consistent behavior with other file creation operations
//
// The repository completes the attributes with:
//   - File type (symlink)
//   - Creation/modification times
//   - Size (length of target path)
//   - Target path storage
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - Name already exists → NFS3ErrExist
//   - Permission denied → NFS3ErrAcces
//   - No space left → NFS3ErrNoSpc
//   - I/O error → types.NFS3ErrIO
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients maintain cache coherency for the parent directory:
//  1. Capture directory attributes before the operation (WccBefore)
//  2. Perform the symlink creation
//  3. Capture directory attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect if directory changed during the operation
//   - Update their cached directory attributes
//   - Invalidate stale cached data
//
// **Performance Considerations:**
//
// SYMLINK is a metadata-only operation:
//   - No data blocks need to be allocated
//   - Target path is stored in metadata (or small inline data)
//   - Operation should be relatively fast
//   - May require directory block updates
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces write permission on parent directory
//   - Name validation prevents directory traversal attacks
//   - Target path is not validated (allows flexibility but requires care)
//   - Client context enables audit logging
//   - Symlinks can create security issues (symlink attacks, race conditions)
//
// **Parameters:**
//   - repository: The metadata repository for symlink operations
//   - req: The symlink request containing directory handle, name, target, and attributes
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *SymlinkResponse: Response with status, symlink handle/attributes, and directory WCC
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.10: SYMLINK Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &SymlinkRequest{
//	    DirHandle: dirHandle,
//	    Name:      "mylink",
//	    Target:    "/usr/bin/python3",
//	    Attr:      SetAttrs{Mode: &mode},
//	}
//	ctx := &SymlinkContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Symlink(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Symlink created successfully
//	    // Use resp.FileHandle for subsequent operations
//	}
func (h *DefaultNFSHandler) Symlink(
	repository metadata.Repository,
	req *SymlinkRequest,
	ctx *SymlinkContext,
) (*SymlinkResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("SYMLINK: name='%s' target='%s' dir=%x client=%s auth=%d",
		req.Name, req.Target, req.DirHandle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateSymlinkRequest(req); err != nil {
		logger.Warn("SYMLINK validation failed: name='%s' target='%s' client=%s error=%v",
			req.Name, req.Target, clientIP, err)
		return &SymlinkResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify parent directory exists and capture pre-op attributes
	// ========================================================================

	dirHandle := metadata.FileHandle(req.DirHandle)
	dirAttr, err := repository.GetFile(dirHandle)
	if err != nil {
		logger.Warn("SYMLINK failed: directory not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &SymlinkResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(dirAttr)

	// Verify parent is a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("SYMLINK failed: handle not a directory: dir=%x type=%d client=%s",
			req.DirHandle, dirAttr.Type, clientIP)

		// Include directory attributes even on error for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &SymlinkResponse{
			Status:        types.NFS3ErrNotDir,
			DirAttrBefore: wccBefore,
			DirAttrAfter:  nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 3: Build authentication context for repository
	// ========================================================================

	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	// ========================================================================
	// Step 4: Convert client attributes to metadata format
	// ========================================================================
	// Use convertSetAttrsToMetadata to ensure consistent attribute handling
	// across all file creation operations (MKDIR, MKNOD, SYMLINK, CREATE).
	// This properly applies:
	// - Client-provided attributes (mode, uid, gid)
	// - Defaults from authentication context when not provided by client
	// - Default permissions (0777 for symlinks)
	//
	// The repository will complete the attributes with:
	// - Timestamps (atime, mtime, ctime)
	// - Size (length of target path)
	// - Target path (stored in SymlinkTarget field)

	symlinkAttr := xdr.ConvertSetAttrsToMetadata(metadata.FileTypeSymlink, &req.Attr, authCtx)

	// ========================================================================
	// Step 5: Create symlink via repository
	// ========================================================================
	// The repository is responsible for:
	// - Checking write permission on parent directory
	// - Verifying name doesn't already exist
	// - Completing symlink attributes (timestamps, size, target path)
	// - Creating the symlink metadata
	// - Linking it to the parent directory
	// - Updating parent directory timestamps

	symlinkHandle, err := repository.CreateSymlink(dirHandle, req.Name, req.Target, symlinkAttr, authCtx)
	if err != nil {
		logger.Error("SYMLINK failed: repository error: name='%s' target='%s' client=%s error=%v",
			req.Name, req.Target, clientIP, err)

		// Get updated directory attributes for WCC data (best effort)
		var wccAfter *types.NFSFileAttr
		if updatedDirAttr, getErr := repository.GetFile(dirHandle); getErr == nil {
			dirID := xdr.ExtractFileID(dirHandle)
			wccAfter = xdr.MetadataToNFS(updatedDirAttr, dirID)
		}

		// Map repository errors to NFS status codes
		status := xdr.MapRepositoryErrorToNFSStatus(err, clientIP, "SYMLINK")

		return &SymlinkResponse{
			Status:        status,
			DirAttrBefore: wccBefore,
			DirAttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Retrieve symlink attributes for response
	// ========================================================================

	symlinkAttr, err = repository.GetFile(symlinkHandle)
	if err != nil {
		logger.Warn("SYMLINK: created but cannot get symlink attributes: handle=%x error=%v",
			symlinkHandle, err)

		// Get updated directory attributes for WCC data
		var wccAfter *types.NFSFileAttr
		if updatedDirAttr, getErr := repository.GetFile(dirHandle); getErr == nil {
			dirID := xdr.ExtractFileID(dirHandle)
			wccAfter = xdr.MetadataToNFS(updatedDirAttr, dirID)
		}

		// Return success with nil symlink attributes
		return &SymlinkResponse{
			Status:        types.NFS3OK,
			FileHandle:    symlinkHandle,
			Attr:          nil,
			DirAttrBefore: wccBefore,
			DirAttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 7: Build success response with attributes and WCC data
	// ========================================================================

	// Generate symlink attributes for response
	symlinkID := xdr.ExtractFileID(symlinkHandle)
	nfsSymlinkAttr := xdr.MetadataToNFS(symlinkAttr, symlinkID)

	// Get updated directory attributes for WCC data
	updatedDirAttr, err := repository.GetFile(dirHandle)
	if err != nil {
		logger.Warn("SYMLINK: successful but cannot get updated directory attributes: dir=%x error=%v",
			req.DirHandle, err)
		// Continue with nil WccAfter rather than failing
	}

	var wccAfter *types.NFSFileAttr
	if updatedDirAttr != nil {
		dirID := xdr.ExtractFileID(dirHandle)
		wccAfter = xdr.MetadataToNFS(updatedDirAttr, dirID)
	}

	logger.Info("SYMLINK successful: name='%s' target='%s' dir=%x handle=%x client=%s",
		req.Name, req.Target, req.DirHandle, symlinkHandle, clientIP)

	logger.Debug("SYMLINK details: symlink_id=%d mode=%o uid=%d gid=%d size=%d target_len=%d",
		symlinkID, symlinkAttr.Mode, symlinkAttr.UID, symlinkAttr.GID,
		symlinkAttr.Size, len(req.Target))

	return &SymlinkResponse{
		Status:        types.NFS3OK,
		FileHandle:    symlinkHandle,
		Attr:          nfsSymlinkAttr,
		DirAttrBefore: wccBefore,
		DirAttrAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// symlinkValidationError represents a SYMLINK request validation error.
type symlinkValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *symlinkValidationError) Error() string {
	return e.message
}

// validateSymlinkRequest validates SYMLINK request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Symlink name is valid (not empty, not "." or "..", length, characters)
//   - Target path is not empty and within reasonable length limits
//
// Returns:
//   - nil if valid
//   - *symlinkValidationError with NFS status if invalid
func validateSymlinkRequest(req *SymlinkRequest) *symlinkValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &symlinkValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate symlink name
	if req.Name == "" {
		return &symlinkValidationError{
			message:   "empty symlink name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &symlinkValidationError{
			message:   fmt.Sprintf("cannot create symlink named '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check symlink name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("symlink name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if strings.ContainsAny(req.Name, "\x00") {
		return &symlinkValidationError{
			message:   "symlink name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if strings.ContainsAny(req.Name, "/") {
		return &symlinkValidationError{
			message:   "symlink name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &symlinkValidationError{
				message:   fmt.Sprintf("symlink name contains control character at position %d", i),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	// Validate target path
	if req.Target == "" {
		return &symlinkValidationError{
			message:   "empty target path",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check target path length (POSIX PATH_MAX is typically 1024 bytes)
	// Some systems support longer paths, but 4096 is a reasonable upper limit
	if len(req.Target) > 4096 {
		return &symlinkValidationError{
			message:   fmt.Sprintf("target path too long: %d bytes (max 4096)", len(req.Target)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes in target path
	if strings.ContainsAny(req.Target, "\x00") {
		return &symlinkValidationError{
			message:   "target path contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeSymlinkRequest decodes a SYMLINK request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.10 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Symlink name length (4 bytes, big-endian uint32)
//  5. Symlink name data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//  7. Symlink attributes (sattr3 structure)
//  8. Target path length (4 bytes, big-endian uint32)
//  9. Target path data (variable length, up to 4096 bytes)
//  10. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the SYMLINK request
//
// Returns:
//   - *SymlinkRequest: The decoded request containing directory handle, name, target, and attributes
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded SYMLINK request from network
//	req, err := DecodeSymlinkRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Name, req.Target in SYMLINK procedure
func DecodeSymlinkRequest(data []byte) (*SymlinkRequest, error) {
	// Validate minimum data length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

	dirHandle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// ========================================================================
	// Decode symlink name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode symlink name: %w", err)
	}

	// ========================================================================
	// Decode symlink attributes
	// ========================================================================

	attr, err := xdr.DecodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}

	// ========================================================================
	// Decode target path
	// ========================================================================

	target, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode target path: %w", err)
	}

	logger.Debug("Decoded SYMLINK request: handle_len=%d name='%s' target='%s' target_len=%d",
		len(dirHandle), name, target, len(target))

	return &SymlinkRequest{
		DirHandle: dirHandle,
		Name:      name,
		Target:    target,
		Attr:      *attr,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the SymlinkResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.10 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op file handle (present flag + handle if present)
//     b. Post-op symlink attributes (present flag + attributes if present)
//  3. Directory WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
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
//	resp := &SymlinkResponse{
//	    Status:        types.NFS3OK,
//	    FileHandle:    symlinkHandle,
//	    Attr:          symlinkAttr,
//	    DirAttrBefore: wccBefore,
//	    DirAttrAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *SymlinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Success case: Write symlink handle and attributes
	// ========================================================================

	if resp.Status == types.NFS3OK {
		// Write post-op file handle (optional)
		if err := xdr.EncodeOptionalOpaque(&buf, resp.FileHandle); err != nil {
			return nil, fmt.Errorf("encode file handle: %w", err)
		}

		// Write post-op symlink attributes (optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("encode symlink attributes: %w", err)
		}
	}

	// ========================================================================
	// Write directory WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.

	if err := xdr.EncodeWccData(&buf, resp.DirAttrBefore, resp.DirAttrAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded SYMLINK response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

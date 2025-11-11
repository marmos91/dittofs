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

// RmdirRequest represents a RMDIR request from an NFS client.
// The client provides a parent directory handle and the name of the
// directory to remove.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.13 specifies the RMDIR procedure as:
//
//	RMDIR3res NFSPROC3_RMDIR(RMDIR3args) = 13;
//
// The RMDIR procedure removes (deletes) a subdirectory from a directory.
// The directory must be empty (contain no entries other than "." and "..").
type RmdirRequest struct {
	// DirHandle is the file handle of the parent directory containing
	// the directory to be removed.
	// Must be a valid directory handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Name is the name of the directory to remove within the parent directory.
	// Must follow NFS naming conventions:
	//   - Cannot be empty, ".", or ".."
	//   - Maximum length is 255 bytes per NFS specification
	//   - Should not contain null bytes or path separators (/)
	//   - Should not contain control characters
	Name string
}

// RmdirResponse represents the response to a RMDIR request.
// It contains the status of the operation and WCC (Weak Cache Consistency)
// data for the parent directory to help clients maintain cache coherency.
//
// The response is encoded in XDR format before being sent back to the client.
type RmdirResponse struct {
	// Status indicates the result of the rmdir operation.
	// Common values:
	//   - types.NFS3OK (0): Success - directory removed
	//   - types.NFS3ErrNoEnt (2): Directory or parent not found
	//   - types.NFS3ErrNotDir (20): Parent handle or target is not a directory
	//   - NFS3ErrNotEmpty (66): Directory is not empty
	//   - NFS3ErrAcces (13): Permission denied
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	//   - NFS3ErrInval (22): Invalid argument (bad name)
	//   - NFS3ErrNameTooLong (63): Name too long
	Status uint32

	// DirWccBefore contains pre-operation attributes of the parent directory.
	// Used for weak cache consistency to help clients detect if the parent
	// directory changed during the operation. May be nil.
	DirWccBefore *types.WccAttr

	// DirWccAfter contains post-operation attributes of the parent directory.
	// Used for weak cache consistency to provide the updated parent state.
	// May be nil on error, but should be present on success.
	DirWccAfter *types.NFSFileAttr
}

// RmdirContext contains the context information needed to process a RMDIR request.
// This includes client identification and authentication details for access control
// and auditing purposes.
type RmdirContext struct {
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

// Rmdir removes an empty directory from the filesystem.
//
// This implements the NFS RMDIR procedure as defined in RFC 1813 Section 3.3.13.
//
// **Purpose:**
//
// RMDIR is used to delete empty subdirectories from the filesystem. It provides
// a safe way to remove directories by ensuring they contain no files or
// subdirectories (other than the special "." and ".." entries).
//
// Common use cases:
//   - Cleaning up temporary or empty directories
//   - Removing obsolete directory structures
//   - Filesystem maintenance and organization
//
// **Process:**
//
//  1. Validate request parameters (handle format, name syntax)
//  2. Extract client IP and authentication credentials from context
//  3. Verify parent directory exists and is a directory (via repository)
//  4. Capture pre-operation parent state (for WCC)
//  5. Delegate directory removal to repository (includes validation and deletion)
//  6. Return status and WCC data for parent directory
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (removal, validation, access control) delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Directory Removal Requirements:**
//
// Per RFC 1813 and POSIX semantics:
//   - Directory must be empty (no entries except "." and "..")
//   - User must have write permission on parent directory
//   - Cannot remove "." (current directory)
//   - Cannot remove ".." (parent directory)
//   - Directory being removed must not be in use (no open file handles in some implementations)
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which can implement:
//   - Write permission checking on the parent directory
//   - Access control based on UID/GID
//   - Ownership verification (some systems require ownership for deletion)
//
// **Naming Restrictions:**
//
// Per RFC 1813 and POSIX conventions, the name to remove:
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
//   - Directory not found → types.NFS3ErrNoEnt
//   - Target not directory → types.NFS3ErrNotDir
//   - Directory not empty → NFS3ErrNotEmpty
//   - Invalid name → NFS3ErrInval
//   - Name too long → NFS3ErrNameTooLong
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients detect if the parent directory changed between
// operations and maintain cache consistency. The server:
//  1. Captures parent attributes before the operation (WccBefore)
//  2. Performs the directory removal
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
//   - Access control prevents unauthorized directory removal
//
// **Parameters:**
//   - repository: The metadata repository for directory operations
//   - req: The rmdir request containing parent handle and directory name
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *RmdirResponse: Response with status and WCC data for the parent directory
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.13: RMDIR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &RmdirRequest{
//	    DirHandle: parentHandle,
//	    Name:      "temp",
//	}
//	ctx := &RmdirContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Rmdir(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Directory removed successfully
//	}
func (h *DefaultNFSHandler) Rmdir(
	ctx *RmdirContext,
	repository metadata.Repository,
	req *RmdirRequest,
) (*RmdirResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("RMDIR: name='%s' dir=%x client=%s auth=%d",
		req.Name, req.DirHandle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateRmdirRequest(req); err != nil {
		logger.Warn("RMDIR validation failed: name='%s' client=%s error=%v",
			req.Name, clientIP, err)
		return &RmdirResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify parent directory exists and is valid
	// ========================================================================

	parentHandle := metadata.FileHandle(req.DirHandle)
	parentAttr, err := repository.GetFile(ctx.Context, parentHandle)
	if err != nil {
		logger.Warn("RMDIR failed: parent not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &RmdirResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(parentAttr)

	// Verify parent is actually a directory
	if parentAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("RMDIR failed: parent not a directory: dir=%x type=%d client=%s",
			req.DirHandle, parentAttr.Type, clientIP)

		// Get current parent state for WCC
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		return &RmdirResponse{
			Status:       types.NFS3ErrNotDir,
			DirWccBefore: wccBefore,
			DirWccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 3: Remove directory via repository
	// ========================================================================
	// The repository is responsible for:
	// - Verifying the directory exists
	// - Verifying it's actually a directory
	// - Checking it's empty (no entries except "." and "..")
	// - Checking write permission on parent directory
	// - Removing the directory entry from parent
	// - Deleting the directory metadata
	// - Updating parent directory timestamps

	// Build authentication context for repository
	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	// Delegate to repository for directory removal
	err = repository.RemoveDirectory(authCtx, parentHandle, req.Name)
	if err != nil {
		logger.Error("RMDIR failed: repository error: name='%s' client=%s error=%v",
			req.Name, clientIP, err)

		// Get updated parent attributes for WCC data
		parentAttr, _ = repository.GetFile(ctx.Context, parentHandle)
		dirID := xdr.ExtractFileID(parentHandle)
		wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

		// Map repository errors to NFS status codes
		status := mapRmdirErrorToNFSStatus(err)

		return &RmdirResponse{
			Status:       status,
			DirWccBefore: wccBefore,
			DirWccAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Build success response with updated parent attributes
	// ========================================================================

	// Get updated parent directory attributes
	parentAttr, _ = repository.GetFile(ctx.Context, parentHandle)
	dirID := xdr.ExtractFileID(parentHandle)
	wccAfter := xdr.MetadataToNFS(parentAttr, dirID)

	logger.Info("RMDIR successful: name='%s' dir=%x client=%s",
		req.Name, req.DirHandle, clientIP)

	logger.Debug("RMDIR details: parent_id=%d", dirID)

	return &RmdirResponse{
		Status:       types.NFS3OK,
		DirWccBefore: wccBefore,
		DirWccAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// rmdirValidationError represents a RMDIR request validation error.
type rmdirValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *rmdirValidationError) Error() string {
	return e.message
}

// validateRmdirRequest validates RMDIR request parameters.
//
// Checks performed:
//   - Parent directory handle is not empty and within limits
//   - Directory name is valid (not empty, not "." or "..", length, characters)
//
// Returns:
//   - nil if valid
//   - *rmdirValidationError with NFS status if invalid
func validateRmdirRequest(req *RmdirRequest) *rmdirValidationError {
	// Validate parent directory handle
	if len(req.DirHandle) == 0 {
		return &rmdirValidationError{
			message:   "empty parent directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.DirHandle) > 64 {
		return &rmdirValidationError{
			message:   fmt.Sprintf("parent handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &rmdirValidationError{
			message:   fmt.Sprintf("parent handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate directory name
	if req.Name == "" {
		return &rmdirValidationError{
			message:   "empty directory name",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for reserved names
	if req.Name == "." || req.Name == ".." {
		return &rmdirValidationError{
			message:   fmt.Sprintf("directory name cannot be '%s'", req.Name),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check name length (NFS limit is typically 255 bytes)
	if len(req.Name) > 255 {
		return &rmdirValidationError{
			message:   fmt.Sprintf("directory name too long: %d bytes (max 255)", len(req.Name)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if strings.ContainsAny(req.Name, "\x00") {
		return &rmdirValidationError{
			message:   "directory name contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	if strings.ContainsAny(req.Name, "/") {
		return &rmdirValidationError{
			message:   "directory name contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for control characters (including tab, newline, etc.)
	// This prevents potential issues with terminal output and logs
	for i, r := range req.Name {
		if r < 0x20 || r == 0x7F {
			return &rmdirValidationError{
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

// DecodeRmdirRequest decodes a RMDIR request from XDR-encoded bytes.
//
// The RMDIR request has the following XDR structure (RFC 1813 Section 3.3.13):
//
//	struct RMDIR3args {
//	    diropargs3   object;  // Parent dir handle + name
//	};
//
// Decoding process:
//  1. Read parent directory handle (variable length with padding)
//  2. Read directory name (variable length string with padding)
//
// XDR encoding details:
//   - All integers are 4-byte aligned (32-bit)
//   - Variable-length data (handles, strings) are length-prefixed
//   - Padding is added to maintain 4-byte alignment
//
// Parameters:
//   - data: XDR-encoded bytes containing the rmdir request
//
// Returns:
//   - *RmdirRequest: The decoded request
//   - error: Decoding error if data is malformed or incomplete
//
// Example:
//
//	data := []byte{...} // XDR-encoded RMDIR request from network
//	req, err := DecodeRmdirRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle, req.Name in RMDIR procedure
func DecodeRmdirRequest(data []byte) (*RmdirRequest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("data too short: need at least 8 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode parent directory handle
	// ========================================================================

	handle, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory handle: %w", err)
	}

	// ========================================================================
	// Decode directory name
	// ========================================================================

	name, err := xdr.DecodeString(reader)
	if err != nil {
		return nil, fmt.Errorf("decode directory name: %w", err)
	}

	logger.Debug("Decoded RMDIR request: handle_len=%d name='%s'",
		len(handle), name)

	return &RmdirRequest{
		DirHandle: handle,
		Name:      name,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the RmdirResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The RMDIR response has the following XDR structure (RFC 1813 Section 3.3.13):
//
//	struct RMDIR3res {
//	    nfsstat3    status;
//	    union switch (status) {
//	    case NFS3_OK:
//	        wcc_data    dir_wcc;
//	    default:
//	        wcc_data    dir_wcc;
//	    } resfail;
//	};
//
// Encoding process:
//  1. Write status code (4 bytes)
//  2. Write WCC data for parent directory (both success and failure)
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
//	resp := &RmdirResponse{
//	    Status:       types.NFS3OK,
//	    DirWccBefore: wccBefore,
//	    DirWccAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *RmdirResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write WCC data for parent directory (both success and failure)
	// ========================================================================

	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the parent directory.
	if err := xdr.EncodeWccData(&buf, resp.DirWccBefore, resp.DirWccAfter); err != nil {
		return nil, fmt.Errorf("encode directory wcc data: %w", err)
	}

	logger.Debug("Encoded RMDIR response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

// ============================================================================
// Error Mapping
// ============================================================================

// mapRmdirErrorToNFSStatus maps repository errors to NFS status codes.
// This provides consistent error mapping for RMDIR operations.
func mapRmdirErrorToNFSStatus(err error) uint32 {
	// Check for specific error types from repository
	if exportErr, ok := err.(*metadata.ExportError); ok {
		switch exportErr.Code {
		case metadata.ExportErrAccessDenied:
			return types.NFS3ErrAcces
		case metadata.ExportErrNotFound:
			return types.NFS3ErrNoEnt
		default:
			return types.NFS3ErrIO
		}
	}

	// Check for specific error messages that indicate specific conditions
	errMsg := err.Error()

	// Directory not empty
	if bytes.Contains([]byte(errMsg), []byte("not empty")) ||
		bytes.Contains([]byte(errMsg), []byte("has children")) {
		return types.NFS3ErrNotEmpty
	}

	// Not a directory
	if bytes.Contains([]byte(errMsg), []byte("not a directory")) {
		return types.NFS3ErrNotDir
	}

	// Default to I/O error for unknown errors
	return types.NFS3ErrIO
}

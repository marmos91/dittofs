package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// SetAttrRequest represents a SETATTR request from an NFS client.
// The client provides a file handle and a set of attributes to modify.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.2 specifies the SETATTR procedure as:
//
//	SETATTR3res NFSPROC3_SETATTR(SETATTR3args) = 2;
//
// The SETATTR procedure changes one or more attributes of a file system object.
// It is one of the fundamental operations for file management, allowing changes
// to permissions, ownership, size, and timestamps.
type SetAttrRequest struct {
	// Handle is the file handle of the object whose attributes should be changed.
	// Must be a valid file handle obtained from MOUNT, LOOKUP, CREATE, etc.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// NewAttr contains the attributes to set.
	// Only the attributes with their corresponding Set* flags set to true
	// will be modified. Other attributes remain unchanged.
	NewAttr metadata.SetAttrs

	// Guard provides a conditional update mechanism based on ctime.
	// If Guard.Check is true, the server only proceeds with the update
	// if the current ctime matches Guard.Time. This prevents lost updates
	// when multiple clients modify the same file.
	//
	// The guard mechanism implements optimistic concurrency control:
	//   - Client reads file attributes (including ctime)
	//   - Client performs local operations
	//   - Client sends SETATTR with ctime guard
	//   - Server checks if ctime has changed
	//   - If unchanged: Apply updates (success)
	//   - If changed: Reject updates (NFS3ErrNotSync)
	Guard TimeGuard
}

// SetAttrResponse represents the response to a SETATTR request.
// It contains the status of the operation and WCC (Weak Cache Consistency)
// data for the modified object.
//
// The response is encoded in XDR format before being sent back to the client.
type SetAttrResponse struct {
	// Status indicates the result of the setattr operation.
	// Common values:
	//   - NFS3OK (0): Success
	//   - NFS3ErrPerm (1): Not owner (for ownership changes)
	//   - NFS3ErrNoEnt (2): File not found
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrRoFs (30): Read-only file system
	//   - NFS3ErrNotSync (10002): Guard check failed
	//   - NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - NFS3ErrBadHandle (10001): Invalid file handle
	//   - NFS3ErrInval (22): Invalid argument (e.g., invalid size)
	Status uint32

	// AttrBefore contains pre-operation weak cache consistency data.
	// Used for cache consistency to help clients detect changes.
	// May be nil if attributes could not be captured.
	AttrBefore *WccAttr

	// AttrAfter contains post-operation attributes of the object.
	// Used for cache consistency to provide updated object state.
	// May be nil on error, but should be present on success.
	AttrAfter *FileAttr
}

// SetAttrContext contains the context information needed to process a SETATTR request.
// This includes client identification and authentication details for access control.
type SetAttrContext struct {
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
	// Used for checking if user belongs to file's group.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// SetAttr sets the attributes of a file system object.
//
// This implements the NFS SETATTR procedure as defined in RFC 1813 Section 3.3.2.
//
// **Purpose:**
//
// SETATTR changes one or more attributes of a file system object. This includes:
//   - Permissions (mode)
//   - Ownership (UID/GID)
//   - File size (truncation/extension)
//   - Access time (atime)
//   - Modification time (mtime)
//
// Common use cases:
//   - chmod: Change file permissions
//   - chown: Change file ownership
//   - truncate: Change file size
//   - touch: Update timestamps
//   - utimes: Set specific timestamps
//
// **Process:**
//
//  1. Validate request parameters (handle format)
//  2. Extract client IP and authentication credentials from context
//  3. Verify file handle exists (via repository)
//  4. Check guard condition if specified (optimistic concurrency control)
//  5. Delegate attribute updates to repository.SetFileAttributes()
//  6. Return updated WCC data
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (attribute modification, validation, access control) delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which implements:
//   - Ownership checks (only owner or root can chown)
//   - Permission checks (owner can chmod, write permission for size/time)
//   - Access control based on UID/GID
//   - Read-only filesystem checks
//
// **Guard Mechanism:**
//
// The guard provides optimistic concurrency control:
//  1. Client reads file attributes including ctime
//  2. Client performs local operations
//  3. Client sends SETATTR with ctime guard
//  4. Server checks if ctime changed (another client modified file)
//  5. If unchanged: Apply updates (success)
//  6. If changed: Reject updates (NFS3ErrNotSync - client should retry)
//
// This prevents lost updates when multiple clients modify the same file.
//
// **Attribute Selection:**
//
// Attributes are selectively updated based on Set* flags in NewAttr:
//   - SetMode: Update permission bits
//   - SetUID: Update owner user ID
//   - SetGID: Update owner group ID
//   - SetSize: Truncate or extend file
//   - SetAtime: Update access time
//   - SetMtime: Update modification time
//
// Unset attributes remain unchanged. This allows atomic multi-attribute
// updates without race conditions.
//
// **Size Changes:**
//
// Size changes require coordination with the content repository:
//   - Truncation (new size < old size): Remove trailing content
//   - Extension (new size > old size): Pad with zeros
//   - Zero size: Delete all content
//
// The metadata repository delegates content operations to the content repository.
//
// **Timestamp Semantics:**
//
// Per RFC 1813:
//   - SetAtime: Set to specified time or SET_TO_SERVER_TIME (current time)
//   - SetMtime: Set to specified time or SET_TO_SERVER_TIME (current time)
//   - Ctime: Always set to current time when any attribute changes
//
// Ctime (change time) is automatically updated by the server and cannot
// be set by the client - it's a server-managed metadata field.
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - File not found → NFS3ErrNoEnt
//   - Guard check failed → NFS3ErrNotSync
//   - Permission denied → NFS3ErrAcces
//   - Not owner → NFS3ErrPerm
//   - Read-only filesystem → NFS3ErrRoFs
//   - Invalid size → NFS3ErrInval
//   - I/O error → NFS3ErrIO
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps NFS clients maintain cache coherency:
//  1. Capture file attributes before the operation (WccBefore)
//  2. Perform the attribute updates
//  3. Capture file attributes after the operation (WccAfter)
//
// Clients use this to:
//   - Detect if file changed during the operation
//   - Update their cached file attributes
//   - Invalidate stale cached data
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository enforces ownership and permission checks
//   - Only owner or root can change ownership
//   - Only owner can change permissions
//   - Size/time changes require write permission
//   - Guard prevents lost updates in concurrent scenarios
//   - Client context enables audit logging
//
// **Parameters:**
//   - repository: The metadata repository for file operations
//   - req: The setattr request containing handle and new attributes
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *SetAttrResponse: Response with status and WCC data
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.2: SETATTR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &SetAttrRequest{
//	    Handle: fileHandle,
//	    NewAttr: SetAttrs{
//	        SetMode: true,
//	        Mode:    0644, // rw-r--r--
//	    },
//	    Guard: TimeGuard{Check: false}, // No guard
//	}
//	ctx := &SetAttrContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.SetAttr(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == NFS3OK {
//	    // Attributes updated successfully
//	}
func (h *DefaultNFSHandler) SetAttr(
	repository metadata.Repository,
	req *SetAttrRequest,
	ctx *SetAttrContext,
) (*SetAttrResponse, error) {
	// Extract client IP for logging
	clientIP := extractClientIP(ctx.ClientAddr)

	logger.Info("SETATTR: handle=%x guard=%v client=%s auth=%d",
		req.Handle, req.Guard.Check, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateSetAttrRequest(req); err != nil {
		logger.Warn("SETATTR validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &SetAttrResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Get current file attributes for WCC and guard check
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	currentAttr, err := repository.GetFile(fileHandle)
	if err != nil {
		logger.Warn("SETATTR failed: file not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &SetAttrResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := captureWccAttr(currentAttr)

	// ========================================================================
	// Step 3: Check guard condition if specified
	// ========================================================================
	// The guard implements optimistic concurrency control by checking if
	// the file's ctime has changed since the client last read it.
	// If it has changed, another client has modified the file and this
	// operation should be rejected to prevent lost updates.

	if req.Guard.Check {
		currentCtime := timeToTimeVal(currentAttr.Ctime)

		// Compare ctime from guard with current ctime
		if currentCtime.Seconds != req.Guard.Time.Seconds ||
			currentCtime.Nseconds != req.Guard.Time.Nseconds {
			logger.Debug("SETATTR guard check failed: handle=%x expected=%d.%d got=%d.%d client=%s",
				req.Handle,
				req.Guard.Time.Seconds, req.Guard.Time.Nseconds,
				currentCtime.Seconds, currentCtime.Nseconds,
				clientIP)

			// Get updated attributes for WCC data (best effort)
			var wccAfter *FileAttr
			if attr, err := repository.GetFile(fileHandle); err == nil {
				fileID := extractFileID(fileHandle)
				wccAfter = MetadataToNFSAttr(attr, fileID)
			}

			return &SetAttrResponse{
				Status:     NFS3ErrNotSync,
				AttrBefore: wccBefore,
				AttrAfter:  wccAfter,
			}, nil
		}

		logger.Debug("SETATTR guard check passed: handle=%x ctime=%d.%d",
			req.Handle, currentCtime.Seconds, currentCtime.Nseconds)
	}

	// ========================================================================
	// Step 4: Build authentication context for repository
	// ========================================================================

	authCtx := &metadata.AuthContext{
		AuthFlavor: ctx.AuthFlavor,
		UID:        ctx.UID,
		GID:        ctx.GID,
		GIDs:       ctx.GIDs,
		ClientAddr: clientIP,
	}

	// ========================================================================
	// Step 5: Apply attribute updates via repository
	// ========================================================================
	// The repository is responsible for:
	// - Checking ownership (for chown/chmod)
	// - Checking write permission (for size/time changes)
	// - Validating attribute values (e.g., invalid size)
	// - Coordinating with content repository for size changes
	// - Updating ctime automatically
	// - Ensuring atomicity of updates

	// Log which attributes are being set (for debugging)
	logSetAttrRequest(req, clientIP)

	err = repository.SetFileAttributes(fileHandle, &req.NewAttr, authCtx)
	if err != nil {
		logger.Error("SETATTR failed: repository error: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)

		// Get updated attributes for WCC data (best effort)
		var wccAfter *FileAttr
		if attr, getErr := repository.GetFile(fileHandle); getErr == nil {
			fileID := extractFileID(fileHandle)
			wccAfter = MetadataToNFSAttr(attr, fileID)
		}

		// Map repository errors to NFS status codes
		status := mapRepositoryErrorToNFSStatus(err, clientIP, "SETATTR")

		return &SetAttrResponse{
			Status:     status,
			AttrBefore: wccBefore,
			AttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 6: Build success response with updated attributes
	// ========================================================================

	// Get updated file attributes for WCC data
	updatedAttr, err := repository.GetFile(fileHandle)
	if err != nil {
		logger.Warn("SETATTR: attributes updated but cannot get new attributes: handle=%x error=%v",
			req.Handle, err)
		// Continue with nil WccAfter rather than failing the entire operation
	}

	var wccAfter *FileAttr
	if updatedAttr != nil {
		fileID := extractFileID(fileHandle)
		wccAfter = MetadataToNFSAttr(updatedAttr, fileID)
	}

	logger.Info("SETATTR successful: handle=%x client=%s", req.Handle, clientIP)

	logger.Debug("SETATTR details: old_size=%d new_size=%d old_mode=%o new_mode=%o",
		currentAttr.Size, updatedAttr.Size, currentAttr.Mode, updatedAttr.Mode)

	return &SetAttrResponse{
		Status:     NFS3OK,
		AttrBefore: wccBefore,
		AttrAfter:  wccAfter,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// setAttrValidationError represents a SETATTR request validation error.
type setAttrValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *setAttrValidationError) Error() string {
	return e.message
}

// validateSetAttrRequest validates SETATTR request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - At least one attribute is being set
//   - Size value is valid (if being set)
//   - Mode value is valid (if being set)
//
// Returns:
//   - nil if valid
//   - *setAttrValidationError with NFS status if invalid
func validateSetAttrRequest(req *SetAttrRequest) *setAttrValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &setAttrValidationError{
			message:   "empty file handle",
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	if len(req.Handle) > 64 {
		return &setAttrValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &setAttrValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// Check that at least one attribute is being set
	if !req.NewAttr.SetMode && !req.NewAttr.SetUID && !req.NewAttr.SetGID &&
		!req.NewAttr.SetSize && !req.NewAttr.SetAtime && !req.NewAttr.SetMtime {
		return &setAttrValidationError{
			message:   "no attributes specified for update",
			nfsStatus: NFS3ErrInval,
		}
	}

	// Validate mode value if being set (only use lower 12 bits)
	if req.NewAttr.SetMode {
		// Mode should only use file type and permission bits (lower 12 bits)
		// The upper bits are reserved and should be zero
		if req.NewAttr.Mode > 0o7777 {
			return &setAttrValidationError{
				message:   fmt.Sprintf("invalid mode value: 0%o (max 0o7777)", req.NewAttr.Mode),
				nfsStatus: NFS3ErrInval,
			}
		}
	}

	// Size validation is done by the repository as it depends on file type
	// (e.g., cannot set size on directories)

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeSetAttrRequest decodes a SETATTR request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.2 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. New attributes (sattr3 structure):
//     - Mode (present flag + value if present)
//     - UID (present flag + value if present)
//     - GID (present flag + value if present)
//     - Size (present flag + value if present)
//     - Atime (how flag + value if SET_TO_CLIENT_TIME)
//     - Mtime (how flag + value if SET_TO_CLIENT_TIME)
//  5. Guard (sattrguard3 structure):
//     - Check flag (4 bytes, 0 or 1)
//     - If check==1: ctime (8 bytes: 4 for seconds, 4 for nanoseconds)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the SETATTR request
//
// Returns:
//   - *SetAttrRequest: The decoded request
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded SETATTR request from network
//	req, err := DecodeSetAttrRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.NewAttr, req.Guard in SETATTR procedure
func DecodeSetAttrRequest(data []byte) (*SetAttrRequest, error) {
	// Validate minimum data length
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode file handle
	// ========================================================================

	handle, err := decodeOpaque(reader)
	if err != nil {
		return nil, fmt.Errorf("decode handle: %w", err)
	}

	// ========================================================================
	// Decode new attributes (sattr3)
	// ========================================================================

	newAttr, err := decodeSetAttrs(reader)
	if err != nil {
		return nil, fmt.Errorf("decode attributes: %w", err)
	}

	// ========================================================================
	// Decode guard (sattrguard3)
	// ========================================================================

	guard := TimeGuard{}

	// Read guard check flag (4 bytes, 0 or 1)
	var guardCheck uint32
	if err := binary.Read(reader, binary.BigEndian, &guardCheck); err != nil {
		return nil, fmt.Errorf("decode guard check: %w", err)
	}

	guard.Check = (guardCheck == 1)

	// If check is enabled, read the expected ctime
	if guard.Check {
		// Read seconds (4 bytes)
		if err := binary.Read(reader, binary.BigEndian, &guard.Time.Seconds); err != nil {
			return nil, fmt.Errorf("decode guard time seconds: %w", err)
		}

		// Read nanoseconds (4 bytes)
		if err := binary.Read(reader, binary.BigEndian, &guard.Time.Nseconds); err != nil {
			return nil, fmt.Errorf("decode guard time nseconds: %w", err)
		}

		logger.Debug("Decoded SETATTR guard: check=true ctime=%d.%d",
			guard.Time.Seconds, guard.Time.Nseconds)
	} else {
		logger.Debug("Decoded SETATTR guard: check=false")
	}

	logger.Debug("Decoded SETATTR request: handle_len=%d guard=%v", len(handle), guard.Check)

	return &SetAttrRequest{
		Handle:  handle,
		NewAttr: *newAttr,
		Guard:   guard,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the SetAttrResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.2 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
//
// WCC data is included for both success and failure cases to help
// clients maintain cache consistency.
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
//	resp := &SetAttrResponse{
//	    Status:     NFS3OK,
//	    AttrBefore: wccBefore,
//	    AttrAfter:  wccAfter,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *SetAttrResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("write status: %w", err)
	}

	// ========================================================================
	// Write WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the file.

	if err := encodeWccData(&buf, resp.AttrBefore, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("encode wcc data: %w", err)
	}

	logger.Debug("Encoded SETATTR response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

// ============================================================================
// Utility Functions
// ============================================================================

// logSetAttrRequest logs which attributes are being set in a SETATTR request.
// This provides detailed debugging information about the operation.
func logSetAttrRequest(req *SetAttrRequest, clientIP string) {
	attrs := make([]string, 0, 6)

	if req.NewAttr.SetMode {
		attrs = append(attrs, fmt.Sprintf("mode=0%o", req.NewAttr.Mode))
	}
	if req.NewAttr.SetUID {
		attrs = append(attrs, fmt.Sprintf("uid=%d", req.NewAttr.UID))
	}
	if req.NewAttr.SetGID {
		attrs = append(attrs, fmt.Sprintf("gid=%d", req.NewAttr.GID))
	}
	if req.NewAttr.SetSize {
		attrs = append(attrs, fmt.Sprintf("size=%d", req.NewAttr.Size))
	}
	if req.NewAttr.SetAtime {
		attrs = append(attrs, fmt.Sprintf("atime=%v", req.NewAttr.Atime))
	}
	if req.NewAttr.SetMtime {
		attrs = append(attrs, fmt.Sprintf("mtime=%v", req.NewAttr.Mtime))
	}

	if len(attrs) > 0 {
		logger.Debug("SETATTR attributes: %v client=%s", attrs, clientIP)
	}
}

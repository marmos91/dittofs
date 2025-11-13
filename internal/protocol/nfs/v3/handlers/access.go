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

// AccessRequest represents an ACCESS request from an NFS client.
// The client wants to verify if it has specific permissions on a file or directory.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.4 specifies the ACCESS procedure as:
//
//	ACCESS3res NFSPROC3_ACCESS(ACCESS3args) = 4;
//
// The ACCESS procedure is used by clients to check permissions before
// attempting operations, avoiding the overhead of failed operations.
type AccessRequest struct {
	// Handle is the file handle of the object to check permissions for.
	// Must be a valid file handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Access is a bitmap of requested access permissions.
	// The client specifies which permissions it wants to check using the
	// Access* constants (AccessRead, AccessLookup, AccessModify, etc.).
	// Multiple permissions can be combined with bitwise OR.
	// Example: AccessRead | AccessExecute checks for read and execute.
	Access uint32
}

// AccessResponse represents the response to an ACCESS request.
// It contains the status of the check and, if successful, which permissions
// were granted and optional post-operation attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type AccessResponse struct {
	// Status indicates the result of the access check.
	// Common values:
	//   - types.NFS3OK (0): Success - check Access field for granted permissions
	//   - types.NFS3ErrNoEnt (2): File handle not found
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrAcces (13): Permission denied (rare - usually returns with Access=0)
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// Attr contains the post-operation attributes for the file handle.
	// This is optional and may be nil.
	// Including attributes helps clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Access is a bitmap of granted access permissions.
	// Only present when Status == types.NFS3OK.
	// This is a subset (or equal to) the requested permissions.
	// A bit set to 1 means that permission is granted.
	// A bit set to 0 means that permission is denied or unknown.
	// The client should check each bit individually.
	Access uint32
}

// AccessContext contains the context information needed to process an ACCESS request.
// This includes client identification, authentication details for permission checks,
// and cancellation handling.
type AccessContext struct {
	// Context carries cancellation signals and deadlines
	// The Access handler checks this context to abort operations if the client
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
	// Used for permission checking against file ownership and mode bits.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for permission checking against file group ownership and mode bits.
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

// Access checks access permissions for a file system object.
//
// This implements the NFS ACCESS procedure as defined in RFC 1813 Section 3.3.4.
//
// **Purpose:**
//
// ACCESS allows clients to check permissions efficiently before attempting
// operations. This is more efficient than trying an operation and handling errors.
// It's particularly useful for implementing UI elements (graying out unavailable
// actions) and for optimizing batch operations.
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format and length)
//  3. Extract client IP and authentication credentials from context
//  4. Verify file handle exists via store.GetFile()
//  5. Check for cancellation before expensive permission check
//  6. Delegate permission checking to store.CheckPermissions()
//  7. Retrieve file attributes for cache consistency
//  8. Return granted permissions bitmap to client
//
// **Context cancellation:**
//
//   - Checks at the beginning to respect client disconnection
//   - Checks after GetFile, before the potentially expensive CheckPermissions
//   - No check after CheckPermissions since response building is fast
//   - Returns NFS3ErrIO status with context error for cancellation
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (permission checking) is delegated to store
//   - File handle validation is performed by store.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer extracts and passes these to the store, which
// implements the actual permission checking logic based on:
//   - File ownership (UID/GID)
//   - File mode bits (rwx permissions)
//   - ACLs or other access control mechanisms (implementation-specific)
//
// **Permission Bitmap:**
//
// The Access field uses these bits (can be combined with bitwise OR):
//   - AccessRead (0x0001): Read file data or list directory
//   - AccessLookup (0x0002): Look up names in directory
//   - AccessModify (0x0004): Modify file data
//   - AccessExtend (0x0008): Extend file (write beyond EOF)
//   - AccessDelete (0x0010): Delete file or directory
//   - AccessExecute (0x0020): Execute file or search directory
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// The procedure rarely returns errors - instead, it returns success
// with Access=0 (no permissions granted) when access would be denied.
//
// Store errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Permission denied → types.NFS3OK with Access=0 (or NFS3ErrAcces)
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Store layer enforces actual access control
//   - Client context enables audit logging
//   - No sensitive information leaked in error messages
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication credentials
//   - store: The metadata store for file access and permission checks
//   - req: The access request containing handle and requested permissions
//
// **Returns:**
//   - *AccessResponse: Response with status and granted permissions (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.4: ACCESS Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &AccessRequest{
//	    Handle: fileHandle,
//	    Access: AccessRead | AccessExecute,
//	}
//	ctx := &AccessContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Access(ctx, store, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    if resp.Access & AccessRead != 0 {
//	        // Client has read permission
//	    }
//	}
func (h *DefaultNFSHandler) Access(
	ctx *AccessContext,
	store metadata.MetadataStore,
	req *AccessRequest,
) (*AccessResponse, error) {
	// Check for cancellation before starting any work
	// This handles the case where the client disconnects before we begin processing
	select {
	case <-ctx.Context.Done():
		logger.Debug("ACCESS cancelled before processing: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, ctx.Context.Err())
		return &AccessResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("ACCESS: handle=%x requested=0x%x client=%s auth=%d",
		req.Handle, req.Access, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateAccessRequest(req); err != nil {
		logger.Warn("ACCESS validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &AccessResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify file handle exists and is valid
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	attr, err := store.GetFile(ctx.Context, fileHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("ACCESS cancelled during file lookup: handle=%x client=%s error=%v",
				req.Handle, clientIP, ctx.Context.Err())
			return &AccessResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Debug("ACCESS failed: handle not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &AccessResponse{Status: types.NFS3ErrStale}, nil
	}

	// Check for cancellation before the permission check
	// CheckPermissions may involve complex ACL evaluation, so it's worth checking here
	select {
	case <-ctx.Context.Done():
		logger.Debug("ACCESS cancelled before permission check: handle=%x client=%s error=%v",
			req.Handle, clientIP, ctx.Context.Err())
		return &AccessResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// ========================================================================
	// Step 3: Build AuthContext for permission checking
	// ========================================================================

	authCtx := buildAuthContext(ctx)

	// ========================================================================
	// Step 4: Translate NFS access bits to generic permissions
	// ========================================================================

	requestedPerms := nfsAccessToPermissions(req.Access, attr.Type)

	logger.Debug("ACCESS translation: nfs_access=0x%x -> generic_perms=0x%x type=%d",
		req.Access, requestedPerms, attr.Type)

	// ========================================================================
	// Step 5: Check permissions via store
	// ========================================================================

	grantedPerms, err := store.CheckPermissions(authCtx, fileHandle, requestedPerms)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("ACCESS cancelled during permission check: handle=%x client=%s error=%v",
				req.Handle, clientIP, ctx.Context.Err())
			return &AccessResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Error("ACCESS failed: permission check error: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &AccessResponse{Status: types.NFS3ErrIO}, nil
	}

	// ========================================================================
	// Step 6: Translate granted permissions back to NFS access bits
	// ========================================================================

	grantedAccess := permissionsToNFSAccess(grantedPerms, attr.Type)

	logger.Debug("ACCESS translation: generic_perms=0x%x -> nfs_access=0x%x",
		grantedPerms, grantedAccess)

	// ========================================================================
	// Step 7: Build response with granted permissions and attributes
	// ========================================================================

	// Generate file ID from handle for NFS attributes
	fileid := xdr.ExtractFileID(fileHandle)
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("ACCESS successful: handle=%x granted=0x%x requested=0x%x client=%s",
		req.Handle, grantedAccess, req.Access, clientIP)

	logger.Debug("ACCESS details: type=%d mode=%o uid=%d gid=%d client_uid=%v client_gid=%v",
		attr.Type, attr.Mode, attr.UID, attr.GID, ctx.UID, ctx.GID)

	return &AccessResponse{
		Status: types.NFS3OK,
		Attr:   nfsAttr,
		Access: grantedAccess,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// buildAuthContext creates an AuthContext from AccessContext.
//
// This translates the NFS-specific authentication context into the generic
// AuthContext used by the metadata store.
func buildAuthContext(ctx *AccessContext) *metadata.AuthContext {
	// Map auth flavor to auth method string
	authMethod := "anonymous"
	if ctx.AuthFlavor == 1 {
		authMethod = "unix"
	}

	// Build identity from Unix credentials
	identity := &metadata.Identity{
		UID:  ctx.UID,
		GID:  ctx.GID,
		GIDs: ctx.GIDs,
	}

	// Set username from UID if available (for logging/auditing)
	if ctx.UID != nil {
		identity.Username = fmt.Sprintf("uid:%d", *ctx.UID)
	}

	return &metadata.AuthContext{
		Context:    ctx.Context,
		AuthMethod: authMethod,
		Identity:   identity,
		ClientAddr: ctx.ClientAddr,
	}
}

// nfsAccessToPermissions translates NFS ACCESS bits to generic Permission flags.
//
// NFS ACCESS bits (RFC 1813 Section 3.3.4):
//   - AccessRead (0x0001): Read file data or list directory
//   - AccessLookup (0x0002): Look up names in directory
//   - AccessModify (0x0004): Modify file data
//   - AccessExtend (0x0008): Extend file (write beyond EOF)
//   - AccessDelete (0x0010): Delete file or directory
//   - AccessExecute (0x0020): Execute file or search directory
//
// Generic Permission flags:
//   - PermissionRead: Read file data
//   - PermissionWrite: Modify file data
//   - PermissionExecute: Execute files
//   - PermissionDelete: Delete files/directories
//   - PermissionListDirectory: List directory contents (read for directories)
//   - PermissionTraverse: Search/traverse directories (execute for directories)
//
// The translation is context-sensitive based on file type:
//   - For files: AccessRead -> PermissionRead, AccessModify/AccessExtend -> PermissionWrite
//   - For directories: AccessRead -> PermissionListDirectory, AccessLookup -> PermissionTraverse
func nfsAccessToPermissions(nfsAccess uint32, fileType metadata.FileType) metadata.Permission {
	var perms metadata.Permission

	// Handle directories specially
	if fileType == metadata.FileTypeDirectory {
		// AccessRead for directories means list contents
		if nfsAccess&types.AccessRead != 0 {
			perms |= metadata.PermissionListDirectory
		}
		// AccessLookup means search/traverse
		if nfsAccess&types.AccessLookup != 0 {
			perms |= metadata.PermissionTraverse
		}
		// AccessExecute also means traverse for directories
		if nfsAccess&types.AccessExecute != 0 {
			perms |= metadata.PermissionTraverse
		}
		// AccessModify and AccessExtend mean write (add/remove entries)
		if nfsAccess&(types.AccessModify|types.AccessExtend) != 0 {
			perms |= metadata.PermissionWrite
		}
	} else {
		// For files: straightforward mapping
		if nfsAccess&types.AccessRead != 0 {
			perms |= metadata.PermissionRead
		}
		if nfsAccess&(types.AccessModify|types.AccessExtend) != 0 {
			perms |= metadata.PermissionWrite
		}
		if nfsAccess&types.AccessExecute != 0 {
			perms |= metadata.PermissionExecute
		}
	}

	// AccessDelete maps directly
	if nfsAccess&types.AccessDelete != 0 {
		perms |= metadata.PermissionDelete
	}

	return perms
}

// permissionsToNFSAccess translates generic Permission flags back to NFS ACCESS bits.
//
// This is the inverse of nfsAccessToPermissions and must maintain consistency.
func permissionsToNFSAccess(perms metadata.Permission, fileType metadata.FileType) uint32 {
	var nfsAccess uint32

	// Handle directories specially
	if fileType == metadata.FileTypeDirectory {
		// PermissionListDirectory -> AccessRead
		if perms&metadata.PermissionListDirectory != 0 {
			nfsAccess |= types.AccessRead
		}
		// PermissionTraverse -> AccessLookup and AccessExecute
		if perms&metadata.PermissionTraverse != 0 {
			nfsAccess |= types.AccessLookup | types.AccessExecute
		}
		// PermissionWrite -> AccessModify and AccessExtend
		if perms&metadata.PermissionWrite != 0 {
			nfsAccess |= types.AccessModify | types.AccessExtend
		}
	} else {
		// For files: straightforward mapping
		if perms&metadata.PermissionRead != 0 {
			nfsAccess |= types.AccessRead
		}
		if perms&metadata.PermissionWrite != 0 {
			nfsAccess |= types.AccessModify | types.AccessExtend
		}
		if perms&metadata.PermissionExecute != 0 {
			nfsAccess |= types.AccessExecute
		}
	}

	// PermissionDelete maps directly
	if perms&metadata.PermissionDelete != 0 {
		nfsAccess |= types.AccessDelete
	}

	return nfsAccess
}

// ============================================================================
// Request Validation
// ============================================================================

// accessValidationError represents an ACCESS request validation error.
type accessValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *accessValidationError) Error() string {
	return e.message
}

// validateAccessRequest validates ACCESS request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//   - Access bitmap is valid (only uses defined bits)
//
// Returns:
//   - nil if valid
//   - *accessValidationError with NFS status if invalid
func validateAccessRequest(req *AccessRequest) *accessValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &accessValidationError{
			message:   "file handle is empty",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &accessValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &accessValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate access bitmap - only defined bits should be set
	// Valid bits: AccessRead | AccessLookup | AccessModify | AccessExtend | AccessDelete | AccessExecute
	validBits := uint32(0x003F) // Bits 0-5
	if req.Access&^validBits != 0 {
		return &accessValidationError{
			message:   fmt.Sprintf("invalid access bitmap: 0x%x (invalid bits set)", req.Access),
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeAccessRequest decodes an ACCESS request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.4 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Access bitmap (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the ACCESS request
//
// Returns:
//   - *AccessRequest: The decoded request containing handle and access bitmap
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded ACCESS request from network
//	req, err := DecodeAccessRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle and req.Access in ACCESS procedure
func DecodeAccessRequest(data []byte) (*AccessRequest, error) {
	// Validate minimum data length for handle length field
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// Read handle length (4 bytes, big-endian)
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("failed to read handle length: %w", err)
	}

	// Validate handle length (NFS v3 handles are typically <= 64 bytes per RFC 1813)
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	// Prevent zero-length handles
	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Ensure we have enough data for the handle
	// 4 bytes for length + handleLen bytes for data + up to 3 bytes padding + 4 bytes for access
	minRequired := 4 + handleLen
	if uint32(len(data)) < minRequired {
		return nil, fmt.Errorf("data too short for handle: need at least %d bytes, got %d", minRequired, len(data))
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := range padding {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read padding byte %d: %w", i, err)
		}
	}

	// Read access bitmap (4 bytes, big-endian)
	var access uint32
	if err := binary.Read(reader, binary.BigEndian, &access); err != nil {
		return nil, fmt.Errorf("failed to read access bitmap: %w", err)
	}

	return &AccessRequest{
		Handle: handle,
		Access: access,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the AccessResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.4 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. Access bitmap (4 bytes, granted permissions)
//  3. If status != types.NFS3OK:
//     a. Post-op attributes (present flag + attributes if present)
//     b. No access bitmap
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
//	resp := &AccessResponse{
//	    Status: types.NFS3OK,
//	    Attr:   fileAttr,
//	    Access: AccessRead | AccessLookup,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *AccessResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code (4 bytes, big-endian)
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// Write post-op attributes (present flag + attributes if present)
	// These are included for both success and failure cases to help
	// clients maintain cache consistency
	if resp.Attr != nil {
		// attributes_follow = TRUE (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write attr present flag: %w", err)
		}
		// Encode file attributes using helper function
		if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
			return nil, fmt.Errorf("failed to encode attributes: %w", err)
		}
	} else {
		// attributes_follow = FALSE (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("failed to write attr absent flag: %w", err)
		}
	}

	// If status is not OK, we're done - no access bitmap on error
	if resp.Status != types.NFS3OK {
		return buf.Bytes(), nil
	}

	// Write granted access bitmap (4 bytes, big-endian)
	// This indicates which of the requested permissions were granted
	if err := binary.Write(&buf, binary.BigEndian, resp.Access); err != nil {
		return nil, fmt.Errorf("failed to write access bitmap: %w", err)
	}

	return buf.Bytes(), nil
}

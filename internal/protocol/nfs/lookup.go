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

// LookupRequest represents a LOOKUP request from an NFS client.
// The client provides a directory handle and a filename to search for.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.3 specifies the LOOKUP procedure as:
//
//	LOOKUP3res NFSPROC3_LOOKUP(LOOKUP3args) = 3;
//
// The LOOKUP procedure is fundamental to NFS path resolution. It's used to:
//   - Navigate directory hierarchies
//   - Resolve pathnames component by component
//   - Obtain file handles for files and subdirectories
//   - Build complete paths from the root
type LookupRequest struct {
	// DirHandle is the file handle of the directory to search in.
	// Must be a valid directory handle obtained from MOUNT or a previous LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	DirHandle []byte

	// Filename is the name to search for within the directory.
	// Must follow NFS naming conventions:
	//   - Maximum 255 bytes
	//   - Cannot contain null bytes or path separators
	//   - Case-sensitive (unless filesystem is case-insensitive)
	Filename string
}

// LookupResponse represents the response to a LOOKUP request.
// It contains the status and, if successful, the file handle and attributes
// of the found file, plus optional post-operation directory attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type LookupResponse struct {
	// Status indicates the result of the lookup operation.
	// Common values:
	//   - types.NFS3OK (0): Success - file found
	//   - types.NFS3ErrNoEnt (2): File not found in directory
	//   - types.NFS3ErrNotDir (20): DirHandle is not a directory
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrStale (70): Stale directory handle
	//   - types.NFS3ErrBadHandle (10001): Invalid directory handle
	//   - NFS3ErrNameTooLong (63): Filename exceeds limits
	Status uint32

	// FileHandle is the handle of the found file or directory.
	// Only present when Status == types.NFS3OK.
	// This handle can be used in subsequent NFS operations.
	FileHandle []byte

	// Attr contains the attributes of the found file or directory.
	// Only present when Status == types.NFS3OK.
	// Includes type, permissions, size, timestamps, etc.
	Attr *types.NFSFileAttr

	// DirAttr contains post-operation attributes of the directory.
	// Optional, may be nil even on success.
	// Helps clients maintain cache consistency for the directory.
	DirAttr *types.NFSFileAttr
}

// LookupContext contains the context information needed to process a LOOKUP request.
// This includes client identification and authentication details for access control.
type LookupContext struct {
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

// Lookup searches a directory for a specific name and returns its file handle.
//
// This implements the NFS LOOKUP procedure as defined in RFC 1813 Section 3.3.3.
//
// **Purpose:**
//
// LOOKUP is the fundamental building block for pathname resolution in NFS.
// Clients use it to traverse directory hierarchies one component at a time:
//   - Start with root handle from MOUNT
//   - LOOKUP "usr" in root → get handle for /usr
//   - LOOKUP "local" in /usr → get handle for /usr/local
//   - LOOKUP "bin" in /usr/local → get handle for /usr/local/bin
//
// **Process:**
//
//  1. Validate request parameters (handles, filename)
//  2. Extract client IP and authentication credentials from context
//  3. Verify directory handle exists and is a directory (via repository)
//  4. Delegate lookup to repository.LookupChild()
//  5. Retrieve file attributes for found child
//  6. Return file handle and attributes to client
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (child lookup, access control) is delegated to repository
//   - File handle validation is performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the repository, which can implement:
//   - Execute permission checking on the directory (search permission)
//   - Access control based on UID/GID
//   - Hiding files based on permissions
//
// **Path Resolution:**
//
// LOOKUP operates on single filename components only. For path resolution:
//   - Client splits "/usr/local/bin" into ["usr", "local", "bin"]
//   - Client performs separate LOOKUP for each component
//   - Server never sees or processes full paths
//   - This enables proper permission checking at each level
//
// **Special Names:**
//
//   - "." (current directory): Returns the directory's own handle
//   - ".." (parent directory): Returns the parent's handle
//   - Regular names: Search for child in directory
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - Directory not found → types.NFS3ErrNoEnt
//   - Not a directory → types.NFS3ErrNotDir
//   - Child not found → types.NFS3ErrNoEnt
//   - Access denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//
// **Performance Considerations:**
//
// LOOKUP is one of the most frequently called NFS procedures. Implementations should:
//   - Cache directory listings when possible
//   - Optimize child lookup algorithms
//   - Minimize repository access overhead
//   - Return post-op directory attributes when available
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository layer enforces directory search permission
//   - Filename validation prevents directory traversal
//   - Client context enables audit logging
//
// **Parameters:**
//   - repository: The metadata repository for file and directory operations
//   - req: The lookup request containing directory handle and filename
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *LookupResponse: Response with status and file handle (if successful)
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.3: LOOKUP Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &LookupRequest{
//	    DirHandle: dirHandle,
//	    Filename:  "myfile.txt",
//	}
//	ctx := &LookupContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Lookup(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.FileHandle for subsequent operations
//	}
func (h *DefaultNFSHandler) Lookup(
	repository metadata.Repository,
	req *LookupRequest,
	ctx *LookupContext,
) (*LookupResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("LOOKUP: file='%s' dir=%x client=%s auth=%d",
		req.Filename, req.DirHandle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateLookupRequest(req); err != nil {
		logger.Warn("LOOKUP validation failed: file='%s' client=%s error=%v",
			req.Filename, clientIP, err)
		return &LookupResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify directory handle exists and is valid
	// ========================================================================

	dirHandle := metadata.FileHandle(req.DirHandle)
	dirAttr, err := repository.GetFile(dirHandle)
	if err != nil {
		logger.Warn("LOOKUP failed: directory not found: dir=%x client=%s error=%v",
			req.DirHandle, clientIP, err)
		return &LookupResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Verify parent is actually a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		logger.Warn("LOOKUP failed: handle not a directory: dir=%x type=%d client=%s",
			req.DirHandle, dirAttr.Type, clientIP)

		// Include directory attributes even on error for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &LookupResponse{
			Status:  types.NFS3ErrNotDir,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 3: Look up the child via repository
	// ========================================================================
	// The repository is responsible for:
	// - Checking search/execute permission on the directory
	// - Finding the child by name (including "." and "..")
	// - Enforcing any access control policies

	childHandle, err := repository.GetChild(dirHandle, req.Filename)
	if err != nil {
		logger.Debug("LOOKUP failed: child not found: file='%s' dir=%x client=%s error=%v",
			req.Filename, req.DirHandle, clientIP, err)

		// Include directory post-op attributes for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &LookupResponse{
			Status:  types.NFS3ErrNoEnt,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 4: Retrieve child attributes
	// ========================================================================

	childAttr, err := repository.GetFile(childHandle)
	if err != nil {
		logger.Error("LOOKUP failed: child handle exists but attributes missing: file='%s' handle=%x client=%s error=%v",
			req.Filename, childHandle, clientIP, err)

		// This is an internal consistency error - handle exists but no attributes
		// Include directory post-op attributes for cache consistency
		dirID := xdr.ExtractFileID(dirHandle)
		nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

		return &LookupResponse{
			Status:  types.NFS3ErrIO,
			DirAttr: nfsDirAttr,
		}, nil
	}

	// ========================================================================
	// Step 5: Build success response with file handle and attributes
	// ========================================================================

	// Generate file IDs from handles for NFS attributes
	childID := xdr.ExtractFileID(childHandle)
	nfsChildAttr := xdr.MetadataToNFS(childAttr, childID)

	dirID := xdr.ExtractFileID(dirHandle)
	nfsDirAttr := xdr.MetadataToNFS(dirAttr, dirID)

	logger.Info("LOOKUP successful: file='%s' handle=%x type=%d size=%d client=%s",
		req.Filename, childHandle, childAttr.Type, childAttr.Size, clientIP)

	logger.Debug("LOOKUP details: child_id=%d child_mode=%o dir_id=%d",
		childID, childAttr.Mode, dirID)

	return &LookupResponse{
		Status:     types.NFS3OK,
		FileHandle: childHandle,
		Attr:       nfsChildAttr,
		DirAttr:    nfsDirAttr,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// lookupValidationError represents a LOOKUP request validation error.
type lookupValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *lookupValidationError) Error() string {
	return e.message
}

// validateLookupRequest validates LOOKUP request parameters.
//
// Checks performed:
//   - Directory handle is not nil or empty
//   - Directory handle length is within RFC 1813 limits (max 64 bytes)
//   - Directory handle is long enough for file ID extraction (min 8 bytes)
//   - Filename is not empty
//   - Filename length doesn't exceed 255 bytes
//   - Filename doesn't contain invalid characters (null bytes)
//   - Filename doesn't contain path separators (prevents traversal)
//
// Returns:
//   - nil if valid
//   - *lookupValidationError with NFS status if invalid
func validateLookupRequest(req *LookupRequest) *lookupValidationError {
	// Validate directory handle
	if len(req.DirHandle) == 0 {
		return &lookupValidationError{
			message:   "empty directory handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.DirHandle) > 64 {
		return &lookupValidationError{
			message:   fmt.Sprintf("directory handle too long: %d bytes (max 64)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.DirHandle) < 8 {
		return &lookupValidationError{
			message:   fmt.Sprintf("directory handle too short: %d bytes (min 8)", len(req.DirHandle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate filename
	if req.Filename == "" {
		return &lookupValidationError{
			message:   "empty filename",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// NFS filename limit is typically 255 bytes
	if len(req.Filename) > 255 {
		return &lookupValidationError{
			message:   fmt.Sprintf("filename too long: %d bytes (max 255)", len(req.Filename)),
			nfsStatus: types.NFS3ErrNameTooLong,
		}
	}

	// Check for null bytes (string terminator, invalid in filenames)
	if bytes.ContainsAny([]byte(req.Filename), "\x00") {
		return &lookupValidationError{
			message:   "filename contains null byte",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	// Check for path separators (prevents directory traversal attacks)
	// Note: "." and ".." are allowed (handled specially by repository)
	if bytes.ContainsAny([]byte(req.Filename), "/") {
		return &lookupValidationError{
			message:   "filename contains path separator",
			nfsStatus: types.NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeLookupRequest decodes a LOOKUP request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.3 specifications:
//  1. Directory handle length (4 bytes, big-endian uint32)
//  2. Directory handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Filename length (4 bytes, big-endian uint32)
//  5. Filename data (variable length, up to 255 bytes)
//  6. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the LOOKUP request
//
// Returns:
//   - *LookupRequest: The decoded request containing directory handle and filename
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded LOOKUP request from network
//	req, err := DecodeLookupRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.DirHandle and req.Filename in LOOKUP procedure
func DecodeLookupRequest(data []byte) (*LookupRequest, error) {
	// Validate minimum data length for handle length field
	if len(data) < 4 {
		return nil, fmt.Errorf("data too short: need at least 4 bytes for handle length, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode directory handle
	// ========================================================================

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
	if len(data) < int(4+handleLen) {
		return nil, fmt.Errorf("data too short for handle: need %d bytes, got %d", 4+handleLen, len(data))
	}

	// Read handle data
	dirHandle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &dirHandle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := range padding {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode filename
	// ========================================================================

	// Read filename length (4 bytes, big-endian)
	var filenameLen uint32
	if err := binary.Read(reader, binary.BigEndian, &filenameLen); err != nil {
		return nil, fmt.Errorf("failed to read filename length: %w", err)
	}

	// Validate filename length (NFS limit is typically 255 bytes)
	if filenameLen > 255 {
		return nil, fmt.Errorf("invalid filename length: %d (max 255)", filenameLen)
	}

	// Prevent zero-length filenames
	if filenameLen == 0 {
		return nil, fmt.Errorf("invalid filename length: 0 (must be > 0)")
	}

	// Read filename data
	filenameBytes := make([]byte, filenameLen)
	if err := binary.Read(reader, binary.BigEndian, &filenameBytes); err != nil {
		return nil, fmt.Errorf("failed to read filename data: %w", err)
	}

	logger.Debug("Decoded LOOKUP request: handle_len=%d filename='%s'", handleLen, string(filenameBytes))

	return &LookupRequest{
		DirHandle: dirHandle,
		Filename:  string(filenameBytes),
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the LookupResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.3 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. File handle (opaque: length + data + padding)
//     b. Object attributes (present flag + attributes if present)
//     c. Directory post-op attributes (present flag + attributes if present)
//  3. If status != types.NFS3OK:
//     a. Directory post-op attributes (present flag + attributes if present)
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
//	resp := &LookupResponse{
//	    Status:     types.NFS3OK,
//	    FileHandle: fileHandle,
//	    Attr:       fileAttr,
//	    DirAttr:    dirAttr,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *LookupResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Error case: Return status + optional directory attributes
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding LOOKUP error response: status=%d", resp.Status)

		// Write post-op directory attributes (optional)
		if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
			return nil, fmt.Errorf("failed to encode directory attributes: %w", err)
		}

		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write file handle, file attributes, dir attributes
	// ========================================================================

	// Write file handle (opaque data: length + data + padding)
	handleLen := uint32(len(resp.FileHandle))
	if err := binary.Write(&buf, binary.BigEndian, handleLen); err != nil {
		return nil, fmt.Errorf("failed to write handle length: %w", err)
	}

	if _, err := buf.Write(resp.FileHandle); err != nil {
		return nil, fmt.Errorf("failed to write handle data: %w", err)
	}

	// Add padding to 4-byte boundary (XDR alignment requirement)
	padding := (4 - (handleLen % 4)) % 4
	for i := range padding {
		if err := buf.WriteByte(0); err != nil {
			return nil, fmt.Errorf("failed to write handle padding byte %d: %w", i, err)
		}
	}

	// Write object attributes (present flag + attributes if present)
	// attributes_follow = TRUE (1)
	if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
		return nil, fmt.Errorf("failed to write attr present flag: %w", err)
	}

	// Encode file attributes using helper function
	if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode file attributes: %w", err)
	}

	// Write post-op directory attributes (optional)
	if err := xdr.EncodeOptionalFileAttr(&buf, resp.DirAttr); err != nil {
		return nil, fmt.Errorf("failed to encode directory attributes: %w", err)
	}

	logger.Debug("Encoded LOOKUP response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

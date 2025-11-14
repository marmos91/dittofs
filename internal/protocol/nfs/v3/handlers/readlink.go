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

// ReadLinkRequest represents a READLINK request from an NFS client.
// The client provides a file handle for a symbolic link and requests
// the target path that the symlink points to.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.5 specifies the READLINK procedure as:
//
//	READLINK3res NFSPROC3_READLINK(READLINK3args) = 5;
//
// The READLINK procedure reads the data associated with a symbolic link.
// Symbolic links are special files that contain a pathname to another file
// or directory. Following a symbolic link involves reading the link contents
// and using that path for the next lookup operation.
type ReadLinkRequest struct {
	// Handle is the file handle of the symbolic link to read.
	// Must be a valid symlink handle obtained from MOUNT, LOOKUP, or CREATE.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// ReadLinkResponse represents the response to a READLINK request.
// It contains the status of the operation and, if successful, the target
// path and optional post-operation attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type ReadLinkResponse struct {
	// Status indicates the result of the readlink operation.
	// Common values:
	//   - types.NFS3OK (0): Success - target path returned
	//   - types.NFS3ErrNoEnt (2): Symlink not found
	//   - types.NFS3ErrIO (5): I/O error reading symlink
	//   - NFS3ErrInval (22): Handle is not a symbolic link
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// Attr contains the post-operation attributes of the symbolic link.
	// Optional, may be nil. These attributes help clients maintain cache
	// consistency for the symlink itself (not the target).
	Attr *types.NFSFileAttr

	// Target is the symbolic link target path.
	// Only present when Status == types.NFS3OK.
	// This is the path string stored in the symlink file.
	// May be absolute (/usr/bin/python) or relative (../lib/file.so).
	// Maximum length is 1024 bytes per POSIX PATH_MAX.
	Target string
}

// ReadLinkContext contains the context information needed to process a READLINK request.
// This includes client identification and authentication details for access control
// and auditing purposes.
type ReadLinkContext struct {
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
	// Used for access control checks by the store.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for access control checks by the store.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Used for checking if user belongs to symlink's group.
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// Implement NFSAuthContext interface for ReadLinkContext
func (c *ReadLinkContext) GetContext() context.Context { return c.Context }
func (c *ReadLinkContext) GetClientAddr() string       { return c.ClientAddr }
func (c *ReadLinkContext) GetAuthFlavor() uint32       { return c.AuthFlavor }
func (c *ReadLinkContext) GetUID() *uint32             { return c.UID }
func (c *ReadLinkContext) GetGID() *uint32             { return c.GID }
func (c *ReadLinkContext) GetGIDs() []uint32           { return c.GIDs }


// ============================================================================
// Protocol Handler
// ============================================================================

// ReadLink reads the target path of a symbolic link.
//
// This implements the NFS READLINK procedure as defined in RFC 1813 Section 3.3.5.
//
// **Purpose:**
//
// READLINK retrieves the pathname stored in a symbolic link. This is essential
// for pathname resolution when traversing symbolic links:
//  1. Client encounters a symlink during LOOKUP
//  2. Client calls READLINK to get the target path
//  3. Client uses the target path for further lookups
//  4. Process repeats until a non-symlink is reached
//
// **Process:**
//
//  1. Check for context cancellation (client disconnect, timeout)
//  2. Validate request parameters (handle format and length)
//  3. Extract client IP and authentication credentials from context
//  4. Delegate symlink reading to store.ReadSymlink()
//  5. Retrieve symlink attributes for cache consistency
//  6. Return target path to client
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (symlink reading, access control) delegated to store
//   - File handle validation performed by store
//   - Context cancellation respected for client disconnect scenarios
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// The protocol layer passes these to the store, which can implement:
//   - Read permission checking on the symlink
//   - Access control based on UID/GID
//   - Directory search permission enforcement for symlink parent
//
// **Symlink Semantics:**
//
// Per RFC 1813 and POSIX semantics:
//   - Reading a symlink requires read permission on the symlink itself
//   - Symlink permissions are typically 0777 (lrwxrwxrwx)
//   - Following a symlink requires execute permission on parent directories
//   - Symlink targets may be absolute or relative paths
//   - Symlink targets may contain multiple path components
//   - Symlink loops should be detected by limiting depth (client's responsibility)
//
// **Target Path Handling:**
//
// The target path is returned as-is without interpretation:
//   - Absolute paths: /usr/bin/python
//   - Relative paths: ../lib/file.so, ../../data/config.txt
//   - Path components: usr/local/bin
//   - Empty paths: "" (though unusual, technically valid)
//
// The client is responsible for:
//   - Resolving relative paths against the symlink's directory
//   - Handling absolute vs relative paths
//   - Detecting and preventing symlink loops
//   - Following symlink chains to the final target
//
// **Context Cancellation:**
//
// READLINK is a lightweight metadata operation that typically completes quickly.
// However, context cancellation is still checked at key points:
//   - Before starting the operation (client disconnect detection)
//   - store operations respect context (passed through)
//
// Cancellation scenarios include:
//   - Client disconnects before receiving response
//   - Client timeout expires
//   - Server shutdown initiated
//   - Network connection lost
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store errors are mapped to NFS status codes:
//   - Symlink not found → types.NFS3ErrNoEnt
//   - Not a symlink → NFS3ErrInval
//   - Target path missing → types.NFS3ErrIO
//   - Access denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Context cancelled → returns context error (client disconnect)
//
// **Performance Considerations:**
//
// READLINK is frequently called during pathname resolution:
//   - Symlink targets should be cached by clients when possible
//   - store should efficiently retrieve symlink targets
//   - Post-op attributes help clients maintain cache consistency
//   - Context check adds negligible overhead (nanoseconds)
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store enforces read permission on symlink
//   - Target path should not be interpreted or validated by server
//   - Client context enables audit logging
//   - No validation of target path content (may contain special characters)
//   - Cancellation prevents resource exhaustion
//
// **Parameters:**
//   - ctx: Context with client address, authentication, and cancellation support
//   - metadataStore: The metadata store for symlink operations
//   - req: The readlink request containing the symlink handle
//
// **Returns:**
//   - *ReadLinkResponse: Response with status and target path (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.5: READLINK Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &ReadLinkRequest{Handle: symlinkHandle}
//	ctx := &ReadLinkContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.ReadLink(ctx, store, req)
//	if err == context.Canceled {
//	    // Client disconnected during readlink
//	    return nil, err
//	}
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.Target for symlink resolution
//	}
func (h *DefaultNFSHandler) ReadLink(
	ctx *ReadLinkContext,
	metadataStore metadata.MetadataStore,
	req *ReadLinkRequest,
) (*ReadLinkResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start processing. While READLINK is fast, we should still
	// respect cancellation to avoid wasted work on abandoned requests.
	select {
	case <-ctx.Context.Done():
		logger.Debug("READLINK: request cancelled at entry: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
		// Context not cancelled, continue processing
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("READLINK: handle=%x client=%s auth=%d",
		req.Handle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadLinkRequest(req); err != nil {
		logger.Warn("READLINK validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadLinkResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Build authentication context for store
	// ========================================================================
	// The store needs authentication details to enforce access control
	// on the symbolic link (read permission checking)

	fileHandle := metadata.FileHandle(req.Handle)
	authCtx, err := BuildAuthContextWithMapping(ctx, metadataStore, fileHandle)
	if err != nil {
		// Check if error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("READLINK cancelled during auth context building: handle=%x client=%s error=%v",
				req.Handle, clientIP, ctx.Context.Err())
			return &ReadLinkResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Error("READLINK failed: failed to build auth context: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadLinkResponse{Status: types.NFS3ErrIO}, nil
	}

	// ========================================================================
	// Step 3: Read symlink target via store
	// ========================================================================
	// The store is responsible for:
	// - Verifying the handle is a valid symlink
	// - Checking read permission on the symlink
	// - Retrieving the target path
	// - Handling any I/O errors
	// - Respecting context cancellation

	target, attr, err := metadataStore.ReadSymlink(authCtx, fileHandle)
	if err != nil {
		// Check if error is due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("READLINK: store operation cancelled: handle=%x client=%s",
				req.Handle, clientIP)
			return nil, err
		}

		logger.Warn("READLINK failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)

		// Map store errors to NFS status codes
		status := mapReadLinkErrorToNFSStatus(err)

		return &ReadLinkResponse{Status: status}, nil
	}

	// ========================================================================
	// Step 4: Generate file attributes for cache consistency
	// ========================================================================

	fileid := xdr.ExtractFileID(fileHandle)
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("READLINK successful: handle=%x target='%s' target_len=%d client=%s",
		req.Handle, target, len(target), clientIP)

	logger.Debug("READLINK details: fileid=%d mode=%o uid=%d gid=%d size=%d",
		fileid, attr.Mode, attr.UID, attr.GID, attr.Size)

	return &ReadLinkResponse{
		Status: types.NFS3OK,
		Attr:   nfsAttr,
		Target: target,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// readLinkValidationError represents a READLINK request validation error.
type readLinkValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *readLinkValidationError) Error() string {
	return e.message
}

// validateReadLinkRequest validates READLINK request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *readLinkValidationError with NFS status if invalid
func validateReadLinkRequest(req *ReadLinkRequest) *readLinkValidationError {
	// Validate file handle presence
	if len(req.Handle) == 0 {
		return &readLinkValidationError{
			message:   "file handle is empty",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &readLinkValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &readLinkValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}

// ============================================================================
// Error Mapping
// ============================================================================

// mapReadLinkErrorToNFSStatus maps store errors to NFS status codes.
// This provides consistent error handling across the READLINK operation.
func mapReadLinkErrorToNFSStatus(err error) uint32 {
	// Use the common metadata error mapper
	return mapMetadataErrorToNFS(err)
}


// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeReadLinkRequest decodes a READLINK request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.5 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READLINK request
//
// Returns:
//   - *ReadLinkRequest: The decoded request containing the symlink handle
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READLINK request from network
//	req, err := DecodeReadLinkRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in READLINK procedure
func DecodeReadLinkRequest(data []byte) (*ReadLinkRequest, error) {
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
	if len(data) < int(4+handleLen) {
		return nil, fmt.Errorf("data too short for handle: need %d bytes total, got %d", 4+handleLen, len(data))
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	logger.Debug("Decoded READLINK request: handle_len=%d", handleLen)

	return &ReadLinkRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadLinkResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.5 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. Post-op symlink attributes (present flag + attributes if present)
//     b. Target path (length + string data + padding)
//  3. If status != types.NFS3OK:
//     a. Post-op symlink attributes (present flag + attributes if present)
//     b. No target path
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
//	resp := &ReadLinkResponse{
//	    Status: types.NFS3OK,
//	    Attr:   symlinkAttr,
//	    Target: "/usr/bin/python3",
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadLinkResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op symlink attributes (both success and error cases)
	// ========================================================================
	// Including attributes on error helps clients maintain cache consistency
	// for the symlink itself, even when reading the target fails

	if err := xdr.EncodeOptionalFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return without target path
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READLINK error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write target path
	// ========================================================================

	// Write target path as XDR string (length + data + padding)
	targetLen := uint32(len(resp.Target))

	// Write length
	if err := binary.Write(&buf, binary.BigEndian, targetLen); err != nil {
		return nil, fmt.Errorf("failed to write target length: %w", err)
	}

	// Write target string data
	if _, err := buf.Write([]byte(resp.Target)); err != nil {
		return nil, fmt.Errorf("failed to write target data: %w", err)
	}

	// Add padding to 4-byte boundary (XDR alignment requirement)
	padding := (4 - (targetLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if err := buf.WriteByte(0); err != nil {
			return nil, fmt.Errorf("failed to write target padding byte %d: %w", i, err)
		}
	}

	logger.Debug("Encoded READLINK response: %d bytes target_len=%d", buf.Len(), targetLen)
	return buf.Bytes(), nil
}

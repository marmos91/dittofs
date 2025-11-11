package nfs

import (
	"bytes"
	"context"
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

// GetAttrRequest represents a GETATTR request from an NFS client.
// The client provides a file handle to retrieve attributes for.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.1 specifies the GETATTR procedure as:
//
//	GETATTR3res NFSPROC3_GETATTR(GETATTR3args) = 1;
//
// The GETATTR procedure is used to obtain attributes for a file system object.
// This is one of the most frequently called NFS procedures.
type GetAttrRequest struct {
	// Handle is the file handle of the object to get attributes for.
	// Must be a valid file handle obtained from MOUNT or LOOKUP.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte
}

// GetAttrResponse represents the response to a GETATTR request.
// It contains the status of the operation and, if successful, the file attributes.
//
// The response is encoded in XDR format before being sent back to the client.
type GetAttrResponse struct {
	// Status indicates the result of the getattr operation.
	// Common values:
	//   - types.NFS3OK (0): Success
	//   - types.NFS3ErrNoEnt (2): File handle not found
	//   - types.NFS3ErrIO (5): I/O error
	//   - NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// Attr contains the file attributes.
	// Only present when Status == types.NFS3OK.
	// Includes file type, permissions, ownership, size, timestamps, etc.
	Attr *types.NFSFileAttr
}

// GetAttrContext contains the context information needed to process a GETATTR request.
// This includes client identification for auditing purposes and cancellation handling.
type GetAttrContext struct {
	// Context carries cancellation signals and deadlines
	// The GetAttr handler checks this context minimally to maintain high performance
	// while still respecting client disconnection
	Context context.Context

	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	// Note: GETATTR typically doesn't require authentication in standard NFS,
	// but the context is provided for consistency and potential future use.
	AuthFlavor uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// GetAttr returns the attributes for a file system object.
//
// This implements the NFS GETATTR procedure as defined in RFC 1813 Section 3.3.1.
//
// **Purpose:**
//
// GETATTR is the fundamental operation for retrieving file metadata. It's used by:
//   - Clients to check if cached attributes are still valid
//   - The 'stat' and 'ls' commands to display file information
//   - The NFS client to validate file handles before operations
//   - Cache consistency protocols to detect file changes
//
// **Process:**
//
//  1. Check for context cancellation (early exit if client disconnected)
//  2. Validate request parameters (handle format and length)
//  3. Verify file handle exists via repository.GetFile()
//  4. Generate file attributes with proper file ID
//  5. Return attributes to client
//
// **Context cancellation:**
//
//   - Single check at the beginning to respect client disconnection
//   - Check after GetFile to catch cancellation during lookup
//   - Minimal overhead to maintain high performance for this frequent operation
//   - Returns NFS3ErrIO status with context error for cancellation
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - All business logic (file lookup, attribute generation) is delegated to repository
//   - File handle validation is performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Performance Considerations:**
//
// GETATTR is one of the most frequently called NFS procedures. Implementations should:
//   - Cache attributes when possible
//   - Minimize repository access overhead
//   - Use efficient file ID generation
//   - Avoid unnecessary data copying
//   - Minimize context cancellation checks (only 2 checks for performance)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Stale handle → NFS3ErrStale
//   - I/O error → types.NFS3ErrIO
//   - Invalid handle → types.NFS3ErrBadHandle
//   - Context cancelled → types.NFS3ErrIO with error return
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository layer can enforce access control if needed
//   - Client context enables audit logging
//   - No sensitive information leaked in error messages
//
// **Parameters:**
//   - ctx: Context with cancellation, client address and authentication flavor
//   - repository: The metadata repository for file access
//   - req: The getattr request containing the file handle
//
// **Returns:**
//   - *GetAttrResponse: Response with status and attributes (if successful)
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.1: GETATTR Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &GetAttrRequest{Handle: fileHandle}
//	ctx := &GetAttrContext{
//	    Context: context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 0, // AUTH_NULL
//	}
//	resp, err := handler.GetAttr(ctx, repository, req)
//	if err != nil {
//	    if errors.Is(err, context.Canceled) {
//	        // Client disconnected
//	    } else {
//	        // Internal server error
//	    }
//	}
//	if resp.Status == types.NFS3OK {
//	    // Use resp.Attr for file information
//	}
func (h *DefaultNFSHandler) GetAttr(
	ctx *GetAttrContext,
	repository metadata.Repository,
	req *GetAttrRequest,
) (*GetAttrResponse, error) {
	// Check for cancellation before starting any work
	// This is the only pre-operation check for GETATTR to minimize overhead
	// GETATTR is one of the most frequently called procedures, so we optimize
	// for the common case of no cancellation
	select {
	case <-ctx.Context.Done():
		logger.Debug("GETATTR cancelled before processing: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, ctx.Context.Err())
		return &GetAttrResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
	default:
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("GETATTR: handle=%x client=%s auth=%d",
		req.Handle, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateGetAttrRequest(req); err != nil {
		logger.Warn("GETATTR validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &GetAttrResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify file handle exists and retrieve attributes
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	attr, err := repository.GetFile(ctx.Context, fileHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("GETATTR cancelled during file lookup: handle=%x client=%s error=%v",
				req.Handle, clientIP, ctx.Context.Err())
			return &GetAttrResponse{Status: types.NFS3ErrIO}, ctx.Context.Err()
		}

		logger.Debug("GETATTR failed: handle not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &GetAttrResponse{Status: types.NFS3ErrStale}, nil
	}

	// ========================================================================
	// Step 3: Generate file attributes with proper file ID
	// ========================================================================
	// The file ID is extracted from the handle for NFS protocol purposes.
	// This is a protocol-layer concern for creating the wire format.
	// No cancellation check here - this operation is extremely fast (pure computation)

	fileid := xdr.ExtractFileID(fileHandle)
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("GETATTR successful: handle=%x type=%d mode=%o size=%d client=%s",
		req.Handle, nfsAttr.Type, nfsAttr.Mode, nfsAttr.Size, clientIP)

	logger.Debug("GETATTR details: fileid=%d uid=%d gid=%d mtime=%d.%d",
		fileid, nfsAttr.UID, nfsAttr.GID, nfsAttr.Mtime.Seconds, nfsAttr.Mtime.Nseconds)

	return &GetAttrResponse{
		Status: types.NFS3OK,
		Attr:   nfsAttr,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// getAttrValidationError represents a GETATTR request validation error.
type getAttrValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *getAttrValidationError) Error() string {
	return e.message
}

// validateGetAttrRequest validates GETATTR request parameters.
//
// Checks performed:
//   - File handle is not nil or empty
//   - File handle length is within RFC 1813 limits (max 64 bytes)
//   - File handle is long enough for file ID extraction (min 8 bytes)
//
// Returns:
//   - nil if valid
//   - *getAttrValidationError with NFS status if invalid
func validateGetAttrRequest(req *GetAttrRequest) *getAttrValidationError {
	// Validate file handle presence
	if len(req.Handle) == 0 {
		return &getAttrValidationError{
			message:   "file handle is empty",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &getAttrValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	// This is a protocol-specific requirement for generating the fileid field
	if len(req.Handle) < 8 {
		return &getAttrValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeGetAttrRequest decodes a GETATTR request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.1 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the GETATTR request
//
// Returns:
//   - *GetAttrRequest: The decoded request containing the file handle
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded GETATTR request from network
//	req, err := DecodeGetAttrRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle in GETATTR procedure
func DecodeGetAttrRequest(data []byte) (*GetAttrRequest, error) {
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
	// 4 bytes for length + handleLen bytes for data
	if uint32(len(data)) < 4+handleLen {
		return nil, fmt.Errorf("data too short for handle: need %d bytes total, got %d", 4+handleLen, len(data))
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	logger.Debug("Decoded GETATTR request: handle_len=%d", handleLen)

	return &GetAttrRequest{Handle: handle}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the GetAttrResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.1 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. If status == types.NFS3OK:
//     a. File attributes (fattr3 structure)
//  3. If status != types.NFS3OK:
//     a. No additional data
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
//	resp := &GetAttrResponse{
//	    Status: types.NFS3OK,
//	    Attr:   fileAttr,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *GetAttrResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// Write status code (4 bytes, big-endian)
	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// If status is not OK, return just the status (no attributes)
	// Per RFC 1813, error responses contain only the status code
	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding GETATTR error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// Write file attributes using helper function
	// This encodes the complete fattr3 structure as defined in RFC 1813
	if err := xdr.EncodeFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode file attributes: %w", err)
	}

	logger.Debug("Encoded GETATTR response: %d bytes", buf.Len())
	return buf.Bytes(), nil
}

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

// CommitRequest represents a COMMIT request from an NFS client.
// The client requests that the server flush any cached writes for a specified
// range of a file to stable storage.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.21 specifies the COMMIT procedure as:
//
//	COMMIT3res NFSPROC3_COMMIT(COMMIT3args) = 21;
//
// COMMIT ensures that data previously written with WRITE operations using
// the UNSTABLE storage option is committed to stable storage. This is
// critical for maintaining data integrity across server crashes.
type CommitRequest struct {
	// Handle is the file handle of the file to commit.
	// Must be a valid file handle obtained from CREATE, LOOKUP, etc.
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Offset is the starting byte offset of the region to commit.
	// If 0, commit starts from the beginning of the file.
	// Must be less than or equal to the file size.
	Offset uint64

	// Count is the number of bytes to commit starting from Offset.
	// If 0, commit from Offset to the end of the file.
	// The range [Offset, Offset+Count) should be committed.
	Count uint32
}

// CommitResponse represents the response to a COMMIT request.
// It contains the status of the operation, WCC data for the file,
// and a write verifier for detecting server state changes.
//
// The response is encoded in XDR format before being sent back to the client.
type CommitResponse struct {
	// Status indicates the result of the commit operation.
	// Common values:
	//   - types.NFS3OK (0): Success, data committed to stable storage
	//   - types.NFS3ErrNoEnt (2): File not found
	//   - types.NFS3ErrIO (5): I/O error during commit
	//   - types.NFS3ErrStale (70): Stale file handle
	//   - types.NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// AttrBefore contains pre-operation attributes of the file.
	// Used for weak cache consistency to detect concurrent changes.
	// May be nil if attributes could not be captured.
	AttrBefore *types.WccAttr

	// AttrAfter contains post-operation attributes of the file.
	// Used for weak cache consistency and to provide updated file state.
	// May be nil on error, but should be present on success.
	AttrAfter *types.NFSFileAttr

	// WriteVerifier is a unique value that changes when the server restarts.
	// Clients use this to detect server reboots and resubmit unstable writes.
	// Per RFC 1813: "The server is free to accept the data written by the client
	// and not write it to stable storage. The client can use the COMMIT operation
	// to force the server to write the data to stable storage."
	//
	// In this implementation:
	//   - Value of 0 indicates no server reboot tracking
	//   - A production implementation should track server start time or boot ID
	WriteVerifier uint64
}

// CommitContext contains the context information needed to process a COMMIT request.
// This includes client identification and authentication details.
type CommitContext struct {
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
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Only valid when AuthFlavor == AUTH_UNIX.
	GID *uint32

	// GIDs is a list of supplementary group IDs (from AUTH_UNIX).
	// Only valid when AuthFlavor == AUTH_UNIX.
	GIDs []uint32
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Commit forces cached writes to stable storage for a file or file range.
//
// This implements the NFS COMMIT procedure as defined in RFC 1813 Section 3.3.21.
//
// **Purpose:**
//
// COMMIT is a companion to WRITE with the UNSTABLE storage option. When clients
// write data with UNSTABLE storage, the server may cache the data in memory for
// performance. COMMIT forces the server to flush that cached data to stable
// storage (disk), ensuring durability.
//
// Key use cases:
//   - File synchronization after buffered writes
//   - Ensuring data durability before closing files
//   - Implementing fsync() or fdatasync() semantics
//   - Transaction commit points
//
// **Process:**
//
//  1. Check for context cancellation
//  2. Validate request parameters (handle format, offset/count range)
//  3. Extract client IP and authentication from context
//  4. Verify file exists and capture pre-operation state
//  5. Delegate commit operation to repository (when implemented)
//  6. Return file WCC data and write verifier
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Business logic (actual flushing) will be delegated to repository
//   - File handle validation performed by repository.GetFile()
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//   - Respects context cancellation for graceful shutdown and timeouts
//
// **Current Implementation:**
//
// This implementation is a no-op because the in-memory metadata repository
// doesn't have write caching. All writes are immediately "committed" to the
// in-memory data structures.
//
// **Future Production Implementation:**
//
// A production implementation with actual write caching should:
//
//  1. Add a CommitFile method to the Repository interface:
//     CommitFile(handle FileHandle, offset uint64, count uint32, ctx *AuthContext) error
//
//  2. The repository should:
//     - Identify cached writes in the specified range [offset, offset+count)
//     - Flush those writes to stable storage (disk)
//     - Ensure durability (fsync, fdatasync, or equivalent)
//     - Update file metadata if needed (size, timestamps)
//     - Handle partial failures gracefully
//     - Respect context cancellation during long flush operations
//
//  3. Consider write cache coherency:
//     - Track which byte ranges have unstable writes
//     - Handle overlapping commit ranges
//     - Deal with commits during active writes
//
//  4. Implement write verifier tracking:
//     - Generate verifier on server start (e.g., boot timestamp)
//     - Return consistent verifier across all WRITE/COMMIT operations
//     - Change verifier on server restart to invalidate client caches
//
// **Atomicity and Consistency:**
//
// COMMIT should be atomic for the specified range:
//   - Either all data in the range is committed to stable storage
//   - Or the operation fails and returns an error status
//   - Partial commits should not be visible to clients
//
// **Special Cases:**
//
//   - Offset=0, Count=0: Commit entire file
//   - Count=0: Commit from Offset to end of file
//   - Range beyond file size: Commit what exists, ignore rest (not an error)
//   - Directory handle: Return types.NFS3ErrIsDir
//   - No unstable writes: Success (idempotent no-op)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Stale handle → types.NFS3ErrStale
//   - I/O error during flush → types.NFS3ErrIO
//   - Directory handle → types.NFS3ErrIsDir
//   - Invalid handle → types.NFS3ErrBadHandle
//   - Context cancelled → types.NFS3ErrIO (no specific NFS code for cancellation)
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps clients maintain cache coherency:
//  1. Capture file attributes before commit (WccBefore)
//  2. Perform the commit operation
//  3. Capture file attributes after commit (WccAfter)
//
// Clients use this to:
//   - Detect if file changed during commit
//   - Update cached file attributes
//   - Invalidate stale cached data
//
// **Write Verifier:**
//
// The write verifier is critical for handling server crashes:
//
//  1. Server returns same verifier for all WRITE/COMMIT operations
//  2. When server restarts, verifier changes
//  3. Client compares verifiers to detect server restart
//  4. If verifier changed, client must resend all unstable writes
//
// This ensures clients don't lose data when the server crashes before
// committing unstable writes to stable storage.
//
// **Performance Considerations:**
//
//   - COMMIT can be expensive (triggers disk I/O and fsync)
//   - Clients should batch commits when possible
//   - Server may optimize by tracking dirty ranges
//   - Consider group commits for multiple clients
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Range validation prevents integer overflow attacks
//   - Client context enables audit logging
//   - No special permission checks (commit doesn't modify data)
//
// **Context Cancellation:**
//
// This operation respects context cancellation:
//   - Checks at operation start before any work
//   - Checks before metadata repository calls
//   - Returns types.NFS3ErrIO on cancellation (NFS has no specific cancel status)
//
// For a lightweight metadata operation like this, cancellation is primarily
// useful for:
//   - Handling client disconnects
//   - Supporting server shutdown
//   - Enforcing request timeouts
//
// **Parameters:**
//   - repository: The metadata repository for file operations
//   - req: The commit request containing file handle and range
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *CommitResponse: Response with status, WCC data, and write verifier
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.21: COMMIT Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &CommitRequest{
//	    Handle: fileHandle,
//	    Offset: 0,
//	    Count:  0, // Commit entire file
//	}
//	ctx := &CommitContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Commit(repository, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Data committed successfully
//	}
func (h *DefaultNFSHandler) Commit(
	ctx *CommitContext,
	repository metadata.Repository,
	req *CommitRequest,
) (*CommitResponse, error) {
	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("COMMIT: handle=%x offset=%d count=%d client=%s auth=%d",
		req.Handle, req.Offset, req.Count, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Check for context cancellation before starting work
	// ========================================================================

	select {
	case <-ctx.Context.Done():
		logger.Warn("COMMIT cancelled: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())
		return &CommitResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	// ========================================================================
	// Step 2: Validate request parameters
	// ========================================================================

	if err := validateCommitRequest(req); err != nil {
		logger.Warn("COMMIT validation failed: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, req.Count, clientIP, err)
		return &CommitResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 3: Verify file exists and capture pre-operation state
	// ========================================================================

	// Check context before repository call
	select {
	case <-ctx.Context.Done():
		logger.Warn("COMMIT cancelled before GetFile: handle=%x client=%s error=%v",
			req.Handle, clientIP, ctx.Context.Err())
		return &CommitResponse{Status: types.NFS3ErrIO}, nil
	default:
	}

	handle := metadata.FileHandle(req.Handle)
	fileAttr, err := repository.GetFile(ctx.Context, handle)
	if err != nil {
		logger.Warn("COMMIT failed: file not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &CommitResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Capture pre-operation attributes for WCC data
	wccBefore := xdr.CaptureWccAttr(fileAttr)

	// Verify this is not a directory
	if fileAttr.Type == metadata.FileTypeDirectory {
		logger.Warn("COMMIT failed: handle is a directory: handle=%x client=%s",
			req.Handle, clientIP)

		fileID := xdr.ExtractFileID(handle)
		wccAfter := xdr.MetadataToNFS(fileAttr, fileID)

		return &CommitResponse{
			Status:     types.NFS3ErrIsDir,
			AttrBefore: wccBefore,
			AttrAfter:  wccAfter,
		}, nil
	}

	// ========================================================================
	// Step 4: Perform commit operation
	// ========================================================================
	// TODO: When the repository supports write caching, delegate to:
	//   repository.CommitFile(handle, req.Offset, req.Count, authCtx)
	//
	// For now, this is a no-op since the in-memory repository doesn't
	// have write caching - all writes are immediately "committed".

	// In a production implementation with write caching, you would:
	// 1. Build authentication context
	// authCtx := &metadata.AuthContext{
	//     AuthFlavor: ctx.AuthFlavor,
	//     UID:        ctx.UID,
	//     GID:        ctx.GID,
	//     GIDs:       ctx.GIDs,
	//     ClientAddr: clientIP,
	// }
	//
	// 2. Check context before potentially long flush operation
	// select {
	// case <-ctx.Context.Done():
	//     logger.Warn("COMMIT cancelled before flush: handle=%x offset=%d count=%d client=%s error=%v",
	//         req.Handle, req.Offset, req.Count, clientIP, ctx.Context.Err())
	//
	//     // Get updated attributes for WCC data (best effort)
	//     var wccAfter *types.NFSFileAttr
	//     if updatedAttr, getErr := repository.GetFile(ctx.Context, handle); getErr == nil {
	//         fileID := xdr.ExtractFileID(handle)
	//         wccAfter = xdr.MetadataToNFS(updatedAttr, fileID)
	//     }
	//
	//     return &CommitResponse{
	//         Status:     types.NFS3ErrIO,
	//         AttrBefore: wccBefore,
	//         AttrAfter:  wccAfter,
	//     }, nil
	// default:
	// }
	//
	// 3. Call repository to flush writes (should pass context for cancellation)
	// if err := repository.CommitFile(ctx.Context, handle, req.Offset, req.Count, authCtx); err != nil {
	//     logger.Error("COMMIT failed: repository error: handle=%x offset=%d count=%d client=%s error=%v",
	//         req.Handle, req.Offset, req.Count, clientIP, err)
	//
	//     // Get updated attributes for WCC data (best effort)
	//     var wccAfter *types.NFSFileAttr
	//     if updatedAttr, getErr := repository.GetFile(ctx.Context, handle); getErr == nil {
	//         fileID := xdr.ExtractFileID(handle)
	//         wccAfter = xdr.MetadataToNFS(updatedAttr, fileID)
	//     }
	//
	//     status := xdr.MapRepositoryErrorToNFSStatus(err, clientIP, "COMMIT")
	//     return &CommitResponse{
	//         Status:        status,
	//         AttrBefore:    wccBefore,
	//         AttrAfter:     wccAfter,
	//         WriteVerifier: 0,
	//     }, nil
	// }

	// ========================================================================
	// Step 5: Build success response with updated file attributes
	// ========================================================================

	// Get updated file attributes for WCC data
	fileAttr, err = repository.GetFile(ctx.Context, handle)
	if err != nil {
		logger.Warn("COMMIT: successful but cannot get updated file attributes: handle=%x error=%v",
			req.Handle, err)
		// Continue with nil WccAfter rather than failing
	}

	var wccAfter *types.NFSFileAttr
	if fileAttr != nil {
		fileID := xdr.ExtractFileID(handle)
		wccAfter = xdr.MetadataToNFS(fileAttr, fileID)
		logger.Debug("COMMIT details: file_size=%d file_type=%d",
			fileAttr.Size, fileAttr.Type)
	}

	logger.Info("COMMIT successful: handle=%x offset=%d count=%d client=%s (no-op in current implementation)",
		req.Handle, req.Offset, req.Count, clientIP)
	return &CommitResponse{
		Status:        types.NFS3OK,
		AttrBefore:    wccBefore,
		AttrAfter:     wccAfter,
		WriteVerifier: serverBootTime,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// commitValidationError represents a COMMIT request validation error.
type commitValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *commitValidationError) Error() string {
	return e.message
}

// validateCommitRequest validates COMMIT request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - Handle is at least 8 bytes (for file ID extraction)
//   - Offset + Count doesn't overflow uint64
//
// Returns:
//   - nil if valid
//   - *commitValidationError with NFS status if invalid
func validateCommitRequest(req *CommitRequest) *commitValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &commitValidationError{
			message:   "empty file handle",
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	if len(req.Handle) > 64 {
		return &commitValidationError{
			message:   fmt.Sprintf("handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &commitValidationError{
			message:   fmt.Sprintf("handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Validate offset + count doesn't overflow
	// This prevents potential integer overflow attacks
	if req.Count > 0 {
		if req.Offset > ^uint64(0)-uint64(req.Count) {
			return &commitValidationError{
				message:   fmt.Sprintf("offset + count overflow: offset=%d count=%d", req.Offset, req.Count),
				nfsStatus: types.NFS3ErrInval,
			}
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeCommitRequest decodes a COMMIT request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.21 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the COMMIT request
//
// Returns:
//   - *CommitRequest: The decoded request containing handle, offset, and count
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded COMMIT request from network
//	req, err := DecodeCommitRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Count in COMMIT procedure
func DecodeCommitRequest(data []byte) (*CommitRequest, error) {
	// Validate minimum data length (4 bytes handle len + at least 1 byte handle + 8 bytes offset + 4 bytes count)
	if len(data) < 17 {
		return nil, fmt.Errorf("data too short: need at least 17 bytes, got %d", len(data))
	}

	reader := bytes.NewReader(data)

	// ========================================================================
	// Decode file handle
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

	// Read handle
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			break // tolerate missing padding bytes, as in DecodeWriteRequest
		}
	}

	// ========================================================================
	// Decode offset
	// ========================================================================

	// Read offset (8 bytes, big-endian)
	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("failed to read offset: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	// Read count (4 bytes, big-endian)
	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("failed to read count: %w", err)
	}

	logger.Debug("Decoded COMMIT request: handle_len=%d offset=%d count=%d",
		handleLen, offset, count)

	return &CommitRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the CommitResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.21 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. File WCC data (always present):
//     a. Pre-op attributes (present flag + attributes if present)
//     b. Post-op attributes (present flag + attributes if present)
//  3. Write verifier (8 bytes, big-endian uint64) - only on success
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
//	resp := &CommitResponse{
//	    Status:        types.NFS3OK,
//	    AttrBefore:    wccBefore,
//	    AttrAfter:     wccAfter,
//	    WriteVerifier: 0,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *CommitResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write file WCC data (both success and failure cases)
	// ========================================================================
	// WCC (Weak Cache Consistency) data helps clients maintain cache coherency
	// by providing before-and-after snapshots of the file.

	if err := xdr.EncodeWccData(&buf, resp.AttrBefore, resp.AttrAfter); err != nil {
		return nil, fmt.Errorf("failed to encode file wcc data: %w", err)
	}

	// ========================================================================
	// Write write verifier (only on success)
	// ========================================================================

	if resp.Status == types.NFS3OK {
		if err := binary.Write(&buf, binary.BigEndian, resp.WriteVerifier); err != nil {
			return nil, fmt.Errorf("failed to write verifier: %w", err)
		}
	}

	logger.Debug("Encoded COMMIT response: %d bytes status=%d", buf.Len(), resp.Status)
	return buf.Bytes(), nil
}

package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

// ============================================================================
// Write Stability Levels (RFC 1813 Section 3.3.7)
// ============================================================================

// Write stability levels control how the server handles data persistence.
// These constants define when data must be committed to stable storage.
const (
	// UnstableWrite (0): Data may be cached in server memory.
	// The server may lose data on crash before COMMIT is called.
	// Offers best performance for sequential writes.
	UnstableWrite = 0

	// DataSyncWrite (1): Data must be committed to stable storage,
	// but metadata (file size, timestamps) may be cached.
	// Provides good balance of performance and safety.
	DataSyncWrite = 1

	// FileSyncWrite (2): Both data and metadata must be committed
	// to stable storage before returning success.
	// Safest option but slowest performance.
	FileSyncWrite = 2
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// WriteRequest represents a WRITE request from an NFS client.
// The client specifies a file handle, offset, data to write, and
// stability requirements for the write operation.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.7 specifies the WRITE procedure as:
//
//	WRITE3res NFSPROC3_WRITE(WRITE3args) = 7;
//
// WRITE is used to write data to a regular file. It's one of the fundamental
// operations for file modification in NFS.
type WriteRequest struct {
	// Handle is the file handle of the file to write to.
	// Must be a valid file handle for a regular file (not a directory).
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Offset is the byte offset in the file where writing should begin.
	// Can be any value from 0 to max file size.
	// Writing beyond EOF extends the file (sparse files supported).
	Offset uint64

	// Count is the number of bytes the client intends to write.
	// Should match the length of Data field.
	// May differ from len(Data) if client implementation varies.
	Count uint32

	// Stable indicates the stability level for this write.
	// Values:
	//   - UnstableWrite (0): May cache in memory
	//   - DataSyncWrite (1): Commit data to disk
	//   - FileSyncWrite (2): Commit data and metadata to disk
	Stable uint32

	// Data contains the actual bytes to write.
	// Length typically matches Count field.
	// Maximum size limited by server's wtmax (from FSINFO).
	Data []byte
}

// WriteResponse represents the response to a WRITE request.
// It contains the status, WCC data for cache consistency, and
// information about how the write was committed.
//
// The response is encoded in XDR format before being sent back to the client.
type WriteResponse struct {
	Status     uint32
	AttrBefore *types.WccAttr     // Pre-op attributes (optional)
	AttrAfter  *types.NFSFileAttr // Post-op attributes (optional)
	Count      uint32             // Number of bytes written
	Committed  uint32             // How data was committed
	Verf       uint64             // Write verifier
}

// WriteContext contains the context information needed to process a WRITE request.
// This includes client identification and authentication details for access control.
type WriteContext struct {
	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the authenticated user ID (from AUTH_UNIX).
	// Used for write permission checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for write permission checks by the repository.
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

// Write writes data to a regular file.
//
// This implements the NFS WRITE procedure as defined in RFC 1813 Section 3.3.7.
//
// **Purpose:**
//
// WRITE is the fundamental operation for modifying file contents over NFS. It's used by:
//   - Applications writing to files
//   - Editors saving changes
//   - Build systems creating output files
//   - Any operation that needs to modify file data
//
// **Process:**
//
//  1. Validate request parameters (handle, offset, count, data)
//  2. Extract client IP and authentication credentials from context
//  3. Verify file exists and is a regular file (via repository)
//  4. Check write permissions and update metadata (via repository)
//  5. Capture pre-operation attributes for WCC
//  6. Write data to content repository
//  7. Return updated attributes and commit status
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Protocol layer coordinates between metadata and content repositories
//   - Content repository handles actual data writing
//   - Metadata repository handles file attributes and permissions
//   - Access control enforced by metadata repository
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// Write permission checking is implemented by the metadata repository
// based on Unix-style permission bits (owner/group/other).
//
// **Write Stability:**
//
// Clients can request different stability levels:
//   - UnstableWrite: Fastest, data may be lost on server crash
//   - DataSyncWrite: Data committed, metadata may be cached
//   - FileSyncWrite: Everything committed to disk
//
// The server can return a higher stability level than requested
// (e.g., always do FileSyncWrite for simplicity).
//
// **Weak Cache Consistency (WCC):**
//
// WCC data helps clients maintain cache consistency:
//   - AttrBefore: Attributes before the operation
//   - AttrAfter: Attributes after the operation
//
// Clients compare these to detect concurrent modifications by other clients.
// If AttrBefore doesn't match client's cached attributes, the cache is stale.
//
// **Write Verifier:**
//
// The write verifier is a unique value that changes when the server restarts.
// Clients use it to detect server reboots and re-send unstable writes.
// Typical implementation: server boot time or instance UUID.
//
// **File Size Extension:**
//
// Writing beyond EOF extends the file:
//   - Bytes between EOF and offset are zero-filled (sparse file)
//   - File size is updated to offset + count
//   - Sparse regions may not consume disk space on many filesystems
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository/Content errors are mapped to NFS status codes:
//   - File not found → NFS3ErrNoEnt
//   - Not a regular file → NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - No space left → NFS3ErrNoSpc
//   - Read-only filesystem → NFS3ErrRofs
//   - I/O error → NFS3ErrIO
//   - Stale handle → NFS3ErrStale
//
// **Performance Considerations:**
//
// WRITE is frequently called and performance-critical:
//   - Use efficient content repository implementation
//   - Support write-behind caching (UnstableWrite)
//   - Minimize data copying
//   - Batch small writes when possible
//   - Consider write alignment with filesystem blocks
//   - Respect FSINFO wtpref for optimal performance
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository layers enforce write permissions
//   - Read-only exports prevent writes
//   - Client context enables audit logging
//   - Prevent writes to system files via export configuration
//
// **Parameters:**
//   - contentRepo: Content repository for file data operations
//   - metadataRepo: Metadata repository for file attributes
//   - req: The write request containing handle, offset, and data
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *WriteResponse: Response with status, WCC data, and commit info
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.7: WRITE Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &WriteRequest{
//	    Handle: fileHandle,
//	    Offset: 0,
//	    Count:  1024,
//	    Stable: FileSyncWrite,
//	    Data:   dataBytes,
//	}
//	ctx := &WriteContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Write(contentRepo, metadataRepo, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == NFS3OK {
//	    // Write successful, resp.Count bytes written
//	    // Check resp.Committed for actual stability level
//	}
func (h *DefaultNFSHandler) Write(
	contentRepo content.Repository,
	metadataRepo metadata.Repository,
	req *WriteRequest,
	ctx *WriteContext,
) (*WriteResponse, error) {
	// Extract client IP for logging
	clientIP := extractClientIP(ctx.ClientAddr)

	logger.Info("WRITE: handle=%x offset=%d count=%d stable=%d client=%s auth=%d",
		req.Handle, req.Offset, req.Count, req.Stable, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateWriteRequest(req); err != nil {
		logger.Warn("WRITE validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &WriteResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify file exists and is a regular file
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	attr, err := metadataRepo.GetFile(fileHandle)
	if err != nil {
		logger.Warn("File not found: %v", err)
		return &WriteResponse{Status: types.NFS3ErrNoEnt}, nil
	}

	// Verify it's a regular file (not a directory or special file)
	if attr.Type != metadata.FileTypeRegular {
		logger.Warn("Handle is not a regular file")
		return &WriteResponse{Status: types.NFS3ErrIsDir}, nil
	}

	// Store pre-op attributes for WCC
	wccAttr := &types.WccAttr{
		Size: attr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}

	if err != nil {
		// Check for specific error types
		if exportErr, ok := err.(*metadata.ExportError); ok {
			var status uint32
			switch exportErr.Code {
			case metadata.ExportErrNotFound:
				status = NFS3ErrNoEnt
			case metadata.ExportErrAccessDenied:
				status = NFS3ErrAcces
			case metadata.ExportErrServerFault:
				status = NFS3ErrIO
			default:
				status = NFS3ErrIO
			}

			logger.Warn("WRITE failed: %s: handle=%x offset=%d count=%d client=%s",
				exportErr.Message, req.Handle, req.Offset, len(req.Data), clientIP)

			fileid := extractFileID(fileHandle)
			nfsAttr := MetadataToNFSAttr(attr, fileid)

			return &WriteResponse{
				Status:     status,
				AttrBefore: nfsWccAttr,
				AttrAfter:  nfsAttr,
			}, nil
		}

		// Generic error
		logger.Error("WRITE failed: repository error: handle=%x offset=%d count=%d client=%s error=%v",
			req.Handle, req.Offset, len(req.Data), clientIP, err)

		fileid := extractFileID(fileHandle)
		nfsAttr := MetadataToNFSAttr(attr, fileid)

		return &WriteResponse{
			Status:     NFS3ErrIO,
			AttrBefore: nfsWccAttr,
			AttrAfter:  nfsAttr,
		}, nil
	}

	// ========================================================================
	// Step 4: Check if content repository supports writing
	// ========================================================================

	writeRepo, ok := contentRepo.(content.WriteRepository)
	if !ok {
		logger.Error("Content repository does not support writing")
		return &WriteResponse{Status: types.NFS3ErrRofs}, nil
	}

	// ========================================================================
	// Step 5: Write data to content repository
	// ========================================================================
	// The content repository handles:
	// - Physical storage of data
	// - Space availability checks
	// - I/O operations
	// - Write stability (sync vs async)

	err = writeRepo.WriteAt(updatedAttr.ContentID, req.Data, int64(req.Offset))
	if err != nil {
		logger.Error("Failed to write content: %v", err)
		return &WriteResponse{Status: types.NFS3ErrIO}, nil
	}

	// ========================================================================
	// Step 6: Build success response
	// ========================================================================
	// Metadata is already updated by WriteFile, so we just need to
	// convert to NFS wire format

	fileid := extractFileID(fileHandle)
	nfsAttr := MetadataToNFSAttr(updatedAttr, fileid)

	// Save updated attributes
	if err := metadataRepo.UpdateFile(metadata.FileHandle(req.Handle), attr); err != nil {
		logger.Error("Failed to update file metadata: %v", err)
		return &WriteResponse{Status: types.NFS3ErrIO}, nil
	}

	// Generate file ID
	fileid := binary.BigEndian.Uint64(req.Handle[:8])
	nfsAttr := xdr.MetadataToNFS(attr, fileid)

	logger.Info("WRITE successful: wrote %d bytes at offset %d", len(req.Data), req.Offset)

	return &WriteResponse{
		Status:     types.NFS3OK,
		AttrBefore: wccAttr,
		AttrAfter:  nfsAttr,
		Count:      uint32(len(req.Data)),
		Committed:  FileSyncWrite, // We commit immediately for simplicity
		Verf:       0,             // TODO: Use server boot time or instance ID
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// writeValidationError represents a WRITE request validation error.
type writeValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *writeValidationError) Error() string {
	return e.message
}

// validateWriteRequest validates WRITE request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count matches actual data length
//   - Count doesn't exceed reasonable limits
//   - Offset + Count doesn't overflow uint64
//   - Stability level is valid
//
// Returns:
//   - nil if valid
//   - *writeValidationError with NFS status if invalid
func validateWriteRequest(req *WriteRequest) *writeValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &writeValidationError{
			message:   "empty file handle",
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &writeValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &writeValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// Validate data length matches count
	// Some tolerance is acceptable, but large mismatches indicate corruption
	dataLen := uint32(len(req.Data))
	if dataLen != req.Count {
		logger.Warn("WRITE: count mismatch: count=%d data_len=%d (proceeding with actual data length)",
			req.Count, dataLen)
		// Not fatal - we'll use actual data length
	}

	// Validate count doesn't exceed reasonable limits (1GB)
	// While RFC 1813 doesn't specify a maximum, extremely large writes should be rejected
	const maxWriteSize = 1024 * 1024 * 1024 // 1GB
	if dataLen > maxWriteSize {
		return &writeValidationError{
			message:   fmt.Sprintf("write data too large: %d bytes (max %d)", dataLen, maxWriteSize),
			nfsStatus: NFS3ErrFBig,
		}
	}

	// Validate offset + count doesn't overflow
	// This prevents integer overflow attacks
	if req.Offset > ^uint64(0)-uint64(dataLen) {
		return &writeValidationError{
			message:   fmt.Sprintf("offset + count would overflow: offset=%d count=%d", req.Offset, dataLen),
			nfsStatus: NFS3ErrInval,
		}
	}

	// Validate stability level
	if req.Stable > FileSyncWrite {
		return &writeValidationError{
			message:   fmt.Sprintf("invalid stability level: %d (max %d)", req.Stable, FileSyncWrite),
			nfsStatus: NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeWriteRequest decodes a WRITE request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.7 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//  6. Stable (4 bytes, big-endian uint32)
//  7. Data length (4 bytes, big-endian uint32)
//  8. Data bytes (variable length)
//  9. Padding to 4-byte boundary (0-3 bytes)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the WRITE request
//
// Returns:
//   - *WriteRequest: The decoded request containing handle, offset, and data
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded WRITE request from network
//	req, err := DecodeWriteRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Data in WRITE procedure
func DecodeWriteRequest(data []byte) (*WriteRequest, error) {
	// Validate minimum data length
	// 4 bytes (handle length) + 8 bytes (offset) + 4 bytes (count) +
	// 4 bytes (stable) + 4 bytes (data length) = 24 bytes minimum
	if len(data) < 24 {
		return nil, fmt.Errorf("data too short: need at least 24 bytes, got %d", len(data))
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

	// Validate handle length
	if handleLen > 64 {
		return nil, fmt.Errorf("invalid handle length: %d (max 64)", handleLen)
	}

	if handleLen == 0 {
		return nil, fmt.Errorf("invalid handle length: 0 (must be > 0)")
	}

	// Read handle data
	handle := make([]byte, handleLen)
	if err := binary.Read(reader, binary.BigEndian, &handle); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}

	// Skip padding to 4-byte boundary
	padding := (4 - (handleLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("failed to read handle padding byte %d: %w", i, err)
		}
	}

	// ========================================================================
	// Decode offset
	// ========================================================================

	var offset uint64
	if err := binary.Read(reader, binary.BigEndian, &offset); err != nil {
		return nil, fmt.Errorf("failed to read offset: %w", err)
	}

	// ========================================================================
	// Decode count
	// ========================================================================

	var count uint32
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("failed to read count: %w", err)
	}

	// ========================================================================
	// Decode stability level
	// ========================================================================

	var stable uint32
	if err := binary.Read(reader, binary.BigEndian, &stable); err != nil {
		return nil, fmt.Errorf("failed to read stable: %w", err)
	}

	// ========================================================================
	// Decode data
	// ========================================================================

	// Read data length
	var dataLen uint32
	if err := binary.Read(reader, binary.BigEndian, &dataLen); err != nil {
		return nil, fmt.Errorf("failed to read data length: %w", err)
	}

	// Validate data length is reasonable
	if dataLen > 1024*1024*1024 { // 1GB max
		return nil, fmt.Errorf("data length too large: %d bytes (max 1GB)", dataLen)
	}

	// Read data bytes
	writeData := make([]byte, dataLen)
	if err := binary.Read(reader, binary.BigEndian, &writeData); err != nil {
		return nil, fmt.Errorf("failed to read data: %w", err)
	}

	// Skip padding to 4-byte boundary (XDR alignment requirement)
	dataPadding := (4 - (dataLen % 4)) % 4
	for i := uint32(0); i < dataPadding; i++ {
		if _, err := reader.ReadByte(); err != nil {
			// Padding is optional at end of message, don't fail
			break
		}
	}

	logger.Debug("Decoded WRITE request: handle_len=%d offset=%d count=%d stable=%d data_len=%d",
		handleLen, offset, count, stable, dataLen)

	return &WriteRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
		Stable: stable,
		Data:   writeData,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the WriteResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.7 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. WCC data (weak cache consistency):
//     a. Pre-op attributes (present flag + wcc_attr if present)
//     b. Post-op attributes (present flag + file_attr if present)
//  3. If status == NFS3OK:
//     a. Count (4 bytes, big-endian uint32)
//     b. Committed (4 bytes, big-endian uint32)
//     c. Write verifier (8 bytes, big-endian uint64)
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
//	resp := &WriteResponse{
//	    Status:     NFS3OK,
//	    AttrBefore: wccAttr,
//	    AttrAfter:  fileAttr,
//	    Count:      1024,
//	    Committed:  FileSyncWrite,
//	    Verf:       12345,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *WriteResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write WCC data (Weak Cache Consistency)
	// ========================================================================
	// WCC data is included in both success and error cases to help
	// clients maintain cache consistency.

	// Write pre-op attributes
	if resp.AttrBefore != nil {
		// Present flag = TRUE (1)
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, fmt.Errorf("failed to write pre-op present flag: %w", err)
		}

		// Write WCC attributes (size, mtime, ctime)
		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Size); err != nil {
			return nil, fmt.Errorf("failed to write pre-op size: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Seconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op mtime seconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Mtime.Nseconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op mtime nseconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Seconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op ctime seconds: %w", err)
		}

		if err := binary.Write(&buf, binary.BigEndian, resp.AttrBefore.Ctime.Nseconds); err != nil {
			return nil, fmt.Errorf("failed to write pre-op ctime nseconds: %w", err)
		}
	} else {
		// Present flag = FALSE (0)
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, fmt.Errorf("failed to write pre-op absent flag: %w", err)
		}
	}

	// Post-op attributes
	if resp.AttrAfter != nil {
		if err := binary.Write(&buf, binary.BigEndian, uint32(1)); err != nil {
			return nil, err
		}
		if err := xdr.EncodeFileAttr(&buf, resp.AttrAfter); err != nil {
			return nil, err
		}
	} else {
		if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
			return nil, err
		}
	}

	if resp.Status != types.NFS3OK {
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, committed, and verifier
	// ========================================================================

	// Write count (number of bytes actually written)
	if err := binary.Write(&buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write committed (stability level actually achieved)
	if err := binary.Write(&buf, binary.BigEndian, resp.Committed); err != nil {
		return nil, fmt.Errorf("failed to write committed: %w", err)
	}

	// Write verifier (8 bytes - server instance identifier)
	if err := binary.Write(&buf, binary.BigEndian, resp.Verf); err != nil {
		return nil, fmt.Errorf("failed to write verifier: %w", err)
	}

	logger.Debug("Encoded WRITE response: %d bytes total, status=%d count=%d committed=%d",
		buf.Len(), resp.Status, resp.Count, resp.Committed)

	return buf.Bytes(), nil
}

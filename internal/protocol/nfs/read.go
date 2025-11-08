package nfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// ReadRequest represents a READ request from an NFS client.
// The client specifies a file handle, offset, and number of bytes to read.
//
// This structure is decoded from XDR-encoded data received over the network.
//
// RFC 1813 Section 3.3.6 specifies the READ procedure as:
//
//	READ3res NFSPROC3_READ(READ3args) = 6;
//
// The READ procedure is used to read data from a file. It's one of the most
// fundamental and frequently called NFS operations.
type ReadRequest struct {
	// Handle is the file handle of the file to read from.
	// Must be a valid file handle for a regular file (not a directory).
	// Maximum length is 64 bytes per RFC 1813.
	Handle []byte

	// Offset is the byte offset in the file to start reading from.
	// Can be any value from 0 to file size - 1.
	// Reading beyond EOF returns 0 bytes with Eof=true.
	Offset uint64

	// Count is the number of bytes to read.
	// The server may return fewer bytes than requested if:
	//   - EOF is encountered
	//   - Count exceeds server's maximum read size (rtmax from FSINFO)
	//   - Internal constraints apply
	Count uint32
}

// ReadResponse represents the response to a READ request.
// It contains the status, optional file attributes, and the data read.
//
// The response is encoded in XDR format before being sent back to the client.
type ReadResponse struct {
	// Status indicates the result of the read operation.
	// Common values:
	//   - NFS3OK (0): Success
	//   - NFS3ErrNoEnt (2): File handle not found
	//   - NFS3ErrIO (5): I/O error during read
	//   - NFS3ErrAcces (13): Permission denied
	//   - NFS3ErrIsDir (21): Handle is a directory, not a file
	//   - NFS3ErrStale (70): Stale file handle
	//   - NFS3ErrBadHandle (10001): Invalid file handle
	Status uint32

	// Attr contains post-operation attributes of the file.
	// Optional, may be nil if Status != NFS3OK or attributes unavailable.
	// Helps clients maintain cache consistency.
	Attr *FileAttr

	// Count is the actual number of bytes read.
	// May be less than requested if:
	//   - EOF was reached
	//   - Server constraints apply
	// Only present when Status == NFS3OK.
	Count uint32

	// Eof indicates whether the end of file was reached.
	// true: The read reached or passed the end of file
	// false: More data exists beyond the bytes returned
	// Only present when Status == NFS3OK.
	Eof bool

	// Data contains the actual bytes read from the file.
	// Length matches Count field.
	// Empty if Count == 0 or Status != NFS3OK.
	Data []byte
}

// ReadContext contains the context information needed to process a READ request.
// This includes client identification and authentication details for access control.
type ReadContext struct {
	// ClientAddr is the network address of the client making the request.
	// Format: "IP:port" (e.g., "192.168.1.100:1234")
	ClientAddr string

	// AuthFlavor is the authentication method used by the client.
	// Common values:
	//   - 0: AUTH_NULL (no authentication)
	//   - 1: AUTH_UNIX (Unix UID/GID authentication)
	AuthFlavor uint32

	// UID is the authenticated user ID (from AUTH_UNIX).
	// Used for read permission checks by the repository.
	// Only valid when AuthFlavor == AUTH_UNIX.
	UID *uint32

	// GID is the authenticated group ID (from AUTH_UNIX).
	// Used for read permission checks by the repository.
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

// Read reads data from a regular file.
//
// This implements the NFS READ procedure as defined in RFC 1813 Section 3.3.6.
//
// **Purpose:**
//
// READ is the fundamental operation for retrieving file data over NFS. It's used by:
//   - Applications reading file contents
//   - Editors loading files
//   - Compilers accessing source code
//   - Any operation that needs file data
//
// **Process:**
//
//  1. Validate request parameters (handle, offset, count)
//  2. Extract client IP and authentication credentials from context
//  3. Verify file exists and is a regular file (via repository)
//  4. Check read permissions (delegated to repository/content layer)
//  5. Open content for reading
//  6. Seek to requested offset
//  7. Read requested number of bytes
//  8. Detect EOF condition
//  9. Return data with updated file attributes
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Content repository handles actual data reading
//   - Metadata repository provides file attributes and validation
//   - Access control enforced by repository layers
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// Read permission checking should be implemented by:
//   - Repository layer for file existence validation
//   - Content repository layer for read access control
//
// **EOF Detection:**
//
// The server sets Eof=true when:
//   - The read operation reaches the end of the file
//   - The last byte of the file is included in the returned data
//   - offset + count >= file_size
//
// Clients use this to detect when they've read the entire file.
//
// **Performance Considerations:**
//
// READ is one of the most frequently called NFS procedures. Implementations should:
//   - Use efficient content repository access
//   - Support seekable readers when possible
//   - Minimize data copying
//   - Return reasonable chunk sizes (check FSINFO rtpref)
//   - Cache file attributes when possible
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// Repository/Content errors are mapped to NFS status codes:
//   - File not found → NFS3ErrNoEnt
//   - Not a regular file → NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - I/O error → NFS3ErrIO
//   - Stale handle → NFS3ErrStale
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - Repository/content layers enforce read permissions
//   - Client context enables audit logging
//   - No data leakage on permission errors
//
// **Parameters:**
//   - contentRepo: Content repository for file data access
//   - metadataRepo: Metadata repository for file attributes
//   - req: The read request containing handle, offset, and count
//   - ctx: Context with client address and authentication credentials
//
// **Returns:**
//   - *ReadResponse: Response with status, data, and attributes
//   - error: Returns error only for catastrophic internal failures; protocol-level
//     errors are indicated via the response Status field
//
// **RFC 1813 Section 3.3.6: READ Procedure**
//
// Example:
//
//	handler := &DefaultNFSHandler{}
//	req := &ReadRequest{
//	    Handle: fileHandle,
//	    Offset: 0,
//	    Count:  4096,
//	}
//	ctx := &ReadContext{
//	    ClientAddr: "192.168.1.100:1234",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Read(contentRepo, metadataRepo, req, ctx)
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == NFS3OK {
//	    // Process resp.Data (resp.Count bytes)
//	    if resp.Eof {
//	        // End of file reached
//	    }
//	}
func (h *DefaultNFSHandler) Read(
	contentRepo content.Repository,
	metadataRepo metadata.Repository,
	req *ReadRequest,
	ctx *ReadContext,
) (*ReadResponse, error) {
	// Extract client IP for logging
	clientIP := extractClientIP(ctx.ClientAddr)

	logger.Info("READ: handle=%x offset=%d count=%d client=%s auth=%d",
		req.Handle, req.Offset, req.Count, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadRequest(req); err != nil {
		logger.Warn("READ validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadResponse{Status: err.nfsStatus}, nil
	}

	// ========================================================================
	// Step 2: Verify file exists and is a regular file
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	attr, err := metadataRepo.GetFile(fileHandle)
	if err != nil {
		logger.Warn("READ failed: file not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadResponse{Status: NFS3ErrNoEnt}, nil
	}

	// Verify it's a regular file (not a directory or special file)
	if attr.Type != metadata.FileTypeRegular {
		logger.Warn("READ failed: not a regular file: handle=%x type=%d client=%s",
			req.Handle, attr.Type, clientIP)

		// Return file attributes even on error for cache consistency
		fileid := extractFileID(fileHandle)
		nfsAttr := MetadataToNFSAttr(attr, fileid)

		return &ReadResponse{
			Status: NFS3ErrIsDir, // NFS3ErrIsDir is used for all non-regular files
			Attr:   nfsAttr,
		}, nil
	}

	// ========================================================================
	// Step 3: Check for empty file or invalid offset
	// ========================================================================

	// If file has no content, return empty data with EOF
	if attr.ContentID == "" || attr.Size == 0 {
		logger.Debug("READ: empty file: handle=%x size=%d client=%s",
			req.Handle, attr.Size, clientIP)

		fileid := extractFileID(fileHandle)
		nfsAttr := MetadataToNFSAttr(attr, fileid)

		return &ReadResponse{
			Status: NFS3OK,
			Attr:   nfsAttr,
			Count:  0,
			Eof:    true,
			Data:   []byte{},
		}, nil
	}

	// If offset is at or beyond EOF, return empty data with EOF
	if req.Offset >= attr.Size {
		logger.Debug("READ: offset beyond EOF: handle=%x offset=%d size=%d client=%s",
			req.Handle, req.Offset, attr.Size, clientIP)

		fileid := extractFileID(fileHandle)
		nfsAttr := MetadataToNFSAttr(attr, fileid)

		return &ReadResponse{
			Status: NFS3OK,
			Attr:   nfsAttr,
			Count:  0,
			Eof:    true,
			Data:   []byte{},
		}, nil
	}

	// ========================================================================
	// Step 4: Open content for reading
	// ========================================================================
	// Access control should be enforced by the content repository
	// based on the file's ContentID and client credentials

	reader, err := contentRepo.ReadContent(attr.ContentID)
	if err != nil {
		logger.Error("READ failed: cannot open content: handle=%x content_id=%s client=%s error=%v",
			req.Handle, attr.ContentID, clientIP, err)

		fileid := extractFileID(fileHandle)
		nfsAttr := MetadataToNFSAttr(attr, fileid)

		return &ReadResponse{
			Status: NFS3ErrIO,
			Attr:   nfsAttr,
		}, nil
	}
	defer reader.Close()

	// ========================================================================
	// Step 5: Seek to requested offset
	// ========================================================================

	if req.Offset > 0 {
		if seeker, ok := reader.(io.Seeker); ok {
			// Reader supports seeking - use efficient seek
			_, err = seeker.Seek(int64(req.Offset), io.SeekStart)
			if err != nil {
				logger.Error("READ failed: seek error: handle=%x offset=%d client=%s error=%v",
					req.Handle, req.Offset, clientIP, err)

				fileid := extractFileID(fileHandle)
				nfsAttr := MetadataToNFSAttr(attr, fileid)

				return &ReadResponse{
					Status: NFS3ErrIO,
					Attr:   nfsAttr,
				}, nil
			}
		} else {
			// Reader doesn't support seeking - read and discard bytes
			logger.Debug("READ: reader not seekable, discarding %d bytes", req.Offset)

			discarded, err := io.CopyN(io.Discard, reader, int64(req.Offset))
			if err != nil && err != io.EOF {
				logger.Error("READ failed: cannot skip to offset: handle=%x offset=%d discarded=%d client=%s error=%v",
					req.Handle, req.Offset, discarded, clientIP, err)

				fileid := extractFileID(fileHandle)
				nfsAttr := MetadataToNFSAttr(attr, fileid)

				return &ReadResponse{
					Status: NFS3ErrIO,
					Attr:   nfsAttr,
				}, nil
			}

			// If we hit EOF while discarding, return empty with EOF
			if err == io.EOF {
				logger.Debug("READ: EOF reached while seeking: handle=%x offset=%d client=%s",
					req.Handle, req.Offset, clientIP)

				fileid := extractFileID(fileHandle)
				nfsAttr := MetadataToNFSAttr(attr, fileid)

				return &ReadResponse{
					Status: NFS3OK,
					Attr:   nfsAttr,
					Count:  0,
					Eof:    true,
					Data:   []byte{},
				}, nil
			}
		}
	}

	// ========================================================================
	// Step 6: Read requested data
	// ========================================================================

	data := make([]byte, req.Count)
	n, err := io.ReadFull(reader, data)

	// Determine EOF condition
	eof := false
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// We've reached EOF - this is not an error for READ
		eof = true
		data = data[:n] // Truncate buffer to actual bytes read
		err = nil       // Clear error - EOF is expected
	} else if err != nil {
		// Actual I/O error occurred
		logger.Error("READ failed: I/O error: handle=%x offset=%d client=%s error=%v",
			req.Handle, req.Offset, clientIP, err)

		fileid := extractFileID(fileHandle)
		nfsAttr := MetadataToNFSAttr(attr, fileid)

		return &ReadResponse{
			Status: NFS3ErrIO,
			Attr:   nfsAttr,
		}, nil
	}

	// Even if ReadFull succeeded, check if we're at or past EOF
	if req.Offset+uint64(n) >= attr.Size {
		eof = true
	}

	// ========================================================================
	// Step 7: Build success response
	// ========================================================================

	fileid := extractFileID(fileHandle)
	nfsAttr := MetadataToNFSAttr(attr, fileid)

	logger.Info("READ successful: handle=%x offset=%d requested=%d read=%d eof=%v client=%s",
		req.Handle, req.Offset, req.Count, n, eof, clientIP)

	logger.Debug("READ details: size=%d type=%d mode=%o",
		attr.Size, attr.Type, attr.Mode)

	return &ReadResponse{
		Status: NFS3OK,
		Attr:   nfsAttr,
		Count:  uint32(n),
		Eof:    eof,
		Data:   data,
	}, nil
}

// ============================================================================
// Request Validation
// ============================================================================

// readValidationError represents a READ request validation error.
type readValidationError struct {
	message   string
	nfsStatus uint32
}

func (e *readValidationError) Error() string {
	return e.message
}

// validateReadRequest validates READ request parameters.
//
// Checks performed:
//   - File handle is not empty and within limits
//   - File handle is long enough for file ID extraction
//   - Count is not zero (RFC 1813 allows it, but it's unusual)
//   - Count doesn't exceed reasonable limits
//
// Returns:
//   - nil if valid
//   - *readValidationError with NFS status if invalid
func validateReadRequest(req *ReadRequest) *readValidationError {
	// Validate file handle
	if len(req.Handle) == 0 {
		return &readValidationError{
			message:   "empty file handle",
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: NFS3ErrBadHandle,
		}
	}

	// Validate count - zero is technically valid but unusual
	if req.Count == 0 {
		logger.Debug("READ request with count=0 (unusual but valid)")
	}

	// Validate count doesn't exceed reasonable limits (1GB)
	// While RFC 1813 doesn't specify a maximum, extremely large reads should be rejected
	const maxReadSize = 1024 * 1024 * 1024 // 1GB
	if req.Count > maxReadSize {
		return &readValidationError{
			message:   fmt.Sprintf("read count too large: %d bytes (max %d)", req.Count, maxReadSize),
			nfsStatus: NFS3ErrInval,
		}
	}

	return nil
}

// ============================================================================
// XDR Decoding
// ============================================================================

// DecodeReadRequest decodes a READ request from XDR-encoded bytes.
//
// The decoding follows RFC 1813 Section 3.3.6 specifications:
//  1. File handle length (4 bytes, big-endian uint32)
//  2. File handle data (variable length, up to 64 bytes)
//  3. Padding to 4-byte boundary (0-3 bytes)
//  4. Offset (8 bytes, big-endian uint64)
//  5. Count (4 bytes, big-endian uint32)
//
// XDR encoding uses big-endian byte order and aligns data to 4-byte boundaries.
//
// Parameters:
//   - data: XDR-encoded bytes containing the READ request
//
// Returns:
//   - *ReadRequest: The decoded request containing handle, offset, and count
//   - error: Any error encountered during decoding (malformed data, invalid length)
//
// Example:
//
//	data := []byte{...} // XDR-encoded READ request from network
//	req, err := DecodeReadRequest(data)
//	if err != nil {
//	    // Handle decode error - send error reply to client
//	    return nil, err
//	}
//	// Use req.Handle, req.Offset, req.Count in READ procedure
func DecodeReadRequest(data []byte) (*ReadRequest, error) {
	// Validate minimum data length
	// 4 bytes (handle length) + 8 bytes (offset) + 4 bytes (count) = 16 bytes minimum
	if len(data) < 16 {
		return nil, fmt.Errorf("data too short: need at least 16 bytes, got %d", len(data))
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

	logger.Debug("Decoded READ request: handle_len=%d offset=%d count=%d",
		handleLen, offset, count)

	return &ReadRequest{
		Handle: handle,
		Offset: offset,
		Count:  count,
	}, nil
}

// ============================================================================
// XDR Encoding
// ============================================================================

// Encode serializes the ReadResponse into XDR-encoded bytes suitable for
// transmission over the network.
//
// The encoding follows RFC 1813 Section 3.3.6 specifications:
//  1. Status code (4 bytes, big-endian uint32)
//  2. Post-op attributes (present flag + attributes if present)
//  3. If status == NFS3OK:
//     a. Count (4 bytes, big-endian uint32)
//     b. Eof flag (4 bytes, big-endian bool as uint32)
//     c. Data length (4 bytes, big-endian uint32)
//     d. Data bytes (variable length)
//     e. Padding to 4-byte boundary (0-3 bytes)
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
//	resp := &ReadResponse{
//	    Status: NFS3OK,
//	    Attr:   fileAttr,
//	    Count:  1024,
//	    Eof:    false,
//	    Data:   dataBytes,
//	}
//	data, err := resp.Encode()
//	if err != nil {
//	    // Handle encoding error
//	    return nil, err
//	}
//	// Send 'data' to client over network
func (resp *ReadResponse) Encode() ([]byte, error) {
	var buf bytes.Buffer

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(&buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := encodeOptionalFileAttr(&buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early if status is not OK
	// ========================================================================

	if resp.Status != NFS3OK {
		logger.Debug("Encoding READ error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, EOF flag, and data
	// ========================================================================

	// Write count (number of bytes read)
	if err := binary.Write(&buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write EOF flag (boolean as uint32: 0=false, 1=true)
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(&buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("failed to write eof flag: %w", err)
	}

	// Write data as opaque (length + data + padding)
	dataLen := uint32(len(resp.Data))
	if err := binary.Write(&buf, binary.BigEndian, dataLen); err != nil {
		return nil, fmt.Errorf("failed to write data length: %w", err)
	}

	// Write data bytes
	if _, err := buf.Write(resp.Data); err != nil {
		return nil, fmt.Errorf("failed to write data: %w", err)
	}

	// Add padding to 4-byte boundary (XDR alignment requirement)
	padding := (4 - (dataLen % 4)) % 4
	for i := uint32(0); i < padding; i++ {
		if err := buf.WriteByte(0); err != nil {
			return nil, fmt.Errorf("failed to write data padding byte %d: %w", i, err)
		}
	}

	logger.Debug("Encoded READ response: %d bytes total, %d data bytes, status=%d",
		buf.Len(), dataLen, resp.Status)

	return buf.Bytes(), nil
}

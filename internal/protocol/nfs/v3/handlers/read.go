package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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
	NFSResponseBase // Embeds Status field and GetStatus() method

	// Attr contains post-operation attributes of the file.
	// Optional, may be nil if Status != types.NFS3OK or attributes unavailable.
	// Helps clients maintain cache consistency.
	Attr *types.NFSFileAttr

	// Count is the actual number of bytes read.
	// May be less than requested if:
	//   - EOF was reached
	//   - Server constraints apply
	// Only present when Status == types.NFS3OK.
	Count uint32

	// Eof indicates whether the end of file was reached.
	// true: The read reached or passed the end of file
	// false: More data exists beyond the bytes returned
	// Only present when Status == types.NFS3OK.
	Eof bool

	// Data contains the actual bytes read from the file.
	// Length matches Count field.
	// Empty if Count == 0 or Status != types.NFS3OK.
	Data []byte
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
//  1. Check for context cancellation (client disconnect, timeout)
//  2. Validate request parameters (handle, offset, count)
//  3. Extract client IP and authentication credentials from context
//  4. Verify file exists and is a regular file (via store)
//  5. Check read permissions (delegated to store/content layer)
//  6. Open content for reading
//  7. Seek to requested offset (with cancellation checks)
//  8. Read requested number of bytes (with cancellation checks during read)
//  9. Detect EOF condition
//  10. Return data with updated file attributes
//
// **Design Principles:**
//
//   - Protocol layer handles only XDR encoding/decoding and validation
//   - Content store handles actual data reading
//   - Metadata store provides file attributes and validation
//   - Access control enforced by store layers
//   - Context cancellation checked at key operation points
//   - Comprehensive logging at INFO level for operations, DEBUG for details
//
// **Authentication:**
//
// The context contains authentication credentials from the RPC layer.
// Read permission checking should be implemented by:
//   - store layer for file existence validation
//   - Content store layer for read access control
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
// **Context Cancellation:**
//
// READ operations can be time-consuming, especially for large files or slow storage.
// Context cancellation is checked at multiple points:
//   - Before starting the operation (client disconnect detection)
//   - After metadata lookup (before opening content)
//   - During seek operations (for non-seekable readers)
//   - During data reading (chunked reads for large transfers)
//
// Cancellation scenarios include:
//   - Client disconnects mid-transfer
//   - Client timeout expires
//   - Server shutdown initiated
//   - Network connection lost
//
// For large reads (>1MB), we use chunked reading with periodic cancellation checks
// to ensure responsive cancellation without excessive overhead.
//
// **Performance Considerations:**
//
// READ is one of the most frequently called NFS procedures. Implementations should:
//   - Use efficient content store access
//   - Support seekable readers when possible
//   - Minimize data copying
//   - Return reasonable chunk sizes (check FSINFO rtpref)
//   - Cache file attributes when possible
//   - Balance cancellation checks with performance (avoid checking too frequently)
//
// **Error Handling:**
//
// Protocol-level errors return appropriate NFS status codes.
// store/Content errors are mapped to NFS status codes:
//   - File not found → types.NFS3ErrNoEnt
//   - Not a regular file → types.NFS3ErrIsDir
//   - Permission denied → NFS3ErrAcces
//   - I/O error → types.NFS3ErrIO
//   - Stale handle → NFS3ErrStale
//   - Context cancelled → returns context error (client disconnect)
//
// **Security Considerations:**
//
//   - Handle validation prevents malformed requests
//   - store/content layers enforce read permissions
//   - Client context enables audit logging
//   - No data leakage on permission errors
//   - Cancellation prevents resource exhaustion
//
// **Parameters:**
//   - ctx: Context with client address, authentication, and cancellation support
//   - contentStore: Content repository for file data access
//   - metadataStore: Metadata store for file attributes
//   - req: The read request containing handle, offset, and count
//
// **Returns:**
//   - *ReadResponse: Response with status, data, and attributes
//   - error: Returns error for context cancellation or catastrophic internal failures;
//     protocol-level errors are indicated via the response Status field
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
//	ctx := &NFSHandlerContext{
//	    Context:    context.Background(),
//	    ClientAddr: "192.168.1.100:1234",
//	    Share:      "/export",
//	    AuthFlavor: 1, // AUTH_UNIX
//	    UID:        &uid,
//	    GID:        &gid,
//	}
//	resp, err := handler.Read(ctx, contentStore, metadataStore, req)
//	if err == context.Canceled {
//	    // Client disconnected during read
//	    return nil, err
//	}
//	if err != nil {
//	    // Internal server error
//	}
//	if resp.Status == types.NFS3OK {
//	    // Process resp.Data (resp.Count bytes)
//	    if resp.Eof {
//	        // End of file reached
//	    }
//	}
func (h *Handler) Read(
	ctx *NFSHandlerContext,
	req *ReadRequest,
) (*ReadResponse, error) {
	// ========================================================================
	// Context Cancellation Check - Entry Point
	// ========================================================================
	// Check if the client has disconnected or the request has timed out
	// before we start any expensive operations.
	select {
	case <-ctx.Context.Done():
		logger.Debug("READ: request cancelled at entry: handle=%x client=%s error=%v",
			req.Handle, ctx.ClientAddr, ctx.Context.Err())
		return nil, ctx.Context.Err()
	default:
		// Context not cancelled, continue processing
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.Info("READ: handle=%x offset=%d count=%d client=%s auth=%d",
		req.Handle, req.Offset, req.Count, clientIP, ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadRequest(req); err != nil {
		logger.Warn("READ validation failed: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// ========================================================================
	// Step 2: Decode share name from file handle
	// ========================================================================

	fileHandle := metadata.FileHandle(req.Handle)
	shareName, path, err := metadata.DecodeFileHandle(fileHandle)
	if err != nil {
		logger.Warn("READ failed: invalid file handle: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrBadHandle}}, nil
	}

	// Check if share exists
	if !h.Registry.ShareExists(shareName) {
		logger.Warn("READ failed: share not found: share=%s handle=%x client=%s",
			shareName, req.Handle, clientIP)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrStale}}, nil
	}

	// Get metadata store for this share
	metadataStore, err := h.Registry.GetMetadataStoreForShare(shareName)
	if err != nil {
		logger.Error("READ failed: cannot get metadata store: share=%s handle=%x client=%s error=%v",
			shareName, req.Handle, clientIP, err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	// Get content store for this share
	contentStore, err := h.Registry.GetContentStoreForShare(shareName)
	if err != nil {
		logger.Error("READ failed: cannot get content store: share=%s handle=%x client=%s error=%v",
			shareName, req.Handle, clientIP, err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	logger.Debug("READ: share=%s path=%s", shareName, path)

	// ========================================================================
	// Step 3: Verify file exists and is a regular file
	// ========================================================================

	file, err := metadataStore.GetFile(ctx.Context, fileHandle)
	if err != nil {
		// Check if error is due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("READ: metadata lookup cancelled: handle=%x client=%s",
				req.Handle, clientIP)
			return nil, err
		}

		logger.Warn("READ failed: file not found: handle=%x client=%s error=%v",
			req.Handle, clientIP, err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrNoEnt}}, nil
	}

	// Verify it's a regular file (not a directory or special file)
	if file.Type != metadata.FileTypeRegular {
		logger.Warn("READ failed: not a regular file: handle=%x type=%d client=%s",
			req.Handle, file.Type, clientIP)

		// Return file attributes even on error for cache consistency
		fileid := xdr.ExtractFileID(fileHandle)
		nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIsDir}, // types.NFS3ErrIsDir is used for all non-regular files
			Attr:            nfsAttr,
		}, nil
	}

	// ========================================================================
	// Context Cancellation Check - After Metadata Lookup
	// ========================================================================
	// Check again before opening content (which may be expensive)
	select {
	case <-ctx.Context.Done():
		logger.Debug("READ: request cancelled after metadata lookup: handle=%x client=%s",
			req.Handle, clientIP)
		return nil, ctx.Context.Err()
	default:
		// Context not cancelled, continue processing
	}

	// ========================================================================
	// Step 3: Check for empty file or invalid offset
	// ========================================================================

	// If file has no content, return empty data with EOF
	if file.ContentID == "" || file.Size == 0 {
		logger.Debug("READ: empty file: handle=%x size=%d client=%s",
			req.Handle, file.Size, clientIP)

		fileid := xdr.ExtractFileID(fileHandle)
		nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			Attr:            nfsAttr,
			Count:           0,
			Eof:             true,
			Data:            []byte{},
		}, nil
	}

	// If offset is at or beyond EOF, return empty data with EOF
	if req.Offset >= file.Size {
		logger.Debug("READ: offset beyond EOF: handle=%x offset=%d size=%d client=%s",
			req.Handle, req.Offset, file.Size, clientIP)

		fileid := xdr.ExtractFileID(fileHandle)
		nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			Attr:            nfsAttr,
			Count:           0,
			Eof:             true,
			Data:            []byte{},
		}, nil
	}

	// ========================================================================
	// Step 4: Read content data (with read-through cache)
	// ========================================================================
	// Three read paths (in priority order):
	//   1. Cache (if WriteCache available and has data) - fastest
	//   2. ReadAt (if content store supports it) - efficient for range reads
	//   3. ReadContent (fallback) - sequential read

	var data []byte
	var n int
	var readErr error
	var eof bool
	var readFromCache bool

	// Try cache first (if available)
	// TODO: Phase 5 - Access stores via registry
	/*
		if false { // Disabled until Phase 5
			cacheSize := h.WriteCache.Size(file.ContentID)
			if cacheSize > 0 {
				// Data exists in cache - read from cache
				logger.Debug("READ: using cache path: handle=%x offset=%d count=%d cache_size=%d content_id=%s",
					req.Handle, req.Offset, req.Count, cacheSize, file.ContentID)

				data = make([]byte, req.Count)
				n, readErr = h.WriteCache.ReadAt(file.ContentID, data, int64(req.Offset))

				if readErr == nil || readErr == io.EOF {
					readFromCache = true
					eof = (readErr == io.EOF) || (int64(req.Offset)+int64(n) >= cacheSize)
					logger.Debug("READ: cache hit: handle=%x bytes_read=%d eof=%v content_id=%s",
						req.Handle, n, eof, file.ContentID)
				} else {
					logger.Warn("READ: cache read error, falling back to content store: handle=%x content_id=%s error=%v",
						req.Handle, file.ContentID, readErr)
					readFromCache = false
				}
			}
		}
	*/
	_ = readFromCache // Suppress unused

	// If not read from cache, try content store
	if !readFromCache {
		// Check if content store supports efficient random-access reads
		if readAtStore, ok := contentStore.(content.ReadAtContentStore); ok {
			// ================================================================
			// FAST PATH: Use ReadAt for efficient range reads (S3, etc.)
			// ================================================================
			// This is dramatically more efficient for backends like S3:
			// - ReadAt: Uses HTTP range request for only requested bytes
			// - ReadContent: Downloads entire file then seeks/reads
			//
			// For a 100MB file with 4KB request:
			// - ReadAt: Downloads 4KB (efficient!)
			// - ReadContent: Downloads 100MB then uses 4KB (wasteful!)

			logger.Debug("READ: using content store ReadAt path: handle=%x offset=%d count=%d content_id=%s",
				req.Handle, req.Offset, req.Count, file.ContentID)

			data = make([]byte, req.Count)
			n, readErr = readAtStore.ReadAt(ctx.Context, file.ContentID, data, int64(req.Offset))

			// Handle ReadAt results
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				// EOF is not an error for READ operations
				eof = true
				data = data[:n] // Truncate to actual bytes read
			} else if readErr == context.Canceled || readErr == context.DeadlineExceeded {
				logger.Debug("READ: request cancelled during ReadAt: handle=%x offset=%d read=%d client=%s",
					req.Handle, req.Offset, n, clientIP)
				return nil, readErr
			} else if readErr != nil {
				logger.Error("READ failed: ReadAt error: handle=%x offset=%d client=%s error=%v",
					req.Handle, req.Offset, clientIP, readErr)

				fileid := xdr.ExtractFileID(fileHandle)
				nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

				return &ReadResponse{
					NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
					Attr:            nfsAttr,
				}, nil
			}
		} else {
			// ====================================================================
			// FALLBACK PATH: Use sequential ReadContent + Seek + Read
			// ====================================================================
			// This path is used for content stores that don't support ReadAt.
			// It's less efficient but works for all content stores.

			logger.Debug("READ: using sequential read path (no ReadAt support): handle=%x offset=%d count=%d",
				req.Handle, req.Offset, req.Count)

			reader, err := contentStore.ReadContent(ctx.Context, file.ContentID)
			if err != nil {
				logger.Error("READ failed: cannot open content: handle=%x content_id=%s client=%s error=%v",
					req.Handle, file.ContentID, clientIP, err)

				fileid := xdr.ExtractFileID(fileHandle)
				nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

				return &ReadResponse{
					NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
					Attr:            nfsAttr,
				}, nil
			}
			defer func() { _ = reader.Close() }()

			// Seek to requested offset
			if req.Offset > 0 {
				if seeker, ok := reader.(io.Seeker); ok {
					// Reader supports seeking - use efficient seek
					_, err = seeker.Seek(int64(req.Offset), io.SeekStart)
					if err != nil {
						logger.Error("READ failed: seek error: handle=%x offset=%d client=%s error=%v",
							req.Handle, req.Offset, clientIP, err)

						fileid := xdr.ExtractFileID(fileHandle)
						nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

						return &ReadResponse{
							NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
							Attr:            nfsAttr,
						}, nil
					}
				} else {
					// Reader doesn't support seeking - read and discard bytes
					logger.Debug("READ: reader not seekable, discarding %d bytes", req.Offset)

					// Use chunked discard with cancellation checks for large offsets
					const discardChunkSize = 64 * 1024 // 64KB chunks
					remaining := int64(req.Offset)
					totalDiscarded := int64(0)

					for remaining > 0 {
						// Check for cancellation during discard
						select {
						case <-ctx.Context.Done():
							logger.Debug("READ: request cancelled during seek discard: handle=%x offset=%d discarded=%d client=%s",
								req.Handle, req.Offset, totalDiscarded, clientIP)
							return nil, ctx.Context.Err()
						default:
							// Continue
						}

						// Discard in chunks
						chunkSize := discardChunkSize
						if remaining < int64(chunkSize) {
							chunkSize = int(remaining)
						}

						discardN, discardErr := io.CopyN(io.Discard, reader, int64(chunkSize))
						totalDiscarded += discardN
						remaining -= discardN

						if discardErr == io.EOF {
							// EOF reached while discarding - return empty with EOF
							logger.Debug("READ: EOF reached while seeking: handle=%x offset=%d client=%s",
								req.Handle, req.Offset, clientIP)

							fileid := xdr.ExtractFileID(fileHandle)
							nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

							return &ReadResponse{
								NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
								Attr:            nfsAttr,
								Count:           0,
								Eof:             true,
								Data:            []byte{},
							}, nil
						}

						if discardErr != nil {
							logger.Error("READ failed: cannot skip to offset: handle=%x offset=%d discarded=%d client=%s error=%v",
								req.Handle, req.Offset, totalDiscarded, clientIP, discardErr)

							fileid := xdr.ExtractFileID(fileHandle)
							nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

							return &ReadResponse{
								NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
								Attr:            nfsAttr,
							}, nil
						}
					}
				}
			}

			// Read requested data
			data = make([]byte, req.Count)

			// For large reads (>1MB), use chunked reading with cancellation checks
			const largeReadThreshold = 1024 * 1024 // 1MB
			if req.Count > largeReadThreshold {
				n, readErr = readWithCancellation(ctx.Context, reader, data)
			} else {
				n, readErr = io.ReadFull(reader, data)
			}

			// Handle read results
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				eof = true
				data = data[:n]
			} else if readErr == context.Canceled || readErr == context.DeadlineExceeded {
				logger.Debug("READ: request cancelled during data read: handle=%x offset=%d read=%d client=%s",
					req.Handle, req.Offset, n, clientIP)
				return nil, readErr
			} else if readErr != nil {
				logger.Error("READ failed: I/O error: handle=%x offset=%d client=%s error=%v",
					req.Handle, req.Offset, clientIP, readErr)

				fileid := xdr.ExtractFileID(fileHandle)
				nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

				return &ReadResponse{
					NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
					Attr:            nfsAttr,
				}, nil
			}
		}
	}

	// Even if ReadFull succeeded, check if we're at or past EOF
	if req.Offset+uint64(n) >= file.Size {
		eof = true
	}

	// ========================================================================
	// Step 7: Build success response
	// ========================================================================

	fileid := xdr.ExtractFileID(fileHandle)
	nfsAttr := xdr.MetadataToNFS(&file.FileAttr, fileid)

	logger.Info("READ successful: handle=%x offset=%d requested=%d read=%d eof=%v client=%s",
		req.Handle, req.Offset, req.Count, n, eof, clientIP)

	logger.Debug("READ details: size=%d type=%d mode=%o",
		file.Size, nfsAttr.Type, file.Mode)

	return &ReadResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Count:           uint32(n),
		Eof:             eof,
		Data:            data,
	}, nil
}

// readWithCancellation reads data from a reader with periodic context cancellation checks.
// This is used for large reads to ensure responsive cancellation without checking
// on every byte.
//
// The function reads in chunks, checking for cancellation between chunks to balance
// performance with responsiveness.
//
// Parameters:
//   - ctx: Context for cancellation detection
//   - reader: Source to read from
//   - buf: Destination buffer to fill
//
// Returns:
//   - int: Number of bytes actually read
//   - error: Any error encountered (including context cancellation)
func readWithCancellation(ctx context.Context, reader io.Reader, buf []byte) (int, error) {
	const chunkSize = 256 * 1024 // 256KB chunks for cancellation checks

	totalRead := 0
	remaining := len(buf)

	for remaining > 0 {
		// Check for cancellation before each chunk
		select {
		case <-ctx.Done():
			// Return what we've read so far along with context error
			return totalRead, ctx.Err()
		default:
			// Continue reading
		}

		// Determine chunk size for this iteration
		readSize := min(remaining, chunkSize)

		// Read chunk
		n, err := io.ReadFull(reader, buf[totalRead:totalRead+readSize])
		totalRead += n
		remaining -= n

		if err != nil {
			// Return total read and the error (could be EOF, io.ErrUnexpectedEOF, or I/O error)
			return totalRead, err
		}
	}

	return totalRead, nil
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
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// RFC 1813 specifies maximum handle size of 64 bytes
	if len(req.Handle) > 64 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too long: %d bytes (max 64)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
		}
	}

	// Handle must be at least 8 bytes for file ID extraction
	if len(req.Handle) < 8 {
		return &readValidationError{
			message:   fmt.Sprintf("file handle too short: %d bytes (min 8)", len(req.Handle)),
			nfsStatus: types.NFS3ErrBadHandle,
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
			nfsStatus: types.NFS3ErrInval,
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

	// PERFORMANCE OPTIMIZATION: Use stack-allocated buffer for file handles
	// File handles are max 64 bytes per RFC 1813, so we can avoid heap allocation
	var handleBuf [64]byte
	handleSlice := handleBuf[:handleLen]
	if err := binary.Read(reader, binary.BigEndian, &handleSlice); err != nil {
		return nil, fmt.Errorf("failed to read handle data: %w", err)
	}
	// Make a copy to return (original stack buffer will be reused)
	handle := make([]byte, handleLen)
	copy(handle, handleSlice)

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
//  3. If status == types.NFS3OK:
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
//	    NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
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
	// PERFORMANCE OPTIMIZATION: Pre-allocate buffer with estimated size
	// to avoid multiple allocations during encoding.
	//
	// Size calculation:
	//   - Status: 4 bytes
	//   - Optional file (present): 1 byte + ~84 bytes (NFSFileAttr)
	//   - Count: 4 bytes
	//   - EOF: 4 bytes
	//   - Data length: 4 bytes
	//   - Data: resp.Count bytes
	//   - Padding: 0-3 bytes
	//   Total: ~105 + data length + padding
	estimatedSize := 110 + int(resp.Count) + 3
	buf := bytes.NewBuffer(make([]byte, 0, estimatedSize))

	// ========================================================================
	// Write status code
	// ========================================================================

	if err := binary.Write(buf, binary.BigEndian, resp.Status); err != nil {
		return nil, fmt.Errorf("failed to write status: %w", err)
	}

	// ========================================================================
	// Write post-op attributes (both success and error cases)
	// ========================================================================

	if err := xdr.EncodeOptionalFileAttr(buf, resp.Attr); err != nil {
		return nil, fmt.Errorf("failed to encode attributes: %w", err)
	}

	// ========================================================================
	// Error case: Return early if status is not OK
	// ========================================================================

	if resp.Status != types.NFS3OK {
		logger.Debug("Encoding READ error response: status=%d", resp.Status)
		return buf.Bytes(), nil
	}

	// ========================================================================
	// Success case: Write count, EOF flag, and data
	// ========================================================================

	// Write count (number of bytes read)
	if err := binary.Write(buf, binary.BigEndian, resp.Count); err != nil {
		return nil, fmt.Errorf("failed to write count: %w", err)
	}

	// Write EOF flag (boolean as uint32: 0=false, 1=true)
	eofVal := uint32(0)
	if resp.Eof {
		eofVal = 1
	}
	if err := binary.Write(buf, binary.BigEndian, eofVal); err != nil {
		return nil, fmt.Errorf("failed to write eof flag: %w", err)
	}

	// Write data as opaque (length + data + padding)
	dataLen := uint32(len(resp.Data))
	if err := binary.Write(buf, binary.BigEndian, dataLen); err != nil {
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

package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/bufpool"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
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
//
// ReadResponse implements the Releaser interface. After encoding, Release()
// must be called to return any pooled buffers to the buffer pool.
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

	// pooled indicates the Data buffer came from bufpool and should be returned.
	pooled bool
}

// Release returns the Data buffer to the pool if it was pooled.
// Implements the Releaser interface.
// Safe to call multiple times - subsequent calls are no-ops.
func (r *ReadResponse) Release() {
	if r.pooled && r.Data != nil {
		bufpool.Put(r.Data)
		r.Data = nil
		r.pooled = false
	}
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
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READ: request cancelled at entry", "handle", fmt.Sprintf("0x%x", req.Handle), "client", ctx.ClientAddr, "error", ctx.Context.Err())
		return nil, ctx.Context.Err()
	}

	// Extract client IP for logging
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	logger.DebugCtx(ctx.Context, "READ", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "count", bytesize.ByteSize(req.Count), "client", clientIP, "auth", ctx.AuthFlavor)

	// ========================================================================
	// Step 1: Validate request parameters
	// ========================================================================

	if err := validateReadRequest(req); err != nil {
		logger.WarnCtx(ctx.Context, "READ validation failed", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP, "error", err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: err.nfsStatus}}, nil
	}

	// Clamp offset to OffsetMax per RFC 1813 (match Linux nfs3proc.c behavior)
	// This prevents issues with large offsets on certain platforms or backends
	if req.Offset > uint64(types.OffsetMax) {
		req.Offset = uint64(types.OffsetMax)
	}

	// ========================================================================
	// Step 2: Get content service from registry
	// ========================================================================

	payloadSvc, err := getPayloadService(h.Registry)
	if err != nil {
		logger.ErrorCtx(ctx.Context, "READ failed: payload service not initialized", "client", clientIP, "error", err)
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO}}, nil
	}

	fileHandle := metadata.FileHandle(req.Handle)

	logger.DebugCtx(ctx.Context, "READ: share", "share", ctx.Share)

	// ========================================================================
	// Step 3: Verify file exists and is a regular file
	// ========================================================================

	file, status, err := h.getFileOrError(ctx, fileHandle, "READ", req.Handle)
	if file == nil {
		return &ReadResponse{NFSResponseBase: NFSResponseBase{Status: status}}, err
	}

	// Verify it's a regular file (not a directory or special file)
	if file.Type != metadata.FileTypeRegular {
		logger.WarnCtx(ctx.Context, "READ failed: not a regular file", "handle", fmt.Sprintf("0x%x", req.Handle), "type", file.Type, "client", clientIP)

		// Return file attributes even on error for cache consistency
		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIsDir}, // types.NFS3ErrIsDir is used for all non-regular files
			Attr:            nfsAttr,
		}, nil
	}

	// ========================================================================
	// Context Cancellation Check - After Metadata Lookup
	// ========================================================================
	// Check again before opening content (which may be expensive)
	if ctx.isContextCancelled() {
		logger.DebugCtx(ctx.Context, "READ: request cancelled after metadata lookup", "handle", fmt.Sprintf("0x%x", req.Handle), "client", clientIP)
		return nil, ctx.Context.Err()
	}

	// ========================================================================
	// Step 3: Check for empty file or invalid offset
	// ========================================================================

	// If file has no content, return empty data with EOF
	if file.PayloadID == "" || file.Size == 0 {
		logger.DebugCtx(ctx.Context, "READ: empty file", "handle", fmt.Sprintf("0x%x", req.Handle), "size", bytesize.ByteSize(file.Size), "client", clientIP)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

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
		logger.DebugCtx(ctx.Context, "READ: offset beyond EOF", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "size", bytesize.ByteSize(file.Size), "client", clientIP)

		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
			Attr:            nfsAttr,
			Count:           0,
			Eof:             true,
			Data:            []byte{},
		}, nil
	}

	// Calculate actual read length (clamped to file size)
	readEnd := req.Offset + uint64(req.Count)
	if readEnd > file.Size {
		readEnd = file.Size
	}
	actualLength := uint32(readEnd - req.Offset)

	// ========================================================================
	// Step 4: Read content data from Cache
	// ========================================================================
	// All reads go through ContentService.ReadAt which reads from Cache.
	// Cache handles slice merging (newest-wins semantics).

	readResult, readErr := readFromPayloadService(ctx, payloadSvc, file.PayloadID, file.COWSourcePayloadID, req.Offset, actualLength, clientIP, req.Handle)
	if readErr != nil {
		// Check if cancellation error
		if readErr == context.Canceled || readErr == context.DeadlineExceeded {
			return nil, readErr
		}

		// I/O error
		traceError(ctx.Context, readErr, "READ failed", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", req.Offset, "client", clientIP)
		nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)
		return &ReadResponse{
			NFSResponseBase: NFSResponseBase{Status: types.NFS3ErrIO},
			Attr:            nfsAttr,
		}, nil
	}

	data := readResult.data
	n := readResult.bytesRead
	eof := readResult.eof
	pooled := readResult.pooled

	// Check if we're at or past EOF
	if req.Offset+uint64(n) >= file.Size {
		eof = true
	}

	// ========================================================================
	// Step 5: Build success response
	// ========================================================================

	nfsAttr := h.convertFileAttrToNFS(fileHandle, &file.FileAttr)

	logger.DebugCtx(ctx.Context, "READ successful", "handle", fmt.Sprintf("0x%x", req.Handle), "offset", bytesize.ByteSize(req.Offset), "requested", bytesize.ByteSize(req.Count), "read", bytesize.ByteSize(n), "eof", eof, "client", clientIP)

	logger.DebugCtx(ctx.Context, "READ details", "size", bytesize.ByteSize(file.Size), "type", nfsAttr.Type, "mode", fmt.Sprintf("%o", file.Mode))

	return &ReadResponse{
		NFSResponseBase: NFSResponseBase{Status: types.NFS3OK},
		Attr:            nfsAttr,
		Count:           uint32(n),
		Eof:             eof,
		Data:            data,
		pooled:          pooled,
	}, nil
}

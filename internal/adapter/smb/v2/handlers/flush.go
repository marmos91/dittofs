package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// FlushRequest represents an SMB2 FLUSH request from a client [MS-SMB2] 2.2.17.
//
// The client specifies a FileID for which all cached data should be flushed
// to stable storage. This is the SMB equivalent of NFS COMMIT or Unix fsync().
//
// This structure is decoded from little-endian binary data received over the network.
//
// **Wire Format (24 bytes):**
//
//	Offset  Size  Field           Description
//	------  ----  --------------  ----------------------------------
//	0       2     StructureSize   Always 24
//	2       2     Reserved1       Must be ignored
//	4       4     Reserved2       Must be ignored
//	8       16    FileId          SMB2 file identifier (persistent + volatile)
//
// **Use Cases:**
//
//   - Ensuring data durability before closing files
//   - File synchronization after buffered writes
//   - Transaction commit points
//   - Application-level fsync() requests
type FlushRequest struct {
	// FileID is the SMB2 file identifier returned by CREATE.
	// Both persistent (8 bytes) and volatile (8 bytes) parts must match
	// an open file handle on the server.
	FileID [16]byte
}

// FlushResponse represents an SMB2 FLUSH response [MS-SMB2] 2.2.18.
//
// The response is minimal - just a status code indicating success or failure.
// The actual flushing has already been performed by the time this response
// is sent.
//
// **Wire Format (4 bytes):**
//
//	Offset  Size  Field           Description
//	------  ----  --------------  ----------------------------------
//	0       2     StructureSize   Always 4
//	2       2     Reserved        Must be ignored
//
// **Status Codes:**
//
//   - StatusSuccess: All cached data has been flushed to stable storage
//   - StatusInvalidHandle: The FileID does not refer to a valid open file
//   - StatusUnexpectedIOError: The flush operation failed
type FlushResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method
}

// ============================================================================
// Encoding and Decoding
// ============================================================================

// DecodeFlushRequest parses an SMB2 FLUSH request from wire format [MS-SMB2] 2.2.17.
//
// The decoding extracts the FileID from the binary request body.
// All fields use little-endian byte order per SMB2 specification.
//
// **Parameters:**
//   - body: Raw request bytes (24 bytes minimum)
//
// **Returns:**
//   - *FlushRequest: The decoded request containing the FileID
//   - error: ErrRequestTooShort if body is less than 24 bytes
//
// **Example:**
//
//	body := []byte{...} // SMB2 FLUSH request from network
//	req, err := DecodeFlushRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter)
//	}
//	// Use req.FileID to locate the file to flush
func DecodeFlushRequest(body []byte) (*FlushRequest, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("FLUSH request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	r.Skip(8) // StructureSize(2) + Reserved1(2) + Reserved2(4)
	req := &FlushRequest{}
	copy(req.FileID[:], r.ReadBytes(16))
	if r.Err() != nil {
		return nil, fmt.Errorf("FLUSH decode error: %w", r.Err())
	}
	return req, nil
}

// Encode serializes the FlushResponse to SMB2 wire format [MS-SMB2] 2.2.18.
//
// The response is a minimal 4-byte structure indicating the flush result.
// The status code is conveyed in the SMB2 header, not in the response body.
//
// **Wire Format:**
//
//	Offset  Size  Field           Value
//	------  ----  --------------  ------
//	0       2     StructureSize   4
//	2       2     Reserved        0
//
// **Returns:**
//   - []byte: 4-byte encoded response body
//   - error: Always nil (encoding cannot fail for this simple structure)
//
// **Example:**
//
//	resp := &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}
//	data, _ := resp.Encode()
//	// Send data as response body after SMB2 header
func (resp *FlushResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(4)
	w.WriteUint16(4) // StructureSize
	w.WriteUint16(0) // Reserved
	if w.Err() != nil {
		return nil, w.Err()
	}
	return w.Bytes(), nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Flush handles SMB2 FLUSH command [MS-SMB2] 2.2.17, 2.2.18.
//
// **Purpose:**
//
// FLUSH requests that the server flush all cached data for the specified file
// to stable storage. This is the SMB equivalent of NFS COMMIT and Unix fsync().
//
// Key use cases:
//   - Ensuring data durability before closing files
//   - File synchronization after buffered writes
//   - Transaction commit points
//   - Application-level fsync() requests
//
// **Process:**
//
//  1. Validate FileID maps to an open file
//  2. Get metadata store and verify file exists
//  3. Flush cache to content store using shared flush logic
//  4. Flush pending metadata writes (deferred commit optimization)
//  5. Return success response
//
// **Cache Integration:**
//
// The flush strategy depends on the cache and content store configuration:
//   - With cache + IncrementalWriteStore (S3): Uses FlushIncremental for
//     streaming multipart uploads with parallel part uploads
//   - With cache + WriteAt stores (filesystem, memory): Writes only new bytes
//     since the last flush, using flushed offset tracking
//   - No cache: Immediate success since writes are synchronous
//
// After flushing, the cache state transitions to StateUploading so the
// background flusher can finalize the upload when the file becomes idle.
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidHandle: Invalid FileID
//   - StatusBadNetworkName: Share not found
//   - StatusInternalError: Content store unavailable
//   - StatusUnexpectedIOError: Flush operation failed
//   - StatusSuccess: Flush completed (or no-op if no cache)
//
// **Performance Considerations:**
//
// FLUSH can be expensive (triggers I/O and potential network operations):
//   - Clients should batch flushes when possible
//   - For S3 backends, uses incremental multipart uploads to minimize latency
//   - For filesystem backends, only flushes new data since last flush
//
// **Shared Logic:**
//
// Uses cache.FlushCacheToContentStore() which is shared with NFS COMMIT handler
// to ensure consistent flush behavior across protocols.
//
// **Example:**
//
//	req := &FlushRequest{FileID: fileID}
//	resp, err := handler.Flush(ctx, req)
//	if resp.GetStatus() != types.StatusSuccess {
//	    // Handle flush failure
//	}
func (h *Handler) Flush(ctx *SMBHandlerContext, req *FlushRequest) (*FlushResponse, error) {
	logger.Debug("FLUSH request", "fileID", fmt.Sprintf("%x", req.FileID))

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("FLUSH: file handle not found (closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileClosed}}, nil
	}

	// ========================================================================
	// Step 2: Get services and verify file exists
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	payloadSvc := h.Registry.GetBlockService()

	// Verify file exists
	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("FLUSH: file not found", "path", openFile.Path, "error", err)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// Check if there's content to flush
	if file.PayloadID == "" {
		logger.Debug("FLUSH: no content to flush", "path", openFile.Path)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
	}

	// ========================================================================
	// Step 3: Flush data using ContentService (same as NFS COMMIT)
	// ========================================================================

	_, flushErr := payloadSvc.Flush(ctx.Context, file.PayloadID)
	if flushErr != nil {
		logger.Warn("FLUSH: failed", "path", openFile.Path, "error", flushErr)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUnexpectedIOError}}, nil
	}

	// ========================================================================
	// Step 4: Flush pending metadata writes (deferred commit optimization)
	// ========================================================================
	//
	// The MetadataService uses deferred commits by default for performance.
	// This means CommitWrite only records changes in pending state, not to the store.
	// We must call FlushPendingWriteForFile to persist the metadata changes.
	// Without this, file size and other metadata changes are lost.

	authCtx, authErr := BuildAuthContext(ctx)
	if authErr != nil {
		logger.Warn("FLUSH: failed to build auth context for metadata flush", "path", openFile.Path, "error", authErr)
	} else {
		flushed, metaErr := metaSvc.FlushPendingWriteForFile(authCtx, openFile.MetadataHandle)
		if metaErr != nil {
			logger.Warn("FLUSH: metadata flush failed", "path", openFile.Path, "error", metaErr)
			// Continue - content is flushed, metadata will be fixed eventually
		} else if flushed {
			logger.Debug("FLUSH: metadata flushed", "path", openFile.Path)
		}
	}

	logger.Debug("FLUSH successful", "path", openFile.Path)

	// ========================================================================
	// Step 5: Return success response
	// ========================================================================

	return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
}

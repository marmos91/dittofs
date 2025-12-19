package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/ops"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
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

	req := &FlushRequest{}
	copy(req.FileID[:], body[8:24])

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
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint16(buf[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], 0) // Reserved
	return buf, nil
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
//  3. Check if cache has data to flush
//  4. Flush cache to content store using shared flush logic
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
// Uses ops.FlushCacheToContentStore() which is shared with NFS COMMIT handler
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
		logger.Debug("FLUSH: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// ========================================================================
	// Step 2: Get stores and cache
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("FLUSH: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadNetworkName}}, nil
	}

	// Verify file exists
	file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("FLUSH: file not found", "path", openFile.Path, "error", err)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// Get cache for share
	fileCache := h.Registry.GetCacheForShare(openFile.ShareName)
	if fileCache == nil {
		// No cache configured - writes go directly to store, nothing to flush
		logger.Debug("FLUSH: no cache configured (sync mode)", "path", openFile.Path)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
	}

	// Check if there's data to flush
	if file.ContentID == "" || fileCache.Size(file.ContentID) == 0 {
		logger.Debug("FLUSH: no cached data to flush", "path", openFile.Path)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
	}

	// ========================================================================
	// Step 3: Flush cache to content store using shared flush logic
	// ========================================================================

	contentStore, err := h.Registry.GetContentStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("FLUSH: failed to get content store", "share", openFile.ShareName, "error", err)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}, nil
	}

	// Flush using shared cache flush logic (same as NFS COMMIT)
	_, flushErr := ops.FlushCacheToContentStore(ctx.Context, fileCache, contentStore, file.ContentID)
	if flushErr != nil {
		logger.Warn("FLUSH: cache flush failed", "path", openFile.Path, "error", flushErr)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusUnexpectedIOError}}, nil
	}

	logger.Debug("FLUSH successful", "path", openFile.Path)

	// ========================================================================
	// Step 4: Return success response
	// ========================================================================

	return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
}

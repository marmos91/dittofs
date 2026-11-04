package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/common"
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
//  3. Flush pending metadata writes (deferred commit optimization)
//  4. An empty file succeeds; any other answers the I/O status, since no
//     content store is wired (common.ErrNoContentStore)
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidHandle: Invalid FileID
//   - StatusBadNetworkName: Share not found
//   - StatusUnexpectedIOError: a file with content (no content store is wired)
//   - StatusSuccess: an empty file, after its pending metadata is flushed
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

	path := openFile.Name().Path

	// ========================================================================
	// Step 2: Get services and verify file exists
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()
	// Verify file exists
	file, err := metaSvc.GetFileForRead(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("FLUSH: file not found", "path", path, "error", err)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(err)}}, nil
	}

	// Check if there's content to flush
	if file.PayloadID == "" {
		logger.Debug("FLUSH: no content to flush", "path", path)
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
	}

	// An empty file has no content to flush, only pending metadata. Any other
	// file's content would have to be made durable, and no content store is
	// wired. The handle must belong to this request's session before anything
	// about its content is answered.
	if file.Size != 0 {
		if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
			return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: status}}, nil
		}
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(common.ErrNoContentStore)}}, nil
	}

	// ========================================================================
	// Step 4: Flush pending metadata writes (deferred commit optimization)
	// ========================================================================
	//
	// The MetadataService uses deferred commits by default for performance.
	// This means CommitWrite only records changes in pending state, not to the store.
	// We must call FlushPendingWriteForFile to persist the metadata changes.
	// Without this, file size and other metadata changes are lost.

	// Prime ctx.User / IsGuest / TreeID from the OpenFile's recorded session
	// BEFORE BuildAuthContext — otherwise ctx.User==nil falls into the
	// anonymous arm and synthesises UID-0 (root), bypassing DACL checks on
	// the metadata flush (#619, same class as #603).
	if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
		return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: status}}, nil
	}

	authCtx, authErr := BuildAuthContext(ctx)
	if authErr != nil {
		logger.Warn("FLUSH: failed to build auth context for metadata flush", "path", path, "error", authErr)
	} else {
		// STRICT (durable=true): SMB FLUSH is an explicit client fsync request, so
		// the metadata commit must fsync inline — deliberately NOT relaxed (#1687).
		flushed, metaErr := metaSvc.FlushPendingWriteForFile(authCtx, openFile.MetadataHandle, true)
		if metaErr != nil {
			logger.Warn("FLUSH: metadata flush failed", "path", path, "error", metaErr)
			// Continue - metadata will be fixed eventually
		} else if flushed {
			logger.Debug("FLUSH: metadata flushed", "path", path)
		}
	}

	// Samba parity (fileio.c::trigger_write_time_update_immediate): FLUSH
	// collapses the pending write-time window so the next QUERY_INFO sees
	// the post-write Mtime.
	flushSmbDelayedWrite(openFile)
	h.StoreOpenFile(openFile)

	logger.Debug("FLUSH successful", "path", path)

	// ========================================================================
	// Step 5: Return success response
	// ========================================================================

	return &FlushResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess}}, nil
}

package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/ops"
)

// Flush handles SMB2 FLUSH command [MS-SMB2] 2.2.17, 2.2.18
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
//  1. Decode request to extract FileID
//  2. Validate FileID maps to an open file
//  3. Get metadata store and verify file exists
//  4. Check if cache has data to flush
//  5. Flush cache to content store using shared flush logic
//  6. Return success response
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
func (h *Handler) Flush(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeFlushRequest(body)
	if err != nil {
		logger.Debug("FLUSH: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("FLUSH request", "fileID", fmt.Sprintf("%x", req.FileID))

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("FLUSH: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Get stores and cache
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("FLUSH: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// Verify file exists
	file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("FLUSH: file not found", "path", openFile.Path, "error", err)
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// Get cache for share
	fileCache := h.Registry.GetCacheForShare(openFile.ShareName)
	if fileCache == nil {
		// No cache configured - writes go directly to store, nothing to flush
		logger.Debug("FLUSH: no cache configured (sync mode)", "path", openFile.Path)
		respBytes, _ := EncodeFlushResponse(&FlushResponse{})
		return NewResult(types.StatusSuccess, respBytes), nil
	}

	// Check if there's data to flush
	if file.ContentID == "" || fileCache.Size(file.ContentID) == 0 {
		logger.Debug("FLUSH: no cached data to flush", "path", openFile.Path)
		respBytes, _ := EncodeFlushResponse(&FlushResponse{})
		return NewResult(types.StatusSuccess, respBytes), nil
	}

	// ========================================================================
	// Step 4: Flush cache to content store using shared flush logic
	// ========================================================================

	contentStore, err := h.Registry.GetContentStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("FLUSH: failed to get content store", "share", openFile.ShareName, "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	// Flush using shared cache flush logic (same as NFS COMMIT)
	_, flushErr := ops.FlushCacheToContentStore(ctx.Context, fileCache, contentStore, file.ContentID)
	if flushErr != nil {
		logger.Warn("FLUSH: cache flush failed", "path", openFile.Path, "error", flushErr)
		return NewErrorResult(types.StatusUnexpectedIOError), nil
	}

	logger.Debug("FLUSH successful", "path", openFile.Path)

	// ========================================================================
	// Step 5: Encode response
	// ========================================================================

	respBytes, _ := EncodeFlushResponse(&FlushResponse{})
	return NewResult(types.StatusSuccess, respBytes), nil
}

package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/bytesize"
)

// Write handles SMB2 WRITE command [MS-SMB2] 2.2.21, 2.2.22
//
// **Purpose:**
//
// WRITE allows clients to write data to an open file at a specified offset.
// SMB2 clients typically write in 32KB chunks (similar to NFS 32KB writes).
//
// **Process:**
//
//  1. Decode request to extract FileID, offset, and data
//  2. Validate FileID maps to an open file (not a directory)
//  3. Get session and tree connection for permission checking
//  4. Verify write permission at share level
//  5. Get metadata and content stores for the share
//  6. Build AuthContext for permission validation
//  7. PrepareWrite - validate permissions and get ContentID
//  8. Write data to cache (async) or content store (sync)
//  9. CommitWrite - update file metadata (size, timestamps)
//  10. Return success response with bytes written
//
// **Cache Integration:**
//
// Write behavior depends on cache configuration:
//   - With cache (async mode): Writes go to cache first, flushed on FLUSH
//   - Without cache (sync mode): Writes go directly to content store
//
// Async mode is preferred for performance as it allows batching small writes
// and reduces latency. Clients can call FLUSH when durability is required.
//
// **Two-Phase Write Pattern:**
//
// DittoFS uses a two-phase write pattern to maintain consistency:
//   - PrepareWrite: Validates permissions, doesn't modify metadata
//   - WriteAt: Writes data to cache or content store
//   - CommitWrite: Updates metadata (size, mtime) after successful write
//
// This ensures metadata reflects actual data state and provides rollback
// capability if the write fails.
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request
//   - StatusInvalidHandle: Invalid FileID
//   - StatusInvalidDeviceRequest: Cannot write to directory
//   - StatusUserSessionDeleted: Session no longer valid
//   - StatusAccessDenied: Write permission denied
//   - StatusBadNetworkName: Share not found
//   - StatusUnexpectedIOError: Cache or content store write failed
//   - StatusInternalError: Metadata or encoding error
//
// **Performance Considerations:**
//
// WRITE is frequently called and performance-critical:
//   - Uses cache for async writes (reduces latency)
//   - SMB clients typically use 32KB write chunks
//   - ContentID caching in OpenFile reduces metadata lookups
//   - Parallel writes to different files are supported
func (h *Handler) Write(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeWriteRequest(body)
	if err != nil {
		logger.Debug("WRITE: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("WRITE request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"offset", req.Offset,
		"length", req.Length)

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("WRITE: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Validate file type
	// ========================================================================

	if openFile.IsDirectory {
		logger.Debug("WRITE: cannot write to directory", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// ========================================================================
	// Step 4: Get session and tree connection
	// ========================================================================

	tree, ok := h.GetTree(openFile.TreeID)
	if !ok {
		logger.Debug("WRITE: invalid tree ID", "treeID", openFile.TreeID)
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	sess, ok := h.GetSession(openFile.SessionID)
	if !ok {
		logger.Debug("WRITE: invalid session ID", "sessionID", openFile.SessionID)
		return NewErrorResult(types.StatusUserSessionDeleted), nil
	}

	// Update context
	ctx.ShareName = tree.ShareName
	ctx.User = sess.User
	ctx.IsGuest = sess.IsGuest
	ctx.Permission = tree.Permission

	// ========================================================================
	// Step 5: Check write permission at share level
	// ========================================================================

	if !HasWritePermission(ctx) {
		logger.Debug("WRITE: access denied", "path", openFile.Path, "permission", ctx.Permission)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// ========================================================================
	// Step 6: Get metadata and content stores
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("WRITE: failed to get metadata store", "share", tree.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	contentStore, err := h.Registry.GetContentStoreForShare(tree.ShareName)
	if err != nil {
		logger.Warn("WRITE: failed to get content store", "share", tree.ShareName, "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	// Get unified cache for this share (optional - nil means sync mode)
	cache := h.Registry.GetCacheForShare(tree.ShareName)

	// ========================================================================
	// Step 7: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("WRITE: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// ========================================================================
	// Step 8: Prepare write operation
	// ========================================================================

	newSize := req.Offset + uint64(len(req.Data))
	writeOp, err := metadataStore.PrepareWrite(authCtx, openFile.MetadataHandle, newSize)
	if err != nil {
		logger.Debug("WRITE: prepare failed", "path", openFile.Path, "error", err)
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// ========================================================================
	// Step 9: Write data to storage (cache or direct to content store)
	// ========================================================================

	bytesWritten := len(req.Data)

	if cache != nil {
		// Async mode: write to cache, will be flushed on FLUSH
		err = cache.WriteAt(authCtx.Context, writeOp.ContentID, req.Data, req.Offset)
		if err != nil {
			logger.Warn("WRITE: cache write failed", "path", openFile.Path, "error", err)
			return NewErrorResult(types.StatusUnexpectedIOError), nil
		}
		logger.Debug("WRITE: cached successfully",
			"path", openFile.Path,
			"content_id", writeOp.ContentID,
			"cache_size", bytesize.ByteSize(cache.Size(writeOp.ContentID)))
	} else {
		// Sync mode: write directly to content store
		err = contentStore.WriteAt(authCtx.Context, writeOp.ContentID, req.Data, req.Offset)
		if err != nil {
			logger.Warn("WRITE: content write failed", "path", openFile.Path, "error", err)
			return NewErrorResult(ContentErrorToSMBStatus(err)), nil
		}
	}

	// ========================================================================
	// Step 10: Commit write operation
	// ========================================================================

	_, err = metadataStore.CommitWrite(authCtx, writeOp)
	if err != nil {
		logger.Warn("WRITE: commit failed", "path", openFile.Path, "error", err)
		// Data was written but metadata not updated - this is an inconsistent state
		// but we still report the error
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// Update cached ContentID in OpenFile
	openFile.ContentID = writeOp.ContentID

	logger.Debug("WRITE successful",
		"path", openFile.Path,
		"offset", req.Offset,
		"bytes", bytesWritten)

	// ========================================================================
	// Step 11: Build and encode response
	// ========================================================================

	resp := &WriteResponse{
		Count:     uint32(bytesWritten),
		Remaining: 0,
	}

	respBytes, err := EncodeWriteResponse(resp)
	if err != nil {
		logger.Warn("WRITE: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/ops"
)

// Close handles SMB2 CLOSE command [MS-SMB2] 2.2.15, 2.2.16
//
// **Purpose:**
//
// CLOSE releases the file handle and ensures all data is persisted to storage.
// This is a critical durability point - when CLOSE completes, the client
// expects data to be safely stored.
//
// **Process:**
//
//  1. Decode request to extract FileID
//  2. Validate FileID maps to an open file
//  3. Flush any cached data to content store (ensures durability)
//  4. Optionally return final file attributes (POSTQUERY_ATTRIB flag)
//  5. Remove the open file handle
//  6. Return success response
func (h *Handler) Close(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeCloseRequest(body)
	if err != nil {
		logger.Debug("CLOSE: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("CLOSE request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"flags", req.Flags)

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("CLOSE: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// ========================================================================
	// Step 3: Flush cached data to content store (ensures durability)
	// ========================================================================

	// Flush and finalize cached data to ensure durability
	// Unlike NFS COMMIT which just flushes, SMB CLOSE requires immediate durability
	if !openFile.IsDirectory && openFile.ContentID != "" {
		fileCache := h.Registry.GetCacheForShare(openFile.ShareName)
		if fileCache != nil && fileCache.Size(openFile.ContentID) > 0 {
			contentStore, err := h.Registry.GetContentStoreForShare(openFile.ShareName)
			if err == nil {
				// Use FlushAndFinalizeCache for immediate durability (completes S3 uploads)
				_, flushErr := ops.FlushAndFinalizeCache(ctx.Context, fileCache, contentStore, openFile.ContentID)
				if flushErr != nil {
					logger.Warn("CLOSE: cache flush failed", "path", openFile.Path, "error", flushErr)
					// Continue with close even if flush fails - data is in cache
					// and background flusher will eventually persist it
				} else {
					logger.Debug("CLOSE: flushed and finalized", "path", openFile.Path, "contentID", openFile.ContentID)
				}
			}
		}
	}

	// ========================================================================
	// Step 4: Build response with optional attributes
	// ========================================================================

	resp := &CloseResponse{
		Flags: req.Flags,
	}

	// If SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB was set, return file attributes
	if types.CloseFlags(req.Flags)&types.SMB2ClosePostQueryAttrib != 0 {
		// Get metadata store to retrieve final attributes
		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err == nil {
			file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
			if err == nil {
				creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
				allocationSize := ((file.Size + 4095) / 4096) * 4096

				resp.CreationTime = creation
				resp.LastAccessTime = access
				resp.LastWriteTime = write
				resp.ChangeTime = change
				resp.AllocationSize = allocationSize
				resp.EndOfFile = file.Size
				resp.FileAttributes = FileAttrToSMBAttributes(&file.FileAttr)
			}
		}
	}

	// ========================================================================
	// Step 5: Handle delete-on-close (FileDispositionInformation)
	// ========================================================================

	if openFile.DeletePending {
		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err != nil {
			logger.Warn("CLOSE: failed to get metadata store for delete", "share", openFile.ShareName, "error", err)
		} else {
			authCtx, err := BuildAuthContext(ctx, h.Registry)
			if err != nil {
				logger.Warn("CLOSE: failed to build auth context for delete", "error", err)
			} else {
				if openFile.IsDirectory {
					// Delete directory
					err = metadataStore.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName)
					if err != nil {
						logger.Debug("CLOSE: failed to delete directory", "path", openFile.Path, "error", err)
						// Continue with close even if delete fails
					} else {
						logger.Debug("CLOSE: directory deleted", "path", openFile.Path)
					}
				} else {
					// Delete file
					_, err = metadataStore.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName)
					if err != nil {
						logger.Debug("CLOSE: failed to delete file", "path", openFile.Path, "error", err)
						// Continue with close even if delete fails
					} else {
						logger.Debug("CLOSE: file deleted", "path", openFile.Path)
					}
				}
			}
		}
	}

	// ========================================================================
	// Step 6: Remove the open file handle
	// ========================================================================

	h.DeleteOpenFile(req.FileID)

	logger.Debug("CLOSE successful",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"path", openFile.Path)

	// ========================================================================
	// Step 7: Encode response
	// ========================================================================

	respBytes, err := EncodeCloseResponse(resp)
	if err != nil {
		logger.Warn("CLOSE: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

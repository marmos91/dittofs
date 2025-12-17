package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/mfsymlink"
	"github.com/marmos91/dittofs/pkg/ops"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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
	// Step 3.5: Check for MFsymlink conversion
	// ========================================================================
	//
	// macOS/Windows SMB clients create symlinks by writing MFsymlink content
	// (1067-byte files with XSym\n header). On CLOSE, we convert these to
	// real symlinks in the metadata store for NFS interoperability.

	if !openFile.IsDirectory && openFile.ContentID != "" && !openFile.DeletePending {
		if converted, _ := h.checkAndConvertMFsymlink(ctx, openFile); converted {
			logger.Debug("CLOSE: converted MFsymlink to symlink", "path", openFile.Path)
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

// checkAndConvertMFsymlink checks if a file is an MFsymlink and converts it to a real symlink.
//
// MFsymlinks are 1067-byte files with XSym\n header used by macOS/Windows SMB clients
// for symlink creation. This function:
// 1. Checks file size is exactly 1067 bytes
// 2. Reads content and verifies MFsymlink format
// 3. Parses the symlink target
// 4. Removes the regular file
// 5. Creates a real symlink with the same name
//
// Returns (true, nil) if conversion succeeded, (false, nil) if not an MFsymlink,
// or (false, error) if conversion failed.
func (h *Handler) checkAndConvertMFsymlink(ctx *SMBHandlerContext, openFile *OpenFile) (bool, error) {
	// Get metadata store
	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		return false, err
	}

	// Get file metadata to check size
	file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		return false, err
	}

	// Quick check: must be exactly 1067 bytes
	if file.Size != mfsymlink.Size {
		return false, nil
	}

	// Must be a regular file (not already a symlink)
	if file.Type != metadata.FileTypeRegular {
		return false, nil
	}

	// Read content to verify MFsymlink format
	content, err := h.readMFsymlinkContent(ctx, openFile)
	if err != nil {
		logger.Debug("CLOSE: failed to read MFsymlink content", "path", openFile.Path, "error", err)
		return false, nil // Not fatal, just don't convert
	}

	// Verify it's actually an MFsymlink
	if !mfsymlink.IsMFsymlink(content) {
		return false, nil
	}

	// Parse the symlink target
	target, err := mfsymlink.Decode(content)
	if err != nil {
		logger.Debug("CLOSE: invalid MFsymlink format", "path", openFile.Path, "error", err)
		return false, nil // Don't convert invalid MFsymlinks
	}

	// Convert to real symlink
	err = h.convertToRealSymlink(ctx, openFile, target)
	if err != nil {
		logger.Warn("CLOSE: failed to convert MFsymlink to symlink",
			"path", openFile.Path,
			"target", target,
			"error", err)
		return false, err
	}

	return true, nil
}

// readMFsymlinkContent reads the content of a potential MFsymlink file.
// It tries the cache first, then falls back to the content store.
func (h *Handler) readMFsymlinkContent(ctx *SMBHandlerContext, openFile *OpenFile) ([]byte, error) {
	// Try reading from cache first
	fileCache := h.Registry.GetCacheForShare(openFile.ShareName)
	if fileCache != nil && fileCache.Size(openFile.ContentID) > 0 {
		data := make([]byte, mfsymlink.Size)
		n, err := fileCache.ReadAt(ctx.Context, openFile.ContentID, data, 0)
		if err == nil && n == mfsymlink.Size {
			return data, nil
		}
	}

	// Fall back to content store
	contentStore, err := h.Registry.GetContentStoreForShare(openFile.ShareName)
	if err != nil {
		return nil, err
	}

	reader, err := contentStore.ReadContent(ctx.Context, openFile.ContentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	data := make([]byte, mfsymlink.Size)
	totalRead := 0
	for totalRead < mfsymlink.Size {
		n, err := reader.Read(data[totalRead:])
		if err != nil {
			if totalRead > 0 {
				break
			}
			return nil, err
		}
		totalRead += n
	}

	return data[:totalRead], nil
}

// convertToRealSymlink removes the regular file and creates a symlink in its place.
func (h *Handler) convertToRealSymlink(ctx *SMBHandlerContext, openFile *OpenFile, target string) error {
	// Validate required fields
	if len(openFile.ParentHandle) == 0 || openFile.FileName == "" {
		return fmt.Errorf("missing parent handle or filename for MFsymlink conversion")
	}

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		return err
	}

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		return err
	}

	// Get the parent handle and filename for removal and creation
	parentHandle := openFile.ParentHandle
	fileName := openFile.FileName

	// Remove the regular file
	_, err = metadataStore.RemoveFile(authCtx, parentHandle, fileName)
	if err != nil {
		return fmt.Errorf("failed to remove MFsymlink file: %w", err)
	}

	// Delete content from content store (optional - ignore errors)
	if openFile.ContentID != "" {
		contentStore, err := h.Registry.GetContentStoreForShare(openFile.ShareName)
		if err == nil {
			_ = contentStore.Delete(ctx.Context, openFile.ContentID)
		}
	}

	// Create the real symlink with default attributes
	// Pass empty FileAttr - CreateSymlink will apply defaults
	symlinkAttr := &metadata.FileAttr{}
	_, err = metadataStore.CreateSymlink(authCtx, parentHandle, fileName, target, symlinkAttr)
	if err != nil {
		return fmt.Errorf("failed to create symlink: %w", err)
	}

	logger.Debug("CLOSE: converted MFsymlink",
		"path", openFile.Path,
		"target", target)

	return nil
}

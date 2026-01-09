package handlers

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/internal/protocol/cache"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// CloseRequest represents an SMB2 CLOSE request from a client [MS-SMB2] 2.2.15.
//
// The client specifies a FileID to close and optional flags controlling the
// response behavior. This is the final operation in a file handle's lifecycle.
//
// This structure is decoded from little-endian binary data received over the network.
//
// **Wire Format (24 bytes):**
//
//	Offset  Size  Field           Description
//	------  ----  --------------  ----------------------------------
//	0       2     StructureSize   Always 24
//	2       2     Flags           POSTQUERY_ATTRIB (0x0001) to return attrs
//	4       4     Reserved        Must be ignored
//	8       16    FileId          SMB2 file identifier (persistent + volatile)
//
// **Flags:**
//
//   - SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB (0x0001): Request final attributes in response
//
// **Semantics:**
//
// CLOSE releases the file handle and ensures all cached data is persisted.
// This is a durability point - when CLOSE completes, the client expects
// data to be safely stored. For delete-on-close files, the deletion
// occurs during CLOSE processing.
type CloseRequest struct {
	// Flags controls the close behavior.
	// If SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB (0x0001) is set, the server
	// returns the final file attributes in the response.
	Flags uint16

	// FileID is the SMB2 file identifier returned by CREATE.
	// Both persistent (8 bytes) and volatile (8 bytes) parts must match
	// an open file handle on the server.
	FileID [16]byte
}

// CloseResponse represents an SMB2 CLOSE response [MS-SMB2] 2.2.16.
//
// The response optionally includes the final file attributes if the
// POSTQUERY_ATTRIB flag was set in the request.
//
// **Wire Format (60 bytes):**
//
//	Offset  Size  Field            Description
//	------  ----  ---------------  ----------------------------------
//	0       2     StructureSize    Always 60
//	2       2     Flags            Echo of request flags
//	4       4     Reserved         Must be ignored
//	8       8     CreationTime     File creation time (FILETIME)
//	16      8     LastAccessTime   Last access time (FILETIME)
//	24      8     LastWriteTime    Last modification time (FILETIME)
//	32      8     ChangeTime       Attribute change time (FILETIME)
//	40      8     AllocationSize   Disk allocation size
//	48      8     EndOfFile        Logical file size
//	56      4     FileAttributes   FILE_ATTRIBUTE_* flags
//
// **Status Codes:**
//
//   - StatusSuccess: File handle closed successfully
//   - StatusInvalidHandle: The FileID does not refer to a valid open file
//   - StatusFileDeleted: File was deleted (delete-on-close)
type CloseResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// Flags echoes the request flags.
	Flags uint16

	// CreationTime is when the file was created.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	CreationTime time.Time

	// LastAccessTime is when the file was last accessed.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	LastAccessTime time.Time

	// LastWriteTime is when the file was last modified.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	LastWriteTime time.Time

	// ChangeTime is when file attributes were last changed.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	ChangeTime time.Time

	// AllocationSize is the disk space allocated for the file.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	AllocationSize uint64

	// EndOfFile is the logical file size in bytes.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	EndOfFile uint64

	// FileAttributes contains FILE_ATTRIBUTE_* flags.
	// Only valid if POSTQUERY_ATTRIB flag was set.
	FileAttributes types.FileAttributes
}

// ============================================================================
// Encoding and Decoding
// ============================================================================

// DecodeCloseRequest parses an SMB2 CLOSE request from wire format [MS-SMB2] 2.2.15.
//
// The decoding extracts the Flags and FileID from the binary request body.
// All fields use little-endian byte order per SMB2 specification.
//
// **Parameters:**
//   - body: Raw request bytes (24 bytes minimum)
//
// **Returns:**
//   - *CloseRequest: The decoded request containing Flags and FileID
//   - error: ErrRequestTooShort if body is less than 24 bytes
//
// **Example:**
//
//	body := []byte{...} // SMB2 CLOSE request from network
//	req, err := DecodeCloseRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter)
//	}
//	// Use req.FileID to locate the file handle to close
func DecodeCloseRequest(body []byte) (*CloseRequest, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("CLOSE request too short: %d bytes", len(body))
	}

	req := &CloseRequest{
		Flags: binary.LittleEndian.Uint16(body[2:4]),
	}
	copy(req.FileID[:], body[8:24])

	return req, nil
}

// Encode serializes the CloseResponse to SMB2 wire format [MS-SMB2] 2.2.16.
//
// The response includes the echoed flags and optionally the file attributes
// if POSTQUERY_ATTRIB was requested. Times are converted to FILETIME format.
//
// **Wire Format (60 bytes):**
//
//	Offset  Size  Field            Value
//	------  ----  ---------------  ------
//	0       2     StructureSize    60
//	2       2     Flags            Echo of request flags
//	4       4     Reserved         0
//	8       8     CreationTime     FILETIME
//	16      8     LastAccessTime   FILETIME
//	24      8     LastWriteTime    FILETIME
//	32      8     ChangeTime       FILETIME
//	40      8     AllocationSize   Disk allocation
//	48      8     EndOfFile        File size
//	56      4     FileAttributes   Attribute flags
//
// **Returns:**
//   - []byte: 60-byte encoded response body
//   - error: Always nil (encoding cannot fail for this structure)
//
// **Example:**
//
//	resp := &CloseResponse{
//	    SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
//	    Flags:           0x0001,
//	    EndOfFile:       1024,
//	}
//	data, _ := resp.Encode()
//	// Send data as response body after SMB2 header
func (resp *CloseResponse) Encode() ([]byte, error) {
	buf := make([]byte, 60)
	binary.LittleEndian.PutUint16(buf[0:2], 60)                                          // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], resp.Flags)                                  // Flags
	binary.LittleEndian.PutUint32(buf[4:8], 0)                                           // Reserved
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(resp.CreationTime))    // CreationTime
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(resp.LastAccessTime)) // LastAccessTime
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(resp.LastWriteTime))  // LastWriteTime
	binary.LittleEndian.PutUint64(buf[32:40], types.TimeToFiletime(resp.ChangeTime))     // ChangeTime
	binary.LittleEndian.PutUint64(buf[40:48], resp.AllocationSize)                       // AllocationSize
	binary.LittleEndian.PutUint64(buf[48:56], resp.EndOfFile)                            // EndOfFile
	binary.LittleEndian.PutUint32(buf[56:60], uint32(resp.FileAttributes))               // FileAttributes

	return buf, nil
}

// ============================================================================
// Protocol Handler
// ============================================================================

// Close handles SMB2 CLOSE command [MS-SMB2] 2.2.15, 2.2.16.
//
// **Purpose:**
//
// CLOSE releases the file handle and ensures all data is persisted to storage.
// This is a critical durability point - when CLOSE completes, the client
// expects data to be safely stored.
//
// **Process:**
//
//  1. Validate FileID maps to an open file
//  2. Flush any cached data to content store (ensures durability)
//  3. Check for MFsymlink conversion (SMB→NFS symlink interop)
//  4. Optionally return final file attributes (POSTQUERY_ATTRIB flag)
//  5. Handle delete-on-close if pending
//  6. Remove the open file handle
//  7. Return success response
//
// **Cache Integration:**
//
// Unlike FLUSH which just persists cached data, CLOSE also finalizes any
// pending uploads (e.g., completes S3 multipart uploads). This ensures
// data is fully durable when CLOSE returns.
//
// **MFsymlink Conversion:**
//
// macOS/Windows SMB clients create symlinks by writing MFsymlink content
// (1067-byte files with XSym\n header). On CLOSE, we detect and convert
// these to real symlinks in the metadata store for NFS interoperability.
//
// **Delete-on-Close:**
//
// If SET_INFO with FileDispositionInformation marked the file for deletion,
// CLOSE performs the actual deletion after flushing and before releasing
// the handle.
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidHandle: Invalid FileID
//   - StatusInternalError: Encoding failed
//   - StatusSuccess: Close completed (even if flush or delete failed)
//
// Note: Flush and delete errors are logged but don't fail the CLOSE.
// The handle is always released to prevent resource leaks.
//
// **Example:**
//
//	req := &CloseRequest{FileID: fileID, Flags: 0x0001}
//	resp, err := handler.Close(ctx, req)
//	if resp.GetStatus() == types.StatusSuccess {
//	    // File handle released, data persisted
//	    // resp contains final attributes if POSTQUERY_ATTRIB was set
//	}
func (h *Handler) Close(ctx *SMBHandlerContext, req *CloseRequest) (*CloseResponse, error) {
	logger.Debug("CLOSE request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"flags", req.Flags)

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("CLOSE: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &CloseResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// ========================================================================
	// Step 2: Handle named pipe close
	// ========================================================================

	if openFile.IsPipe {
		// Clean up pipe state
		h.PipeManager.ClosePipe(req.FileID)
		h.DeleteOpenFile(req.FileID)

		logger.Debug("CLOSE pipe successful", "pipeName", openFile.PipeName)
		return &CloseResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Flags:           req.Flags,
		}, nil
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
				_, flushErr := cache.FlushAndFinalizeCache(ctx.Context, fileCache, contentStore, openFile.ContentID)
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
	// Step 3: Check for MFsymlink conversion
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
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Flags:           req.Flags,
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
	// Step 5: Release any byte-range locks held by this session on this file
	// Note: This must happen before delete-on-close so locks are released
	// while the file still exists in the metadata store.
	// ========================================================================

	if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
		metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
		if err == nil {
			if unlockErr := metadataStore.UnlockAllForSession(ctx.Context, openFile.MetadataHandle, ctx.SessionID); unlockErr != nil {
				logger.Warn("CLOSE: failed to release locks", "path", openFile.Path, "error", unlockErr)
				// Continue with close even if unlock fails
			}
		}
	}

	// ========================================================================
	// Step 6: Handle delete-on-close (FileDispositionInformation)
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
						// Notify watchers about deletion
						if h.NotifyRegistry != nil {
							parentPath := GetParentPath(openFile.Path)
							h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, openFile.FileName, FileActionRemoved)
						}
					}
				} else {
					// Delete file
					_, err = metadataStore.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName)
					if err != nil {
						logger.Debug("CLOSE: failed to delete file", "path", openFile.Path, "error", err)
						// Continue with close even if delete fails
					} else {
						logger.Debug("CLOSE: file deleted", "path", openFile.Path)
						// Notify watchers about deletion
						if h.NotifyRegistry != nil {
							parentPath := GetParentPath(openFile.Path)
							h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, openFile.FileName, FileActionRemoved)
						}
					}
				}
			}
		}
	}

	// ========================================================================
	// Step 7: Release oplock if held
	// ========================================================================

	if openFile.OplockLevel != OplockLevelNone {
		oplockPath := BuildOplockPath(openFile.ShareName, openFile.Path)
		h.OplockManager.ReleaseOplock(oplockPath, req.FileID)
	}

	// ========================================================================
	// Step 8: Unregister any pending CHANGE_NOTIFY watches
	// ========================================================================
	//
	// If this is a directory with pending CHANGE_NOTIFY requests, unregister them.
	// The watches are keyed by FileID, so closing the handle invalidates them.

	if openFile.IsDirectory && h.NotifyRegistry != nil {
		if notify := h.NotifyRegistry.Unregister(req.FileID); notify != nil {
			logger.Debug("CLOSE: unregistered pending CHANGE_NOTIFY",
				"path", openFile.Path,
				"messageID", notify.MessageID)
		}
	}

	// ========================================================================
	// Step 9: Remove the open file handle
	// ========================================================================

	h.DeleteOpenFile(req.FileID)

	logger.Debug("CLOSE successful",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"path", openFile.Path)

	// ========================================================================
	// Step 10: Return success response
	// ========================================================================

	return resp, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// checkAndConvertMFsymlink checks if a file is an MFsymlink and converts it to a real symlink.
//
// MFsymlinks are 1067-byte files with XSym\n header used by macOS/Windows SMB clients
// for symlink creation. This function:
//  1. Checks file size is exactly 1067 bytes
//  2. Reads content and verifies MFsymlink format
//  3. Parses the symlink target
//  4. Removes the regular file
//  5. Creates a real symlink with the same name
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

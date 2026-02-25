package handlers

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// CloseRequest represents an SMB2 CLOSE request from a client [MS-SMB2] 2.2.15.
// The client specifies a FileID to close and optional flags controlling the
// response behavior. The fixed wire format is 24 bytes.
//
// When POSTQUERY_ATTRIB (0x0001) is set, the server returns final file
// attributes in the response. CLOSE is a durability point -- the client
// expects data to be safely stored when it completes.
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
// The 60-byte response optionally includes final file attributes if the
// POSTQUERY_ATTRIB flag was set in the request.
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
// Returns an error if the body is less than 24 bytes.
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
// Returns a 60-byte response body with echoed flags and optionally file
// attributes (if POSTQUERY_ATTRIB was requested).
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
// CLOSE releases the file handle and ensures all data is persisted. It flushes
// cached payload data and pending metadata writes, checks for MFsymlink
// conversion (SMB-to-NFS symlink interop), handles delete-on-close, releases
// byte-range locks and oplocks, and unregisters any pending CHANGE_NOTIFY watches.
//
// Flush and delete errors are logged but do not fail the CLOSE -- the handle
// is always released to prevent resource leaks.
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

	// Flush cached data to ensure durability
	// Unlike NFS COMMIT which is non-blocking, SMB CLOSE requires immediate durability
	if !openFile.IsDirectory && openFile.PayloadID != "" {
		payloadSvc := h.Registry.GetBlockService()
		// Use blocking Flush for immediate durability
		_, flushErr := payloadSvc.Flush(ctx.Context, openFile.PayloadID)
		if flushErr != nil {
			logger.Warn("CLOSE: flush failed", "path", openFile.Path, "error", flushErr)
			// Continue with close even if flush fails
		} else {
			logger.Debug("CLOSE: flushed", "path", openFile.Path, "payloadID", openFile.PayloadID)
		}
	}

	// ========================================================================
	// Step 4: Flush pending metadata writes (deferred commit optimization)
	// ========================================================================
	//
	// The MetadataService uses deferred commits by default for performance.
	// This means CommitWrite only records changes in pending state, not to the store.
	// We must call FlushPendingWriteForFile to persist the metadata changes.
	// Without this, file size and other metadata changes are lost.

	if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
		authCtx, authErr := BuildAuthContext(ctx)
		if authErr != nil {
			logger.Warn("CLOSE: failed to build auth context for metadata flush", "path", openFile.Path, "error", authErr)
		} else {
			metaSvc := h.Registry.GetMetadataService()
			flushed, metaErr := metaSvc.FlushPendingWriteForFile(authCtx, openFile.MetadataHandle)
			if metaErr != nil {
				logger.Warn("CLOSE: metadata flush failed", "path", openFile.Path, "error", metaErr)
				// Continue with close even if metadata flush fails
			} else if flushed {
				logger.Debug("CLOSE: metadata flushed", "path", openFile.Path)
			}
		}
	}

	// ========================================================================
	// Step 5: Check for MFsymlink conversion
	// ========================================================================
	//
	// macOS/Windows SMB clients create symlinks by writing MFsymlink content
	// (1067-byte files with XSym\n header). On CLOSE, we convert these to
	// real symlinks in the metadata store for NFS interoperability.

	if !openFile.IsDirectory && openFile.PayloadID != "" && !openFile.DeletePending {
		if converted, _ := h.checkAndConvertMFsymlink(ctx, openFile); converted {
			logger.Debug("CLOSE: converted MFsymlink to symlink", "path", openFile.Path)
		}
	}

	// ========================================================================
	// Step 6: Build response with optional attributes
	// ========================================================================

	resp := &CloseResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Flags:           req.Flags,
	}

	// If SMB2_CLOSE_FLAG_POSTQUERY_ATTRIB was set, return file attributes
	if types.CloseFlags(req.Flags)&types.SMB2ClosePostQueryAttrib != 0 {
		// Get metadata to retrieve final attributes
		metaSvc := h.Registry.GetMetadataService()
		file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
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

	// ========================================================================
	// Step 7: Release any byte-range locks held by this session on this file
	// Note: This must happen before delete-on-close so locks are released
	// while the file still exists in the metadata store.
	// ========================================================================

	if !openFile.IsDirectory && len(openFile.MetadataHandle) > 0 {
		metaSvc := h.Registry.GetMetadataService()
		if unlockErr := metaSvc.UnlockAllForSession(ctx.Context, openFile.MetadataHandle, ctx.SessionID); unlockErr != nil {
			logger.Warn("CLOSE: failed to release locks", "path", openFile.Path, "error", unlockErr)
			// Continue with close even if unlock fails
		}
	}

	// ========================================================================
	// Step 8: Handle delete-on-close (FileDispositionInformation)
	// ========================================================================

	if openFile.DeletePending {
		authCtx, err := BuildAuthContext(ctx)
		if err != nil {
			logger.Warn("CLOSE: failed to build auth context for delete", "error", err)
		} else {
			metaSvc := h.Registry.GetMetadataService()
			var deleteErr error
			if openFile.IsDirectory {
				deleteErr = metaSvc.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName)
			} else {
				_, deleteErr = metaSvc.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName)
			}

			if deleteErr != nil {
				logger.Debug("CLOSE: failed to delete", "path", openFile.Path, "isDir", openFile.IsDirectory, "error", deleteErr)
			} else {
				logger.Debug("CLOSE: deleted", "path", openFile.Path, "isDir", openFile.IsDirectory)
				if h.NotifyRegistry != nil {
					parentPath := GetParentPath(openFile.Path)
					h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, openFile.FileName, FileActionRemoved)
				}
			}
		}
	}

	// ========================================================================
	// Step 9: Release oplock if held
	// ========================================================================

	if openFile.OplockLevel != OplockLevelNone {
		oplockPath := BuildOplockPath(openFile.ShareName, openFile.Path)
		h.OplockManager.ReleaseOplock(oplockPath, req.FileID)
	}

	// ========================================================================
	// Step 10: Unregister any pending CHANGE_NOTIFY watches
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
	// Step 11: Remove the open file handle
	// ========================================================================

	h.DeleteOpenFile(req.FileID)

	logger.Debug("CLOSE successful",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"path", openFile.Path)

	// ========================================================================
	// Step 12: Return success response
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
	metaSvc := h.Registry.GetMetadataService()

	// Get file metadata to check size
	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
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
// It reads from ContentService which uses Cache internally.
func (h *Handler) readMFsymlinkContent(ctx *SMBHandlerContext, openFile *OpenFile) ([]byte, error) {
	payloadSvc := h.Registry.GetBlockService()

	// Read the MFsymlink content (always 1067 bytes)
	data := make([]byte, mfsymlink.Size)
	n, err := payloadSvc.ReadAt(ctx.Context, openFile.PayloadID, data, 0)
	if err != nil {
		return nil, err
	}

	return data[:n], nil
}

// convertToRealSymlink removes the regular file and creates a symlink in its place.
func (h *Handler) convertToRealSymlink(ctx *SMBHandlerContext, openFile *OpenFile, target string) error {
	// Validate required fields
	if len(openFile.ParentHandle) == 0 || openFile.FileName == "" {
		return fmt.Errorf("missing parent handle or filename for MFsymlink conversion")
	}

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return err
	}

	// Get the parent handle and filename for removal and creation
	parentHandle := openFile.ParentHandle
	fileName := openFile.FileName

	// Remove the regular file
	metaSvc := h.Registry.GetMetadataService()
	_, err = metaSvc.RemoveFile(authCtx, parentHandle, fileName)
	if err != nil {
		return fmt.Errorf("failed to remove MFsymlink file: %w", err)
	}

	// Delete content from content store via ContentService (optional - ignore errors)
	if openFile.PayloadID != "" {
		payloadSvc := h.Registry.GetBlockService()
		_ = payloadSvc.Delete(ctx.Context, openFile.PayloadID)
	}

	// Create the real symlink with default attributes
	// Pass empty FileAttr - CreateSymlink will apply defaults
	symlinkAttr := &metadata.FileAttr{}
	_, err = metaSvc.CreateSymlink(authCtx, parentHandle, fileName, target, symlinkAttr)
	if err != nil {
		return fmt.Errorf("failed to create symlink: %w", err)
	}

	logger.Debug("CLOSE: converted MFsymlink",
		"path", openFile.Path,
		"target", target)

	return nil
}

package handlers

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

	r := smbenc.NewReader(body)
	r.Skip(2) // StructureSize
	req := &CloseRequest{
		Flags: r.ReadUint16(),
	}
	r.Skip(4) // Reserved
	copy(req.FileID[:], r.ReadBytes(16))
	if r.Err() != nil {
		return nil, fmt.Errorf("CLOSE decode error: %w", r.Err())
	}
	return req, nil
}

// Encode serializes the CloseResponse to SMB2 wire format [MS-SMB2] 2.2.16.
// Returns a 60-byte response body with echoed flags and optionally file
// attributes (if POSTQUERY_ATTRIB was requested).
func (resp *CloseResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(60)
	w.WriteUint16(60)                                        // StructureSize
	w.WriteUint16(resp.Flags)                                // Flags
	w.WriteUint32(0)                                         // Reserved
	w.WriteUint64(types.TimeToFiletime(resp.CreationTime))   // CreationTime
	w.WriteUint64(types.TimeToFiletime(resp.LastAccessTime)) // LastAccessTime
	w.WriteUint64(types.TimeToFiletime(resp.LastWriteTime))  // LastWriteTime
	w.WriteUint64(types.TimeToFiletime(resp.ChangeTime))     // ChangeTime
	w.WriteUint64(resp.AllocationSize)                       // AllocationSize
	w.WriteUint64(resp.EndOfFile)                            // EndOfFile
	w.WriteUint32(uint32(resp.FileAttributes))               // FileAttributes
	if w.Err() != nil {
		return nil, w.Err()
	}
	return w.Bytes(), nil
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
// Flush errors are logged but do not fail the CLOSE. Delete-on-close unlink
// failures are surfaced to the client per MS-SMB2 3.3.5.10 / MS-FSA 2.1.5.4
// (#388) — the client must know the file was not removed. The handle itself
// is always released regardless, to prevent resource leaks.
func (h *Handler) Close(ctx *SMBHandlerContext, req *CloseRequest) (*CloseResponse, error) {
	logger.Debug("CLOSE request",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"flags", req.Flags)

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("CLOSE: file handle not found (already closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return &CloseResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileClosed}}, nil
	}

	// ========================================================================
	// Step 2: Handle named pipe close
	// ========================================================================

	if openFile.IsPipe {
		// Cancel any pending async READ before closing the pipe.
		if h.PipeReadRegistry != nil {
			if pending := h.PipeReadRegistry.UnregisterByFileID(req.FileID); pending != nil && pending.Callback != nil {
				go func(pr *PendingPipeRead) {
					if err := pr.Callback(pr.SessionID, pr.MessageID, pr.AsyncId, types.StatusCancelled, nil); err != nil {
						logger.Warn("CLOSE: failed to cancel pending pipe READ", "asyncId", pr.AsyncId, "error", err)
					}
				}(pending)
			}
		}
		h.PipeManager.ClosePipe(req.FileID)
		h.DeleteOpenFile(req.FileID)

		logger.Debug("CLOSE pipe successful", "pipeName", openFile.PipeName)
		return &CloseResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Flags:           req.Flags,
		}, nil
	}

	// ========================================================================
	// Step 3: Flush cached data to block store (ensures durability)
	// ========================================================================

	// Flush cached data to ensure durability.
	// Unlike NFS COMMIT which is non-blocking, SMB CLOSE requires immediate durability.
	if !openFile.IsDirectory && openFile.PayloadID != "" {
		blockStore, bsErr := h.Registry.GetBlockStoreForHandle(ctx.Context, openFile.MetadataHandle)
		if bsErr != nil {
			logger.Warn("CLOSE: block store not available for handle", "path", openFile.Path, "error", bsErr)
		} else if _, flushErr := blockStore.Flush(ctx.Context, string(openFile.PayloadID)); flushErr != nil {
			logger.Warn("CLOSE: flush failed", "path", openFile.Path, "error", flushErr)
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

			// Per MS-FSA 2.1.5.14.2: After flushing pending writes (which may overwrite
			// frozen timestamps), restore any timestamps that were frozen via SET_INFO -1.
			// The deferred commit flush sets Mtime/Ctime to the WRITE time, but if the
			// handle has frozen timestamps, those must be preserved.
			h.restoreFrozenTimestamps(authCtx, openFile)
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
			// Apply frozen timestamp overrides before building response
			applyFrozenTimestamps(openFile, file)
			creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
			allocationSize := calculateAllocationSize(file.Size)

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
		if unlockErr := metaSvc.UnlockAllForOpen(ctx.Context, openFile.MetadataHandle, openFile.OpenID()); unlockErr != nil {
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
			// DELETE access was verified upstream before DeletePending was set:
			// either by CREATE honoring FILE_DELETE_ON_CLOSE (create.go:913) or
			// by SET_INFO FileDispositionInformation (set_info.go:752). Signal
			// that to the metadata layer so the owner-of-target delete rule
			// applies without loosening POSIX unlink(2) for NFS callers.
			authCtx.HasDeleteAccess = true
			metaSvc := h.Registry.GetMetadataService()
			var deleteErr error
			if openFile.IsDirectory {
				deleteErr = metaSvc.RemoveDirectory(authCtx, openFile.ParentHandle, openFile.FileName)
			} else {
				_, deleteErr = metaSvc.RemoveFile(authCtx, openFile.ParentHandle, openFile.FileName)
			}

			if deleteErr != nil {
				// Per MS-SMB2 3.3.5.10 and MS-FSA 2.1.5.4, a CLOSE that cannot
				// honor DELETE_ON_CLOSE must surface the failure to the client.
				// Returning STATUS_SUCCESS while the underlying unlink failed
				// causes the client to believe the file is gone and reissue
				// CREATE/CLOSE in a tight loop (smbtorture smb2.session.reauth5,
				// issue #388).
				resp.Status = MetadataErrorToSMBStatus(deleteErr)
				logger.Debug("CLOSE: failed to delete",
					"path", openFile.Path,
					"isDir", openFile.IsDirectory,
					"status", resp.Status,
					"error", deleteErr)
			} else {
				logger.Debug("CLOSE: deleted", "path", openFile.Path, "isDir", openFile.IsDirectory)

				// Cascade delete ADS streams: when a base file is deleted,
				// all its alternate data streams (stored as "baseFile:streamName"
				// children in the same parent directory) must also be removed.
				// Per MS-FSA 2.1.5.9.7: all streams of a file are deleted when
				// the file itself is deleted.
				if !openFile.IsDirectory && !strings.Contains(openFile.FileName, ":") {
					h.cascadeDeleteADSStreams(authCtx, metaSvc, openFile)
				}
				// Per MS-FSA 2.1.5.14.2: Restore frozen timestamps on parent directory
				// after delete updates parent Mtime/Ctime/Atime.
				h.restoreParentDirFrozenTimestamps(authCtx, openFile.ParentHandle)

				// Break parent directory leases: deletion changes directory content.
				h.breakParentDirLeasesForContentChange(authCtx, openFile)

				if h.NotifyRegistry != nil {
					parentPath := GetParentPath(openFile.Path)
					h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, openFile.FileName, FileActionRemoved)
				}
			}
		}
	}

	// ========================================================================
	// Step 9: Release oplock/lease if held
	// ========================================================================

	if openFile.OplockLevel != OplockLevelNone && h.LeaseManager != nil {
		leaseKey := openFile.LeaseKey

		if leaseKey != ([16]byte{}) {
			// Check if any other open on the SAME FILE shares this lease key.
			// Two opens on the same file share one lease record (requestLeaseImpl
			// upgrades in place). Different files with the same key are separate
			// records in distinct handleKey buckets and must not be disturbed.
			hasOtherOpenSameFile := false
			h.files.Range(func(key, value any) bool {
				other := value.(*OpenFile)
				if other.FileID == openFile.FileID {
					return true // skip self
				}
				if other.LeaseKey != leaseKey {
					return true
				}
				if bytes.Equal(other.MetadataHandle, openFile.MetadataHandle) {
					hasOtherOpenSameFile = true
					return false
				}
				return true
			})

			if !hasOtherOpenSameFile {
				// Last open on this file with this lease key — release only
				// this handle's lease record. Other files sharing the key
				// (smbtorture reuses LEASE1/LEASE2 across tests) keep theirs.
				if err := h.LeaseManager.ReleaseLeaseForHandle(ctx.Context, lock.FileHandle(openFile.MetadataHandle), leaseKey, openFile.ShareName); err != nil {
					logger.Debug("CLOSE: failed to release lease",
						"path", openFile.Path,
						"leaseKey", fmt.Sprintf("%x", leaseKey),
						"error", err)
				} else {
					logger.Debug("CLOSE: released lease (last open on this file)",
						"path", openFile.Path,
						"leaseKey", fmt.Sprintf("%x", leaseKey))
				}
				// Unregister oplock FileID mapping if this was a traditional oplock
				if openFile.OplockLevel != OplockLevelLease {
					h.LeaseManager.UnregisterOplockFileID(leaseKey)
				}
			} else {
				logger.Debug("CLOSE: lease handle closed (other opens share lease key on same file)",
					"path", openFile.Path)
			}
		}
	}

	// ========================================================================
	// Step 10: Unregister any pending CHANGE_NOTIFY watches
	// ========================================================================
	//
	// If this is a directory with pending CHANGE_NOTIFY requests, unregister them.
	// The watches are keyed by FileID, so closing the handle invalidates them.

	if openFile.IsDirectory && h.NotifyRegistry != nil {
		if notify := h.NotifyRegistry.Unregister(req.FileID); notify != nil {
			// Per MS-SMB2 3.3.4.1 and 3.3.5.16.1: when the directory handle for
			// a pending CHANGE_NOTIFY is closed, complete the request with
			// STATUS_NOTIFY_CLEANUP. This response MUST be sent AFTER the CLOSE
			// response — CHANGE_NOTIFY responses "MUST be the last responses
			// sent for the FileId". If sent before, WPTS (and Windows clients)
			// that arm their async-receive callback only after consuming the
			// CLOSE response miss the cleanup and time out
			// (BVT_SMB2Basic_ChangeNotify_ServerReceiveSmb2Close). Defer the
			// delivery via ctx.PostSend, which the dispatch layer invokes after
			// the CLOSE response has been written. The notify entry is already
			// unregistered, so capturing the pointer is safe — nothing else can
			// see or mutate it.
			if notify.AsyncCallback != nil {
				ctx.PostSend = func() {
					cleanupResp := &ChangeNotifyResponse{
						SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyCleanup},
					}
					if err := notify.AsyncCallback(notify.SessionID, notify.MessageID, notify.AsyncId, cleanupResp); err != nil {
						logger.Warn("CLOSE: failed to send STATUS_NOTIFY_CLEANUP",
							"messageID", notify.MessageID,
							"error", err)
						return
					}
					logger.Debug("CLOSE: sent STATUS_NOTIFY_CLEANUP (post-close)",
						"path", openFile.Path,
						"messageID", notify.MessageID,
						"asyncId", notify.AsyncId)
				}
			}
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
// It reads from the block store which uses local cache internally.
func (h *Handler) readMFsymlinkContent(ctx *SMBHandlerContext, openFile *OpenFile) ([]byte, error) {
	blockStore, err := h.Registry.GetBlockStoreForHandle(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		return nil, fmt.Errorf("block store not available: %w", err)
	}

	// Read the MFsymlink content (always 1067 bytes)
	data := make([]byte, mfsymlink.Size)
	n, err := blockStore.ReadAt(ctx.Context, string(openFile.PayloadID), data, 0)
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

	// Delete content from block store (optional - ignore errors)
	if openFile.PayloadID != "" {
		if blockStore, bsErr := h.Registry.GetBlockStoreForHandle(ctx.Context, openFile.MetadataHandle); bsErr == nil {
			_ = blockStore.Delete(ctx.Context, string(openFile.PayloadID))
		}
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

// cascadeDeleteADSStreams removes all alternate data streams belonging to a
// base file that was just deleted. ADS streams are stored as sibling entries
// in the parent directory with names like "baseFile:streamName:$DATA".
// Per MS-FSA 2.1.5.9.7, deleting a file deletes all its streams.
func (h *Handler) cascadeDeleteADSStreams(authCtx *metadata.AuthContext, metaSvc *metadata.MetadataService, openFile *OpenFile) {
	prefix := openFile.FileName + ":"

	// Enumerate parent directory children to find ADS entries.
	// Use ReadDirectory with a large buffer to get all entries.
	page, err := metaSvc.ReadDirectory(authCtx, openFile.ParentHandle, 0, 1<<20)
	if err != nil {
		logger.Debug("CLOSE: cascade ADS delete: failed to read parent directory",
			"path", openFile.Path,
			"error", err)
		return
	}

	for _, entry := range page.Entries {
		if strings.HasPrefix(entry.Name, prefix) {
			_, deleteErr := metaSvc.RemoveFile(authCtx, openFile.ParentHandle, entry.Name)
			if deleteErr != nil {
				logger.Debug("CLOSE: cascade ADS delete: failed to remove stream",
					"stream", entry.Name,
					"error", deleteErr)
			} else {
				logger.Debug("CLOSE: cascade ADS delete: removed stream",
					"stream", entry.Name)
			}
		}
	}
}

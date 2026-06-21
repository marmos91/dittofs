package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
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
// Flush errors fail the CLOSE: CLOSE is a durability point (MS-SMB2 3.3.5.10),
// so a failed block-store (or pending-metadata) flush is mapped to a non-success
// status rather than silently acknowledged — otherwise the client treats data
// that never persisted as committed (#1267). Delete-on-close unlink failures are
// likewise surfaced per MS-SMB2 3.3.5.10 / MS-FSA 2.1.5.4 (#388) — the client
// must know the file was not removed. The handle itself is always released
// regardless of any flush/delete failure, to prevent resource leaks.
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
	// Step 2b: Prime auth context from OpenFile's recorded session
	// ========================================================================
	// CLOSE arrives keyed only by FileID — the SMB2 dispatcher has no user
	// state to prefill ctx.User with. Without this hand-off BuildAuthContext
	// would take the ctx.User==nil arm and synthesise UID-0 (root), bypassing
	// DACL checks on the downstream metadata flush, delete-on-close unlink,
	// and MFsymlink conversion (#619, same class as #603).
	h.primeAuthContextFromOpenFile(ctx, openFile)

	// ========================================================================
	// Step 3: Flush cached data to block store (ensures durability)
	// ========================================================================

	// Flush cached data to ensure durability.
	// Unlike NFS COMMIT which is non-blocking, SMB CLOSE requires immediate durability.
	// Routed through common.CommitBlockStore so any future []BlockRef
	// plumbing lands in one place (see common/doc.go).
	//
	// CLOSE is a durability point (MS-SMB2 3.3.5.10): the client treats a
	// successful CLOSE as a guarantee that its writes reached stable storage.
	// If the durable flush fails (e.g. append-log backpressure surfaces
	// fs.ErrPressureTimeout when the rollup pool is wedged), we MUST surface
	// the failure as a non-success status rather than silently ack the close —
	// otherwise the client believes data it never persisted is committed
	// (silent write truncation, #1267). The mapped status is recorded and
	// applied to the response below; handle teardown still runs unconditionally
	// so the failed flush does not leak the open-file/lease state.
	var flushFailStatus types.Status
	if !openFile.IsDirectory && openFile.PayloadID != "" {
		blockStore, bsErr := common.ResolveForWrite(ctx.Context, h.Registry, openFile.MetadataHandle)
		if bsErr != nil {
			logger.Warn("CLOSE: block store not available for handle", "path", openFile.Path, "error", bsErr)
			flushFailStatus = common.MapContentToSMB(bsErr)
		} else if flushErr := common.CommitBlockStore(ctx.Context, blockStore, openFile.PayloadID); flushErr != nil {
			logger.Warn("CLOSE: flush failed", "path", openFile.Path, "error", flushErr)
			flushFailStatus = common.MapContentToSMB(flushErr)
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
				// Surface the metadata-flush failure too (#1267): if the
				// deferred size/mtime never persisted, the client must not be
				// told the CLOSE succeeded. Lower severity than the byte-data
				// flush above, but still a durability gap. Only record it when
				// no block-store flush failure was already captured, so the
				// data-loss signal (block flush) takes precedence in the status.
				if flushFailStatus == types.StatusSuccess {
					flushFailStatus = common.MapToSMB(metaErr)
				}
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

	if !openFile.IsDirectory && openFile.PayloadID != "" && !openFile.DeletePending && !openFile.InitialDeleteOnClose {
		// MFsymlink conversion promotes client-controlled file content to a real
		// symlink, so it is opt-in per share (default disabled).
		if tree, treeOK := h.GetTree(ctx.TreeID); treeOK && tree.AllowMFsymlink {
			if converted, _ := h.checkAndConvertMFsymlink(ctx, openFile); converted {
				logger.Debug("CLOSE: converted MFsymlink to symlink", "path", openFile.Path)
			}
		}
	}

	// ========================================================================
	// Step 6: Build response with optional attributes
	// ========================================================================

	// Seed the response status with the durable-flush outcome. If the block
	// store flush above failed, the CLOSE is reported as a failure
	// (data-integrity, #1267); otherwise it starts as StatusSuccess. The
	// delete-on-close path below may override this with its own error status,
	// but it never resets a recorded failure back to success.
	closeStatus := types.StatusSuccess
	if flushFailStatus != types.StatusSuccess {
		closeStatus = flushFailStatus
	}
	resp := &CloseResponse{
		SMBResponseBase: SMBResponseBase{Status: closeStatus},
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
		// Cancel any pending (parked) blocking LOCKs for this handle before
		// releasing held locks. The resume goroutine would fail with
		// STATUS_FILE_CLOSED on its next retry, but smbtorture expects
		// immediate STATUS_RANGE_NOT_LOCKED on handle close (Samba
		// brl_close_fnum).
		if h.PendingLockRegistry != nil {
			for _, parked := range h.PendingLockRegistry.UnregisterAllForOwner(openFile.OpenID()) {
				if parked.Callback != nil {
					if err := parked.Callback(parked.SessionID, parked.MessageID, parked.AsyncId, types.StatusRangeNotLocked, nil); err != nil {
						logger.Debug("CLOSE: failed to send LOCK cancel response",
							"asyncId", parked.AsyncId, "error", err)
					}
				}
				if h.LockWaitGraph != nil && parked.OwnerID != "" {
					h.LockWaitGraph.RemoveWaiter(parked.OwnerID)
				}
			}
		}

		metaSvc := h.Registry.GetMetadataService()
		if unlockErr := metaSvc.UnlockAllForOpen(ctx.Context, openFile.MetadataHandle, openFile.OpenID()); unlockErr != nil {
			logger.Warn("CLOSE: failed to release locks", "path", openFile.Path, "error", unlockErr)
		}
	}

	// ========================================================================
	// Step 8: Handle delete-on-close (FileDispositionInformation)
	// ========================================================================

	// Promote per-handle InitialDeleteOnClose to shared committed DOC at CLOSE
	// time, mirroring Samba close.c::close_normal_file: if the closing handle
	// requested initial DOC at CREATE and nobody else has committed a shared
	// DOC via SET_INFO disposition, set DeletePending so the deletion path
	// fires (whether this is the last handle or DOC propagates to siblings
	// via the propagation block below). InitialDeleteOnClose is a per-handle
	// flag and must NOT block subsequent opens prior to this CLOSE — required
	// by smbtorture smb2.dirlease.{unlink_same, unlink_different}_initial_and_close.
	if openFile.InitialDeleteOnClose && !openFile.DeletePending {
		openFile.DeletePending = true
	}

	if openFile.DeletePending || openFile.BaseFileDeletePending {
		// Per MS-FSA 2.1.5.4: delete-on-close only removes the file when the
		// LAST handle closes. If other handles on the same file exist, propagate
		// the DOC flag + the original DOC-setter's parent key to remaining handles
		// so the delete fires on their eventual close.
		//
		// For base-file DOC on a non-stream handle, also check for open stream
		// handles (ADS) on the same base file. Per MS-FSA 2.1.5.9.7 / MS-SMB2
		// 3.3.5.10, the actual deletion is deferred until all handles — including
		// stream handles — are closed. The stream handles are marked with
		// BaseFileDeletePending so the CLOSE of the last stream triggers the
		// base file removal (smbtorture smb2.streams.delete).
		isBaseFile := !strings.Contains(openFile.FileName, ":")
		otherHandleExists := false
		streamHandleExists := false

		if len(openFile.MetadataHandle) > 0 {
			h.files.Range(func(_, value any) bool {
				other := value.(*OpenFile)
				if other.FileID == openFile.FileID {
					return true // skip self
				}
				if bytes.Equal(other.MetadataHandle, openFile.MetadataHandle) {
					otherHandleExists = true
					return false
				}
				return true
			})
		}

		// For base file DOC: also check for open stream handles.
		if !otherHandleExists && isBaseFile && !openFile.BaseFileDeletePending {
			basePrefix := openFile.FileName + ":"
			h.rangeStreamsOfBase(openFile.FileID, openFile.ParentHandle, basePrefix, func(other *OpenFile) bool {
				streamHandleExists = true
				return false
			})
		}

		// For stream handle with BaseFileDeletePending: check if other stream
		// handles with the same base file delete are still open. The actual
		// base file removal must wait until ALL such handles close.
		if !otherHandleExists && !streamHandleExists && openFile.BaseFileDeletePending {
			basePrefix := openFile.BaseFileDeleteFileName + ":"
			h.rangeStreamsOfBase(openFile.FileID, openFile.BaseFileDeleteParentHandle, basePrefix, func(other *OpenFile) bool {
				if other.BaseFileDeletePending {
					otherHandleExists = true
					return false
				}
				return true
			})
		}

		if otherHandleExists {
			// Not the last handle: propagate DOC to remaining handles so the
			// actual delete fires when they close. The DOC-setter's parent key
			// is preserved so the closer can compare it for dir-lease
			// suppression (test_unlink_different_* vs test_unlink_same_*).
			h.files.Range(func(_, value any) bool {
				other := value.(*OpenFile)
				if other.FileID == openFile.FileID {
					return true
				}
				if bytes.Equal(other.MetadataHandle, openFile.MetadataHandle) {
					// Guard the write: concurrent QUERY_INFO/WRITE goroutines on
					// the same session may be reading these fields on `other`.
					other.mu.Lock()
					other.DeletePending = true
					other.DeleteOnCloseParentKey = openFile.DeleteOnCloseParentKey
					other.HasDeleteOnCloseParentKey = openFile.HasDeleteOnCloseParentKey
					other.mu.Unlock()
					h.StoreOpenFile(other)
				}
				return true
			})
			logger.Debug("CLOSE: DOC propagated to other handles (not last)",
				"path", openFile.Path)
		} else if streamHandleExists {
			// Base file has DOC but open stream handles remain. Per MS-FSA
			// 2.1.5.4 / 2.1.5.9.7, defer the actual deletion until all
			// stream handles close. Mark them with BaseFileDeletePending so
			// the last stream CLOSE triggers the base file removal.
			basePrefix := openFile.FileName + ":"
			h.rangeStreamsOfBase(openFile.FileID, openFile.ParentHandle, basePrefix, func(other *OpenFile) bool {
				// Guard the write: concurrent readers on the stream handle
				// (QUERY_INFO / open path via isFileOrBaseDeletePending) may be
				// reading these fields on `other`.
				other.mu.Lock()
				other.BaseFileDeletePending = true
				other.BaseFileDeleteParentHandle = openFile.ParentHandle
				other.BaseFileDeleteFileName = openFile.FileName
				other.DeleteOnCloseParentKey = openFile.DeleteOnCloseParentKey
				other.HasDeleteOnCloseParentKey = openFile.HasDeleteOnCloseParentKey
				other.mu.Unlock()
				h.StoreOpenFile(other)
				return true
			})
			logger.Debug("CLOSE: base file DOC deferred to stream handles",
				"path", openFile.Path)
		} else {
			// Last handle: perform the actual delete.
			//
			// If this is a stream handle with BaseFileDeletePending, delete the
			// base file (not the stream — the stream is a child of the base).
			deleteParentHandle := openFile.ParentHandle
			deleteFileName := openFile.FileName
			isBaseFileDelete := false
			if openFile.BaseFileDeletePending {
				deleteParentHandle = openFile.BaseFileDeleteParentHandle
				deleteFileName = openFile.BaseFileDeleteFileName
				isBaseFileDelete = true
			}

			authCtx, err := BuildAuthContext(ctx)
			if err != nil {
				logger.Warn("CLOSE: failed to build auth context for delete", "error", err)
			} else {
				authCtx.HasDeleteAccess = true

				// Dir-lease parent-key suppression: when the closer's
				// parent key matches the DOC-setter's parent key, use the closer's
				// key for suppression (test_unlink_same_*). When they differ, no
				// suppression — ALL parent dir leases break (test_unlink_different_*).
				docSetterKeysDiffer := openFile.HasDeleteOnCloseParentKey &&
					openFile.HasParentLeaseKey &&
					openFile.DeleteOnCloseParentKey != openFile.ParentLeaseKey
				if !docSetterKeysDiffer {
					// Same key (or no DOC key tracking): use closer's parent key
					PropagateOpenFileParentLeaseKey(authCtx, openFile)
				}
				// else: different keys — don't propagate, all dir leases break

				metaSvc := h.Registry.GetMetadataService()
				// For deferred base-file delete (via stream handle), check the
				// actual target type — stream handles always have IsDirectory=false.
				//
				// Capture the delete target's metadata handle BEFORE removal so
				// we can route its block-store payload purge afterwards (the
				// handle encodes share identity and stays valid). The PayloadID
				// to purge comes from RemoveFile's RETURN value, which is empty
				// when content must survive (surviving hard link, or recycle to
				// trash) — purging the open handle's PayloadID instead would
				// destroy still-referenced content.
				isDeleteTargetDir := openFile.IsDirectory
				deleteTargetHandle := openFile.MetadataHandle
				if isBaseFileDelete {
					if targetFile, _, lookupErr := metaSvc.LookupCaseInsensitive(authCtx, deleteParentHandle, deleteFileName); lookupErr == nil && targetFile != nil {
						isDeleteTargetDir = targetFile.Type == metadata.FileTypeDirectory
						if encoded, encErr := metadata.EncodeFileHandle(targetFile); encErr == nil {
							deleteTargetHandle = encoded
						}
					}
				}
				var deleteErr error
				var removedPayloadID metadata.PayloadID
				if isDeleteTargetDir {
					_, deleteErr = metaSvc.RemoveDirectory(authCtx, deleteParentHandle, deleteFileName)
				} else {
					var removed *metadata.File
					removed, _, deleteErr = metaSvc.RemoveFile(authCtx, deleteParentHandle, deleteFileName)
					if removed != nil {
						removedPayloadID = removed.PayloadID
					}
				}

				if deleteErr != nil {
					resp.Status = common.MapToSMB(deleteErr)
					logger.Debug("CLOSE: failed to delete",
						"path", openFile.Path,
						"isDir", openFile.IsDirectory,
						"deleteTarget", deleteFileName,
						"status", resp.Status,
						"error", deleteErr)
				} else {
					logger.Debug("CLOSE: deleted",
						"path", openFile.Path,
						"deleteTarget", deleteFileName,
						"isDir", openFile.IsDirectory,
						"isBaseFileDelete", isBaseFileDelete)

					if !openFile.IsDirectory && !strings.Contains(deleteFileName, ":") {
						// For base file deletes, cascade ADS streams.
						// Use a synthetic OpenFile with the base file's info.
						cascadeOF := openFile
						if isBaseFileDelete {
							cascadeOF = &OpenFile{
								ParentHandle: deleteParentHandle,
								FileName:     deleteFileName,
								Path:         openFile.Path,
							}
						}
						h.cascadeDeleteADSStreams(authCtx, metaSvc, cascadeOF)
					}

					h.purgeBlockStorePayload(ctx.Context, deleteTargetHandle, removedPayloadID, openFile.Path, "CLOSE")
					h.restoreParentDirFrozenTimestamps(authCtx, deleteParentHandle)

					// Break parent directory leases: deletion changes directory
					// content. When docSetterKeysDiffer, clear the closer's
					// parent key so no suppression applies — all dir leases break.
					if docSetterKeysDiffer {
						savedKey := openFile.ParentLeaseKey
						savedHas := openFile.HasParentLeaseKey
						openFile.HasParentLeaseKey = false
						openFile.ParentLeaseKey = [16]byte{}
						h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)
						openFile.ParentLeaseKey = savedKey
						openFile.HasParentLeaseKey = savedHas
					} else {
						h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)
					}

					if h.NotifyRegistry != nil {
						parentPath := GetParentPath(openFile.Path)
						// Use the resolved delete-target type (base file's, not
						// the stream open's) and the actual deleteFileName.
						// NameChangeFilterFor routes ADS names via
						// FILE_NOTIFY_CHANGE_STREAM_NAME automatically.
						nameFilter := NameChangeFilterFor(deleteFileName, isDeleteTargetDir)
						h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, deleteFileName, FileActionRemoved, nameFilter)
					}
				}
			}
		}
	}

	// ========================================================================
	// Step 8b: Break parent dir leases on close of modified file
	// ========================================================================
	// Per MS-SMB2 3.3.4.7 / Samba open.c close_directory: closing a handle
	// that modified the file (WRITE occurred) breaks parent-dir leases. The
	// WRITE itself does NOT break — only the CLOSE triggers the break so
	// directory caching is only invalidated once the mutation is committed.
	// Parent-key suppression (MS-SMB2 §3.3.4.20) applies: if the
	// closing handle carried a ParentLeaseKey matching the parent's dir lease,
	// that lease is suppressed. Covers smb2.dirlease.v2_request: write without
	// parent key -> close breaks dir lease; write with parent key -> close
	// does NOT break (suppressed).

	// SmbWriteTriggered is written under openFile.mu (write) in
	// armSmbDelayedWrite. Snapshot it under the read lock so we observe a
	// consistent value against a parallel WRITE on the same handle (#606).
	openFile.mu.RLock()
	smbWriteTriggered := openFile.SmbWriteTriggered
	openFile.mu.RUnlock()
	if !openFile.DeletePending && !openFile.IsDirectory && smbWriteTriggered {
		authCtx, authErr := BuildAuthContext(ctx)
		if authErr != nil {
			logger.Warn("CLOSE: failed to build auth context for modified-file dir-lease break", "path", openFile.Path, "error", authErr)
		} else {
			PropagateOpenFileParentLeaseKey(authCtx, openFile)
			h.breakParentDirLeasesForContentChange(ctx, authCtx, openFile)
		}
	}

	// ========================================================================
	// Step 9: Unregister any pending CHANGE_NOTIFY watches
	// ========================================================================
	//
	// If this is a directory with pending CHANGE_NOTIFY requests, unregister them.
	// The watches are keyed by FileID, so closing the handle invalidates them.

	if openFile.IsDirectory && h.NotifyRegistry != nil {
		// Disarm buffered-event accounting first so any in-flight event
		// after this point can't keep charging against a closed handle
		// (the OnOverflow callback would race against DeleteOpenFile and
		// touch a stale OpenFile struct).
		h.NotifyRegistry.Disarm(req.FileID)
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
				n := notify
				ctx.PostSend = func() {
					cleanupResp := &ChangeNotifyResponse{
						SMBResponseBase: SMBResponseBase{Status: types.StatusNotifyCleanup},
					}
					h.NotifyRegistry.QueueFinalAfterInterim(n, func() {
						if err := n.AsyncCallback(n.SessionID, n.MessageID, n.AsyncId, cleanupResp); err != nil {
							logger.Warn("CLOSE: failed to send STATUS_NOTIFY_CLEANUP",
								"messageID", n.MessageID,
								"error", err)
							return
						}
						logger.Debug("CLOSE: sent STATUS_NOTIFY_CLEANUP (post-close)",
							"path", openFile.Path,
							"messageID", n.MessageID,
							"asyncId", n.AsyncId)
					})
				}
			}
			logger.Debug("CLOSE: unregistered pending CHANGE_NOTIFY",
				"path", openFile.Path,
				"messageID", notify.MessageID)
		}
	}

	// ========================================================================
	// Step 10: Remove the open file handle
	// ========================================================================

	// Drain in-flight operations (e.g., a concurrent QueryDirectory on the
	// same handle) BEFORE removing the handle from the map. This prevents a
	// CLOSE goroutine from racing ahead of a FIND goroutine on the same
	// connection and causing a spurious FILE_CLOSED
	// (smbtorture compound_find.compound_find_close). The drain runs OUTSIDE
	// renameScanMu because it can block on a slow in-flight op and the rename
	// path must remain free to take the mutex for its own conflict scan.
	h.DrainHandleOps(req.FileID)

	// Steps 10 (map removal) + 11 (lease release/signal) run under
	// renameScanMu so that a concurrent SET_INFO rename's post-break conflict
	// re-scan cannot observe this handle half-removed: either the rename's
	// authoritative scan runs entirely before we begin (sees a live holder →
	// correct SHARING_VIOLATION) or entirely after we finish (sees the shrunk
	// table + a drained break → correct STATUS_OK). No interleaving, hence no
	// flake. The break-WAIT the rename performs to reach this point happens
	// OUTSIDE the mutex (set_info.go), so this CLOSE is never blocked behind a
	// rename that is itself waiting on this CLOSE — see the renameScanMu
	// doc comment on Handler for the full deadlock-safety argument.
	h.renameScanMu.Lock()
	h.deleteOpenFileEntry(req.FileID)

	// ========================================================================
	// Step 11: Release oplock/lease if held
	// ========================================================================

	// Release any lease/oplock record associated with this open. The gate
	// previously checked openFile.OplockLevel != None, but an oplock that was
	// broken-to-None has its OplockLevel updated to None on ACK (see
	// handleOplockBreakAck) — gating on the demoted level would leave the
	// synthetic-key lease record alive past CLOSE. Per Samba ack-to-None
	// semantics the record IS kept alive at LeaseState=None until CLOSE
	// removes it (Samba `share_mode_cleanup_disconnected`); CLOSE is the
	// release point. Gate on LeaseKey instead so the same release runs for
	// real leases (OplockLevelLease) and for traditional oplocks (LEVEL_II
	// / Exclusive / Batch and their broken-to-None descendants). Required by
	// smbtorture smb2.oplock.exclusive9 (multi-iter EXCLUSIVE+SUPERSEDE loop
	// where each iter's tree1 ack-to-None must clean up before the next
	// iter's tree1 EXCLUSIVE request).
	//
	// MUST run AFTER WaitAndDeleteOpenFile (step 10), not before: when this
	// holder closes its conflicting directory handle in response to a dir-lease
	// break, ReleaseLeaseForHandle signals WaitForBreakCompletion waiters (the
	// SET_INFO-rename dst-parent Handle-break wait). That waiter wakes and
	// re-runs checkParentDirRenameConflict against h.files. If the lease
	// release (and its signal) fired while this OpenFile were still in h.files,
	// the woken rename would observe the stale holder and return a spurious
	// STATUS_SHARING_VIOLATION instead of STATUS_OK — the intermittent
	// smbtorture smb2.dirlease.rename_dst_parent phase-2 failure. Removing the
	// OpenFile first makes the post-wait recheck see the shrunk table. This
	// mirrors the SignalParkedCreates ordering below, which was already moved
	// after the open-file removal for the dir-CREATE park path (v2_request).
	h.releaseHandleLeaseRecord(ctx.Context, openFile, "CLOSE")

	// Wake any parked dir-CREATE on this handle so it can re-evaluate
	// share-mode against the now-shrunk open-file table. The signal MUST
	// follow the open-file removal (not just the lease release) so the
	// parked CREATE's share-mode recheck does not still observe the
	// closing holder. Required by smbtorture smb2.dirlease.v2_request:
	// tree2's conflicting CREATE parks on tree1's breaking RH dir lease;
	// tree1's CLOSE must wake it without waiting for the 5 s async-break
	// timeout.
	//
	// Scoped to directory closes only: file-CREATE parks in
	// breakAndMaybeParkCreate use the same breakWait channel, but their
	// resume contract is "wait for actual ACK (or 5 s timeout
	// auto-downgrade)", not "wake on any CLOSE on the file". Signaling
	// for file closes prematurely wakes a parked file CREATE whose
	// holder closed without acking — smbtorture
	// smb2.kernel-oplocks.kernel_oplocks7 relies on tree2's parked
	// CREATE staying parked until the timeout while tree1's re-open
	// arrives first (CREATE returns EXCL with no other holder visible).
	if openFile.IsDirectory && h.LeaseManager != nil && len(openFile.MetadataHandle) > 0 {
		h.LeaseManager.SignalParkedCreates(lock.FileHandle(openFile.MetadataHandle), openFile.ShareName)
	}
	h.renameScanMu.Unlock()

	logger.Debug("CLOSE successful",
		"fileID", fmt.Sprintf("%x", req.FileID),
		"path", openFile.Path)

	// ========================================================================
	// Step 12: Return success response
	// ========================================================================

	return resp, nil
}

// releaseHandleLeaseRecord releases the per-handle lease/oplock record held by
// openFile, and (for traditional oplocks) unregisters the synthetic lease key →
// FileID mapping from the notifier. It is the single release point shared by the
// explicit CLOSE handler (close.go step 9) and the session/tree/transport
// teardown path (closeFilesWithFilter) so both clean up lease state identically.
//
// Why teardown can't rely on LeaseManager.ReleaseSessionLeases alone:
// ReleaseSessionLeases scans the LeaseManager's sessionMap (leaseKey → sessionID)
// for entries whose value equals the disconnecting session. That map is keyed by
// lease key only, so when a LATER session reuses the same numeric lease key on a
// DIFFERENT file (smbtorture reuses fixed LEASE1/LEASE2 macros across tests on
// fresh connections), the map entry is overwritten to point at the later session.
// The disconnecting session's own lock-manager record on its file then never
// matches and leaks — a stale lease holder that poisons the next test's CREATE
// (#568 rotating cross-test flake). Releasing by the open's OWN MetadataHandle +
// LeaseKey here is immune to that overwrite. Likewise the oplock FileID registry
// is server-global and was only ever unregistered on explicit CLOSE; teardown
// left stale entries behind.
//
// Release is scoped per-handle (ReleaseLeaseForHandle) so opens of OTHER files
// that legitimately share the same numeric lease key keep their records.
func (h *Handler) releaseHandleLeaseRecord(ctx context.Context, openFile *OpenFile, caller string) {
	if h.LeaseManager == nil {
		return
	}
	leaseKey := openFile.LeaseKey
	if leaseKey == ([16]byte{}) {
		return
	}

	// Check if any other open on the SAME FILE shares this lease key.
	// Two opens on the same file share one lease record (requestLeaseImpl
	// upgrades in place). Different files with the same key are separate
	// records in distinct handleKey buckets and must not be disturbed.
	hasOtherOpenSameFile := false
	h.files.Range(func(_, value any) bool {
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

	if hasOtherOpenSameFile {
		logger.Debug(caller+": lease handle closed (other opens share lease key on same file)",
			"path", openFile.Path)
		return
	}

	// Last open on this file with this lease key — release only this handle's
	// lease record. Other files sharing the key keep theirs.
	if err := h.LeaseManager.ReleaseLeaseForHandle(ctx, lock.FileHandle(openFile.MetadataHandle), leaseKey, openFile.ShareName); err != nil {
		logger.Debug(caller+": failed to release lease",
			"path", openFile.Path,
			"leaseKey", fmt.Sprintf("%x", leaseKey),
			"error", err)
	} else {
		logger.Debug(caller+": released lease (last open on this file)",
			"path", openFile.Path,
			"leaseKey", fmt.Sprintf("%x", leaseKey))
	}
	// Unregister oplock FileID mapping if this was a traditional oplock.
	if openFile.OplockLevel != OplockLevelLease {
		h.LeaseManager.UnregisterOplockFileID(leaseKey)
	}
}

// ============================================================================
// Helper Functions
// ============================================================================

// checkAndConvertMFsymlink checks if a file is an MFsymlink and converts it to a real symlink.
//
// MFsymlinks are 1067-byte files with XSym\n header used by macOS/Windows SMB clients
// for symlink creation. Steps:
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
	blockStore, err := common.ResolveForRead(ctx.Context, h.Registry, openFile.MetadataHandle)
	if err != nil {
		return nil, fmt.Errorf("block store not available: %w", err)
	}

	// Read the MFsymlink content (always 1067 bytes).
	// Routed through common.ReadFromBlockStore so any future []BlockRef
	// plumbing lands in one place (see common/doc.go).
	// The bytes are copied into a caller-owned slice because the MFsymlink
	// parse path retains them past Release().
	result, err := common.ReadFromBlockStore(ctx.Context, blockStore, openFile.PayloadID, 0, uint32(mfsymlink.Size))
	if err != nil {
		return nil, err
	}
	defer result.Release()

	out := make([]byte, len(result.Data))
	copy(out, result.Data)
	return out, nil
}

// convertToRealSymlink removes the regular file and creates a symlink in its place.
func (h *Handler) convertToRealSymlink(ctx *SMBHandlerContext, openFile *OpenFile, target string) error {
	// Validate required fields
	if len(openFile.ParentHandle) == 0 || openFile.FileName == "" {
		return fmt.Errorf("missing parent handle or filename for MFsymlink conversion")
	}

	// Prime ctx.User / IsGuest / TreeID from the OpenFile's recorded session
	// BEFORE BuildAuthContext — otherwise ctx.User==nil falls into the
	// anonymous arm and synthesises UID-0 (root), bypassing DACL checks on
	// the RemoveFile + CreateSymlink (#619, same class as #603).
	h.primeAuthContextFromOpenFile(ctx, openFile)

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return err
	}

	// Reject targets that escape the share root (absolute or `..`-traversal).
	// MFsymlink content is fully client-controlled, so an unsanitized target
	// could otherwise point an NFS client outside the share.
	if err := metadata.ValidateMFsymlinkTarget(target); err != nil {
		return fmt.Errorf("MFsymlink target rejected: %w", err)
	}

	// Get the parent handle and filename for removal and creation
	parentHandle := openFile.ParentHandle
	fileName := openFile.FileName

	// Remove the regular file
	metaSvc := h.Registry.GetMetadataService()
	removed, _, err := metaSvc.RemoveFile(authCtx, parentHandle, fileName)
	if err != nil {
		return fmt.Errorf("failed to remove MFsymlink file: %w", err)
	}

	// Delete content from block store (best-effort). RemoveFile returns an
	// empty PayloadID when content must survive (hard link / recycle).
	var removedPayloadID metadata.PayloadID
	if removed != nil {
		removedPayloadID = removed.PayloadID
	}
	h.purgeBlockStorePayload(ctx.Context, openFile.MetadataHandle, removedPayloadID, openFile.Path, "CLOSE")

	// Create the real symlink with default attributes
	// Pass empty FileAttr - CreateSymlink will apply defaults
	symlinkAttr := &metadata.FileAttr{}
	_, _, err = metaSvc.CreateSymlink(authCtx, parentHandle, fileName, target, symlinkAttr)
	if err != nil {
		return fmt.Errorf("failed to create symlink: %w", err)
	}

	logger.Debug("CLOSE: converted MFsymlink",
		"path", openFile.Path,
		"target", target)

	return nil
}

// rangeStreamsOfBase iterates over open files that are streams of a base file,
// filtering out self (by FileID), pipes, handles in a different parent
// directory, and names that don't start with basePrefix. The caller's fn
// receives each matching *OpenFile and returns true to continue or false to
// stop. This consolidates the repeated skip-self/skip-pipe/check-prefix
// pattern used in the DOC block of Close.
func (h *Handler) rangeStreamsOfBase(selfFileID [16]byte, parentHandle metadata.FileHandle, basePrefix string, fn func(*OpenFile) bool) {
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == selfFileID || other.IsPipe {
			return true
		}
		if !bytes.Equal(other.ParentHandle, parentHandle) {
			return true
		}
		if len(other.FileName) <= len(basePrefix) || !strings.EqualFold(other.FileName[:len(basePrefix)], basePrefix) {
			return true
		}
		return fn(other)
	})
}

// cascadeDeleteADSStreams removes all alternate data streams belonging to a
// base file that was just deleted. ADS streams are stored as sibling entries
// in the parent directory with names like "baseFile:streamName:$DATA".
// Per MS-FSA 2.1.5.9.7, deleting a file deletes all its streams.
func (h *Handler) cascadeDeleteADSStreams(authCtx *metadata.AuthContext, metaSvc *metadata.Service, openFile *OpenFile) {
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
		if len(entry.Name) > len(prefix) && strings.EqualFold(entry.Name[:len(prefix)], prefix) {
			_, _, deleteErr := metaSvc.RemoveFile(authCtx, openFile.ParentHandle, entry.Name)
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

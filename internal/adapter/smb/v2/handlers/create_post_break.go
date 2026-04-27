package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// createDraft carries the pre-break CREATE state between Create()'s
// parse/lookup phase and completeCreateAfterBreak. It exists so the post-break
// flow can run either inline (sync) or from a resume goroutine (async park on
// lease break), without rewriting the large post-break block as a closure.
type createDraft struct {
	req            *CreateRequest
	tree           *TreeConnection
	authCtx        *metadata.AuthContext
	filename       string
	baseName       string
	parentHandle   metadata.FileHandle
	existingFile   *metadata.File
	existingHandle metadata.FileHandle
	fileExists     bool
	createAction   types.CreateAction
	// isDirectoryRequest: FILE_DIRECTORY_FILE set in CreateOptions. Needed by
	// createNewFile to decide directory vs regular file creation.
	isDirectoryRequest bool
	// excludeOwner scopes lease-break exclusions to the opener's own key. Used
	// for parent-directory breaks (Step 7c) and post-break oplock/lease work.
	excludeOwner *lock.LockOwner
}

// finalize computes the opaque file handle for the existing file (if any) and
// returns the draft ready for completeCreateAfterBreak. A nil existingHandle
// on encode failure is safe: the share-mode recheck and lease-break dispatch
// both treat it as "no pre-existing open to contend with".
func (d *createDraft) finalize() *createDraft {
	if d.fileExists {
		if enc, err := metadata.EncodeFileHandle(d.existingFile); err == nil {
			d.existingHandle = enc
		}
	}
	return d
}

// isDestructiveDisposition reports whether a CreateDisposition will replace
// existing file content. OVERWRITE/OVERWRITE_IF/SUPERSEDE invalidate cached
// data and handles entirely (per MS-SMB2 3.3.4.7 / Samba delay_for_oplock_fn);
// other dispositions only require flushing dirty data.
func isDestructiveDisposition(d types.CreateDisposition) bool {
	switch d {
	case types.FileSupersede, types.FileOverwrite, types.FileOverwriteIf:
		return true
	}
	return false
}

// breakAndMaybeParkCreate dispatches the handle-lease break required before
// the post-break share-mode check, then decides between parking the CREATE on
// an interim STATUS_PENDING (returning a non-zero AsyncId) or waiting
// synchronously (returning 0). The caller proceeds to completeCreateAfterBreak
// when 0 is returned.
//
// Cases by return value:
//   - 0: no break dispatched OR break dispatched and waited inline. Caller
//     continues with the sync post-break flow.
//   - non-zero: CREATE is parked; caller must emit a STATUS_PENDING interim
//     response carrying this AsyncId. The resume goroutine delivers the final
//     response via AsyncCreateCompleteCallback.
func (h *Handler) breakAndMaybeParkCreate(ctx *SMBHandlerContext, d *createDraft) uint64 {
	// No existing file → no lease holder on this handle to break.
	if !d.fileExists || d.existingHandle == nil {
		return 0
	}
	// No lease manager or stat-only open (MS-SMB2 3.3.5.9.8) → skip break.
	if h.LeaseManager == nil || isStatOnlyOpen(d.req.DesiredAccess) {
		return 0
	}

	lockFileHandle := lock.FileHandle(d.existingHandle)
	shareName := d.tree.ShareName

	// Per Samba delay_for_oplock_fn: break-target depends on the new
	// opener's intent. Destructive disposition (OVERWRITE/SUPERSEDE) →
	// break to None; share-mode violation or DELETE_ON_CLOSE → strip
	// Handle so other holders drop cached handles ahead of the delete;
	// otherwise → strip Write.
	reason := lock.BreakReasonDefault
	switch {
	case isDestructiveDisposition(d.req.CreateDisposition):
		reason = lock.BreakReasonDestructive
	case d.req.CreateOptions&types.FileDeleteOnClose != 0,
		h.checkShareModeConflict(d.existingHandle, d.req.DesiredAccess, d.req.ShareAccess, d.filename):
		reason = lock.BreakReasonSharingViolation
	}

	var waitExceptKey [16]byte
	if d.excludeOwner != nil {
		waitExceptKey = d.excludeOwner.ExcludeLeaseKey
	}

	// Snapshot the per-reason delay-mask intersection BEFORE dispatching.
	// Per Samba `delay_for_oplock_fn` (source3/smbd/open.c lines 2458, 2577):
	//   - sharing violation              → delay_mask = SMB2_LEASE_HANDLE
	//   - non-violation (default/destr)  → delay_mask = SMB2_LEASE_WRITE
	// A CREATE only delays for a lease break when the existing holder's lease
	// type intersects the delay_mask. Without an intersecting bit, the break
	// is informational and the new opener proceeds inline while the holder is
	// notified asynchronously (smbtorture breaking4 contract). With the bit
	// set, dirty/cached state must be flushed before the new opener can see
	// consistent post-break state (timeout-disconnect / breaking3 contract).
	var delayMask uint32
	if reason == lock.BreakReasonSharingViolation {
		delayMask = lock.LeaseStateHandle
	} else {
		delayMask = lock.LeaseStateWrite
	}
	needsParkForFlush := h.LeaseManager.AnyHolderHasLeaseBits(lockFileHandle, shareName, waitExceptKey, delayMask)

	// Directory branch: fire-and-forget. The single-threaded test driver
	// can't ack until this CREATE returns, so waiting would deadlock.
	if d.existingFile.Type == metadata.FileTypeDirectory {
		if err := h.LeaseManager.BreakHandleLeasesOnOpenAsync(lockFileHandle, shareName, reason, d.excludeOwner); err != nil {
			logger.Debug("CREATE: directory handle lease break failed", "error", err)
		}
		return 0
	}

	// File branch: dispatch the break (non-blocking), then decide between
	// inline completion (no W to flush), async park, or sync wait.
	if err := h.LeaseManager.BreakHandleLeasesOnOpenAsync(lockFileHandle, shareName, reason, d.excludeOwner); err != nil {
		logger.Debug("CREATE: handle lease break failed", "error", err)
	}

	// No conflicting holder intersects the per-reason delay_mask (W for
	// non-violation/destructive, H for sharing-violation) ⇒ no wait needed.
	// Let the CREATE complete inline. The break notification still went out
	// so the holder invalidates its caches; we just don't block on its ACK.
	if !needsParkForFlush {
		return 0
	}

	// Async park is only safe when this CREATE is the last op in its message
	// (MS-SMB2 §3.3.4.4). NextCommand == 0 ⇒ standalone or last-in-compound;
	// nonzero means a related follow-up wants a FileID we haven't allocated
	// yet, so we must block inline instead.
	if ctx.NextCommand == 0 && h.LeaseManager.HasOtherBreakingLeases(lockFileHandle, shareName, waitExceptKey) {
		if asyncId := h.parkCreateOnLeaseBreak(ctx, d, lockFileHandle, waitExceptKey); asyncId != 0 {
			return asyncId
		}
	}

	// Sync fallback: bounded wait. On timeout forceCompleteBreaks auto-downgrades
	// other-key leases so the post-break recheck runs against deterministic state.
	waitCtx, cancelWait := context.WithTimeout(d.authCtx.Context, lease.AsyncCreateBreakWaitTimeout)
	defer cancelWait()
	if err := h.LeaseManager.WaitForOtherKeyBreaks(waitCtx, lockFileHandle, shareName, waitExceptKey); err != nil {
		logger.Debug("CREATE: sync break wait completed", "error", err)
	}
	return 0
}

// completeCreateAfterBreak runs the CREATE flow from the share-mode recheck
// through the final response build. Split out of Create() so the same code
// path serves both the synchronous break-wait path and the async-park resume
// goroutine (MS-SMB2 §3.3.5.9 + §3.3.4.7).
func (h *Handler) completeCreateAfterBreak(ctx *SMBHandlerContext, d *createDraft) *CreateResponse {
	req := d.req
	tree := d.tree
	authCtx := d.authCtx
	filename := d.filename
	baseName := d.baseName
	parentHandle := d.parentHandle
	existingFile := d.existingFile
	fileExists := d.fileExists
	createAction := d.createAction
	excludeOwner := d.excludeOwner

	// Share-mode recheck (fileExists path) after any lease breaks have drained.
	// A pre-break violation selected the Handle-strip break mask so the holder
	// could close cached handles on ack; the recheck runs against the post-break
	// open table to decide the final CREATE outcome.
	if fileExists && d.existingHandle != nil {
		if shareConflict := h.checkShareModeConflict(d.existingHandle, req.DesiredAccess, req.ShareAccess, filename); shareConflict {
			logger.Debug("CREATE: sharing violation",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
				"shareAccess", fmt.Sprintf("0x%x", req.ShareAccess))
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSharingViolation}}
		}
	}

	// Step 6d: Validate delete-on-close requirements per MS-FSA 2.1.5.1.2.1.
	if req.CreateOptions&types.FileDeleteOnClose != 0 {
		if !hasDeleteAccess(req.DesiredAccess) {
			logger.Debug("CREATE: delete-on-close without DELETE access",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess))
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}
		}
		if fileExists && existingFile.Type != metadata.FileTypeDirectory {
			attrs := FileAttrToSMBAttributes(&existingFile.FileAttr)
			if attrs&types.FileAttributeReadonly != 0 {
				logger.Debug("CREATE: delete-on-close on read-only file", "path", filename)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusCannotDelete}}
			}
		}
	}

	// Step 7: Perform create/open.
	var file *metadata.File
	var fileHandle metadata.FileHandle
	metaSvc := h.Registry.GetMetadataService()
	switch createAction {
	case types.FileOpened:
		file = existingFile
		var err error
		fileHandle, err = metadata.EncodeFileHandle(file)
		if err != nil {
			logger.Warn("CREATE: failed to encode handle", "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInternalError}}
		}
	case types.FileCreated:
		var err error
		file, fileHandle, err = h.createNewFile(authCtx, parentHandle, baseName, req, d.isDirectoryRequest)
		if err != nil {
			logger.Warn("CREATE: failed to create file", "name", baseName, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}
		}
	case types.FileOverwritten, types.FileSuperseded:
		var err error
		file, fileHandle, err = h.overwriteFile(authCtx, existingFile, req)
		if err != nil {
			logger.Warn("CREATE: failed to overwrite file", "name", baseName, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}
		}
	}

	// Step 7a: Restore frozen timestamps on parent directory (MS-FSA 2.1.5.14.2).
	if createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded {
		h.restoreParentDirFrozenTimestamps(authCtx, parentHandle)
	}

	// Step 7b: Update base object ChangeTime for ADS operations (MS-FSA / NTFS).
	if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 && (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) {
		h.updateBaseObjectCtime(authCtx, metaSvc, parentHandle, baseName[:colonIdx])
	}

	// Step 7c: Break parent directory Handle / Read leases on create/overwrite/supersede.
	if (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) && h.LeaseManager != nil {
		parentLockHandle := lock.FileHandle(parentHandle)
		excludeClientID := fmt.Sprintf("smb:%d", ctx.SessionID)
		if breakErr := h.LeaseManager.BreakParentHandleLeasesOnCreate(authCtx.Context, parentLockHandle, tree.ShareName, excludeClientID); breakErr != nil {
			logger.Debug("CREATE: parent directory Handle lease break failed", "error", breakErr)
		}
		if breakErr := h.LeaseManager.BreakParentReadLeasesOnModify(authCtx.Context, parentLockHandle, tree.ShareName, excludeClientID); breakErr != nil {
			logger.Debug("CREATE: parent directory Read lease break failed", "error", breakErr)
		}
	}

	// Step 8: Generate FileID.
	smbFileID := h.GenerateFileID()

	// Step 8a: Break conflicting oplocks/leases on existing files for no-oplock opens.
	if fileExists && h.LeaseManager != nil && file.Type != metadata.FileTypeDirectory &&
		req.OplockLevel == OplockLevelNone && !isStatOnlyOpen(req.DesiredAccess) {
		lockFileHandle := lock.FileHandle(fileHandle)
		if breakErr := h.LeaseManager.BreakConflictingOplocksOnOpen(lockFileHandle, tree.ShareName, excludeOwner); breakErr != nil {
			logger.Debug("CREATE: oplock break on open failed", "error", breakErr)
		}
	}

	// Step 8b: Request oplock or lease if applicable.
	var grantedOplock uint8
	var leaseResponse *LeaseResponseContext
	var syntheticLeaseKey [16]byte

	if req.OplockLevel == OplockLevelLease && h.LeaseManager != nil {
		if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
			lockFileHandle := lock.FileHandle(fileHandle)
			var err error
			leaseResponse, err = ProcessLeaseCreateContext(
				authCtx.Context,
				h.LeaseManager,
				leaseCtx.Data,
				lockFileHandle,
				ctx.SessionID,
				fmt.Sprintf("smb:%d", ctx.SessionID),
				tree.ShareName,
				file.Type == metadata.FileTypeDirectory,
			)
			if err != nil {
				if errors.Is(err, lock.ErrInvalidLeaseState) || errors.Is(err, lock.ErrLeaseKeyInUse) {
					// Roll back the open we already performed at Step 7. Without
					// this, a CREATE_NEW that fails the lease_match check leaves
					// an orphaned inode in the metadata store with no handle
					// referencing it. Samba's open path closes the fd on
					// lease_match failure for the same reason. Rollback only
					// applies to FileCreated; FileOpened reuses an existing
					// file we did not create, and FileOverwritten/FileSuperseded
					// would lose user data — leave those intact.
					if createAction == types.FileCreated {
						if _, delErr := metaSvc.RemoveFile(authCtx, parentHandle, baseName); delErr != nil {
							logger.Warn("CREATE: failed to roll back orphaned file after lease rejection",
								"name", baseName, "error", delErr)
						}
					}
					return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
				}
				logger.Debug("CREATE: lease context processing failed", "error", err)
			}
			if leaseResponse != nil {
				grantedOplock = OplockLevelLease
				if leaseResponse.LeaseState == lock.LeaseStateNone {
					logger.Debug("CREATE: lease denied, returning OplockLevel=0xFF with LeaseState=None")
				}
			}
		} else {
			grantedOplock = OplockLevelNone
			logger.Debug("CREATE: OplockLevel=Lease without RqLs context, granting None")
		}
	}

	if grantedOplock == OplockLevelNone && req.OplockLevel != OplockLevelNone &&
		req.OplockLevel != OplockLevelLease && h.LeaseManager != nil &&
		file.Type != metadata.FileTypeDirectory {
		var requestedState uint32
		switch req.OplockLevel {
		case OplockLevelBatch:
			requestedState = lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
		case OplockLevelExclusive:
			requestedState = lock.LeaseStateRead | lock.LeaseStateWrite
		case OplockLevelII:
			requestedState = lock.LeaseStateRead
		}
		if requestedState != 0 {
			syntheticKey := generateSyntheticLeaseKey(smbFileID)
			lockFileHandle := lock.FileHandle(fileHandle)
			ownerID := fmt.Sprintf("smb:oplock:%x", smbFileID)
			clientID := fmt.Sprintf("smb:%d", ctx.SessionID)
			grantedState, _, err := h.LeaseManager.RequestLease(
				authCtx.Context,
				lockFileHandle,
				syntheticKey,
				[16]byte{},
				ctx.SessionID,
				ownerID,
				clientID,
				tree.ShareName,
				requestedState,
				false,
			)
			if err != nil {
				logger.Debug("CREATE: traditional oplock lease request failed", "error", err)
			} else {
				grantedOplock = leaseStateToOplockLevel(grantedState)
				if grantedOplock != OplockLevelNone {
					syntheticLeaseKey = syntheticKey
					h.LeaseManager.RegisterOplockFileID(syntheticKey, smbFileID)
				}
				logger.Debug("CREATE: traditional oplock mapped to lease",
					"requestedOplock", oplockLevelName(req.OplockLevel),
					"grantedOplock", oplockLevelName(grantedOplock),
					"leaseState", lock.LeaseStateToString(grantedState))
			}
		}
	}

	// Step 8c: Process App Instance ID and durable handle grant.
	var durableResponseCtx *CreateContext
	var appInstanceId [16]byte
	if h.DurableStore != nil {
		appInstanceId = ProcessAppInstanceId(
			authCtx.Context, h.DurableStore, h, req.CreateContexts,
		)
	}

	openFile := &OpenFile{
		FileID:         smbFileID,
		TreeID:         ctx.TreeID,
		SessionID:      ctx.SessionID,
		Path:           filename,
		ShareName:      tree.ShareName,
		OpenTime:       time.Now(),
		DesiredAccess:  req.DesiredAccess,
		IsDirectory:    file.Type == metadata.FileTypeDirectory,
		MetadataHandle: fileHandle,
		PayloadID:      file.PayloadID,
		ParentHandle:   parentHandle,
		FileName:       baseName,
		OplockLevel:    grantedOplock,
		ShareAccess:    req.ShareAccess,
		CreateOptions:  req.CreateOptions,
		DeletePending:  req.CreateOptions&types.FileDeleteOnClose != 0,
	}

	if leaseResponse != nil && leaseResponse.LeaseState != lock.LeaseStateNone {
		openFile.LeaseKey = leaseResponse.LeaseKey
	} else if syntheticLeaseKey != ([16]byte{}) {
		openFile.LeaseKey = syntheticLeaseKey
	}

	if h.DurableStore != nil {
		hasHandleLease := leaseResponse != nil && leaseResponse.LeaseState&lock.LeaseStateHandle != 0
		if respCtx := ProcessDurableHandleContext(
			req.CreateContexts, openFile, h.DurableTimeoutMs, hasHandleLease,
		); respCtx != nil {
			durableResponseCtx = respCtx
		}
		if openFile.IsDurable && appInstanceId != ([16]byte{}) {
			openFile.AppInstanceId = appInstanceId
		}
	}

	h.StoreOpenFile(openFile)

	logger.Debug("CREATE successful",
		"fileID", fmt.Sprintf("%x", smbFileID),
		"filename", filename,
		"action", createAction,
		"isDirectory", openFile.IsDirectory,
		"fileType", int(file.Type),
		"fileSize", file.Size,
		"oplock", oplockLevelName(grantedOplock))

	// Step 9: Notify change watchers.
	if h.NotifyRegistry != nil {
		parentPath := GetParentPath(filename)
		switch createAction {
		case types.FileCreated:
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionAdded)
		case types.FileOverwritten, types.FileSuperseded:
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionModified)
		}
	}

	// Step 10: Build success response.
	creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
	size := getSMBSize(&file.FileAttr)
	allocationSize := calculateAllocationSize(size)

	resp := &CreateResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		OplockLevel:     grantedOplock,
		CreateAction:    createAction,
		CreationTime:    creation,
		LastAccessTime:  access,
		LastWriteTime:   write,
		ChangeTime:      change,
		AllocationSize:  allocationSize,
		EndOfFile:       size,
		FileAttributes:  FileAttrToSMBAttributes(&file.FileAttr),
		FileID:          smbFileID,
	}

	if leaseResponse != nil {
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: LeaseContextTagResponse,
			Data: leaseResponse.Encode(),
		})
		logger.Debug("CREATE: lease granted in response",
			"leaseKey", fmt.Sprintf("%x", leaseResponse.LeaseKey),
			"grantedState", lock.LeaseStateToString(leaseResponse.LeaseState),
			"epoch", leaseResponse.Epoch)
	}

	if durableResponseCtx != nil {
		resp.CreateContexts = append(resp.CreateContexts, *durableResponseCtx)
		logger.Debug("CREATE: durable handle granted in response",
			"isDurable", openFile.IsDurable,
			"createGuid", fmt.Sprintf("%x", openFile.CreateGuid),
			"timeoutMs", openFile.DurableTimeoutMs)
	}

	if FindCreateContext(req.CreateContexts, "MxAc") != nil {
		maxAccess := computeMaximalAccess(file, authCtx)
		mxW := smbenc.NewWriter(8)
		mxW.WriteUint32(0)
		mxW.WriteUint32(maxAccess)
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: "MxAc",
			Data: mxW.Bytes(),
		})
		logger.Debug("CREATE: MxAc response added",
			"maximalAccess", fmt.Sprintf("0x%08x", maxAccess))
	}

	if FindCreateContext(req.CreateContexts, "QFid") != nil {
		qfidResp := make([]byte, 32)
		copy(qfidResp[0:16], file.ID[:16])
		copy(qfidResp[16:32], h.ServerGUID[:])
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: "QFid",
			Data: qfidResp,
		})
		logger.Debug("CREATE: QFid response added",
			"diskFileId", fmt.Sprintf("%x", file.ID[:16]))
	}

	return resp
}

// parkCreateOnLeaseBreak reserves an async slot, registers a pending CREATE,
// and spawns a resume goroutine that waits for the break to drain and then
// completes the CREATE via AsyncCreateCompleteCallback. Returns the generated
// AsyncId on success, or 0 when async parking is not possible (e.g. no async
// slots left; registry rejected the entry). Callers fall back to sync wait.
func (h *Handler) parkCreateOnLeaseBreak(
	ctx *SMBHandlerContext,
	d *createDraft,
	lockFileHandle lock.FileHandle,
	waitExceptKey [16]byte,
) uint64 {
	if h.PendingCreateRegistry == nil || ctx.AsyncCreateCompleteCallback == nil ||
		ctx.TryReserveAsync == nil || ctx.ReleaseAsync == nil {
		return 0
	}

	if !ctx.TryReserveAsync() {
		logger.Debug("CREATE: async park rejected — max async credits",
			"sessionID", ctx.SessionID,
			"messageID", ctx.MessageID)
		return 0
	}

	asyncId := h.generateAsyncId()

	// Wait context: independent of the request's Context (which would be torn
	// down as soon as we return StatusPending). Cancellable by SMB2_CANCEL and
	// session teardown, bounded by AsyncCreateBreakWaitTimeout so a missing ACK
	// auto-downgrades other-key leases and lets the CREATE proceed.
	waitCtx, cancel := context.WithTimeout(context.Background(), lease.AsyncCreateBreakWaitTimeout)

	pending := &PendingCreate{
		ConnID:    ctx.ConnID,
		SessionID: ctx.SessionID,
		MessageID: ctx.MessageID,
		AsyncId:   asyncId,
		Cancel:    cancel,
		Callback:  ctx.AsyncCreateCompleteCallback,
	}

	if err := h.PendingCreateRegistry.Register(pending); err != nil {
		cancel()
		ctx.ReleaseAsync()
		logger.Warn("CREATE: async park rejected — registry full",
			"sessionID", ctx.SessionID,
			"messageID", ctx.MessageID,
			"error", err)
		return 0
	}

	shareName := d.tree.ShareName
	messageID := ctx.MessageID

	go func() {
		defer cancel()

		// Wait for the other-key break to drain (or timeout auto-downgrade).
		// Errors here are logged but not propagated: on timeout the lease
		// manager has auto-downgraded other-key leases, so the CREATE can
		// still proceed (same semantics as the sync wait path).
		if err := h.LeaseManager.WaitForOtherKeyBreaks(waitCtx, lockFileHandle, shareName, waitExceptKey); err != nil {
			logger.Debug("CREATE async: break wait completed",
				"messageID", messageID,
				"asyncId", asyncId,
				"error", err)
		}

		// Ensure our entry is still live (not preempted by CANCEL or teardown).
		// If it was, CANCEL / teardown already sent the final response.
		if h.PendingCreateRegistry.Unregister(asyncId) == nil {
			return
		}

		// Guard against tree disconnect that happened while we were parked.
		// If the tree is gone, the session-level OpenFile map would leak a
		// stale handle because CloseAllFilesForTree ran before we stored
		// ours. Fail the CREATE with STATUS_NETWORK_NAME_DELETED instead.
		if _, ok := h.GetTree(ctx.TreeID); !ok {
			logger.Debug("CREATE async: tree disconnected while parked",
				"messageID", messageID,
				"asyncId", asyncId,
				"treeID", ctx.TreeID)
			if err := pending.Callback(pending.SessionID, messageID, asyncId, types.StatusNetworkNameDeleted, nil); err != nil {
				logger.Debug("CREATE async: failed to send tree-deleted response", "error", err)
			}
			return
		}

		resp := h.completeCreateAfterBreak(ctx, d)
		status := resp.GetStatus()
		var body []byte
		if status == types.StatusSuccess {
			encoded, err := resp.Encode()
			if err != nil {
				logger.Warn("CREATE async: encode failed", "error", err)
				status = types.StatusInternalError
			} else {
				body = encoded
			}
		}

		if err := pending.Callback(pending.SessionID, messageID, asyncId, status, body); err != nil {
			logger.Warn("CREATE async: failed to send final response",
				"messageID", messageID,
				"asyncId", asyncId,
				"error", err)
		}
	}()

	logger.Debug("CREATE: parked on lease break — sent interim STATUS_PENDING",
		"sessionID", ctx.SessionID,
		"messageID", ctx.MessageID,
		"asyncId", asyncId)
	return asyncId
}

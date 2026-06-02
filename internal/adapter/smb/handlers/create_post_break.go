package handlers

import (
	"bytes"
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
	// appInstanceProcessed records that ProcessAppInstanceId already ran in the
	// pre-break CREATE path (so any conflicting open carrying the same
	// AppInstanceId was force-closed BEFORE the oplock/lease break dispatch).
	// completeCreateAfterBreak reuses appInstanceId rather than re-running the
	// force-close. Per MS-SMB2 §3.3.5.9.13 the AppInstanceId failover MUST NOT
	// generate an oplock break on the displaced open (smbtorture
	// smb2.durable-v2-open.app-instance asserts break_info.count == 0).
	appInstanceProcessed bool
	appInstanceId        [16]byte
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

// effectiveAccessForOpen folds disposition-implied access bits into the
// client-requested DesiredAccess, mirroring Samba's `open_access_mask`
// computation in source3/smbd/open.c::open_file_ntcreate:
//
//	open_access_mask = access_mask;
//	if (flags & O_TRUNC) {
//	    open_access_mask |= FILE_WRITE_DATA; /* This will cause oplock breaks. */
//	}
//
// Per MS-FSA §2.1.5.1.2.1, OVERWRITE / OVERWRITE_IF / SUPERSEDE on an existing
// file inherently truncate it and so require FILE_WRITE_DATA regardless of
// what the client put in DesiredAccess. Samba uses `open_access_mask` for
// BOTH the DACL access-rights check (smbd_check_access_rights_fsp) AND the
// share-mode conflict check (open_mode_check). We mirror that here so:
//   - a DACL granting only READ_DATA fails OVERWRITE/SUPERSEDE with
//     STATUS_ACCESS_DENIED (smb2.acls.OVERWRITE_READ_ONLY_FILE fs_tcases arm,
//     #565)
//   - an existing handle with SHARE_READ but no SHARE_WRITE causes a second
//     destructive open to fail with STATUS_SHARING_VIOLATION (smb2.acls.
//     OVERWRITE_READ_ONLY_FILE sharing_tcases arm, #575)
//
// MAXIMUM_ALLOWED skips the augmentation: MAX expansion already reflects the
// requester's full effective rights without exposing the implied write as an
// explicit-bit denial, and the share-mode `hasWrite` helper already keys off
// MAX as implying write. Matches Samba — the FILE_WRITE_DATA fold only fires
// for the non-MAX disposition path.
func effectiveAccessForOpen(desiredAccess uint32, disposition types.CreateDisposition) uint32 {
	const maxAllowedBit uint32 = 0x02000000
	if desiredAccess&maxAllowedBit != 0 {
		return desiredAccess
	}
	if !isDestructiveDisposition(disposition) {
		return desiredAccess
	}
	return desiredAccess | uint32(types.FileWriteData)
}

// hasOtherNonStatOpenForFile reports whether any open in h.files refers to
// the same metadata handle as fileHandle (excluding the open identified by
// selfFileID) AND has a non-stat-only access mask. Stat-only opens are
// excluded because Samba's `disallow_write_lease` predicate
// (source3/smbd/open.c lines 2397-2403) ignores them — they do not invalidate
// the exclusive caching premise of a Batch/Exclusive grant on a subsequent
// opener.
//
// Used by the traditional-oplock grant path to coerce Batch/Exclusive to
// LEVEL_II when a previously-existing raw (non-oplocked) open is present.
// Callers must additionally verify that no lease/oplock record exists for the
// file via LeaseManager.HasAnyLeaseRecord — when a record exists (even at
// LeaseStateNone post-break-timeout), bestGrantableState already handles the
// grant correctly and the coarse OpenFile coercion would incorrectly demote
// a grant that the lease layer would otherwise allow (smbtorture batch22b
// post-timeout re-grant).
func (h *Handler) hasOtherNonStatOpenForFile(fileHandle metadata.FileHandle, selfFileID [16]byte) bool {
	if len(fileHandle) == 0 {
		return false
	}
	found := false
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == selfFileID {
			return true
		}
		if len(other.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(other.MetadataHandle, fileHandle) {
			return true
		}
		// Skip stat-only existing opens (Samba `is_oplock_stat_open` carve-out
		// in `delay_for_oplock_fn`).
		if isOplockStatOpen(other.DesiredAccess) {
			return true
		}
		found = true
		return false
	})
	return found
}

// hasSameClientNonStatOpenForFile is the ClientGUID-scoped variant of
// hasOtherNonStatOpenForFile. It reports whether any open in h.files on the
// same metadata handle (excluding selfFileID) is owned by the SAME SMB
// ClientGUID AND has a non-stat-only access mask.
//
// Used by the post-break trad-oplock grant path to distinguish the two arms
// of smbtorture smb2.oplock.batch22{a,b}:
//
//   - batch22b (different ClientGUID, tree2 opens after tree1's batch break
//     times out): tree2 must receive a fresh BATCH grant — the abandoned
//     holder is on a different client, so its still-alive OpenFile does not
//     invalidate batch caching semantics for the new client.
//   - batch22a (same ClientGUID, h2 opens on tree1 after h1's batch break
//     times out): h2 must receive LEVEL_II — h1 is still alive on this
//     client and may hold dirty cached data, so exclusive batch caching
//     cannot be granted again to the same client.
//
// The all-tombstones gate (OnlyTimeoutTombstoneRecords) handles the
// different-client side by skipping the strip when only timeout tombstones
// remain. This helper carves out the same-client exception: even after
// timeout, a same-ClientGUID non-stat open constrains the new grant.
//
// Mirrors Samba's `disallow_write_lease` predicate (source3/smbd/open.c
// lines 2397-2403) which gates on whether the existing entry's connection
// matches the requestor's client — Samba's share entries carry the open's
// connection identity directly; we approximate via ClientGUID equality.
func (h *Handler) hasSameClientNonStatOpenForFile(
	fileHandle metadata.FileHandle,
	selfFileID [16]byte,
	clientGUID [16]byte,
) bool {
	if len(fileHandle) == 0 {
		return false
	}
	found := false
	h.files.Range(func(_, value any) bool {
		other := value.(*OpenFile)
		if other.FileID == selfFileID {
			return true
		}
		if len(other.MetadataHandle) == 0 {
			return true
		}
		if !bytes.Equal(other.MetadataHandle, fileHandle) {
			return true
		}
		if other.ClientGUID != clientGUID {
			return true
		}
		if isOplockStatOpen(other.DesiredAccess) {
			return true
		}
		found = true
		return false
	})
	return found
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
	if h.LeaseManager == nil {
		return 0
	}

	lockFileHandle := lock.FileHandle(d.existingHandle)
	shareName := d.tree.ShareName

	// Stat-only opens skip the break for non-destructive dispositions only.
	// Per MS-SMB2 §3.3.5.9.8 + Samba `is_lease_stat_open` (source3/smbd/open.c),
	// READ_ATTRIBUTES / WRITE_ATTRIBUTES / SYNCHRONIZE / READ_CONTROL
	// combinations do not trigger lease breaks — UNLESS the disposition is
	// destructive (OVERWRITE / OVERWRITE_IF / SUPERSEDE), in which case the
	// new open will replace the file's content and a break is required
	// regardless of the access mask. smbtorture smb2.oplock.batch13/14/16 +
	// smb2.oplock.exclusive5 cover destructive+stat-only break.
	//
	// A narrower oplock variant (no READ_CONTROL) applies when an existing
	// holder is a traditional oplock — Samba's `is_oplock_stat_open` — so a
	// READ_CONTROL-only new open MUST break a traditional-oplock holder even
	// for non-destructive dispositions. Covers smbtorture
	// smb2.oplock.statopen1 test 8.
	if isStatOnlyOpen(d.req.DesiredAccess) && !isDestructiveDisposition(d.req.CreateDisposition) {
		// Stat-only per the lease mask. Apply the narrower oplock rule only
		// when there is a traditional-oplock holder AND the new opener carries
		// READ_CONTROL (the only bit that differs between the two masks).
		if d.req.DesiredAccess&uint32(0x00020000) == 0 {
			return 0
		}
		if !h.LeaseManager.AnyHolderIsTraditionalOplock(lockFileHandle, shareName) {
			return 0
		}
		// Fall through to dispatch the break for the READ_CONTROL-on-oplock
		// case.
	}

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

	// Directory branch: dispatch the break fire-and-forget, then decide
	// between inline completion and async park (mirrors the file branch
	// below, scoped to the share-mode-conflict case).
	//
	// Most dir CREATEs do NOT need to park: the existing dir-lease holder is
	// either same-client / same-key (suppressed) or holds RHW so a new opener
	// with the same share-mode is admitted alongside it. Only a share-mode
	// conflict actually delays the new opener — the holder must release
	// (close) before the CREATE can proceed. Required by smbtorture
	// smb2.dirlease.v2_request second-attempt step: tree2 sends an
	// `share_access=""` CREATE on a dir already opened by tree1 with `RWD`,
	// which conflicts; the test expects STATUS_PENDING + a deferred response
	// that completes with OK once tree1 closes its handle.
	if d.existingFile.Type == metadata.FileTypeDirectory {
		if err := h.LeaseManager.BreakHandleLeasesOnOpenAsync(lockFileHandle, shareName, reason, d.excludeOwner); err != nil {
			logger.Debug("CREATE: directory handle lease break failed", "error", err)
		}

		// Park only when the conflicting holder both intersects the
		// per-reason delay-mask AND there is at least one OTHER breaking
		// lease to wait on (the holder's own break we just dispatched).
		// Without these guards, dir CREATEs that don't need parking would
		// pay an unnecessary roundtrip.
		if reason != lock.BreakReasonSharingViolation || !needsParkForFlush {
			return 0
		}
		if !h.LeaseManager.HasOtherBreakingLeases(lockFileHandle, shareName, waitExceptKey) {
			return 0
		}
		if asyncId := h.parkCreateOnLeaseBreak(ctx, d, lockFileHandle, waitExceptKey, lease.AsyncCreateBreakWaitTimeout); asyncId != 0 {
			return asyncId
		}
		// Park failed (no slots / registry full): fall through to sync
		// wait, then let completeCreateAfterBreak re-evaluate share mode.
		waitCtx, cancelWait := context.WithTimeout(d.authCtx.Context, lease.AsyncCreateBreakWaitTimeout)
		defer cancelWait()
		if err := h.LeaseManager.WaitForOtherKeyBreaks(waitCtx, lockFileHandle, shareName, waitExceptKey); err != nil {
			logger.Debug("CREATE: sync directory break wait completed", "error", err)
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

	// Per MS-SMB2 §3.3.4.6 step 4: when the existing holder is a traditional
	// SMB oplock (LEVEL_II / Exclusive / Batch), the server waits for the
	// implementation-specific default of ~35 s before declaring the break
	// failed. SMB2.1+ leases keep the shorter 5 s bound (different timing
	// semantics; existing breaking3 / timeout-disconnect tests rely on it).
	// smbtorture batch22a / batch22b assert te ∈ [34, 50] when the holder
	// does not ack — verifying the 35 s grace.
	breakWaitTimeout := lease.AsyncCreateBreakWaitTimeout
	if h.LeaseManager.AnyHolderIsTraditionalOplock(lockFileHandle, shareName) {
		breakWaitTimeout = lease.TraditionalOplockBreakWaitTimeout
	}

	// Per MS-SMB2 §3.3.4.4 and smbtorture compound_async.getinfo_middle:
	// When a compound CREATE needs to wait for a lease break, it MUST go async
	// (STATUS_PENDING) even if it is not the last command in the compound.
	// The compound processor handles non-last STATUS_PENDING by sending the
	// interim response standalone and deferring remaining compound commands
	// until the CREATE completes. Without this, the sync wait path deadlocks:
	// the test (and real clients) cannot ACK the lease break until they
	// receive the STATUS_PENDING interim response.
	if h.LeaseManager.HasOtherBreakingLeases(lockFileHandle, shareName, waitExceptKey) {
		if asyncId := h.parkCreateOnLeaseBreak(ctx, d, lockFileHandle, waitExceptKey, breakWaitTimeout); asyncId != 0 {
			return asyncId
		}
	}

	// Sync fallback: bounded wait. On timeout forceCompleteBreaks auto-downgrades
	// other-key leases so the post-break recheck runs against deterministic state.
	waitCtx, cancelWait := context.WithTimeout(d.authCtx.Context, breakWaitTimeout)
	defer cancelWait()
	if err := h.LeaseManager.WaitForOtherKeyBreaks(waitCtx, lockFileHandle, shareName, waitExceptKey); err != nil {
		logger.Debug("CREATE: sync break wait completed", "error", err)
	}
	return 0
}

// recheckExistingFileGates runs the share-mode conflict check, the DACL
// access-rights gate, and the delete-on-close (read-only) checks against the
// CURRENT draft view (d.existingFile / d.existingHandle / d.fileExists /
// d.createAction). It is intentionally view-driven rather than closing over
// the caller's local snapshot so it can be re-run after the draft is resynced
// to the winner of a metadata-store TOCTOU create race (completeCreateAfterBreak
// ErrAlreadyExists branch): the pre-break invocation evaluated these gates
// against the (nil) draft view, so a race winner must be re-gated before the
// overwrite/open path proceeds (#765).
//
// Returns:
//   - grantedAccess / grantedComputed: the effective rights for the open,
//     computed from the existing file's DACL (per MS-SMB2 §3.3.5.9 paragraph 8).
//     grantedComputed is false on the new-file path where no existing DACL gate
//     runs; the caller falls back to resolveAccessFlags(DesiredAccess).
//   - failResp: non-nil when a gate denies the open; the caller returns it
//     verbatim. nil on success.
func (h *Handler) recheckExistingFileGates(d *createDraft, effectiveAccess uint32) (grantedAccess uint32, grantedComputed bool, failResp *CreateResponse) {
	req := d.req
	authCtx := d.authCtx
	filename := d.filename
	parentHandle := d.parentHandle
	existingFile := d.existingFile
	fileExists := d.fileExists
	createAction := d.createAction
	metaSvc := h.Registry.GetMetadataService()

	// Share-mode recheck (fileExists path) after any lease breaks have drained.
	// A pre-break violation selected the Handle-strip break mask so the holder
	// could close cached handles on ack; the recheck runs against the post-break
	// open table to decide the final CREATE outcome.
	//
	// Uses effectiveAccess (not raw req.DesiredAccess) so a destructive
	// disposition with a read-only DesiredAccess (e.g. SUPERSEDE+READ_DATA)
	// still trips the SHARE_WRITE deny check against an existing SHARE_READ-only
	// holder, returning STATUS_SHARING_VIOLATION. Covers
	// smb2.acls.OVERWRITE_READ_ONLY_FILE sharing_tcases arm (#575).
	if fileExists && d.existingHandle != nil {
		if shareConflict := h.checkShareModeConflict(d.existingHandle, effectiveAccess, req.ShareAccess, filename); shareConflict {
			logger.Debug("CREATE: sharing violation",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
				"effectiveAccess", fmt.Sprintf("0x%x", effectiveAccess),
				"shareAccess", fmt.Sprintf("0x%x", req.ShareAccess),
				"disposition", req.CreateDisposition)
			return 0, false, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusSharingViolation}}
		}
	}

	// Step 6c-bis: Enforce DesiredAccess against the existing file's DACL.
	//
	// Per MS-SMB2 §3.3.5.9 and MS-FSA §2.1.5.1.2.1, the server MUST evaluate
	// the requested access bits against the object's security descriptor and
	// fail the open with STATUS_ACCESS_DENIED when any non-MAXIMUM_ALLOWED bit
	// is denied. New files (createAction == FileCreated) inherit their ACL from
	// the parent at create time and don't need this check — parent write was
	// already gated via CheckParentWriteAccess in Create(). Tracking #529.
	//
	// grantedAccess captures the effective rights for the open per
	// MS-SMB2 §3.3.5.9 paragraph 8: the per-bit intersection of the
	// requested mask with the file's DACL. Carried onto OpenFile.GrantedAccess
	// below and consumed by FileAccessInformation (#548, MS-FSCC §2.4.1) and
	// the QUERY_INFO open-level access gate (MS-SMB2 §3.3.5.20.1).
	if fileExists && existingFile != nil {
		// Fetch parent so CheckFileAccessWithParent can apply the
		// FILE_DELETE_CHILD override per MS-FSA §2.1.4.13 (Samba
		// parent_override_delete). Best-effort: a parent-lookup failure
		// falls back to file-only DACL evaluation (nil parent is safe).
		var parentFile *metadata.File
		if parentHandle != nil {
			if pf, err := metaSvc.GetFile(authCtx.Context, parentHandle); err == nil {
				parentFile = pf
			}
		}

		// effectiveAccess (computed by the caller) folds the
		// disposition-implied FILE_WRITE_DATA into the mask. The DACL check
		// uses the same augmented mask Samba's smbd_check_access_rights_fsp
		// receives, so a DACL that grants only READ_DATA fails OVERWRITE /
		// OVERWRITE_IF / SUPERSEDE with STATUS_ACCESS_DENIED. Covers
		// smb2.acls.OVERWRITE_READ_ONLY_FILE fs_tcases arm (#565).
		granted, err := metaSvc.CheckFileAccessWithParentGeneric(existingFile, parentFile, authCtx, effectiveAccess, req.GenericDerivedAccess)
		if err != nil {
			logger.Debug("CREATE: DesiredAccess denied by DACL",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess),
				"effectiveAccess", fmt.Sprintf("0x%x", effectiveAccess),
				"granted", fmt.Sprintf("0x%x", granted),
				"disposition", req.CreateDisposition,
				"error", err)
			return 0, false, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}
		}
		// Preserve the client-visible grant: clear the disposition-implied
		// bit before propagating so QUERY_INFO / FileAccessInformation report
		// only what was actually requested (Samba `fsp->access_mask` mirrors
		// the original DesiredAccess after the open succeeds — see
		// open_file_ntcreate line where access_mask is restored).
		if effectiveAccess != req.DesiredAccess {
			granted &^= (effectiveAccess &^ req.DesiredAccess)
		}
		grantedAccess = granted
		grantedComputed = true
	}

	// Step 6d: Validate delete-on-close requirements per MS-FSA 2.1.5.1.2.1.
	if req.CreateOptions&types.FileDeleteOnClose != 0 {
		if !hasDeleteAccess(req.DesiredAccess) {
			logger.Debug("CREATE: delete-on-close without DELETE access",
				"path", filename,
				"desiredAccess", fmt.Sprintf("0x%x", req.DesiredAccess))
			return 0, false, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}
		}
		if fileExists && existingFile.Type != metadata.FileTypeDirectory {
			attrs := FileAttrToSMBAttributes(&existingFile.FileAttr)
			if attrs&types.FileAttributeReadonly != 0 {
				logger.Debug("CREATE: delete-on-close on read-only file", "path", filename)
				return 0, false, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusCannotDelete}}
			}
		}
		// READONLY+DOC is forbidden per MS-FSA 2.1.5.1.2.1: the resulting file
		// would be marked DOC and READONLY simultaneously, but READONLY blocks
		// the eventual unlink. Only fires when req.FileAttributes actually
		// propagates to the resulting file — dispositions that create or
		// rewrite (CREATE/CREATE_IF/SUPERSEDE/OVERWRITE/OVERWRITE_IF). For
		// FILE_OPEN / FILE_OPEN_IF on an existing file the request attrs are
		// ignored and the disk attrs apply; that path is covered by the
		// existing-file arm above.
		propagatesReqAttrs := createAction == types.FileCreated ||
			createAction == types.FileOverwritten ||
			createAction == types.FileSuperseded
		if propagatesReqAttrs && req.FileAttributes&types.FileAttributeReadonly != 0 {
			logger.Debug("CREATE: delete-on-close with read-only attribute in request",
				"path", filename,
				"createAction", createAction)
			return 0, false, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusCannotDelete}}
		}
	}

	return grantedAccess, grantedComputed, nil
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

	// Compute the effective access mask once for both the share-mode conflict
	// check below and the DACL access-rights check downstream. Mirrors Samba's
	// `open_access_mask` (source3/smbd/open.c::open_file_ntcreate) which folds
	// FILE_WRITE_DATA into the mask for O_TRUNC dispositions and then uses the
	// augmented mask for BOTH checks. See effectiveAccessForOpen for spec refs.
	effectiveAccess := effectiveAccessForOpen(req.DesiredAccess, req.CreateDisposition)

	// Share-mode recheck + DACL access gate + delete-on-close (read-only)
	// checks, evaluated against the existing-file draft view after any lease
	// breaks have drained. Lifted into a helper so the race-recovery branch
	// (ErrAlreadyExists resync below) can replay the SAME gates against the
	// winner's view before the overwrite/open path proceeds (#765).
	metaSvc := h.Registry.GetMetadataService()
	grantedAccess, grantedComputed, failResp := h.recheckExistingFileGates(d, effectiveAccess)
	if failResp != nil {
		return failResp
	}

	// Step 6e: Break parent directory leases on create/overwrite/supersede
	// BEFORE the actual file mutation so the metadata-layer notifyDirChange
	// (fire-and-forget) finds the dir lease already broken and does not
	// mark the recently-broken cache — which would block the test's lease
	// rearm (test_rearm_dirlease). Mirrors Samba's delay_for_oplock_fn
	// running before the actual create.
	//
	// Parent-key suppression (MS-SMB2 §3.3.4.20 / Samba dirlease_should_break,
	// #470 C2): if this CREATE carried an RqLs with
	// LEASE_FLAG_PARENT_LEASE_KEY_SET, extract the ParentLeaseKey from the
	// incoming request so the matching dir-lease is NOT broken.
	//
	// Single break-to-None per Samba do_dirlease_break_to_none
	// (source3/smbd/smb2_oplock.c): a directory-content change emits ONE
	// LEASE_BREAK to None per dir-lease holder, not the two-step strip-H /
	// strip-R pattern used for file leases. Applies uniformly to CREATE,
	// OVERWRITE, and SUPERSEDE — WPTS BVT_DirectoryLeasing_ReadWriteHandleCaching
	// (#454) asserts the break notification carries NewLeaseState=None.
	if (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) && h.LeaseManager != nil {
		parentLockHandle := lock.FileHandle(parentHandle)
		// Per Samba dirlease_should_break: no ClientID exclusion for parent
		// dir lease breaks. Same-client CREATEs break the parent dir lease
		// unless the ParentLeaseKey matches (smb2.dirlease.v2_request,
		// smb2.dirlease.leases). Only ParentLeaseKey suppression applies.
		var excludeParentKey [16]byte
		var hasExcludeKey bool
		if leaseCtx := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); leaseCtx != nil {
			if leaseReq, decodeErr := DecodeLeaseCreateContext(leaseCtx.Data); decodeErr == nil &&
				leaseReq.Flags&smbenc.LeaseResponseFlagParentKeySet != 0 {
				excludeParentKey = leaseReq.ParentLeaseKey
				hasExcludeKey = true
			}
		}
		if breakErr := h.LeaseManager.BreakParentDirLeasesOnDestructiveCreate(authCtx.Context, parentLockHandle, tree.ShareName, "", excludeParentKey, hasExcludeKey); breakErr != nil {
			logger.Debug("CREATE: parent directory lease break failed", "error", breakErr)
		}
	}

	// Step 7: Perform create/open.
	var file *metadata.File
	var fileHandle metadata.FileHandle
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
			// Concurrent-create race: another opener won between our
			// pre-create lookup and the metadata-store transaction. For
			// dispositions that accept an existing target (OPEN_IF,
			// OVERWRITE_IF, SUPERSEDE), MS-FSA §2.1.5.1.1 requires falling
			// back to the existing-file branch — not surfacing the race as
			// OBJECT_NAME_COLLISION. Required by smbtorture
			// smb2.create.mkdir-dup (two parallel OPEN_IF on the same
			// directory name MUST yield 1 CREATED + 1 EXISTED). Strict-CREATE
			// (FILE_CREATE / FILE_OVERWRITE) still surfaces the collision per
			// MS-FSA, matching Samba's open_file_ntcreate fallthrough.
			var storeErr *metadata.StoreError
			if errors.As(err, &storeErr) && storeErr.Code == metadata.ErrAlreadyExists {
				switch req.CreateDisposition {
				case types.FileOpenIf, types.FileOverwriteIf, types.FileSupersede:
					winner, _, lookupErr := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseName)
					if lookupErr == nil && winner != nil {
						// Resync draft state to the winner so downstream
						// share-mode + DACL gates and the lease/open
						// bookkeeping run against the real file. The
						// original pre-break share-mode recheck ran on
						// our stale (nil) view; rerun it now. The local
						// existingFile snapshot is not touched: from here the
						// race branch drives file/fileHandle directly and the
						// replayed gates read the winner from d.existingFile.
						fileExists = true
						d.existingFile = winner
						d.fileExists = true
						if enc, encErr := metadata.EncodeFileHandle(winner); encErr == nil {
							d.existingHandle = enc
						}
						// Re-resolve createAction: with the winner in
						// place, OPEN_IF → Opened; OVERWRITE_IF /
						// SUPERSEDE → Overwritten / Superseded.
						newAction, dispErr := ResolveCreateDisposition(req.CreateDisposition, true)
						if dispErr != nil {
							return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(dispErr)}}
						}
						createAction = newAction
						d.createAction = newAction
						// Replay the share-mode + DACL + delete-on-close
						// gates against the winner BEFORE the overwrite/open
						// proceeds. The first invocation (top of
						// completeCreateAfterBreak) ran against the stale nil
						// draft, so a winner with an incompatible share-mode
						// or denying DACL — surfaced only under real path
						// contention on OVERWRITE_IF / SUPERSEDE — would
						// otherwise slip past every access gate (#765). On the
						// OPEN_IF dir-mkdir-dup path the winner is a fresh
						// share-compatible directory, so this is a no-op there.
						// effectiveAccess is immutable across the resync
						// (DesiredAccess / CreateDisposition don't change), so
						// the value computed at the top still applies.
						rg, rc, raceFail := h.recheckExistingFileGates(d, effectiveAccess)
						if raceFail != nil {
							return raceFail
						}
						grantedAccess = rg
						grantedComputed = rc
						if createAction == types.FileOpened {
							file = winner
							fileHandle = d.existingHandle
						} else {
							var owErr error
							file, fileHandle, owErr = h.overwriteFile(authCtx, winner, req)
							if owErr != nil {
								logger.Warn("CREATE: overwrite-after-create-race failed", "name", baseName, "error", owErr)
								return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(owErr)}}
							}
						}
						break // exits inner CreateDisposition switch (race handled)
					}
				}
			}
			if file == nil {
				logger.Warn("CREATE: failed to create file", "name", baseName, "error", err)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}
			}
		}
	case types.FileOverwritten, types.FileSuperseded:
		var err error
		file, fileHandle, err = h.overwriteFile(authCtx, existingFile, req)
		if err != nil {
			logger.Warn("CREATE: failed to overwrite file", "name", baseName, "error", err)
			return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: common.MapToSMB(err)}}
		}
	}

	// Apply security descriptor from SMB2_CREATE_SD_BUFFER create context.
	// MS-SMB2 §2.2.13.2.2: client supplies an initial SD at CREATE time.
	if createAction == types.FileCreated {
		if sdCtx := FindCreateContext(req.CreateContexts, SDBufferCreateContextTag); sdCtx != nil && len(sdCtx.Data) > 0 {
			opts := h.parseSDOptsForShare(d.tree.ShareName)
			ownerUID, ownerGID, fileACL, parseErr := ParseSecurityDescriptorWithOptions(sdCtx.Data, opts)
			if parseErr != nil {
				logger.Debug("CREATE: failed to parse SD_BUFFER", "path", baseName, "error", parseErr)
			} else {
				setAttrs := &metadata.SetAttrs{}
				apply := false
				if ownerUID != nil {
					setAttrs.UID = ownerUID
					apply = true
				}
				if ownerGID != nil {
					setAttrs.GID = ownerGID
					apply = true
				}
				if fileACL != nil {
					setAttrs.ACL = fileACL
					apply = true
				}
				if apply {
					metaSvc := h.Registry.GetMetadataService()
					if setErr := metaSvc.SetFileAttributes(authCtx, fileHandle, setAttrs); setErr != nil {
						logger.Debug("CREATE: failed to apply SD_BUFFER", "path", baseName, "error", setErr)
					} else {
						if updated, getErr := metaSvc.GetFile(authCtx.Context, fileHandle); getErr == nil {
							file = updated
						}
					}
				}
			}
		}
	}

	// Apply extended attributes from an SMB2_CREATE_EA_BUFFER ("ExtA") create
	// context. MS-SMB2 §2.2.13.2.1: the client may attach a
	// FILE_FULL_EA_INFORMATION chain (MS-FSCC §2.4.15) at CREATE so the file is
	// born with EAs (smbtorture smb2.setinfo opens its test file this way, then
	// asserts the pre-existing EAs survive a later SET_INFO). Only applied on a
	// freshly created file; reopening an existing file does not re-seed EAs.
	if createAction == types.FileCreated {
		if eaCtx := FindCreateContext(req.CreateContexts, CreateContextTagExtendedAttributes); eaCtx != nil && len(eaCtx.Data) > 0 {
			entries, decErr := decodeFullEaEntries(eaCtx.Data)
			if decErr != nil {
				logger.Debug("CREATE: failed to decode EA_BUFFER", "path", baseName, "error", decErr)
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
			}
			// The reserved ACL-xattr slot is server-private and cannot be set
			// through the EA channel (parity with SET_INFO EA).
			reserved := false
			for _, e := range entries {
				if isReservedACLXattrName(e.name) {
					reserved = true
					break
				}
			}
			if reserved {
				return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}
			}
			if muts := eaMutationsFromEntries(entries); len(muts) > 0 {
				metaSvc := h.Registry.GetMetadataService()
				if setErr := metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{EAMutations: muts}); setErr != nil {
					logger.Debug("CREATE: failed to apply EA_BUFFER", "path", baseName, "error", setErr)
				} else if updated, getErr := metaSvc.GetFile(authCtx.Context, fileHandle); getErr == nil {
					file = updated
				}
			}
		}
	}

	// Compute GrantedAccess for new/overwritten files. The fileExists branch
	// above already populated grantedAccess from CheckFileAccess on the
	// existing file's DACL (the open-time gate).
	//
	// For FileCreated the file is brand-new: Windows/Samba grant the creator
	// the resolved DesiredAccess as-is and never re-check it against the
	// inherited DACL. The inherited DACL governs subsequent opens by other
	// principals — it does not narrow the creator's handle. This matches
	// Samba `source3/smbd/open.c::open_file_ntcreate`, which only runs the
	// DACL check on existing-file paths, and MS-FSA §2.1.5.1.2 CreateFile
	// (the DesiredAccess check is gated by the parent DACL, not the new
	// child's inherited DACL). Re-checking would surface the smbtorture
	// failure in smb2.acls.INHERITANCE/INHERITFLAGS/SDFLAGSVSCHOWN, where
	// a parent DACL grants the creator only WRITE_DATA|WRITE_DAC
	// inheritably but the test still expects the new handle to carry
	// SEC_RIGHTS_FILE_ALL (per Samba behavior).
	//
	// For FileOverwritten/Superseded the prior DACL is preserved and the
	// open-time gate at step 6c-bis already validated DesiredAccess against
	// it; grantedAccess was set there. If grantedComputed is false in that
	// branch (defensive — should not happen in practice because
	// fileExists==true for overwrite/supersede) we fall back to the
	// resolved DesiredAccess to avoid under-granting.
	if !grantedComputed {
		grantedAccess = resolveAccessFlags(req.DesiredAccess)
	}

	// Step 7a: Restore frozen timestamps on parent directory (MS-FSA 2.1.5.14.2).
	if createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded {
		h.restoreParentDirFrozenTimestamps(authCtx, parentHandle)
	}

	// Step 7b: Update base object ChangeTime for ADS operations (MS-FSA / NTFS).
	if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 && (createAction == types.FileCreated || createAction == types.FileOverwritten || createAction == types.FileSuperseded) {
		h.updateBaseObjectCtime(authCtx, metaSvc, parentHandle, baseName[:colonIdx])
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

	// Step 8a-bis: MS-SMB2 §3.3.4.18 disconnected-handle preservation/purge.
	//
	// Evaluate any disconnected durable handles on this metadata handle
	// against the new open's lease/share-mode and purge those that the new
	// open's required break_to would knock below H caching. Runs after the
	// live-lease break (8a) so the disconnected-side check sees the same
	// post-break view, and before lease grant (8b) so the granted state
	// reflects only handles that survive the purge. See
	// disconnected_state_machine.go for the predicate.
	if fileExists && h.DurableStore != nil && len(fileHandle) > 0 {
		var newLeaseState uint32
		var newLeaseKey [16]byte
		if req.OplockLevel == OplockLevelLease {
			if lc := FindCreateContext(req.CreateContexts, LeaseContextTagRequest); lc != nil {
				if parsed, decErr := DecodeLeaseCreateContext(lc.Data); decErr == nil && parsed != nil {
					newLeaseState = parsed.LeaseState
					newLeaseKey = parsed.LeaseKey
				}
			}
		}
		// This callsite is the FRESH-CREATE path; durable reconnect early-returns
		// in create.go before reaching here (see create.go::handleCreate, the
		// ProcessDurableReconnectContext branch). The predicate's contract
		// relies on this — see disconnectedConflictOnNewOpen doc.
		if purged := h.purgeConflictingDisconnectedHandlesForOpen(
			authCtx.Context,
			fileHandle,
			newLeaseState,
			newLeaseKey,
			req.ShareAccess,
			req.DesiredAccess,
		); purged > 0 {
			logger.Debug("CREATE: purged disconnected handles on conflicting open",
				"path", filename,
				"count", purged)
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
			// Samba `disallow_write_lease`: strip W from this lease grant when a
			// conflicting other open or a disconnected durable handle exists.
			// The new open's own lease key (parsed from the RqLs blob) and SMB
			// FileID are excluded so a same-handle reopen/upgrade is never
			// self-capped (mirrors Samba `is_same_lease`). bestGrantableState in
			// the lock manager already caps W against live *lease* records; this
			// predicate adds the cases it cannot see — non-lease live opens and
			// disconnected durable handles (smb2.durable-v2-open.nonstat-and-lease
			// and keep-disconnected-rh-with-rwh-open).
			var newLeaseKey [16]byte
			if parsed, decErr := DecodeLeaseCreateContext(leaseCtx.Data); decErr == nil && parsed != nil {
				newLeaseKey = parsed.LeaseKey
			}
			disallowWriteLease := h.disallowWriteLeaseForFile(
				authCtx.Context, fileHandle, newLeaseKey, smbFileID, connClientGUID(ctx),
			)
			// A stat-open-only, non-destructive CREATE must not break an
			// existing holder when it requests its own lease — the same
			// carve-out breakAndMaybeParkCreate applies to the CREATE-layer
			// break. Routing the lease grant through the break-suppressing
			// variant closes the timing window that produced the intermittent
			// break in smb2.lease.statopen4 (#751).
			statOpenLease := isStatOnlyOpen(req.DesiredAccess) &&
				!isDestructiveDisposition(req.CreateDisposition)
			leaseResponse, err = ProcessLeaseCreateContext(
				authCtx.Context,
				h.LeaseManager,
				leaseCtx.Data,
				lockFileHandle,
				ctx.SessionID,
				connClientGUID(ctx),
				fmt.Sprintf("smb:%d", ctx.SessionID),
				tree.ShareName,
				file.Type == metadata.FileTypeDirectory,
				disallowWriteLease,
				statOpenLease,
			)
			if err != nil {
				if errors.Is(err, lock.ErrLeaseKeyInUse) {
					// Step 7 already executed the open. The cleanup here depends
					// on what that open did:
					//   - FileCreated: roll back the orphan inode so a CREATE_NEW
					//     that failed lease_match doesn't leave a metadata entry
					//     no client can reach. Samba's open path closes the fd
					//     for the same reason.
					//   - FileOverwritten / FileSuperseded: the prior file's
					//     content was already truncated. Returning
					//     STATUS_INVALID_PARAMETER would tell the client the
					//     CREATE failed while the data is gone — unrecoverable.
					//     Complete the CREATE with no lease instead; the data
					//     loss is committed, but the client at least gets a
					//     working handle.
					//   - FileOpened: nothing destructive happened, fail safely.
					switch createAction {
					case types.FileCreated:
						if _, delErr := metaSvc.RemoveFile(authCtx, parentHandle, baseName); delErr != nil {
							logger.Warn("CREATE: failed to roll back orphaned file after lease rejection",
								"name", baseName, "error", delErr)
						}
						return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
					case types.FileOverwritten, types.FileSuperseded:
						logger.Warn("CREATE: lease request rejected after destructive open; completing CREATE without lease",
							"name", baseName, "createAction", createAction, "error", err)
						leaseResponse = nil
						grantedOplock = OplockLevelNone
					default:
						return &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}
					}
				} else {
					logger.Debug("CREATE: lease context processing failed", "error", err)
				}
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
		file.Type != metadata.FileTypeDirectory &&
		// Strip a traditional oplock request to NONE when the new opener's
		// EFFECTIVE access mask is oplock-stat-only (Samba
		// `is_oplock_stat_open` — FILE_READ_ATTRIBUTES / FILE_WRITE_ATTRIBUTES
		// / SYNCHRONIZE only, NO READ_CONTROL). Mirrors `open_file_ntcreate`
		// source3/smbd/open.c line 4000:
		//
		//	if (is_oplock_stat_open(open_access_mask) && lease == NULL) {
		//	    oplock_request &= SAMBA_PRIVATE_OPLOCK_MASK;
		//	}
		//
		// Samba's comment on this gate is load-bearing:
		//
		//	stat opens on existing files don't get oplocks. They can get
		//	leases. Note that we check for stat open on the
		//	*open_access_mask*, i.e. the access mask we actually used to do
		//	the open, not the one the client asked for (which is in
		//	fsp->access_mask). This is due to the fact that FILE_OVERWRITE
		//	and FILE_OVERWRITE_IF add in O_TRUNC, which adds FILE_WRITE_DATA
		//	to open_access_mask.
		//
		// So a destructive disposition (OVERWRITE / OVERWRITE_IF /
		// SUPERSEDE) with attrs-only DesiredAccess still gets the requested
		// oplock — the implicit truncate-WRITE_DATA disqualifies it from
		// the stat-open carve-out. smbtorture smb2.oplock.exclusive5 covers
		// this: OVERWRITE_IF + attrs-only request must grant LEVEL_II
		// (NOT NONE). batch8 / exclusive4 (non-destructive + attrs-only)
		// must strip to NONE.
		(!fileExists || !isOplockStatOpen(effectiveAccessForOpen(req.DesiredAccess, req.CreateDisposition))) {

		var requestedState uint32
		switch req.OplockLevel {
		case OplockLevelBatch:
			requestedState = lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
		case OplockLevelExclusive:
			requestedState = lock.LeaseStateRead | lock.LeaseStateWrite
		case OplockLevelII:
			requestedState = lock.LeaseStateRead
		}
		// Coerce Batch/Exclusive → LEVEL_II when another "effective"
		// holder is present on the same file, mirroring Samba's
		// `disallow_write_lease` predicate (source3/smbd/open.c:2397).
		// The lock layer's bestGrantableState already handles records
		// with active R/W/H bits; this branch covers the case where the
		// record is absent or at LeaseStateNone but the underlying open
		// is still alive — those would otherwise slip past
		// bestGrantableState and yield a too-permissive grant.
		//
		// An "effective" holder is either:
		//   1) A non-stat-only OpenFile on the same metadata handle —
		//      batch10 (tree1 raw-open → tree2 BATCH coerced to LEVEL_II).
		//   2) A live lease/oplock record (not a BrokenViaTimeout tombstone)
		//      regardless of whether the holder's OpenFile is stat-only —
		//      batch9a / batch13 / batch14 / batch16 where tree1's attrs-only
		//      open holds a trad-oplock record that has been acked through a
		//      break.  Samba's `disallow_write_lease` gates on `op_type !=
		//      NO_OPLOCK` independent of the access mask, so an attrs-only
		//      oplock holder still constrains the next opener.
		// Timeout tombstones (BrokenViaTimeout=true) are excluded from BOTH
		// paths so smb2.oplock.batch22b can grant a fresh BATCH after the
		// abandoned holder times out (tree1's OpenFile is still alive but
		// its record is a tombstone).
		//
		// Same-client carve-out (smbtorture smb2.oplock.batch22a, MS-SMB2
		// §3.3.4.6): even when only timeout tombstones remain, if another
		// non-stat OpenFile on the same ClientGUID is still alive, the new
		// grant for THIS client must collapse to LEVEL_II. The abandoned
		// holder may still hold dirty cached state on the client side, so
		// exclusive batch caching cannot be regranted to the same client.
		// Cross-client re-opens (batch22b) bypass this since the new client
		// has no caching relationship with the abandoned holder.
		lockFileHandle := lock.FileHandle(fileHandle)
		onlyTimeoutTombstone := h.LeaseManager.OnlyTimeoutTombstoneRecords(lockFileHandle, tree.ShareName)
		hasActiveRecord := h.LeaseManager.HasActiveLeaseRecord(lockFileHandle, tree.ShareName, [16]byte{})
		hasNonStatOpen := h.hasOtherNonStatOpenForFile(fileHandle, smbFileID)
		// Only consult the same-client carve-out once NEGOTIATE has
		// established a connection identity. Without CryptoState the
		// requestor's identity is unknown — falling through would zero-
		// vs-zero match unrelated pre-NEGOTIATE opens (regressed
		// smb2.compound.interim2 when the early gate was removed).
		hasSameClientOpen := false
		if ctx != nil && ctx.ConnCryptoState != nil {
			hasSameClientOpen = h.hasSameClientNonStatOpenForFile(fileHandle, smbFileID, connClientGUID(ctx))
		}
		if (!onlyTimeoutTombstone && (hasActiveRecord || hasNonStatOpen)) ||
			(onlyTimeoutTombstone && hasSameClientOpen) {
			requestedState &^= (lock.LeaseStateWrite | lock.LeaseStateHandle)
		}
		if requestedState != 0 {
			syntheticKey := generateSyntheticLeaseKey(smbFileID)
			ownerID := fmt.Sprintf("smb:oplock:%x", smbFileID)
			clientID := fmt.Sprintf("smb:%d", ctx.SessionID)
			// RequestLeaseAsOplock tags the new record IsTraditionalOplock=true
			// so MS-SMB2 §3.3.5.9 cross-tier rules apply on subsequent grants
			// (Samba `state.got_handle_lease` / `state.got_oplock`). See
			// `pkg/metadata/lock/leases.go::bestGrantableState`.
			grantedState, _, err := h.LeaseManager.RequestLeaseAsOplock(
				authCtx.Context,
				lockFileHandle,
				syntheticKey,
				[16]byte{},
				ctx.SessionID,
				connClientGUID(ctx),
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

	// Step 8b-bis: Directory-CREATE oplock gate (MS-SMB2 §3.3.5.9; Samba
	// smbd_smb2_create_oplock_check). See clampDirectoryOplockLevel for the
	// rationale; covers smbtorture smb2.dirlease.oplocks.
	if file.Type == metadata.FileTypeDirectory {
		clamped, cleared := clampDirectoryOplockLevel(grantedOplock)
		if clamped != grantedOplock {
			logger.Debug("CREATE: clamping non-lease oplock to NONE on directory CREATE",
				"requestedOplock", oplockLevelName(req.OplockLevel),
				"grantedOplock", oplockLevelName(grantedOplock))
			grantedOplock = clamped
			if cleared {
				syntheticLeaseKey = [16]byte{}
			}
		}
	}

	// Step 8c: Process App Instance ID and durable handle grant.
	//
	// The AppInstanceId force-close normally runs in the pre-break CREATE path
	// (Create, before breakAndMaybeParkCreate) so a conflicting open carrying
	// the same AppInstanceId is displaced BEFORE any oplock/lease break is
	// computed — MS-SMB2 §3.3.5.9.13 requires the failover to be silent (no
	// break on the displaced open; smbtorture smb2.durable-v2-open.app-instance
	// asserts break_info.count == 0). When that ran, reuse its result. Only
	// fall back to processing here for callers that bypass the pre-break path.
	var durableResponseCtx *CreateContext
	var appInstanceId [16]byte
	if d.appInstanceProcessed {
		appInstanceId = d.appInstanceId
	} else if h.DurableStore != nil {
		appInstanceId = ProcessAppInstanceId(
			authCtx.Context, h.DurableStore, h, req.CreateContexts,
		)
	}

	openFile := &OpenFile{
		FileID:               smbFileID,
		TreeID:               ctx.TreeID,
		SessionID:            ctx.SessionID,
		Path:                 filename,
		ShareName:            tree.ShareName,
		OpenTime:             time.Now(),
		DesiredAccess:        req.DesiredAccess,
		GrantedAccess:        grantedAccess,
		IsDirectory:          file.Type == metadata.FileTypeDirectory,
		MetadataHandle:       fileHandle,
		PayloadID:            file.PayloadID,
		ParentHandle:         parentHandle,
		FileName:             baseName,
		OplockLevel:          grantedOplock,
		ShareAccess:          req.ShareAccess,
		CreateOptions:        req.CreateOptions,
		InitialDeleteOnClose: req.CreateOptions&types.FileDeleteOnClose != 0,
		ClientGUID:           connClientGUID(ctx),
		// Honour the client-requested AllocationSize [MS-SMB2] 2.2.13.2.2 for
		// regular files only. Directories never report the requested reservation
		// (smb2.create.dir-alloc-size), so leave it zero for them. Tracked
		// per-handle so the CREATE response and a later QUERY_INFO on this handle
		// agree (smb2.create.open).
		RequestedAllocSize: allocReservationFor(file.Type == metadata.FileTypeDirectory, req.RequestedAllocSize),
		// Record the requested DH2Q CreateGuid for replay-cache keying,
		// independent of whether V2 durability is granted below. A no-oplock
		// open never sets CreateGuid (durability requires Batch/Handle lease),
		// but its CREATE must still be replay-cacheable (MS-SMB2 §3.3.5.9).
		ReplayCreateGuid: dh2qCreateGuid(req),
	}

	if leaseResponse != nil && leaseResponse.LeaseState != lock.LeaseStateNone {
		openFile.LeaseKey = leaseResponse.LeaseKey
	} else if syntheticLeaseKey != ([16]byte{}) {
		openFile.LeaseKey = syntheticLeaseKey
	}

	// Snapshot the opener's identity so handle-bound ops (notably SET_INFO
	// SecurityDescriptor) stay anchored to the user who actually opened
	// the handle after SESSION_SETUP re-auth swaps Session.User out from
	// under us. MS-SMB2 §3.3.5.5.3 + smbtorture smb2.session.reauth4/5.
	h.CaptureOpenerIdentity(ctx, openFile)

	// Record the RqLs parent-lease-key linkage so downstream operations on
	// this handle (SET_INFO, WRITE, CLOSE-on-delete) can apply the MS-SMB2
	// §3.3.4.20 / Samba `dirlease_should_break` parent-key suppression rule
	// against the parent directory's lease. Captured even when the file
	// lease itself was denied (response leaseState=None) — the linkage
	// applies regardless of the per-file grant outcome. Gated on HasParent
	// so a zero-key request without LEASE_FLAG_PARENT_LEASE_KEY_SET is
	// treated as "no linkage" (#470 C2).
	if leaseResponse != nil && leaseResponse.HasParent {
		openFile.ParentLeaseKey = leaseResponse.ParentLeaseKey
		openFile.HasParentLeaseKey = true
	}

	// Record the DOC setter's parent key for initial delete-on-close
	// (CREATE with FILE_DELETE_ON_CLOSE). Covers dirlease unlink tests.
	if openFile.InitialDeleteOnClose {
		openFile.DeleteOnCloseParentKey = openFile.ParentLeaseKey
		openFile.HasDeleteOnCloseParentKey = openFile.HasParentLeaseKey
	}

	if h.DurableStore != nil {
		hasHandleLease := leaseResponse != nil && leaseResponse.LeaseState&lock.LeaseStateHandle != 0
		if respCtx := ProcessDurableHandleContext(
			req.CreateContexts, openFile, DurableGrantOptions{
				ConfiguredTimeoutMs:    h.DurableTimeoutMs,
				LeaseIncludesHandle:    hasHandleLease,
				ContinuousAvailability: tree.ContinuousAvailability,
			},
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
			nameFilter := NameChangeFilterFor(baseName, openFile.IsDirectory)
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionAdded, nameFilter)
		case types.FileOverwritten, types.FileSuperseded:
			nameFilter := NameChangeFilterFor(baseName, openFile.IsDirectory)
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionRemoved, nameFilter)
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionAdded, nameFilter)
			h.NotifyRegistry.NotifyChange(tree.ShareName, parentPath, baseName, FileActionModified, FileNotifyChangeAttributes|FileNotifyChangeLastWrite|FileNotifyChangeSize)
		}
	}

	// Step 10: Build success response.
	//
	// Per NTFS: alternate data streams share the base file's timestamps
	// and attributes. When the open is for an ADS, resolve the base file
	// and use its metadata for the CREATE response times and attributes.
	// The stream's own size is used for EndOfFile / AllocationSize.
	respAttr := &file.FileAttr
	if colonIdx := strings.Index(baseName, ":"); colonIdx > 0 && len(parentHandle) > 0 {
		baseFileName := baseName[:colonIdx]
		if baseFile, _, _ := h.lookupCaseInsensitive(authCtx, metaSvc, parentHandle, baseFileName); baseFile != nil {
			respAttr = &baseFile.FileAttr
		}
	}
	creation, access, write, change := FileAttrToSMBTimes(respAttr)
	size := getSMBSize(&file.FileAttr)
	// Report the larger of the file's cluster-aligned size and the per-handle
	// requested reservation [MS-SMB2] 2.2.13.2.2 (zero for directories), so a
	// freshly-created empty file opened with in.alloc_size reports a non-zero
	// out.alloc_size (smb2.durable-open.alloc-size).
	allocationSize := effectiveAllocationSize(size, openFile.RequestedAllocSize)

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
		FileAttributes:  FileAttrToSMBAttributes(respAttr),
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
		qfidFileID := h.baseFileUUID(authCtx, parentHandle, baseName, file.ID)
		qfidResp := make([]byte, 32)
		copy(qfidResp[0:16], qfidFileID[:16])
		copy(qfidResp[16:32], h.ServerGUID[:])
		resp.CreateContexts = append(resp.CreateContexts, CreateContext{
			Name: "QFid",
			Data: qfidResp,
		})
		logger.Debug("CREATE: QFid response added",
			"diskFileId", fmt.Sprintf("%x", qfidFileID[:16]))
	}

	// SMB3 replay protection (MS-SMB2 §3.3.5.9): record the freshly-built
	// success response keyed by the REQUESTED DH2Q CreateGuid so a
	// FLAGS_REPLAY_OPERATION retry within the replay window returns the
	// cached result (same FileId) instead of re-running CREATE. Keyed on
	// ReplayCreateGuid, not the durability-granting CreateGuid: a no-oplock
	// open never gets V2 durability (Batch/Handle lease required) yet its
	// CREATE must still be replay-cacheable, and the replay must echo the
	// same handle (smb2.replay.dhv2-pending1n-vs-{oplock,lease}-sane io24).
	if h.CreateReplayCache != nil && openFile != nil && openFile.ReplayCreateGuid != ([16]byte{}) {
		h.CreateReplayCache.Store(openFile.SessionID, openFile.ReplayCreateGuid, resp, openFile)
	}

	return resp
}

// parkCreateOnLeaseBreak reserves an async slot, registers a pending CREATE,
// and spawns a resume goroutine that waits for the break to drain and then
// completes the CREATE via AsyncCreateCompleteCallback. Returns the generated
// AsyncId on success, or 0 when async parking is not possible (e.g. no async
// slots left; registry rejected the entry). Callers fall back to sync wait.
//
// breakWaitTimeout bounds the server-side wait for the break to drain (or
// auto-downgrade on expiry). Callers pass TraditionalOplockBreakWaitTimeout
// (~35 s, MS-SMB2 §3.3.4.6) when the holder is a traditional oplock and
// AsyncCreateBreakWaitTimeout (~5 s) otherwise.
func (h *Handler) parkCreateOnLeaseBreak(
	ctx *SMBHandlerContext,
	d *createDraft,
	lockFileHandle lock.FileHandle,
	waitExceptKey [16]byte,
	breakWaitTimeout time.Duration,
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
	// session teardown, bounded by breakWaitTimeout so a missing ACK
	// auto-downgrades other-key leases and lets the CREATE proceed.
	waitCtx, cancel := context.WithTimeout(context.Background(), breakWaitTimeout)

	pending := &PendingCreate{
		ConnID:    ctx.ConnID,
		SessionID: ctx.SessionID,
		MessageID: ctx.MessageID,
		AsyncId:   asyncId,
		Cancel:    cancel,
		Callback:  ctx.AsyncCreateCompleteCallback,
		// started is closed by the dispatcher (response.go single-cmd path
		// or compound.go after ReplaceCallback) once the Callback has been
		// finalized. The resume goroutine waits on it before invoking
		// Callback so a fast localhost break ACK can't fire the original
		// callback before the compound dispatcher has had a chance to swap
		// it for the continue-compound wrapper (smb2.compound.compound-break
		// IO_TIMEOUT race observed in CI).
		started: make(chan struct{}),
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

	// The DH2Q CreateGuid was already Reserved by the caller (Create, before the
	// breakAndMaybeParkCreate dispatch) so a replayed CREATE fails fast with
	// STATUS_FILE_NOT_AVAILABLE instead of blocking on the same break (smbtorture
	// replay-dhv2-pending* / *-vs-{oplock,lease}). Since this CREATE is parking
	// async, ownership of the matching Release transfers to the resume goroutine
	// below: the caller returns STATUS_PENDING immediately and does NOT release.
	// This keeps exactly one Reserve (in Create) and one Release (here for the
	// parked path) per CREATE attempt. A zero CreateGuid is a no-op in Release.
	replayGuid := dh2qCreateGuid(d.req)

	go func() {
		defer cancel()
		defer func() {
			if h.CreateReplayCache != nil {
				h.CreateReplayCache.Release(ctx.SessionID, replayGuid)
			}
		}()

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

		// Wait for the dispatcher to finalize the callback assignment before
		// any release path. Done BEFORE Unregister so a CANCEL/teardown that
		// pulls the entry first can still hand off via markStarted — otherwise
		// the gate would never close and this goroutine would block forever.
		// See PendingCreate.started doc for the race this closes.
		<-pending.started

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

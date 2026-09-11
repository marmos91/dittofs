package handlers

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

type createDraft struct {
	req          *CreateRequest
	tree         *TreeConnection
	authCtx      *metadata.AuthContext
	filename     string
	baseName     string
	parentHandle metadata.FileHandle
	// parentFile is the parent directory inode loaded once in the pre-break
	// CREATE path (alongside the parent-create permission gate). createNewFile
	// reuses it for compression-attribute inheritance instead of re-reading the
	// same handle. Nil for pure-open dispositions, which never reach createNewFile.
	parentFile     *metadata.File
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
	// adsBaseFileName is the name of the base file that was implicitly
	// created by the ADS auto-create path (create.go Step 6) — e.g.
	// "file.txt" when baseName is "file.txt:StreamName".
	// Empty for non-ADS opens.
	adsBaseFileName string
	// adsBaseCreatedByUs is true when THIS CREATE request called
	// CreateFile for adsBaseFileName and succeeded (i.e. the base was
	// newly created by us, not pre-existing and not raced-in by a peer).
	// Used by the ErrLeaseKeyInUse rollback to decide whether to also
	// remove the base file.
	adsBaseCreatedByUs bool
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
// Per MS-FSA §2.1.5.1.2 ("Open of an Existing File"), OVERWRITE / OVERWRITE_IF / SUPERSEDE on an existing
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

// scanNonStatOpensForFile walks h.files once and reports two predicates used by
// the post-break traditional-oplock grant path:
//
//   - hasNonStat: any non-stat-only OpenFile on the same metadata handle
//     (excluding the open identified by selfFileID) exists. Stat-only opens
//     are excluded because Samba's `disallow_write_lease` predicate
//     (source3/smbd/open.c lines 2397-2403) ignores them — they do not
//     invalidate the exclusive caching premise of a Batch/Exclusive grant on a
//     subsequent opener. Used to coerce Batch/Exclusive to LEVEL_II when a
//     previously-existing raw (non-oplocked) open is present. Callers must
//     additionally verify that no lease/oplock record exists for the file via
//     LeaseManager.HasAnyLeaseRecord — when a record exists (even at
//     LeaseStateNone post-break-timeout) bestGrantableState already handles the
//     grant correctly and the coarse OpenFile coercion would incorrectly demote
//     a grant the lease layer would otherwise allow (smbtorture batch22b
//     post-timeout re-grant).
//   - hasSameClient: any such open is also owned by clientGUID (the
//     ClientGUID-scoped variant). Only computed when checkSameClient is true —
//     callers that don't yet have a connection identity skip it.
//
// The same-client carve-out distinguishes the two arms of smbtorture
// smb2.oplock.batch22{a,b}:
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
// remain; hasSameClient carves out the same-client exception: even after
// timeout, a same-ClientGUID non-stat open constrains the new grant. Mirrors
// Samba's `disallow_write_lease` predicate (source3/smbd/open.c lines
// 2397-2403), which gates on whether the existing entry's connection matches
// the requestor's client; we approximate via ClientGUID equality.
//
// Folding both predicates into one Range pass avoids a redundant O(F) scan per
// CREATE on the traditional-oplock grant path.

func (h *Handler) scanNonStatOpensForFile(
	fileHandle metadata.FileHandle,
	selfFileID [16]byte,
	clientGUID [16]byte,
	checkSameClient bool,
) (hasNonStat, hasSameClient bool) {
	if len(fileHandle) == 0 {
		return false, false
	}
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
		hasNonStat = true
		if checkSameClient && other.ClientGUID == clientGUID {
			hasSameClient = true
		}
		// Stop early once both flags are settled (hasSameClient implies
		// hasNonStat; when same-client is not requested, the first non-stat
		// open is conclusive).
		if !checkSameClient || hasSameClient {
			return false
		}
		return true
	})
	return hasNonStat, hasSameClient
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
	//
	// Compute the share-mode conflict once for the break-reason decision and
	// reuse it as the FIRST poll of the share-conflict-wait closure below.
	// Whenever we reach the conflict scan the disposition is non-destructive
	// (destructive is handled by the prior switch case), so
	// effectiveAccessForOpen(req) == req.DesiredAccess here and the wait
	// closure's effectiveAccess matches this scan's input — the cached result
	// is exact. The scan only runs when delete-on-close is unset (otherwise the
	// reason is already SharingViolation and no scan is needed).
	//
	// initialConflict is consumed at most once by the wait closure; every
	// subsequent poll must re-scan because a holder CLOSE/ACK can clear the
	// conflict. conflictComputed records whether the scan actually ran.
	var waitExceptKey [16]byte
	if d.excludeOwner != nil {
		waitExceptKey = d.excludeOwner.ExcludeLeaseKey
	}

	var initialConflict, conflictComputed bool
	reason := lock.BreakReasonDefault
	switch {
	case isDestructiveDisposition(d.req.CreateDisposition):
		reason = lock.BreakReasonDestructive
	case d.req.CreateOptions&types.FileDeleteOnClose != 0:
		reason = lock.BreakReasonSharingViolation
	default:
		initialConflict = h.checkShareModeConflict(d.existingHandle, d.req.DesiredAccess, d.req.ShareAccess, d.parentHandle, d.baseName)
		conflictComputed = true
		if initialConflict {
			reason = lock.BreakReasonSharingViolation
		} else if newOpenIsShareRestrictive(d.req.DesiredAccess, d.req.ShareAccess) &&
			h.LeaseManager.AnyHolderHasLeaseBits(lockFileHandle, shareName, waitExceptKey, lock.LeaseStateHandle) {
			// Decorrelation race (#1331): checkShareModeConflict scans only live
			// h.files opens, but a Handle-caching lease record can outlive its
			// OpenFile by a few microseconds during the holder's close /
			// durable-reconnect teardown. When the live-open scan misses that
			// holder, a share-restrictive new open (one whose own ShareAccess
			// would deny a co-located data holder — e.g. FILE_SHARE_NONE) is
			// still a sharing-violation Handle-strip break, NOT a Default
			// Write-flush break. Classifying it as Default picks the wrong break
			// target (RWH→RH instead of RWH→RW) AND routes through the 5 s
			// force-complete path, which tombstones the holder's lease; the
			// holder's subsequent (late) LEASE_BREAK_ACK then fails
			// STATUS_UNSUCCESSFUL (the #1322 replay flake).
			//
			// Routing it as a sharing violation uses the deferred-open park
			// (WaitForShareConflictClear), which never force-completes, so a late
			// ACK still succeeds (the same mechanism #749 introduced for the
			// live-holder share-violation park).
			//
			// Why this does not regress the force-complete-reliant break tests
			// (breaking3 / batch22 / timeout-disconnect): this branch is only
			// reachable when checkShareModeConflict already returned false. Any
			// holder with a LIVE OpenFile and a conflicting share mode — lease OR
			// traditional oplock — is caught by that scan first and takes the
			// initialConflict path above, so it never reaches here. (Note
			// AnyHolderHasLeaseBits also matches traditional oplocks, which are
			// stored with their RWH caching bits set; the gate is not lease-only.)
			// The only holder that slips past the live-open scan is one whose
			// OpenFile is already gone — the decorrelation window — and there the
			// post-break recheckExistingFileGates re-evaluates the true share-mode
			// outcome, with WaitForShareConflictClear's conflictPresent() seeing
			// no live conflict and exiting immediately. The share-restrictiveness
			// gate additionally keeps a share-permissive caching break (the
			// genuine Default/Write-flush case) on the Default path.
			reason = lock.BreakReasonSharingViolation
			logger.Debug("CREATE: reclassified Default→SharingViolation (decorrelated handle-caching holder, #1331)",
				"file", d.filename,
				"desiredAccess", fmt.Sprintf("0x%x", d.req.DesiredAccess),
				"shareAccess", fmt.Sprintf("0x%x", d.req.ShareAccess))
		}
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
		if asyncId := h.parkCreateOnLeaseBreak(ctx, d, lockFileHandle, waitExceptKey, lease.AsyncCreateBreakWaitTimeout, false); asyncId != 0 {
			return asyncId
		}
		// Park failed (no slots / registry full): fall through to sync
		// wait, then let completeCreateAfterBreak re-evaluate share mode.
		// The wait ends on an ACK or CLOSE the client has not sent yet, so
		// step out of the response order before blocking on it.
		releaseResponseOrder(ctx)
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

	// Deferred-open resume for the share-violation case: the parked CREATE
	// waits for the live share-mode conflict to clear (holder CLOSE) rather
	// than force-completing the holder's lease on timeout. The holder may
	// release at any time within the deferred-open window, so the ceiling is
	// the longer ~35 s grace (Samba defer_open retry window); ack-sane exits
	// early when the holder's break drains without the conflict clearing, so
	// it does not actually wait the full ceiling. The non-violation paths
	// (default / destructive break-to-Write, smbtorture breaking3 /
	// timeout-disconnect / batch22) keep the existing force-complete wait.
	shareConflictWait := reason == lock.BreakReasonSharingViolation
	if shareConflictWait {
		breakWaitTimeout = lease.TraditionalOplockBreakWaitTimeout
	}

	// Per MS-SMB2 §3.3.4.2 ("Sending an Interim Response for an Asynchronous Operation")
	// and smbtorture compound_async.getinfo_middle:
	// When a compound CREATE needs to wait for a lease break, it MUST go async
	// (STATUS_PENDING) even if it is not the last command in the compound.
	// The compound processor handles non-last STATUS_PENDING by sending the
	// interim response standalone and deferring remaining compound commands
	// until the CREATE completes. Without this, the sync wait path deadlocks:
	// the test (and real clients) cannot ACK the lease break until they
	// receive the STATUS_PENDING interim response.
	if h.LeaseManager.HasOtherBreakingLeases(lockFileHandle, shareName, waitExceptKey) {
		if asyncId := h.parkCreateOnLeaseBreak(ctx, d, lockFileHandle, waitExceptKey, breakWaitTimeout, shareConflictWait); asyncId != 0 {
			return asyncId
		}
	}

	// Sync fallback (async park unavailable — no slots / registry full): bounded
	// wait, then let the caller's post-break recheck run. The share-violation
	// case uses the same deferred-open wait-for-conflict-clear as the async
	// resume so a CLOSE wakes it promptly and the holder's lease is never
	// force-completed (otherwise its ACK would fail STATUS_UNSUCCESSFUL). The
	// non-violation case keeps WaitForOtherKeyBreaks, whose timeout
	// auto-downgrades other-key leases for a deterministic post-break recheck.
	//
	// Both waits below end only on an ACK or a CLOSE the client has not sent
	// yet, so step out of the response order before blocking on them.
	releaseResponseOrder(ctx)
	waitCtx, cancelWait := context.WithTimeout(d.authCtx.Context, breakWaitTimeout)
	defer cancelWait()
	if shareConflictWait {
		effectiveAccess := effectiveAccessForOpen(d.req.DesiredAccess, d.req.CreateDisposition)
		// Reuse the conflict scan already performed for the break-reason
		// decision on the FIRST poll, then re-scan on every subsequent poll
		// (a holder CLOSE/ACK can clear the conflict). The cached result is
		// only valid when the reason was driven by initialConflict
		// (conflictComputed); the delete-on-close path set the reason without
		// scanning, so it falls straight through to a live scan.
		firstPoll := conflictComputed
		conflictPresent := func() bool {
			if firstPoll {
				firstPoll = false
				return initialConflict
			}
			return d.existingHandle != nil &&
				h.checkShareModeConflict(d.existingHandle, effectiveAccess, d.req.ShareAccess, d.parentHandle, d.baseName)
		}
		if err := h.LeaseManager.WaitForShareConflictClear(waitCtx, lockFileHandle, shareName, conflictPresent); err != nil {
			logger.Debug("CREATE: sync share-conflict wait completed", "error", err)
		}
	} else if err := h.LeaseManager.WaitForOtherKeyBreaks(waitCtx, lockFileHandle, shareName, waitExceptKey); err != nil {
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
		if shareConflict := h.checkShareModeConflict(d.existingHandle, effectiveAccess, req.ShareAccess, d.parentHandle, d.baseName); shareConflict {
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
		// FILE_DELETE_CHILD override per MS-FSA §2.1.5.1.2.1 ("Algorithm to Check Access to an Existing File") (Samba
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
			return 0, false, &CreateResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusForErr(err)}}
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
		// READONLY+DOC is forbidden per MS-FSA 2.1.5.1.1 ("Creation of a New File"): the resulting file
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
//
// shareConflictWait selects the deferred-open resume semantics for the
// share-violation case (reason == BreakReasonSharingViolation): instead of
// force-completing the holder's lease on timeout and rechecking once, the
// resume goroutine waits for the live share-mode conflict to clear — the holder
// CLOSEs (conflict gone → CREATE proceeds) or only ACKs the break (open kept →
// SHARING_VIOLATION on the final recheck), and the holder's deferred ACK still
// succeeds because the lease is never tombstoned here (smbtorture replay
// dhv2-pending1n-vs-violation-lease-{close,ack}-sane, MS-SMB2 §3.3.5.9 /
// Samba defer_open→retry_open).
func (h *Handler) parkCreateOnLeaseBreak(
	ctx *SMBHandlerContext,
	d *createDraft,
	lockFileHandle lock.FileHandle,
	waitExceptKey [16]byte,
	breakWaitTimeout time.Duration,
	shareConflictWait bool,
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

	// The DH2Q CreateGuid was already Reserved by the caller (Create, before the
	// breakAndMaybeParkCreate dispatch) so a replayed CREATE fails fast with
	// STATUS_FILE_NOT_AVAILABLE instead of blocking on the same break (smbtorture
	// replay-dhv2-pending* / *-vs-{oplock,lease}). Since this CREATE is parking
	// async, ownership of the matching Release transfers to the entry below: the
	// caller returns STATUS_PENDING immediately and does NOT release. This keeps
	// exactly one Reserve (in Create) and one Release per CREATE attempt. A zero
	// CreateGuid is a no-op in Release.
	replayGuid := dh2qCreateGuid(d.req)

	pending := &PendingCreate{
		ConnID:    ctx.ConnID,
		SessionID: ctx.SessionID,
		MessageID: ctx.MessageID,
		AsyncId:   asyncId,
		Cancel:    cancel,
		Callback:  ctx.AsyncCreateCompleteCallback,
		// releaseReplay is invoked by every path that delivers this CREATE's
		// final response, immediately before the response goes out — the resume
		// goroutine below, and the CANCEL / session-teardown paths that preempt
		// it. See PendingCreate.releaseReplay for why the ordering matters.
		replayReleaser: func() {
			if h.CreateReplayCache != nil {
				h.CreateReplayCache.Release(ctx.SessionID, replayGuid)
			}
		},
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

	go func() {
		defer cancel()
		// Unconditional backstop, so the reservation is cleared on every exit of
		// this goroutine — including the early return below, taken when a CANCEL
		// or session teardown preempted the entry and delivers the response
		// itself. The exits that DO send from here release explicitly first;
		// releaseReplay's once-per-entry cap is what makes that overlap safe.
		defer pending.releaseReplay()

		if shareConflictWait {
			// Deferred-open resume: wait for the live share-mode conflict to
			// clear (holder CLOSE) rather than force-completing the holder's
			// lease. On timeout the conflict is still live (holder only ACKed,
			// keeping its open) and completeCreateAfterBreak's recheck returns
			// SHARING_VIOLATION — but the holder's lease is left intact so its
			// deferred ACK still succeeds. See parkCreateOnLeaseBreak doc.
			effectiveAccess := effectiveAccessForOpen(d.req.DesiredAccess, d.req.CreateDisposition)
			conflictPresent := func() bool {
				return d.existingHandle != nil &&
					h.checkShareModeConflict(d.existingHandle, effectiveAccess, d.req.ShareAccess, d.parentHandle, d.baseName)
			}
			if err := h.LeaseManager.WaitForShareConflictClear(waitCtx, lockFileHandle, shareName, conflictPresent); err != nil {
				logger.Debug("CREATE async: share-conflict wait completed",
					"messageID", messageID,
					"asyncId", asyncId,
					"error", err)
			}
		} else {
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
			pending.releaseReplay()
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

		pending.releaseReplay()

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

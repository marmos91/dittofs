package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// SET_INFO lease breaks: parent-directory lease breaks for content changes
// and renames.
func (h *Handler) breakParentDirLeasesForContentChange(ctx *SMBHandlerContext, authCtx *metadata.AuthContext, openFile *OpenFile) {
	parentHandle := openFile.Name().ParentHandle
	if len(parentHandle) == 0 {
		return
	}
	h.breakParentDirLeasesForContentChangeOn(ctx, authCtx, parentHandle, openFile)
}

// breakParentDirLeasesForContentChangeOn is the multi-parent variant used by
// the rename branch: break parent directory leases to None on an
// arbitrary directory handle (src-parent or dst-parent), honoring the
// originating handle's ParentLeaseKey suppression from C2.
//
// Per Samba `contend_dirleases` / `do_dirlease_break_to_none`
// (source3/smbd/smb2_oplock.c): a directory-content change emits a SINGLE
// LEASE_BREAK to None per holder, not the two-step strip-H / strip-R pattern
// used for file leases. The dispatch is FIRE-AND-FORGET: Samba's
// `send_break_to_none` does not wait for the ACK, and the triggering
// request (rename / hardlink / setinfo / close) returns as soon as the
// notification is queued. Required by smbtorture smb2.dirlease.{rename,
// hardlink, unlink_different_set_and_close, unlink_*_initial_and_close}
// which set lease_skip_ack=true AFTER the triggering request returns and
// then replay the captured ACK manually — waiting inline would let the
// 5 s ack-timeout force-complete the lease, so the manual replay would
// hit STATUS_UNSUCCESSFUL (the lease is no longer in BREAKING state).
//
// Per Samba dirlease_should_break: ClientID is NOT used for suppression —
// a same-client SET_INFO / WRITE / CLOSE / RENAME with a mismatched (or
// absent) ParentLeaseKey MUST still break the parent dir lease held by that
// same client.

func (h *Handler) breakParentDirLeasesForContentChangeOn(ctx *SMBHandlerContext, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, openFile *OpenFile) {
	if h.LeaseManager == nil || len(parentHandle) == 0 {
		return
	}

	parentLockHandle := lock.FileHandle(parentHandle)

	// Apply parent-key suppression only (Samba `dirlease_should_break`): if
	// the originating handle's CREATE carried an RqLs with ParentLeaseKey
	// set, the matching parent dir lease MUST NOT be broken. No ClientID
	// exclusion — same-client breaks fire when the key doesn't match.
	excludeParentKey := openFile.ParentLeaseKey
	hasExcludeKey := openFile.HasParentLeaseKey
	path := openFile.Name().Path
	parentDbg := fmt.Sprintf("%x", parentHandle)
	shareName := openFile.ShareName

	logger.Debug("SET_INFO: parent directory lease break-to-None recorded", "path", path, "parent", parentDbg)
	dispatch := h.LeaseManager.PrepareParentDirLeaseBreakOnContentChange(
		parentLockHandle, shareName, "", excludeParentKey, hasExcludeKey)

	// Defer dispatch until after the triggering request's response is on the
	// wire when an SMB ctx is available. Mirrors Samba `send_break_to_none`
	// (source3/smbd/smb2_oplock.c) which schedules the break via the
	// messaging context for a later tevent cycle — required by smbtorture
	// smb2.dirlease.{rename, hardlink, unlink_different_set_and_close,
	// unlink_different_initial_and_close} which set lease_skip_ack=true
	// AFTER the triggering request returns. With inline dispatch the break
	// arrives before the response, the client's lease handler observes
	// skip_ack=false and auto-acks, and the test's manual replay ACK then
	// fails STATUS_UNSUCCESSFUL because the lease is no longer breaking.
	//
	// authCtx is retained for signature parity with sync-variant call sites;
	// the async dispatch does not consume it.
	_ = authCtx
	if ctx != nil {
		AppendPostSend(ctx, dispatch)
	} else {
		dispatch()
	}
}

// breakDstParentDirHandleLeasesForRename strips the Handle bit only (RH -> R)
// on dst-parent dir leases held by holders that conflict with the rename's
// implicit FILE_ADD_FILE open. Called BEFORE the dst-parent share-mode
// conflict check so the break notification is observed even when the conflict
// surfaces STATUS_SHARING_VIOLATION. Read caching is preserved (RH -> R) — the
// rename hasn't mutated directory contents yet, only the dst-parent's Handle
// caching is invalidated by the pending new name.
//
// Honors ParentLeaseKey suppression from C2 only — no ClientID exclusion,
// same-client dir leases break when the key doesn't match.

func (h *Handler) breakDstParentDirHandleLeasesForRename(authCtx *metadata.AuthContext, dstParent metadata.FileHandle, openFile *OpenFile) {
	if h.LeaseManager == nil || len(dstParent) == 0 {
		return
	}
	parentLockHandle := lock.FileHandle(dstParent)
	excludeParentKey := openFile.ParentLeaseKey
	hasExcludeKey := openFile.HasParentLeaseKey
	if breakErr := h.LeaseManager.BreakParentHandleLeasesOnCreate(authCtx.Context, parentLockHandle, openFile.ShareName, "", excludeParentKey, hasExcludeKey); breakErr != nil {
		logger.Debug("SET_INFO: dst-parent dir Handle lease pre-break failed", "path", openFile.Name().Path, "dstParent", fmt.Sprintf("%x", dstParent), "error", breakErr)
	}
}

// FileLinkInfo represents FILE_LINK_INFORMATION [MS-FSCC] 2.4.28.2 (FileLinkInformation for the SMB2 Protocol).
// The wire format mirrors FILE_RENAME_INFORMATION (same byte layout).

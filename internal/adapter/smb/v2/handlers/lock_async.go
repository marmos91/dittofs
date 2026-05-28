package handlers

import (
	"context"
	goerrors "errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// asyncBlockingLockTimeout bounds how long a parked blocking LOCK retries
// before completing with STATUS_LOCK_NOT_GRANTED. Longer than the inline
// fallback (BlockingLockTimeout = 5s) because async parking frees the
// dispatch goroutine and Samba/Windows let blocking locks wait the full
// 35-second SMB timeout. Picked to match Samba `blocking_lock_timeout`
// (35s) so smbtorture suites that bracket their async-LOCK assertions
// near 30s pass without flake.
const asyncBlockingLockTimeout = 35 * time.Second

// parkLockOnConflict reserves an async slot, registers a pending lock,
// and spawns a resume goroutine that retries the lock acquisition until
// success / timeout / cancellation, then delivers the final response via
// the AsyncLockCompleteCallback.
//
// Returns (asyncId, true) when parked; the caller must emit a STATUS_PENDING
// interim response carrying asyncId. Returns (0, false) when async parking
// is not possible (deadlock detected; async slot exhausted; registry full),
// in which case the caller falls back to the inline-retry path which
// surfaces STATUS_LOCK_NOT_GRANTED on the next deny — the same status Samba
// reports for smb2.lock.open-brlock-deadlock / ctdb-delrec-deadlock.
func (h *Handler) parkLockOnConflict(
	ctx *SMBHandlerContext,
	authCtx *metadata.AuthContext,
	openFile *OpenFile,
	fileLock metadata.FileLock,
	lockElem LockElement,
) (uint64, bool) {
	// Deadlock detection: probe the conflicting holder via TestLock and
	// check the WFG. If granting our wait would close a cycle, refuse to
	// park — the inline-retry fallback then exits with LOCK_NOT_GRANTED.
	owners := h.conflictHolders(openFile.MetadataHandle, fileLock)
	if h.LockWaitGraph != nil && len(owners) > 0 {
		if h.LockWaitGraph.WouldCauseCycle(fileLock.OpenID, owners) {
			logger.Debug("LOCK: deadlock detected, refusing to park",
				"waiter", fileLock.OpenID,
				"owners", owners,
				"messageID", ctx.MessageID)
			return 0, false
		}
	}

	if !ctx.TryReserveAsync() {
		logger.Debug("LOCK: async park rejected — max async credits",
			"sessionID", ctx.SessionID,
			"messageID", ctx.MessageID)
		return 0, false
	}

	asyncId := h.generateAsyncId()

	// Wait context: independent of the request's Context (which would be
	// torn down as soon as we return StatusPending). Cancellable by
	// SMB2_CANCEL, TREE_DISCONNECT, LOGOFF, and connection drop;
	// bounded by asyncBlockingLockTimeout.
	waitCtx, cancel := context.WithTimeout(context.Background(), asyncBlockingLockTimeout)

	pending := &PendingLock{
		ConnID:    ctx.ConnID,
		SessionID: ctx.SessionID,
		TreeID:    ctx.TreeID,
		MessageID: ctx.MessageID,
		AsyncId:   asyncId,
		OwnerID:   fileLock.OpenID,
		Identity:  authCtx.Identity,
		Cancel:    cancel,
		Callback:  ctx.AsyncLockCompleteCallback,
	}

	if err := h.PendingLockRegistry.Register(pending); err != nil {
		cancel()
		ctx.ReleaseAsync()
		logger.Warn("LOCK: async park rejected — registry full",
			"sessionID", ctx.SessionID,
			"messageID", ctx.MessageID,
			"error", err)
		return 0, false
	}

	// Add WFG edges before starting the resume goroutine so concurrent
	// callers see the wait relationship. The resume goroutine prunes them
	// via RemoveWaiter on every exit path.
	if h.LockWaitGraph != nil && len(owners) > 0 {
		h.LockWaitGraph.AddWaiter(fileLock.OpenID, owners)
	}

	go h.resumePendingLock(waitCtx, pending, openFile, fileLock, lockElem)

	logger.Debug("LOCK: parked on conflict — sent interim STATUS_PENDING",
		"sessionID", ctx.SessionID,
		"messageID", ctx.MessageID,
		"asyncId", asyncId)
	return asyncId, true
}

// conflictHolders returns the OwnerIDs of locks currently conflicting with
// fileLock. Used by parkLockOnConflict to feed the Wait-For Graph.
//
// Implementation: walks ListLocks for the handle and filters by
// IsLockConflicting against fileLock. We tolerate a slightly stale view —
// conflicts that resolve between this snapshot and the WFG check just make
// us wait briefly on a stale edge, which is harmless. A stale "no conflict"
// view is impossible because the synchronous LockFile attempt that triggered
// parking already returned ErrLocked.
func (h *Handler) conflictHolders(handle metadata.FileHandle, fileLock metadata.FileLock) []string {
	if h.Registry == nil {
		return nil
	}
	metaSvc := h.Registry.GetMetadataService()
	lm, err := metaSvc.GetLockManagerForHandle(handle)
	if err != nil || lm == nil {
		return nil
	}
	handleKey := string(handle)
	owners := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for _, existing := range lm.ListLocks(handleKey) {
		// Use the package-level constructor so we don't depend on the
		// internal lockOwnerID helper. Two locks conflict iff
		// IsLockConflicting returns true; reuse it.
		if !lock.IsLockConflicting(&existing, &fileLock) {
			continue
		}
		owner := lockOwnerOf(&existing)
		if owner == "" || owner == fileLock.OpenID {
			continue
		}
		if _, dup := seen[owner]; dup {
			continue
		}
		seen[owner] = struct{}{}
		owners = append(owners, owner)
	}
	return owners
}

// lockOwnerOf mirrors `pkg/metadata/lock.lockOwnerID`. The lock package keeps
// that helper unexported; we reproduce the logic here so the SMB handler can
// build WFG edges without expanding the lock package's surface area.
func lockOwnerOf(fl *metadata.FileLock) string {
	if fl.OpenID != "" {
		return fl.OpenID
	}
	return fmt.Sprintf("session:%d", fl.SessionID)
}

// resumePendingLock retries the lock acquisition for a parked blocking LOCK
// until success / timeout / cancellation, then delivers the final response
// via the registered AsyncLockCompleteCallback.
//
// Cancellation paths:
//
//   - SMB2_CANCEL → registry calls Cancel() → waitCtx fires Done → exit
//     with StatusCancelled (the registry already drained our entry, so the
//     Unregister-check below short-circuits).
//   - TREE_DISCONNECT / LOGOFF → registry drains and fires its own callback
//     with StatusRangeNotLocked; same Unregister-check short-circuits.
//   - Timeout → fall through to StatusLockNotGranted (unless it's a
//     repeat-of-same-range deny, in which case StatusFileLockConflict).
func (h *Handler) resumePendingLock(
	waitCtx context.Context,
	pending *PendingLock,
	openFile *OpenFile,
	fileLock metadata.FileLock,
	lockElem LockElement,
) {
	defer pending.Cancel()
	defer func() {
		if h.LockWaitGraph != nil {
			h.LockWaitGraph.RemoveWaiter(pending.OwnerID)
		}
	}()

	metaSvc := h.Registry.GetMetadataService()
	authCtx := &metadata.AuthContext{
		Context:  waitCtx,
		Identity: pending.Identity,
	}

	ticker := time.NewTicker(BlockingLockRetryInterval)
	defer ticker.Stop()

	var finalStatus types.Status
	var finalBody []byte
	for {
		select {
		case <-waitCtx.Done():
			// waitCtx.Done can fire from either:
			//   (a) timeout — DeadlineExceeded; we own delivery and surface
			//       LOCK_NOT_GRANTED / FILE_LOCK_CONFLICT.
			//   (b) external cancel — CANCEL / TDIS / LOGOFF called
			//       pending.Cancel(). The canceller already drained our
			//       registry entry and fired its own callback; the
			//       Unregister below will return nil and we exit cleanly
			//       without a duplicate response.
			//
			// Only compute the conflict status (which mutates lastDeniedLocks)
			// in case (a). On external cancel, the canceller owns delivery and
			// the mutation would taint the next retry from the same OpenID.
			if goerrors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				// Async-parked locks are always cross-handle conflicts (self-
				// conflicts are caught synchronously before parking), so pass
				// empty conflictOwnerID to allow normal escalation logic.
				finalStatus = h.mapLockConflictStatus(pending.OwnerID, lockElem, fileLock.Exclusive, "")
			}
			finalBody = nil
			goto deliver
		case <-ticker.C:
			fileLock.AcquiredAt = time.Now()
			err := metaSvc.LockFile(authCtx, openFile.MetadataHandle, fileLock)
			if err == nil {
				finalStatus = types.StatusSuccess
				finalBody = encodeLockResponseBody()
				h.lastDeniedLocks.Delete(pending.OwnerID)
				// Mirror the sync path in lock.go: parked LOCK requests that
				// finally succeed must also mark the open as having held a
				// byte-range lock, so the disconnect-time durable-persist
				// decision (shouldPersistDurableOnDisconnect, MS-SMB2 §3.3.4.18
				// / smb2.durable-v2-open.lock-noW-lease) sees the lock.
				openFile.HasByteRangeLocks.Store(true)
				goto deliver
			}
			var storeErr *metadata.StoreError
			if !goerrors.As(err, &storeErr) || storeErr.Code != merrs.ErrLocked {
				// Non-conflict error after parking (e.g. file deleted).
				// Surface the mapped status and stop retrying. ErrLocked is
				// excluded above so MapLockToSMB's lock-context routing to
				// LOCK_NOT_GRANTED never fires here.
				finalStatus = common.MapLockToSMB(err)
				finalBody = nil
				goto deliver
			}
			// Still conflicted; loop.
		}
	}

deliver:
	// If a cancellation path already drained our entry, it has fired its
	// own callback. Bail out without sending a duplicate response.
	if h.PendingLockRegistry.Unregister(pending.AsyncId) == nil {
		return
	}
	if pending.Callback == nil {
		logger.Warn("LOCK async: no callback registered, dropping response",
			"messageID", pending.MessageID,
			"asyncId", pending.AsyncId)
		return
	}
	if err := pending.Callback(pending.SessionID, pending.MessageID, pending.AsyncId, finalStatus, finalBody); err != nil {
		logger.Debug("LOCK async: failed to send final response",
			"messageID", pending.MessageID,
			"asyncId", pending.AsyncId,
			"status", finalStatus.String(),
			"error", err)
	}
}

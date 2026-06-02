package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// TreeDisconnect handles SMB2 TREE_DISCONNECT command [MS-SMB2] 2.2.11, 2.2.12.
//
// **Purpose:**
//
// TREE_DISCONNECT terminates a tree connection (share mapping). All open files
// associated with the tree connection are closed and their resources freed.
//
// **Process:**
//
//  1. Verify tree connection exists
//  2. Close all open files for this tree (releases locks, flushes caches)
//  3. Delete the tree connection
//  4. Return success response
//
// **Parameters:**
//   - ctx: SMB handler context with tree information
//   - body: Raw request body (4 bytes minimum)
//
// **Returns:**
//   - *HandlerResult: Success or error result
//   - error: Internal error (rare)
func (h *Handler) TreeDisconnect(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 4 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// ========================================================================
	// Step 1: Verify tree connection exists
	// ========================================================================

	_, ok := h.GetTree(ctx.TreeID)
	if !ok {
		return NewErrorResult(types.StatusNetworkNameDeleted), nil
	}

	// ========================================================================
	// Step 2: Close all open files for this tree
	// ========================================================================
	//
	// CloseAllFilesForTree handles:
	// - Releasing byte-range locks
	// - Flushing cached data
	// - Closing pipe handles
	// - Removing file handles

	filesClosed := h.CloseAllFilesForTree(ctx.Context, ctx.TreeID, ctx.SessionID)
	if filesClosed > 0 {
		logger.Debug("TREE_DISCONNECT: closed files",
			"treeID", ctx.TreeID,
			"sessionID", ctx.SessionID,
			"count", filesClosed)
	}

	// Cancel any blocking-LOCK requests parked on this tree. Per MS-SMB2
	// §3.3.5.14 + smbtorture smb2.lock.cancel-tdis: TREE_DISCONNECT MUST
	// unblock pending LOCKs scoped to the tree and complete them with
	// STATUS_RANGE_NOT_LOCKED (the FileID's open went away with the tree).
	// Failing to do so leaves the dispatch goroutine waiting for a lock
	// that no client will ever release.
	if h.PendingLockRegistry != nil {
		for _, parked := range h.PendingLockRegistry.UnregisterAllForTree(ctx.TreeID) {
			if parked.Callback != nil {
				go func(p *PendingLock) {
					if err := p.Callback(p.SessionID, p.MessageID, p.AsyncId, types.StatusRangeNotLocked, nil); err != nil {
						logger.Debug("TREE_DISCONNECT: failed to cancel pending LOCK",
							"asyncId", p.AsyncId, "messageID", p.MessageID, "error", err)
					}
				}(parked)
			}
			if h.LockWaitGraph != nil && parked.OwnerID != "" {
				h.LockWaitGraph.RemoveWaiter(parked.OwnerID)
			}
		}
	}

	// ========================================================================
	// Step 3: Delete the tree connection
	// ========================================================================

	h.DeleteTree(ctx.TreeID)

	// ========================================================================
	// Step 4: Build and return success response
	// ========================================================================

	w := smbenc.NewWriter(4)
	w.WriteUint16(4) // StructureSize
	w.WriteUint16(0) // Reserved

	return NewResult(types.StatusSuccess, w.Bytes()), nil
}

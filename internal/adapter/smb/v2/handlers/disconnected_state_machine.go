package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// MS-SMB2 §3.3.4.18 — Disconnected durable handle preservation/purge state machine.
//
// When a client transport-disconnects, an open with IsDurable set is persisted
// to the DurableHandleStore. While disconnected, operations from OTHER opens on
// the same underlying file may conflict with the disconnected open's persisted
// state. This file implements the preserve/purge decision per [MS-SMB2] §3.3.4.18
// (Server Receives an Object Store Operation) and §3.3.5.9 (Receiving an SMB2
// CREATE Request, Step 10 share-mode/lease conflict handling) — mirroring
// Samba's `delay_for_oplock_fn` + `share_mode_cleanup_disconnected` semantics
// from source3/smbd/open.c and source3/smbd/scavenger.c.
//
// Rule summary (validated against smbtorture smb2.durable-v2-open):
//
//   - A disconnected open holding a lease that would need to be broken to a
//     state lacking SMB2_LEASE_HANDLE_CACHING is purged on the spot — the
//     disconnected client cannot ack the break, so the durable is irrecoverably
//     lost (Samba sends the break message and the scavenger evicts the entry
//     after timeout; we collapse the two steps).
//
//   - A new open with FILE_SHARE_NONE on a file with a disconnected handle
//     purges the disconnected handle (share-mode conflict, MS-FSA 2.1.5.1.2).
//
//   - A WRITE from a live handle breaks Level-II (Read) leases on the same
//     file to None per MS-SMB2 §3.3.5.16; any disconnected RH lease on the
//     file with a different lease-key is purged (it would break to None).
//
//   - A RENAME from a live handle breaks Handle leases on the same file to
//     Read (loses H) per MS-SMB2 §3.3.5.21; any disconnected lease with a
//     different lease-key is purged (it would lose H).
//
// Non-rules (these MUST NOT purge — covered by smb2.durable-v2-open.keep-*):
//
//   - A new open with a default share (read|write|delete) on a file with a
//     disconnected RH/RWH handle does NOT purge the disconnected handle by
//     itself; the new open instead gets a downgraded lease grant. The grant
//     downgrade is enforced by the LeaseManager (see bestGrantableState in
//     pkg/metadata/lock/leases.go); this state machine only intervenes when
//     the disconnected handle would have to lose H.

// disconnectedHandleAction is the decision the state machine returns for
// each disconnected handle inspected.
type disconnectedHandleAction int

const (
	disconnectedActionPreserve disconnectedHandleAction = iota
	disconnectedActionPurge
)

// SMB2 lease bits. Import from pkg/metadata/lock so we don't drift on rename.
const (
	smbLeaseRead   = lock.LeaseStateRead
	smbLeaseHandle = lock.LeaseStateHandle
	smbLeaseWrite  = lock.LeaseStateWrite
)

// SMB2 share-access bits.
const (
	smbShareRead   uint32 = 0x01
	smbShareWrite  uint32 = 0x02
	smbShareDelete uint32 = 0x04
)

// disconnectedConflictOnNewOpen evaluates whether a FRESH (non-reconnect)
// CREATE on the same underlying file should purge a disconnected durable
// handle D.
//
// Contract: this predicate MUST NOT be called from a durable reconnect
// CREATE (DHnC / DH2C). Reconnect is handled by ProcessDurableReconnectContext
// which restores the persisted handle without consulting this predicate; by
// the time control reaches the steady-state CREATE path that calls into us,
// the disconnect-vs-reconnect choice has already been made. The single
// callsite (create_post_break.go::handleCreate) only runs on the fresh path
// because create.go::handleCreate's reconnect branch early-returns before
// reaching Step 8a-bis.
//
// Inputs:
//   - dLeaseState: the lease state persisted at disconnect (R/RH/RWH bits or None).
//   - dLeaseKey: D's lease key.
//   - newLeaseState: lease state requested by the new open (zero if no lease).
//   - newLeaseKey: lease key of the new open's lease (zero if no lease).
//   - newShareAccess: share-mode bits from the new CREATE.
//
// Decision rules (see file-level comment for spec mapping):
//
//   - newShareAccess excludes all of {READ, WRITE, DELETE} (SHARE_NONE) →
//     purge (MS-FSA 2.1.5.1.2 share-mode conflict).
//   - D holds W (RWH) AND new open requests any non-None lease with a
//     DIFFERENT lease key → purge (two W-capable cachers on different keys
//     cannot coexist; the break_to value strips W and, per Samba's
//     delay_for_oplock_fn lease-strip cascade, leaves W-only which is an
//     invalid lease state → effectively break to NONE → lose H).
//   - Otherwise → preserve.
func disconnectedConflictOnNewOpen(
	dLeaseState uint32,
	dLeaseKey [16]byte,
	newLeaseState uint32,
	newLeaseKey [16]byte,
	newShareAccess uint32,
) disconnectedHandleAction {
	// SHARE_NONE conflict: any disconnected handle is purged because the
	// disconnected open held shared access and the new open denies it.
	if newShareAccess&(smbShareRead|smbShareWrite|smbShareDelete) == 0 {
		return disconnectedActionPurge
	}

	// W-on-W conflict on different keys: the disconnected handle held W,
	// new open requests any non-None lease (W or RH). Even an RH request
	// from a different key forces the W-holder to lose W; in Samba this
	// cascades through the candidate downgrade chain (RWH → RH → R → NONE)
	// and lands on a state lacking H because the disconnected holder cannot
	// ack the in-flight break. Purge.
	//
	// Key comparison treats a zero lease key as "no key" — distinct from any
	// other key, including another zero. This matters for oplock-V2 / no-lease
	// opens on both sides: they cannot coexist with a W-holder either.
	if dLeaseState&smbLeaseWrite != 0 && newLeaseState != 0 {
		if dLeaseKey != newLeaseKey || dLeaseKey == ([16]byte{}) {
			return disconnectedActionPurge
		}
	}

	return disconnectedActionPreserve
}

// disconnectedConflictOnDataChange evaluates whether a WRITE or RENAME from a
// live opener should purge a disconnected durable handle D.
//
// excludeLeaseKey is the lease key of the actor (writer / renamer). Handles
// matching that key are preserved — the actor holding the lease cannot be
// breaking its own lease.
//
// breakToBelowHandle is true when the action's break_to value strips
// SMB2_LEASE_HANDLE from the broken lease. Writes break Level-II to None
// (lose H). Renames break Handle leases to R (lose H). Both cases purge any
// disconnected handle holding a lease whose key differs from the actor's,
// because the disconnected client cannot ack the break.
//
// dLeaseState == 0 is treated as "no caching rights and no reconnect prospect"
// → purge. A disconnected handle whose lease was already downgraded to None
// pre-disconnect (e.g. a lease break the client acked before the transport
// dropped) is unreconnectable: the persisted LeaseState=0 record cannot
// re-establish caching on reconnect, so leaving it in place only serves to
// block subsequent break-cascade decisions until the scavenger times it out.
// Mirrors Samba `share_mode_cleanup_disconnected` which evicts entries
// lacking caching bits on the same trigger paths.
//
// Contract: the caller (purgeConflictingDisconnectedHandlesForDataChange) is
// responsible for the fast-path `breakToBelowHandle == false` early-return.
// This predicate assumes the data-change WILL break to below H and only
// distinguishes own-handle (preserve) from foreign-handle (purge).
func disconnectedConflictOnDataChange(
	dLeaseKey [16]byte,
	excludeLeaseKey [16]byte,
) disconnectedHandleAction {
	// Same actor — never purge our own handle.
	if dLeaseKey == excludeLeaseKey && dLeaseKey != ([16]byte{}) {
		return disconnectedActionPreserve
	}
	return disconnectedActionPurge
}

// purgeConflictingDisconnectedHandlesForOpen scans disconnected handles for
// the underlying file (keyed by metadata handle) and purges those that
// conflict with the new open under §3.3.4.18.
//
// Returns the number of purged handles. Errors looking up the store are
// logged at debug — purge is best-effort and must not block CREATE.
//
// Holds Handler.durablePurgeMu across the Get→Delete window so a concurrent
// disconnect persist (handler.go:closeFilesMatching) cannot Put a new
// disconnected handle between our snapshot and the per-id Delete.
func (h *Handler) purgeConflictingDisconnectedHandlesForOpen(
	ctx context.Context,
	metaHandle []byte,
	newLeaseState uint32,
	newLeaseKey [16]byte,
	newShareAccess uint32,
) int {
	if h.DurableStore == nil || len(metaHandle) == 0 {
		return 0
	}
	h.durablePurgeMu.Lock()
	defer h.durablePurgeMu.Unlock()
	handles, err := h.DurableStore.GetDurableHandlesByFileHandle(ctx, metaHandle)
	if err != nil {
		logger.Debug("purgeConflictingDisconnectedHandlesForOpen: lookup failed",
			"error", err)
		return 0
	}
	if len(handles) == 0 {
		return 0
	}
	var purged int
	for _, d := range handles {
		// Only consider handles that survived a transport disconnect — the
		// store may transiently hold pre-disconnect rows on some backends.
		if d.DisconnectedAt.IsZero() {
			continue
		}
		action := disconnectedConflictOnNewOpen(
			d.LeaseState, d.LeaseKey,
			newLeaseState, newLeaseKey,
			newShareAccess,
		)
		if action != disconnectedActionPurge {
			continue
		}
		h.purgeOneDisconnectedHandle(ctx, d, "new-open conflict")
		purged++
	}
	return purged
}

// purgeConflictingDisconnectedHandlesForDataChange scans disconnected handles
// for the underlying file and purges those that would lose H caching due to
// the actor's WRITE or RENAME.
//
// Holds Handler.durablePurgeMu across the Get→Delete window for the same
// reason as purgeConflictingDisconnectedHandlesForOpen.
func (h *Handler) purgeConflictingDisconnectedHandlesForDataChange(
	ctx context.Context,
	metaHandle []byte,
	excludeLeaseKey [16]byte,
	breakToBelowHandle bool,
) int {
	if h.DurableStore == nil || len(metaHandle) == 0 || !breakToBelowHandle {
		return 0
	}
	h.durablePurgeMu.Lock()
	defer h.durablePurgeMu.Unlock()
	handles, err := h.DurableStore.GetDurableHandlesByFileHandle(ctx, metaHandle)
	if err != nil {
		logger.Debug("purgeConflictingDisconnectedHandlesForDataChange: lookup failed",
			"error", err)
		return 0
	}
	if len(handles) == 0 {
		return 0
	}
	var purged int
	for _, d := range handles {
		if d.DisconnectedAt.IsZero() {
			continue
		}
		action := disconnectedConflictOnDataChange(d.LeaseKey, excludeLeaseKey)
		if action != disconnectedActionPurge {
			continue
		}
		h.purgeOneDisconnectedHandle(ctx, d, "data-change break")
		purged++
	}
	return purged
}

// purgeOneDisconnectedHandle deletes a single disconnected handle and releases
// its locks. Mirrors the cleanup half of DurableHandleScavenger.cleanupAndDelete
// but is callable from CREATE/WRITE/RENAME hot paths without the scavenger
// ticker overhead.
func (h *Handler) purgeOneDisconnectedHandle(
	ctx context.Context,
	d *lock.PersistedDurableHandle,
	reason string,
) {
	if h.Registry != nil {
		if metaSvc := h.Registry.GetMetadataService(); metaSvc != nil && len(d.MetadataHandle) > 0 {
			// Release byte-range locks held by the disconnected open. SMB
			// locks are keyed by per-open OpenID (derived from the original
			// FileID — see OpenFile.OpenID), NOT by SessionID. The persisted
			// OriginalFileID is the full 16-byte FileID captured at the
			// first CREATE; reconstruct the OpenID via the same formula so
			// the release matches the recording side. Older persisted rows
			// (pre-OriginalFileID) decode to all zeros — fall back to
			// FileID for forward compatibility.
			fileID := d.OriginalFileID
			if fileID == ([16]byte{}) {
				fileID = d.FileID
			}
			openID := fmt.Sprintf("%x", fileID)
			if err := metaSvc.UnlockAllForOpen(ctx, d.MetadataHandle, openID); err != nil {
				logger.Warn("purgeOneDisconnectedHandle: lock release failed",
					"id", d.ID, "path", d.Path, "openID", openID, "error", err)
			}
		}
	}
	if err := h.DurableStore.DeleteDurableHandle(ctx, d.ID); err != nil {
		logger.Warn("purgeOneDisconnectedHandle: delete failed",
			"id", d.ID, "path", d.Path, "error", err)
		return
	}
	logger.Debug("purgeOneDisconnectedHandle: purged disconnected handle",
		"id", d.ID,
		"path", d.Path,
		"leaseState", fmt.Sprintf("0x%x", d.LeaseState),
		"reason", reason)
}

// shouldPersistDurableOnDisconnect returns false when an open MUST NOT be
// persisted as a durable handle at transport-disconnect time, even if it
// carries IsDurable. Per MS-SMB2 §3.3.4.18 and smb2.durable-v2-open.lock-noW-lease,
// an open holding a byte-range lock under a lease that lacks W (write caching)
// cannot reliably resume: the BR-lock is bound to the open's OpenID and the
// lease cannot promote to W on reconnect without breaking other holders.
// Samba's `vfs_default_durable_disconnect` mirrors this by returning
// NT_STATUS_NOT_SUPPORTED, which falls through to a normal close.
func shouldPersistDurableOnDisconnect(
	leaseState uint32,
	hasByteRangeLocks bool,
) bool {
	if hasByteRangeLocks && leaseState&smbLeaseWrite == 0 {
		return false
	}
	return true
}

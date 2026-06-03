package handlers

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// AsyncLockCompleteCallback delivers the final async LOCK response for a
// blocking-lock request that was parked on a byte-range conflict. The
// callback is responsible for stamping FlagAsync + AsyncId, signing, and
// releasing the async credit slot. body is the encoded SMB2 LOCK response
// body (or nil for error status, in which case SendAsyncCompletionResponse
// substitutes MakeErrorBody).
type AsyncLockCompleteCallback func(sessionID, messageID, asyncId uint64, status types.Status, body []byte) error

// PendingLock tracks a blocking SMB2 LOCK request parked on a byte-range
// conflict. It holds the identifiers needed to locate the entry on
// CANCEL / tree-disconnect / session teardown and the cancel function that
// tears down the resume goroutine's wait.
//
// MS-SMB2 §3.3.5.14 + Samba `smbd_smb2_lock_send`: a blocking LOCK that
// cannot be granted immediately must emit an interim STATUS_PENDING and
// complete asynchronously. Cancellation paths:
//
//   - SMB2_CANCEL by MessageID (sync) or AsyncId (async-flagged) →
//     UnregisterByMessageID / UnregisterByAsyncId (smb2.lock.cancel).
//   - TREE_DISCONNECT → UnregisterAllForTree (smb2.lock.cancel-tdis).
//   - LOGOFF / connection drop → UnregisterAllForSession
//     (smb2.lock.cancel-logoff).
type PendingLock struct {
	ConnID    uint64
	SessionID uint64
	TreeID    uint32
	MessageID uint64
	AsyncId   uint64

	// OwnerID is the per-open lock owner identifier. Used as the WFG node
	// for this waiter so RemoveWaiter can prune our edges on completion.
	OwnerID string

	// Identity is the authenticated identity from the original LOCK request.
	// Carried into the resume goroutine so permission checks in
	// MetadataService.LockFile succeed on retry.
	Identity *metadata.Identity

	// Cancel tears down the resume goroutine's retry loop (via context
	// cancel) so cancellation paths can unblock it promptly.
	Cancel context.CancelFunc

	// Callback delivers the final SMB2 LOCK response. Invoked by the resume
	// goroutine on normal completion OR by the registry on
	// CANCEL / TDIS / LOGOFF (with StatusCancelled / StatusRangeNotLocked +
	// nil body). Releases the async slot as part of its work.
	Callback AsyncLockCompleteCallback

	// SMB3 LOCK replay-cache coordinates. Carried so the resume goroutine
	// can record success in LockReplayCache after a parked LOCK is finally
	// granted — otherwise a FLAGS_REPLAY_OPERATION retry of the originally
	// parked LOCK would re-execute the acquire path (MS-SMB2 §3.3.5.14
	// step 4).
	FileID         [16]byte
	LockSeqEnabled bool
	LockSeqIndex   uint32
	LockSeqNumber  uint8
}

// lockMsgKey scopes a MessageID to its connection (SMB2 MessageIDs are
// per-connection).
type lockMsgKey struct {
	ConnID    uint64
	MessageID uint64
}

// Secondary-index slots for PendingLockRegistry, in configured order.
const (
	lockIdxMsgKey = iota
)

// Bucket-index slots for PendingLockRegistry, in configured order.
const (
	lockBucketSession = iota
	lockBucketTree
)

// MaxPendingLocks caps concurrent parked blocking LOCKs per server. Picked to
// match MaxPendingCreates.
const MaxPendingLocks = 4096

// ErrTooManyPendingLocks is returned when the global pending-LOCK limit would
// be exceeded.
var ErrTooManyPendingLocks = fmt.Errorf("too many pending LOCKs (max %d)", MaxPendingLocks)

// ErrDuplicateLockAsyncId is returned when Register is called with an AsyncId
// that is already in the registry.
var ErrDuplicateLockAsyncId = fmt.Errorf("duplicate AsyncId in PendingLockRegistry")

// ErrDuplicateLockMessageID is returned when Register is called with a
// (ConnID, MessageID) pair that is already in the registry.
var ErrDuplicateLockMessageID = fmt.Errorf("duplicate (ConnID, MessageID) in PendingLockRegistry")

// PendingLockRegistry indexes pending blocking LOCKs by four keys:
// per-connection MessageID (synchronous CANCEL), AsyncId (async-flagged
// CANCEL), SessionID (LOGOFF / connection cleanup), and TreeID (TREE_DISCONNECT
// — smb2.lock.cancel-tdis). Thread-safe.
//
// An entry is removed exactly once: either by the resume goroutine after it
// delivers the final response (via Unregister) or by a cancellation path
// (UnregisterByMessageID / UnregisterByAsyncId / UnregisterAllForSession /
// UnregisterAllForTree). The resume goroutine must check whether its entry is
// still registered before sending, so a concurrent CANCEL does not produce a
// duplicate response for the same MessageID.
type PendingLockRegistry struct {
	reg *pendingRegistry[PendingLock]
}

// NewPendingLockRegistry builds an empty registry.
func NewPendingLockRegistry() *PendingLockRegistry {
	return &PendingLockRegistry{
		reg: newPendingRegistry(registryConfig[PendingLock]{
			asyncID: func(p *PendingLock) uint64 { return p.AsyncId },
			indexes: []keyFunc[PendingLock]{
				func(p *PendingLock) any {
					return lockMsgKey{ConnID: p.ConnID, MessageID: p.MessageID}
				},
			},
			buckets: []keyFunc[PendingLock]{
				func(p *PendingLock) any { return p.SessionID },
				func(p *PendingLock) any { return p.TreeID },
			},
			maxOps: MaxPendingLocks,
		}),
	}
}

// Register inserts a pending LOCK. Returns an error on overflow or if the
// entry would collide with an existing (ConnID, MessageID) or AsyncId; in all
// failure cases the registry is left untouched and the caller owns releasing
// its reserved async slot.
func (r *PendingLockRegistry) Register(p *PendingLock) error {
	r.reg.mu.Lock()
	defer r.reg.mu.Unlock()

	if len(r.reg.byAsyncID) >= r.reg.maxOps {
		return ErrTooManyPendingLocks
	}
	if r.reg.lookupLocked(lockIdxMsgKey, lockMsgKey{ConnID: p.ConnID, MessageID: p.MessageID}) != nil {
		return ErrDuplicateLockMessageID
	}
	if _, dup := r.reg.byAsyncID[p.AsyncId]; dup {
		return ErrDuplicateLockAsyncId
	}

	r.reg.insertLocked(p)

	logger.Debug("PendingLockRegistry: registered",
		"connID", p.ConnID,
		"sessionID", p.SessionID,
		"treeID", p.TreeID,
		"messageID", p.MessageID,
		"asyncId", p.AsyncId,
		"total", len(r.reg.byAsyncID))
	return nil
}

// Unregister removes a pending LOCK by AsyncId without calling Cancel. Used
// by the resume goroutine AFTER it has successfully delivered the final
// response. Returns the removed entry, or nil if it was already unregistered
// (e.g. by a concurrent CANCEL / TDIS / LOGOFF).
func (r *PendingLockRegistry) Unregister(asyncId uint64) *PendingLock {
	return r.reg.unregisterByAsyncID(asyncId)
}

// UnregisterByMessageID removes a pending LOCK matching (connID, messageID)
// and invokes its Cancel closure to unblock the resume goroutine. Returns the
// removed entry, or nil if none matched. Used by synchronous SMB2_CANCEL.
func (r *PendingLockRegistry) UnregisterByMessageID(connID, messageID uint64) *PendingLock {
	p := r.reg.unregisterByIndex(lockIdxMsgKey, lockMsgKey{ConnID: connID, MessageID: messageID})
	if p != nil && p.Cancel != nil {
		p.Cancel()
	}
	return p
}

// UnregisterByAsyncId removes a pending LOCK matching asyncId and invokes its
// Cancel closure. Used by async-flagged SMB2_CANCEL.
func (r *PendingLockRegistry) UnregisterByAsyncId(asyncId uint64) *PendingLock {
	p := r.reg.unregisterByAsyncID(asyncId)
	if p != nil && p.Cancel != nil {
		p.Cancel()
	}
	return p
}

// UnregisterAllForSession removes and returns every pending LOCK associated
// with sessionID, invoking Cancel on each. Used by session teardown (LOGOFF,
// connection drop) to unblock goroutines before the session state they
// depend on is freed.
func (r *PendingLockRegistry) UnregisterAllForSession(sessionID uint64) []*PendingLock {
	removed := r.reg.unregisterBucket(lockBucketSession, sessionID)
	cancelAll(removed)
	return removed
}

// UnregisterAllForTree removes and returns every pending LOCK associated with
// treeID, invoking Cancel on each. Used by TREE_DISCONNECT to unblock
// goroutines holding state that the tree owns (smb2.lock.cancel-tdis).
func (r *PendingLockRegistry) UnregisterAllForTree(treeID uint32) []*PendingLock {
	removed := r.reg.unregisterBucket(lockBucketTree, treeID)
	cancelAll(removed)
	return removed
}

// UnregisterAllForOwner removes and returns every pending LOCK whose OwnerID
// matches, invoking Cancel on each. Used when a file handle is closed to
// unblock the resume goroutine before the handle state is freed (file-close
// cancellation per Samba brl_close_fnum).
func (r *PendingLockRegistry) UnregisterAllForOwner(ownerID string) []*PendingLock {
	removed := r.reg.unregisterMatching(func(p *PendingLock) bool {
		return p.OwnerID == ownerID
	})
	cancelAll(removed)
	return removed
}

// cancelAll invokes Cancel on each removed pending LOCK.
func cancelAll(removed []*PendingLock) {
	for _, p := range removed {
		if p.Cancel != nil {
			p.Cancel()
		}
	}
}

// Len returns the number of pending LOCKs.
func (r *PendingLockRegistry) Len() int {
	return r.reg.Len()
}

package handlers

import (
	"context"
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// AsyncCreateCompleteCallback delivers the final async CREATE response for a
// request that was parked on a lease break. The callback is responsible for
// stamping FlagAsync + AsyncId, signing, and releasing the async credit slot.
// The body is the encoded SMB2 CREATE response body (or nil for error status,
// in which case SendAsyncCompletionResponse substitutes MakeErrorBody).
type AsyncCreateCompleteCallback func(sessionID, messageID, asyncID uint64, status types.Status, body []byte) error

// PendingCreate tracks a CREATE request parked on a lease break. It holds the
// identifiers needed to locate the entry on CANCEL / session teardown and the
// cancel function that tears down the resume goroutine's wait.
type PendingCreate struct {
	// Request identification.
	ConnID    uint64
	SessionID uint64
	MessageID uint64
	AsyncID   uint64

	// Cancel tears down the resume goroutine's break-wait (via context cancel)
	// so CANCEL / session teardown can unblock it promptly.
	Cancel context.CancelFunc

	// Callback delivers the final SMB2 CREATE response. Invoked by the resume
	// goroutine on normal completion OR by the registry on CANCEL (with
	// StatusCancelled + nil body). Releases the async slot as part of its work.
	Callback AsyncCreateCompleteCallback
}

// PendingCreateRegistry indexes pending CREATEs by three keys: per-connection
// MessageID (for synchronous CANCEL), AsyncId (for async-flagged CANCEL), and
// SessionID (for session teardown). Thread-safe.
//
// An entry is removed exactly once: either by the resume goroutine after it
// delivers the final response (via Unregister) or by CANCEL / teardown (via
// UnregisterByMessageID / UnregisterByAsyncId / UnregisterAllForSession).
// The resume goroutine must check whether its entry is still registered before
// sending, so a concurrent CANCEL does not produce a duplicate response for
// the same MessageID.
type PendingCreateRegistry struct {
	mu        sync.Mutex
	byMsgKey  map[createMsgKey]*PendingCreate
	byAsyncID map[uint64]*PendingCreate
	bySession map[uint64]map[uint64]*PendingCreate // sessionID -> asyncID -> entry
	maxOps    int
}

type createMsgKey struct {
	ConnID    uint64
	MessageID uint64
}

// MaxPendingCreates caps concurrent parked CREATEs per server to protect the
// process from runaway clients. Picked to match MaxPendingWatches.
const MaxPendingCreates = 4096

// ErrTooManyPendingCreates is returned when the global pending-CREATE limit
// would be exceeded.
var ErrTooManyPendingCreates = fmt.Errorf("too many pending CREATEs (max %d)", MaxPendingCreates)

// NewPendingCreateRegistry builds an empty registry.
func NewPendingCreateRegistry() *PendingCreateRegistry {
	return &PendingCreateRegistry{
		byMsgKey:  make(map[createMsgKey]*PendingCreate),
		byAsyncID: make(map[uint64]*PendingCreate),
		bySession: make(map[uint64]map[uint64]*PendingCreate),
		maxOps:    MaxPendingCreates,
	}
}

// Register inserts a pending CREATE. Returns ErrTooManyPendingCreates on
// overflow. The caller must have reserved an async slot first; the caller
// owns releasing it on registration failure.
func (r *PendingCreateRegistry) Register(p *PendingCreate) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.byAsyncID) >= r.maxOps {
		return ErrTooManyPendingCreates
	}

	key := createMsgKey{ConnID: p.ConnID, MessageID: p.MessageID}
	r.byMsgKey[key] = p
	r.byAsyncID[p.AsyncID] = p

	bucket, ok := r.bySession[p.SessionID]
	if !ok {
		bucket = make(map[uint64]*PendingCreate)
		r.bySession[p.SessionID] = bucket
	}
	bucket[p.AsyncID] = p

	logger.Debug("PendingCreateRegistry: registered",
		"connID", p.ConnID,
		"sessionID", p.SessionID,
		"messageID", p.MessageID,
		"asyncID", p.AsyncID,
		"total", len(r.byAsyncID))
	return nil
}

// Unregister removes a pending CREATE by AsyncID without calling Cancel. Used
// by the resume goroutine AFTER it has successfully delivered the final
// response. Returns the removed entry, or nil if it was already unregistered
// (e.g. by a concurrent CANCEL).
func (r *PendingCreateRegistry) Unregister(asyncID uint64) *PendingCreate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removeLocked(asyncID)
}

// UnregisterByMessageID removes a pending CREATE matching (connID, messageID)
// and invokes its Cancel closure to unblock the resume goroutine. Returns the
// removed entry, or nil if none matched. Used by synchronous SMB2_CANCEL.
func (r *PendingCreateRegistry) UnregisterByMessageID(connID, messageID uint64) *PendingCreate {
	r.mu.Lock()
	p, ok := r.byMsgKey[createMsgKey{ConnID: connID, MessageID: messageID}]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	r.removeLocked(p.AsyncID)
	r.mu.Unlock()
	if p.Cancel != nil {
		p.Cancel()
	}
	return p
}

// UnregisterByAsyncID removes a pending CREATE matching asyncID and invokes
// its Cancel closure. Used by async-flagged SMB2_CANCEL.
func (r *PendingCreateRegistry) UnregisterByAsyncID(asyncID uint64) *PendingCreate {
	r.mu.Lock()
	p := r.removeLocked(asyncID)
	r.mu.Unlock()
	if p != nil && p.Cancel != nil {
		p.Cancel()
	}
	return p
}

// UnregisterAllForSession removes and returns every pending CREATE associated
// with sessionID, invoking Cancel on each. Used by session teardown to unblock
// goroutines before the session state they depend on is freed.
func (r *PendingCreateRegistry) UnregisterAllForSession(sessionID uint64) []*PendingCreate {
	r.mu.Lock()
	bucket, ok := r.bySession[sessionID]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	removed := make([]*PendingCreate, 0, len(bucket))
	for asyncID := range bucket {
		if p := r.removeLocked(asyncID); p != nil {
			removed = append(removed, p)
		}
	}
	r.mu.Unlock()
	for _, p := range removed {
		if p.Cancel != nil {
			p.Cancel()
		}
	}
	return removed
}

// removeLocked drops entries keyed on asyncID from all maps. Caller holds mu.
func (r *PendingCreateRegistry) removeLocked(asyncID uint64) *PendingCreate {
	p, ok := r.byAsyncID[asyncID]
	if !ok {
		return nil
	}
	delete(r.byAsyncID, asyncID)
	delete(r.byMsgKey, createMsgKey{ConnID: p.ConnID, MessageID: p.MessageID})
	if bucket, ok := r.bySession[p.SessionID]; ok {
		delete(bucket, asyncID)
		if len(bucket) == 0 {
			delete(r.bySession, p.SessionID)
		}
	}
	return p
}

// Len returns the number of pending CREATEs.
func (r *PendingCreateRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byAsyncID)
}

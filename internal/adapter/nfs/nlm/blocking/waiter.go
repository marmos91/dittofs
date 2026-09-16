// Package blocking implements the blocking lock queue for NLM protocol.
//
// When an NLM LOCK request with block=true conflicts with an existing lock,
// the request is queued rather than immediately denied. When the conflicting
// lock is released, queued waiters are processed in FIFO order and the client
// receives an NLM_GRANTED callback.
package blocking

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Waiter represents a pending blocking lock request.
//
// When a client issues a blocking lock request (block=true) that conflicts
// with an existing lock, we create a Waiter to track the pending request.
// The waiter contains all information needed to:
//   - Identify which lock is being requested
//   - Send NLM_GRANTED callback when lock becomes available
//   - Match against NLM_CANCEL requests
//
// Thread Safety:
// A queued waiter is handed to the grant path by pointer, which builds an
// NLM_GRANTED callback from it while the queue may still be mutating it, so
// every field is guarded by mu rather than by bq.mu. Read the whole struct at
// once with Snapshot(); write through the methods below. The queue's own
// readers (matches, isRetransmissionOf) go through Snapshot too.
type Waiter struct {
	// Lock is the requested lock specification
	Lock *lock.UnifiedLock

	// Cookie is the client's opaque cookie (echoed in callback)
	Cookie []byte

	// Exclusive is whether the lock is exclusive (write) or shared (read)
	Exclusive bool

	// CallbackHost is the client's transport source host (IP), used to resolve
	// the NLM_GRANTED callback target at grant time. The callback PORT is not
	// stored here: it is looked up from the client's portmapper when the grant
	// is actually delivered, so a stale/guessed port is never dialed.
	CallbackHost string

	// CallbackProg is the client's callback program number (NLM)
	CallbackProg uint32

	// CallbackVers is the callback program version
	CallbackVers uint32

	// CallerName is the client hostname from the lock request
	CallerName string

	// Svid is the client's process ID
	Svid int32

	// OH is the client's owner handle (opaque)
	OH []byte

	// FileHandle is the file this lock is for
	FileHandle []byte

	// QueuedAt is when this waiter was queued
	QueuedAt time.Time

	// cancelled indicates if this waiter has been cancelled
	cancelled bool

	// mu protects every field above. It is taken on its own, never while
	// holding bq.mu in the other order, so bq.mu -> mu is the only nesting.
	mu sync.Mutex
}

// WaiterSnapshot is a point-in-time copy of a Waiter, taken under its mutex.
// The grant path builds an NLM_GRANTED callback from a snapshot so it never
// reads a field that the queue is concurrently rewriting. The byte slices are
// copied shallowly: they are set once at construction and never mutated in
// place, so sharing the backing array is safe.
type WaiterSnapshot struct {
	Lock         *lock.UnifiedLock
	Cookie       []byte
	Exclusive    bool
	CallbackHost string
	CallbackProg uint32
	CallbackVers uint32
	CallerName   string
	Svid         int32
	OH           []byte
	FileHandle   []byte
	QueuedAt     time.Time
	Cancelled    bool
}

// Snapshot returns a consistent copy of the waiter's fields. Reading the whole
// struct this way, rather than field by field, is what makes the cancellation
// check and the callback it guards see one state: a waiter cancelled between
// two separate reads would otherwise be checked as live and then dialed.
func (w *Waiter) Snapshot() WaiterSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WaiterSnapshot{
		Lock:         w.Lock,
		Cookie:       w.Cookie,
		Exclusive:    w.Exclusive,
		CallbackHost: w.CallbackHost,
		CallbackProg: w.CallbackProg,
		CallbackVers: w.CallbackVers,
		CallerName:   w.CallerName,
		Svid:         w.Svid,
		OH:           w.OH,
		FileHandle:   w.FileHandle,
		QueuedAt:     w.QueuedAt,
		Cancelled:    w.cancelled,
	}
}

// IsCancelled returns true if this waiter has been cancelled.
//
// Thread safety: Safe to call concurrently.
func (w *Waiter) IsCancelled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancelled
}

// Cancel marks this waiter as cancelled.
//
// This is called when:
//   - NLM_CANCEL request received for this waiter
//   - Waiter is being removed from queue
//
// Thread safety: Safe to call concurrently.
func (w *Waiter) Cancel() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = true
}

// setQueuedAt stamps the waiter with the time it entered the queue.
func (w *Waiter) setQueuedAt(t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.QueuedAt = t
}

// matches reports whether w is the waiter identified by this owner and byte
// range. This is what NLM_CANCEL removes by: CANCEL is specified to be
// idempotent, so it stays deliberately permissive about the lock type.
func (w *Waiter) matches(ownerID string, offset, length uint64) bool {
	s := w.Snapshot()
	return s.Lock.Owner.OwnerID == ownerID &&
		s.Lock.Offset == offset &&
		s.Lock.Length == length
}

// isRetransmissionOf reports whether w is the already-queued form of req: the
// same owner and range, asking for the same kind of lock.
//
// Deliberately stricter than matches. A shared and an exclusive request over
// one range from one owner are different requests, and treating the second as
// a retransmission of the first would answer it NLM4_BLOCKED and then grant it
// the wrong lock type -- a GRANTED the client rejects (Linux nlm_compare_locks
// compares fl_type) over a lock the server keeps holding, with no owner left
// to release it.
func (w *Waiter) isRetransmissionOf(req *Waiter) bool {
	queued, incoming := w.Snapshot(), req.Snapshot()
	return queued.Exclusive == incoming.Exclusive &&
		queued.Lock.Owner.OwnerID == incoming.Lock.Owner.OwnerID &&
		queued.Lock.Offset == incoming.Lock.Offset &&
		queued.Lock.Length == incoming.Lock.Length
}

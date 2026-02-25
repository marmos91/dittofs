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
// The Cancelled field is protected by an internal mutex and can be safely
// accessed concurrently via IsCancelled() and Cancel() methods.
type Waiter struct {
	// Lock is the requested lock specification
	Lock *lock.UnifiedLock

	// Cookie is the client's opaque cookie (echoed in callback)
	Cookie []byte

	// Exclusive is whether the lock is exclusive (write) or shared (read)
	Exclusive bool

	// CallbackAddr is the client's callback address (IP:port)
	CallbackAddr string

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

	// mu protects the cancelled field
	mu sync.Mutex
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

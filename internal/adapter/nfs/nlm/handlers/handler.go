package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DefaultBlockingQueueSize is the default per-file limit for blocking lock requests.
// Per CONTEXT.md: per-file limit on blocking lock queue size (e.g., 100).
const DefaultBlockingQueueSize = 100

// NLMLockService defines the interface for NLM lock operations.
//
// This interface is satisfied by both the NLMService in the NFS adapter
// (which uses LockManager directly) and MetadataService (for backward compatibility).
// It decouples NLM protocol handlers from the MetadataService concrete type.
type NLMLockService interface {
	// LockFileNLM acquires a lock for NLM protocol.
	LockFileNLM(ctx context.Context, handle []byte, owner lock.LockOwner, offset, length uint64, exclusive bool, reclaim bool) (*lock.LockResult, error)

	// TestLockNLM tests if a lock could be granted without acquiring it.
	TestLockNLM(ctx context.Context, handle []byte, owner lock.LockOwner, offset, length uint64, exclusive bool) (bool, *lock.UnifiedLockConflict, error)

	// UnlockFileNLM releases a lock for NLM protocol.
	UnlockFileNLM(ctx context.Context, handle []byte, ownerID string, offset, length uint64) error

	// CancelBlockingLock cancels a pending blocking lock request.
	CancelBlockingLock(ctx context.Context, handle []byte, ownerID string, offset, length uint64) error
}

// Handler processes NLM procedure calls.
//
// Handler holds references to:
//   - NLMLockService for performing lock operations (uses LockManager directly)
//   - BlockingQueue for managing pending blocking lock requests
//
// Thread Safety:
// Handler is safe for concurrent use by multiple goroutines.
// The underlying NLMLockService and BlockingQueue handle synchronization.
type Handler struct {
	nlmService    NLMLockService
	blockingQueue *blocking.BlockingQueue

	// crashCleanup releases all locks held by the named client and drains its
	// blocking-queue waiters. It is invoked by the FREE_ALL handler so that an
	// NLM FREE_ALL RPC (sent by a rebooting client's lock manager) actually
	// frees the crashed client's locks, independently of the NSM SM_NOTIFY path.
	// May be nil (then FREE_ALL only logs). Wired by the adapter via
	// SetCrashCleanup to the same cleanup routine NSM uses.
	crashCleanup func(clientID string)

	// callerBinding pins each NLM caller_name to the transport source host that
	// first locked under it, so UNLOCK/CANCEL from a different host cannot
	// release/cancel another client's locks via a spoofed caller_name.
	callerBinding *callerBinding
}

// NewHandler creates a new NLM handler with the given NLM lock service and blocking queue.
//
// Parameters:
//   - nlmService: The NLM lock service for performing lock operations.
//     Must not be nil.
//   - blockingQueue: The blocking queue for managing pending lock requests.
//     Must not be nil.
//
// Returns a configured Handler ready to process NLM requests.
func NewHandler(nlmService NLMLockService, blockingQueue *blocking.BlockingQueue) *Handler {
	return &Handler{
		nlmService:    nlmService,
		blockingQueue: blockingQueue,
		callerBinding: newCallerBinding(),
	}
}

// GetBlockingQueue returns the blocking queue for this handler.
// Used by the adapter to process waiters when locks are released.
func (h *Handler) GetBlockingQueue() *blocking.BlockingQueue {
	return h.blockingQueue
}

// SetCrashCleanup installs the callback invoked by FREE_ALL to release all locks
// held by a named client. The adapter wires this to the same per-share lock
// cleanup used for NSM SM_NOTIFY crash detection, giving FREE_ALL real effect.
//
// Passing nil disables FREE_ALL cleanup (handler only logs).
func (h *Handler) SetCrashCleanup(fn func(clientID string)) {
	h.crashCleanup = fn
}

package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/blocking"
	"github.com/marmos91/dittofs/pkg/config"
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
//   - Config for configurable timeouts (e.g., lease break timeout)
//
// Thread Safety:
// Handler is safe for concurrent use by multiple goroutines.
// The underlying NLMLockService and BlockingQueue handle synchronization.
type Handler struct {
	nlmService    NLMLockService
	blockingQueue *blocking.BlockingQueue
	config        *config.Config
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
	return NewHandlerWithConfig(nlmService, blockingQueue, nil)
}

// NewHandlerWithConfig creates a new NLM handler with config for cross-protocol settings.
//
// Parameters:
//   - nlmService: The NLM lock service for performing lock operations.
//   - blockingQueue: The blocking queue for managing pending lock requests.
//   - cfg: The config containing lock settings (lease break timeout, etc.)
//
// Returns a configured Handler with cross-protocol support.
func NewHandlerWithConfig(nlmService NLMLockService, blockingQueue *blocking.BlockingQueue, cfg *config.Config) *Handler {
	return &Handler{
		nlmService:    nlmService,
		blockingQueue: blockingQueue,
		config:        cfg,
	}
}

// GetBlockingQueue returns the blocking queue for this handler.
// Used by the adapter to process waiters when locks are released.
func (h *Handler) GetBlockingQueue() *blocking.BlockingQueue {
	return h.blockingQueue
}

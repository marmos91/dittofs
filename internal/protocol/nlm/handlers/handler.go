package handlers

import (
	"github.com/marmos91/dittofs/internal/protocol/nlm/blocking"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// DefaultBlockingQueueSize is the default per-file limit for blocking lock requests.
// Per CONTEXT.md: per-file limit on blocking lock queue size (e.g., 100).
const DefaultBlockingQueueSize = 100

// Handler processes NLM procedure calls.
//
// Handler holds references to:
//   - MetadataService for performing lock operations
//   - BlockingQueue for managing pending blocking lock requests
//
// Thread Safety:
// Handler is safe for concurrent use by multiple goroutines.
// The underlying MetadataService and BlockingQueue handle synchronization.
type Handler struct {
	metadataService *metadata.MetadataService
	blockingQueue   *blocking.BlockingQueue
}

// NewHandler creates a new NLM handler with the given metadata service and blocking queue.
//
// Parameters:
//   - metadataService: The metadata service for performing lock operations.
//     Must not be nil.
//   - blockingQueue: The blocking queue for managing pending lock requests.
//     Must not be nil.
//
// Returns a configured Handler ready to process NLM requests.
func NewHandler(metadataService *metadata.MetadataService, blockingQueue *blocking.BlockingQueue) *Handler {
	return &Handler{
		metadataService: metadataService,
		blockingQueue:   blockingQueue,
	}
}

// GetBlockingQueue returns the blocking queue for this handler.
// Used by the adapter to process waiters when locks are released.
func (h *Handler) GetBlockingQueue() *blocking.BlockingQueue {
	return h.blockingQueue
}

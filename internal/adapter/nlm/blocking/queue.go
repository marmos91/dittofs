package blocking

import (
	"errors"
	"sync"
	"time"
)

// ErrQueueFull is returned when the per-file queue is at capacity.
// Per CONTEXT.md, this should map to NLM4_DENIED_NOLOCKS.
var ErrQueueFull = errors.New("blocking queue full")

// BlockingQueue manages per-file queues of waiting lock requests.
//
// When a blocking lock request (block=true) conflicts with an existing lock,
// the request is added to the queue for that file. When a lock is released,
// the unlock path calls GetWaiters() to process pending requests in FIFO order.
//
// Per CONTEXT.md decisions:
//   - Wait indefinitely for blocked locks until available or client cancels
//   - Per-file limit on queue size (e.g., 100) to prevent resource exhaustion
//   - Queue full returns NLM4_DENIED_NOLOCKS
//
// Thread Safety:
// All methods are safe for concurrent use. Uses RWMutex for reader-writer
// synchronization.
type BlockingQueue struct {
	mu       sync.RWMutex
	queues   map[string][]*Waiter // fileHandle -> waiters (slice, FIFO order)
	maxQueue int                  // Per-file limit (e.g., 100)
}

// NewBlockingQueue creates a new blocking queue with the given per-file limit.
//
// Parameters:
//   - maxPerFile: Maximum number of waiters per file. When this limit is
//     reached, new blocking requests return ErrQueueFull (NLM4_DENIED_NOLOCKS).
//
// Returns a configured BlockingQueue ready for use.
func NewBlockingQueue(maxPerFile int) *BlockingQueue {
	return &BlockingQueue{
		queues:   make(map[string][]*Waiter),
		maxQueue: maxPerFile,
	}
}

// Enqueue adds a waiter to the queue for a file.
//
// The waiter is added to the end of the queue (FIFO order).
// Returns ErrQueueFull if the per-file limit is reached.
//
// Parameters:
//   - fileHandle: String key identifying the file
//   - waiter: The pending lock request to queue
//
// Thread safety: Safe to call concurrently.
func (bq *BlockingQueue) Enqueue(fileHandle string, waiter *Waiter) error {
	bq.mu.Lock()
	defer bq.mu.Unlock()

	queue := bq.queues[fileHandle]
	if len(queue) >= bq.maxQueue {
		return ErrQueueFull
	}

	waiter.QueuedAt = time.Now()
	bq.queues[fileHandle] = append(queue, waiter)
	return nil
}

// Cancel removes a waiter matching the given owner and range.
//
// This is called when NLM_CANCEL is received. It finds the waiter by matching
// owner ID, offset, and length, marks it as cancelled, and removes it from the queue.
//
// Parameters:
//   - fileHandle: String key identifying the file
//   - ownerID: Owner ID of the pending request
//   - offset: Starting byte offset of the pending lock
//   - length: Number of bytes of the pending lock
//
// Returns true if a waiter was found and cancelled, false if no match found.
//
// Thread safety: Safe to call concurrently.
func (bq *BlockingQueue) Cancel(fileHandle string, ownerID string, offset, length uint64) bool {
	bq.mu.Lock()
	defer bq.mu.Unlock()

	queue := bq.queues[fileHandle]
	for i, w := range queue {
		if w.Lock.Owner.OwnerID == ownerID &&
			w.Lock.Offset == offset &&
			w.Lock.Length == length {
			// Mark as cancelled
			w.Cancel()
			// Remove from queue
			bq.queues[fileHandle] = append(queue[:i], queue[i+1:]...)
			if len(bq.queues[fileHandle]) == 0 {
				delete(bq.queues, fileHandle)
			}
			return true
		}
	}
	return false
}

// GetWaiters returns a copy of all waiters for a file.
//
// This is called by the unlock path to try granting locks to pending waiters.
// Returns a copy so the caller can iterate safely while the queue may be modified.
// Waiters are returned in FIFO order (oldest first).
//
// Parameters:
//   - fileHandle: String key identifying the file
//
// Returns a copy of the waiters slice, or nil if no waiters for this file.
//
// Thread safety: Safe to call concurrently.
func (bq *BlockingQueue) GetWaiters(fileHandle string) []*Waiter {
	bq.mu.RLock()
	defer bq.mu.RUnlock()

	queue := bq.queues[fileHandle]
	if len(queue) == 0 {
		return nil
	}

	// Return a copy to avoid races
	result := make([]*Waiter, len(queue))
	copy(result, queue)
	return result
}

// RemoveWaiter removes a specific waiter from the queue.
//
// This is called after successfully granting a lock to the waiter.
// Uses pointer comparison to identify the waiter.
//
// Parameters:
//   - fileHandle: String key identifying the file
//   - waiter: The waiter to remove (matched by pointer)
//
// Thread safety: Safe to call concurrently.
func (bq *BlockingQueue) RemoveWaiter(fileHandle string, waiter *Waiter) {
	bq.mu.Lock()
	defer bq.mu.Unlock()

	queue := bq.queues[fileHandle]
	for i, w := range queue {
		if w == waiter {
			bq.queues[fileHandle] = append(queue[:i], queue[i+1:]...)
			if len(bq.queues[fileHandle]) == 0 {
				delete(bq.queues, fileHandle)
			}
			return
		}
	}
}

// TotalWaiters returns the total number of waiters across all files.
//
// This is used for metrics (nlm_blocking_queue_size gauge).
//
// Thread safety: Safe to call concurrently.
func (bq *BlockingQueue) TotalWaiters() int {
	bq.mu.RLock()
	defer bq.mu.RUnlock()

	total := 0
	for _, queue := range bq.queues {
		total += len(queue)
	}
	return total
}

// FileCount returns the number of files with pending waiters.
//
// This is useful for debugging and metrics.
//
// Thread safety: Safe to call concurrently.
func (bq *BlockingQueue) FileCount() int {
	bq.mu.RLock()
	defer bq.mu.RUnlock()
	return len(bq.queues)
}

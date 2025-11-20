// Package memory implements in-memory content storage for DittoFS.
//
// This file contains batch operations for the memory content store, including
// listing all content and batch deletion for garbage collection.
package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// GarbageCollectableStore Interface Implementation
// ============================================================================

// ListAllContent returns all content IDs stored in memory.
//
// This returns a snapshot of all ContentIDs at the time of the call.
// The list may become stale if content is added/removed concurrently.
//
// Context Cancellation:
// Checked before and during iteration. For large stores, we check
// periodically during iteration.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - []metadata.ContentID: List of all content IDs
//   - error: Returns error for context cancellation
func (s *MemoryContentStore) ListAllContent(ctx context.Context) ([]metadata.ContentID, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Acquire read lock and build list
	// ========================================================================

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Pre-allocate slice with exact capacity
	contentIDs := make([]metadata.ContentID, 0, len(s.data))

	i := 0
	for id := range s.data {
		// Check context periodically (every 100 items)
		if i%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		contentIDs = append(contentIDs, id)
		i++
	}

	return contentIDs, nil
}

// DeleteBatch removes multiple content items in one operation.
//
// For in-memory storage, this performs deletions atomically under a
// single write lock. The operation is best-effort - partial failures
// are allowed and returned in the map.
//
// Context Cancellation:
// Checked before acquiring lock and periodically during iteration.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - ids: Content identifiers to delete
//
// Returns:
//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
//   - error: Only returns error for context cancellation
func (s *MemoryContentStore) DeleteBatch(ctx context.Context, ids []metadata.ContentID) (map[metadata.ContentID]error, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Acquire write lock and delete batch
	// ========================================================================

	s.mu.Lock()
	defer s.mu.Unlock()

	failures := make(map[metadata.ContentID]error)

	for i, id := range ids {
		// Check context periodically (every 100 deletions)
		if i%100 == 0 {
			if err := ctx.Err(); err != nil {
				// Context cancelled - mark remaining as failed
				for j := i; j < len(ids); j++ {
					failures[ids[j]] = ctx.Err()
				}
				return failures, ctx.Err()
			}
		}

		// Delete is idempotent, so no error checking needed
		delete(s.data, id)
	}

	return failures, nil
}

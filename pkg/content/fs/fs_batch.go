// Package fs implements filesystem-based content storage for DittoFS.
//
// This file contains batch operations for the filesystem content store, including
// listing all content and batch deletion for garbage collection.
package fs

import (
	"context"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// GarbageCollectableStore Interface Implementation
// ============================================================================

// ListAllContent returns all content IDs stored in the filesystem.
//
// This implements the GarbageCollectableStore.ListAllContent interface method.
//
// This scans the base directory and returns all file names as ContentIDs.
// For large stores with many files, this may be slow and consume memory.
//
// Context Cancellation:
// This operation checks context periodically during directory scanning.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//
// Returns:
//   - []metadata.ContentID: List of all content IDs
//   - error: Returns error for context cancellation or filesystem failures
func (r *FSContentStore) ListAllContent(ctx context.Context) ([]metadata.ContentID, error) {
	// ========================================================================
	// Step 1: Check context before filesystem operation
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ========================================================================
	// Step 2: Read directory entries
	// ========================================================================

	entries, err := os.ReadDir(r.basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read content directory: %w", err)
	}

	// ========================================================================
	// Step 3: Build list of content IDs
	// ========================================================================

	contentIDs := make([]metadata.ContentID, 0, len(entries))

	for _, entry := range entries {
		// Check context periodically (every 100 entries)
		if len(contentIDs)%100 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		// Skip directories, only include regular files
		if !entry.IsDir() {
			contentIDs = append(contentIDs, metadata.ContentID(entry.Name()))
		}
	}

	return contentIDs, nil
}

// DeleteBatch removes multiple content items in one operation.
//
// This implements the GarbageCollectableStore.DeleteBatch interface method.
//
// For filesystem storage, this performs deletions sequentially. The operation
// is best-effort - partial failures are allowed and returned in the map.
//
// Context Cancellation:
// This operation checks context periodically during batch deletion.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - ids: Content identifiers to delete
//
// Returns:
//   - map[metadata.ContentID]error: Map of failed deletions (empty = all succeeded)
//   - error: Only returns error for context cancellation
func (r *FSContentStore) DeleteBatch(ctx context.Context, ids []metadata.ContentID) (map[metadata.ContentID]error, error) {
	failures := make(map[metadata.ContentID]error)

	for i, id := range ids {
		// Check context periodically (every 10 deletions)
		if i%10 == 0 {
			if err := ctx.Err(); err != nil {
				// Context cancelled - mark remaining as failed
				for j := i; j < len(ids); j++ {
					failures[ids[j]] = ctx.Err()
				}
				return failures, ctx.Err()
			}
		}

		// Attempt to delete
		if err := r.Delete(ctx, id); err != nil {
			failures[id] = err
		}
	}

	return failures, nil
}

package memory

import (
	"context"
	"fmt"
	"maps"

	"github.com/marmos91/dittofs/internal/metadata"
)

// SetParent establishes the parent-child relationship for a file handle.
//
// This records that a file or directory exists within a specific parent
// directory. This relationship is used for:
//   - Path traversal (".." navigation)
//   - Permission inheritance checks
//   - Reference counting (future implementations)
//
// Context Cancellation:
// This lightweight operation checks the context before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - child: The file handle of the child
//   - parent: The file handle of the parent directory
//
// Returns:
//   - error: Returns nil on success, or context.Canceled/context.DeadlineExceeded
//     if context is cancelled
func (r *MemoryRepository) SetParent(ctx context.Context, child metadata.FileHandle, parent metadata.FileHandle) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// ========================================================================
	// Step 2: Establish parent-child relationship
	// ========================================================================

	r.parents[handleToKey(child)] = parent
	return nil
}

// GetParent retrieves the parent directory handle for a file.
//
// This is used for:
//   - Implementing ".." directory entries
//   - Validating rename operations (checking cross-directory moves)
//   - Permission checks that need parent context
//
// Context Cancellation:
// This lightweight operation checks the context before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - child: The file handle to look up
//
// Returns:
//   - FileHandle: The parent directory handle
//   - error: Returns error if parent not found, or context.Canceled/
//     context.DeadlineExceeded if context is cancelled
func (r *MemoryRepository) GetParent(ctx context.Context, child metadata.FileHandle) (metadata.FileHandle, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 2: Look up parent handle
	// ========================================================================

	parent, exists := r.parents[handleToKey(child)]
	if !exists {
		return nil, fmt.Errorf("parent not found")
	}

	return parent, nil
}

// AddChild adds a named entry to a directory.
//
// This creates the directory entry that maps a filename to a file handle.
// This is how files "appear" in directory listings.
//
// The operation will fail if:
//   - The name already exists in the directory
//   - The parent is not a directory (checked by caller)
//
// Context Cancellation:
// This lightweight operation checks the context before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - parent: Handle of the parent directory
//   - name: Name for the directory entry
//   - child: Handle of the file to add
//
// Returns:
//   - error: Returns error if name already exists, or context.Canceled/
//     context.DeadlineExceeded if context is cancelled
func (r *MemoryRepository) AddChild(ctx context.Context, parent metadata.FileHandle, name string, child metadata.FileHandle) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// ========================================================================
	// Step 2: Initialize parent's children map if needed
	// ========================================================================

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	// ========================================================================
	// Step 3: Check for duplicate name
	// ========================================================================

	if _, exists := r.children[parentKey][name]; exists {
		return fmt.Errorf("child already exists: %s", name)
	}

	// ========================================================================
	// Step 4: Add child to directory
	// ========================================================================

	r.children[parentKey][name] = child
	return nil
}

// GetChild looks up a file handle by name within a directory.
//
// This is the core operation for path resolution - it translates a
// filename into its corresponding file handle.
//
// Used by:
//   - LOOKUP procedure (primary use case)
//   - Path traversal operations
//   - Name existence checks
//
// Context Cancellation:
// This lightweight operation checks the context before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - parent: Handle of the parent directory
//   - name: Name to look up
//
// Returns:
//   - FileHandle: The child's file handle
//   - error: Returns error if parent has no children or name not found, or
//     context.Canceled/context.DeadlineExceeded if context is cancelled
func (r *MemoryRepository) GetChild(ctx context.Context, parent metadata.FileHandle, name string) (metadata.FileHandle, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 2: Look up child by name
	// ========================================================================

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return nil, fmt.Errorf("parent has no children")
	}

	child, exists := r.children[parentKey][name]
	if !exists {
		return nil, fmt.Errorf("child not found: %s", name)
	}

	return child, nil
}

// GetChildren returns all directory entries for a directory.
//
// This returns a complete mapping of names to file handles for all
// files in the directory. This is used by:
//   - READDIR/READDIRPLUS procedures
//   - Directory validation operations
//   - Debugging and diagnostics
//
// The returned map is a copy to prevent concurrent modification issues.
// Changes to the returned map do not affect the repository.
//
// Context Cancellation:
// This operation checks the context before acquiring the lock and during
// the map copy operation if the directory is large (>1000 entries).
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - parent: Handle of the directory
//
// Returns:
//   - map[string]FileHandle: Copy of all child name-handle mappings
//   - error: Returns nil on success (empty map if no children), or
//     context.Canceled/context.DeadlineExceeded if context is cancelled
func (r *MemoryRepository) GetChildren(ctx context.Context, parent metadata.FileHandle) (map[string]metadata.FileHandle, error) {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 2: Get children map
	// ========================================================================

	parentKey := handleToKey(parent)
	children := r.children[parentKey]
	if children == nil {
		return make(map[string]metadata.FileHandle), nil
	}

	// ========================================================================
	// Step 3: Copy children map to avoid concurrent access issues
	// ========================================================================

	// For very large directories (>1000 entries), check context during copy
	if len(children) > 1000 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	// Return a copy to avoid concurrent access issues
	result := make(map[string]metadata.FileHandle, len(children))
	maps.Copy(result, children)

	return result, nil
}

// DeleteChild removes a named entry from a directory.
//
// This removes the directory entry but does NOT:
//   - Delete the file's metadata
//   - Update the file's parent pointer
//   - Update directory timestamps
//
// This is a low-level operation. For file deletion, use RemoveFile which
// performs complete cleanup.
//
// Context Cancellation:
// This lightweight operation checks the context before acquiring the lock.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - parent: Handle of the parent directory
//   - name: Name of the entry to remove
//
// Returns:
//   - error: Returns error if parent has no children or name not found, or
//     context.Canceled/context.DeadlineExceeded if context is cancelled
func (r *MemoryRepository) DeleteChild(ctx context.Context, parent metadata.FileHandle, name string) error {
	// ========================================================================
	// Step 1: Check context before acquiring lock
	// ========================================================================

	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// ========================================================================
	// Step 2: Validate parent has children
	// ========================================================================

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return fmt.Errorf("parent has no children")
	}

	// ========================================================================
	// Step 3: Check child exists
	// ========================================================================

	if _, exists := r.children[parentKey][name]; !exists {
		return fmt.Errorf("child not found: %s", name)
	}

	// ========================================================================
	// Step 4: Remove child entry
	// ========================================================================

	delete(r.children[parentKey], name)
	return nil
}

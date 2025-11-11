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
// Parameters:
//   - child: The file handle of the child
//   - parent: The file handle of the parent directory
//
// Returns:
//   - error: Always returns nil (reserved for future validation)
func (r *MemoryRepository) SetParent(ctx context.Context, child metadata.FileHandle, parent metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

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
// Parameters:
//   - child: The file handle to look up
//
// Returns:
//   - FileHandle: The parent directory handle
//   - error: Returns error if parent not found
func (r *MemoryRepository) GetParent(ctx context.Context, child metadata.FileHandle) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

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
// Parameters:
//   - parent: Handle of the parent directory
//   - name: Name for the directory entry
//   - child: Handle of the file to add
//
// Returns:
//   - error: Returns error if name already exists
func (r *MemoryRepository) AddChild(ctx context.Context, parent metadata.FileHandle, name string, child metadata.FileHandle) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		r.children[parentKey] = make(map[string]metadata.FileHandle)
	}

	if _, exists := r.children[parentKey][name]; exists {
		return fmt.Errorf("child already exists: %s", name)
	}

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
// Parameters:
//   - parent: Handle of the parent directory
//   - name: Name to look up
//
// Returns:
//   - FileHandle: The child's file handle
//   - error: Returns error if parent has no children or name not found
func (r *MemoryRepository) GetChild(ctx context.Context, parent metadata.FileHandle, name string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

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
// Parameters:
//   - parent: Handle of the directory
//
// Returns:
//   - map[string]FileHandle: Copy of all child name-handle mappings
//   - error: Always returns nil, empty map if no children
func (r *MemoryRepository) GetChildren(ctx context.Context, parent metadata.FileHandle) (map[string]metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parentKey := handleToKey(parent)
	children := r.children[parentKey]
	if children == nil {
		return make(map[string]metadata.FileHandle), nil
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
// Parameters:
//   - parent: Handle of the parent directory
//   - name: Name of the entry to remove
//
// Returns:
//   - error: Returns error if parent has no children or name not found
func (r *MemoryRepository) DeleteChild(ctx context.Context, parent metadata.FileHandle, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	parentKey := handleToKey(parent)
	if r.children[parentKey] == nil {
		return fmt.Errorf("parent has no children")
	}

	if _, exists := r.children[parentKey][name]; !exists {
		return fmt.Errorf("child not found: %s", name)
	}

	delete(r.children[parentKey], name)
	return nil
}

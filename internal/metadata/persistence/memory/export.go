package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/metadata"
)

// AddExport adds a new export to the repository with the specified configuration.
//
// This creates a new exported filesystem with:
//   - A root directory handle
//   - Configuration options (read-only, auth requirements, etc.)
//   - Initial root directory attributes
//
// The root directory is automatically created and initialized with an empty
// children map.
//
// Parameters:
//   - path: The export path (e.g., "/export/data")
//   - options: Export configuration (access control, auth, etc.)
//   - rootAttr: Attributes for the root directory
//
// Returns:
//   - error: Returns error if export already exists
func (r *MemoryRepository) AddExport(ctx context.Context, path string, options metadata.ExportOptions, rootAttr *metadata.FileAttr) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if export already exists
	if _, exists := r.exports[path]; exists {
		return fmt.Errorf("export already exists: %s", path)
	}

	// Generate root handle
	rootHandle := r.generateFileHandle(path)
	key := handleToKey(rootHandle)

	// Store root attributes
	r.files[key] = rootAttr

	// Initialize empty children map for the root directory
	r.children[key] = make(map[string]metadata.FileHandle)

	// Store export data
	r.exports[path] = &exportData{
		Export: metadata.Export{
			Path:    path,
			Options: options,
		},
		RootHandle: rootHandle,
	}

	return nil
}

// GetExports returns a list of all configured exports.
//
// This retrieves all export configurations without any filtering.
// The returned list includes all exports regardless of mount status
// or access restrictions.
//
// Returns:
//   - []Export: List of all export configurations
//   - error: Always returns nil (reserved for future use)
func (r *MemoryRepository) GetExports(ctx context.Context) ([]metadata.Export, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]metadata.Export, 0, len(r.exports))
	for _, ed := range r.exports {
		result = append(result, ed.Export)
	}
	return result, nil
}

// FindExport looks up an export by its path.
//
// This is used to retrieve export configuration for mount operations
// and access control checks.
//
// Parameters:
//   - path: The export path to look up
//
// Returns:
//   - *Export: The export configuration
//   - error: Returns error if export not found
func (r *MemoryRepository) FindExport(ctx context.Context, path string) (*metadata.Export, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ed, exists := r.exports[path]
	if !exists {
		return nil, fmt.Errorf("export not found: %s", path)
	}
	return &ed.Export, nil
}

// GetRootHandle returns the root directory handle for an export.
//
// This is the entry point for all filesystem operations on an export.
// The root handle is used as the starting point for path traversal
// and as the parent for top-level directories.
//
// Parameters:
//   - exportPath: The path of the export
//
// Returns:
//   - FileHandle: The root directory handle
//   - error: Returns error if export not found
func (r *MemoryRepository) GetRootHandle(ctx context.Context, exportPath string) (metadata.FileHandle, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ed, exists := r.exports[exportPath]
	if !exists {
		return nil, fmt.Errorf("export not found: %s", exportPath)
	}
	return ed.RootHandle, nil
}

// DeleteExport removes an export from the repository.
//
// WARNING: This only removes the export configuration. It does NOT:
//   - Remove the exported files and directories
//   - Unmount active client connections
//   - Clean up mount tracking records
//
// Callers should ensure proper cleanup before deleting exports.
//
// Parameters:
//   - path: The export path to delete
//
// Returns:
//   - error: Returns error if export not found
func (r *MemoryRepository) DeleteExport(ctx context.Context, path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.exports[path]; !exists {
		return fmt.Errorf("export not found: %s", path)
	}

	delete(r.exports, path)
	return nil
}

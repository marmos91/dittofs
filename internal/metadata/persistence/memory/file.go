package memory

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/metadata"
)

// CreateFile creates a new file entry with the specified attributes.
//
// This is a low-level operation that:
//   - Stores the file attributes under the given handle
//   - Initializes a children map if the file is a directory
//   - Does NOT link the file to any parent directory
//
// For creating files within directories, prefer using AddFileToDirectory
// or higher-level operations like CreateDirectory, CreateSpecialFile, etc.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - handle: The file handle (must be unique)
//   - attr: Complete file attributes
//
// Returns:
//   - error: Returns error if handle already exists or context is cancelled
func (r *MemoryRepository) CreateFile(ctx context.Context, handle metadata.FileHandle, attr *metadata.FileAttr) error {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before creating file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled while creating file: %w", err)
	}

	key := handleToKey(handle)
	if _, exists := r.files[key]; exists {
		return fmt.Errorf("file already exists")
	}

	r.files[key] = attr

	// If it's a directory, initialize children map
	if attr.Type == metadata.FileTypeDirectory {
		r.children[key] = make(map[string]metadata.FileHandle)
	}

	return nil
}

// GetFile retrieves file attributes by handle.
//
// This is the primary method for reading file metadata including:
//   - Type (file, directory, symlink, etc.)
//   - Permissions (mode, uid, gid)
//   - Size and timestamps
//   - Content ID for data retrieval
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - handle: The file handle to look up
//
// Returns:
//   - *metadata.FileAttr: The file attributes
//   - error: Returns error if file not found or context is cancelled
func (r *MemoryRepository) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before getting file: %w", err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Check context after acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled while getting file: %w", err)
	}

	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return nil, fmt.Errorf("file not found")
	}

	return attr, nil
}

// UpdateFile replaces file attributes with new values.
//
// This is used for operations that modify file metadata:
//   - SETATTR (change permissions, timestamps, size)
//   - WRITE (update mtime, size)
//   - Internal operations (update ctime after metadata changes)
//
// The caller is responsible for:
//   - Checking permissions before updates
//   - Ensuring attribute consistency
//   - Updating timestamps appropriately
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - handle: The file handle to update
//   - attr: New complete attributes
//
// Returns:
//   - error: Returns error if file not found or context is cancelled
func (r *MemoryRepository) UpdateFile(ctx context.Context, handle metadata.FileHandle, attr *metadata.FileAttr) error {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before updating file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled while updating file: %w", err)
	}

	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return fmt.Errorf("file not found")
	}

	r.files[key] = attr
	return nil
}

// DeleteFile deletes file metadata by handle.
//
// WARNING: This is a low-level operation that bypasses all safety checks.
// It does NOT:
//   - Check permissions
//   - Remove the file from its parent directory
//   - Update parent directory timestamps
//   - Verify the file can be safely deleted
//
// In most cases, you should use RemoveFile instead, which performs
// proper validation and cleanup.
//
// This method should only be used for:
//   - Internal cleanup operations
//   - Orphaned handle garbage collection
//   - Operations that have already performed all necessary checks
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - handle: The file handle to delete
//
// Returns:
//   - error: Returns error if file not found or context is cancelled
func (r *MemoryRepository) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled before deleting file: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check context after acquiring lock
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context cancelled while deleting file: %w", err)
	}

	key := handleToKey(handle)
	if _, exists := r.files[key]; !exists {
		return fmt.Errorf("file not found")
	}

	delete(r.files, key)
	return nil
}

// AddFileToDirectory is a convenience method that combines file creation
// with directory linking.
//
// This performs three operations atomically:
//  1. Creates the file with a generated handle
//  2. Adds it as a child of the parent directory
//  3. Sets up the parent-child relationship
//
// This is useful for protocol handlers that need to create files in a
// single operation without managing handles manually.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new file
//   - attr: Complete file attributes
//
// Returns:
//   - FileHandle: The generated handle for the new file
//   - error: Returns error if any operation fails or context is cancelled
func (r *MemoryRepository) AddFileToDirectory(ctx context.Context, parentHandle metadata.FileHandle, name string, attr *metadata.FileAttr) (metadata.FileHandle, error) {
	// Check context before starting multi-step operation
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("context cancelled before adding file to directory: %w", err)
	}

	fileHandle := r.generateFileHandle(name)

	if err := r.CreateFile(ctx, fileHandle, attr); err != nil {
		return nil, err
	}

	if err := r.AddChild(ctx, parentHandle, name, fileHandle); err != nil {
		return nil, err
	}

	if err := r.SetParent(ctx, fileHandle, parentHandle); err != nil {
		return nil, err
	}

	return fileHandle, nil
}

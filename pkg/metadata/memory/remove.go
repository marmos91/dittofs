package memory

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// RemoveFile removes a file's metadata from its parent directory.
//
// This performs metadata cleanup including permission validation, type checking,
// and directory entry removal. The file's content data is NOT deleted by this
// method - the caller must coordinate content deletion with the content repository
// using the returned ContentID.
//
// Hard Links:
// If the file has multiple hard links (linkCount > 1), this removes only one link.
// The caller should only delete content when the last link is removed (when the
// returned FileAttr shows this was the last link).
//
// Recommended pattern:
//
//	attr, err := repo.RemoveFile(authCtx, parentHandle, filename)
//	if err != nil {
//	    return err
//	}
//	// Check if this was the last link before deleting content
//	// (Implementation would need to track link count in returned attr or separately)
//	if attr.ContentID != "" {
//	    err = contentRepo.Delete(ctx, attr.ContentID)
//	    // Handle error...
//	}
func (store *MemoryMetadataStore) RemoveFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate name
	if name == "" || name == "." || name == ".." {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify parent exists and is a directory
	parentKey := handleToKey(parentHandle)
	parentAttr, exists := store.files[parentKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent directory not found",
		}
	}
	if parentAttr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "parent is not a directory",
		}
	}

	// Check write permission on parent
	granted, err := store.checkPermissionsLocked(ctx, parentHandle, metadata.PermissionWrite)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionWrite == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
			Message: "no write permission on parent directory",
		}
	}

	// Get parent's children
	childrenMap, hasChildren := store.children[parentKey]
	if !hasChildren {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("file not found: %s", name),
			Path:    name,
		}
	}

	// Find the file
	fileHandle, exists := childrenMap[name]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("file not found: %s", name),
			Path:    name,
		}
	}

	// Get file attributes
	fileKey := handleToKey(fileHandle)
	fileAttr, exists := store.files[fileKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Verify it's not a directory
	if fileAttr.Type == metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
			Path:    name,
		}
	}

	// Make a copy of attributes to return (before we delete the file)
	returnAttr := &metadata.FileAttr{
		Type:       fileAttr.Type,
		Mode:       fileAttr.Mode,
		UID:        fileAttr.UID,
		GID:        fileAttr.GID,
		Size:       fileAttr.Size,
		Atime:      fileAttr.Atime,
		Mtime:      fileAttr.Mtime,
		Ctime:      fileAttr.Ctime,
		ContentID:  fileAttr.ContentID,
		LinkTarget: fileAttr.LinkTarget,
	}

	// Decrement link count
	linkCount := store.linkCounts[fileKey]
	if linkCount > 1 {
		// File has other hard links, just decrement count
		store.linkCounts[fileKey]--
	} else {
		// This was the last link, remove all metadata
		delete(store.files, fileKey)
		delete(store.linkCounts, fileKey)
		delete(store.parents, fileKey)
		// Note: File doesn't have children (it's not a directory)
	}

	// Remove from parent's children
	delete(childrenMap, name)

	// Update parent timestamps
	now := parentAttr.Mtime.Add(1) // Ensure time moves forward
	parentAttr.Mtime = now
	parentAttr.Ctime = now

	return returnAttr, nil
}

// RemoveDirectory removes an empty directory's metadata from its parent.
//
// This performs metadata cleanup including permission validation, type checking,
// empty check, and directory entry removal. The method does NOT delete any
// associated content data (directories typically don't have content).
//
// Empty Directory:
// A directory is considered empty if it has no children (the children map is empty).
// The special entries "." and ".." are implicit and don't count as children.
//
// Link Counts:
// Removing a directory decrements the parent directory's link count (because the
// removed directory's ".." entry pointed to the parent).
func (store *MemoryMetadataStore) RemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate name
	if name == "" || name == "." || name == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify parent exists and is a directory
	parentKey := handleToKey(parentHandle)
	parentAttr, exists := store.files[parentKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent directory not found",
		}
	}
	if parentAttr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "parent is not a directory",
		}
	}

	// Check write permission on parent
	granted, err := store.checkPermissionsLocked(ctx, parentHandle, metadata.PermissionWrite)
	if err != nil {
		return err
	}
	if granted&metadata.PermissionWrite == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
			Message: "no write permission on parent directory",
		}
	}

	// Get parent's children
	childrenMap, hasChildren := store.children[parentKey]
	if !hasChildren {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("directory not found: %s", name),
			Path:    name,
		}
	}

	// Find the directory
	dirHandle, exists := childrenMap[name]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("directory not found: %s", name),
			Path:    name,
		}
	}

	// Get directory attributes
	dirKey := handleToKey(dirHandle)
	dirAttr, exists := store.files[dirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "directory not found",
		}
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
			Path:    name,
		}
	}

	// Check if directory is empty
	dirChildrenMap, hasDirChildren := store.children[dirKey]
	if hasDirChildren && len(dirChildrenMap) > 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotEmpty,
			Message: "directory not empty",
			Path:    name,
		}
	}

	// Remove directory metadata
	delete(store.files, dirKey)
	delete(store.linkCounts, dirKey)
	delete(store.parents, dirKey)
	delete(store.children, dirKey) // Remove empty children map

	// Remove from parent's children
	delete(childrenMap, name)

	// Decrement parent's link count
	// (removing a subdirectory removes one ".." reference to parent)
	if store.linkCounts[parentKey] > 0 {
		store.linkCounts[parentKey]--
	}

	// Update parent timestamps
	now := parentAttr.Mtime.Add(1) // Ensure time moves forward
	parentAttr.Mtime = now
	parentAttr.Ctime = now

	return nil
}

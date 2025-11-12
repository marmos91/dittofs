package memory

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// File/Directory Operations
// ============================================================================

// Move moves or renames a file or directory atomically.
//
// This operation can rename within the same directory or move to a different
// directory, and can atomically replace an existing file/directory at the
// destination if type-compatible.
func (store *MemoryMetadataStore) Move(
	ctx *metadata.AuthContext,
	fromDir metadata.FileHandle,
	fromName string,
	toDir metadata.FileHandle,
	toName string,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate names
	if fromName == "" || fromName == "." || fromName == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid source name",
			Path:    fromName,
		}
	}
	if toName == "" || toName == "." || toName == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid destination name",
			Path:    toName,
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Check if it's a no-op (same directory, same name)
	fromDirKey := handleToKey(fromDir)
	toDirKey := handleToKey(toDir)
	if fromDirKey == toDirKey && fromName == toName {
		// No-op, return success
		return nil
	}

	// Verify source directory exists and is a directory
	fromDirData, exists := store.files[fromDirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "source directory not found",
		}
	}
	if fromDirData.Attr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "source parent is not a directory",
		}
	}

	// Verify destination directory exists and is a directory
	toDirData, exists := store.files[toDirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "destination directory not found",
		}
	}
	if toDirData.Attr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "destination parent is not a directory",
		}
	}

	// Check write permission on source directory
	granted, err := store.checkPermissionsLocked(ctx, fromDir, metadata.PermissionWrite)
	if err != nil {
		return err
	}
	if granted&metadata.PermissionWrite == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on source directory",
		}
	}

	// Check write permission on destination directory (if different)
	if fromDirKey != toDirKey {
		granted, err := store.checkPermissionsLocked(ctx, toDir, metadata.PermissionWrite)
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on destination directory",
			}
		}
	}

	// Get source children map
	fromChildren, hasFromChildren := store.children[fromDirKey]
	if !hasFromChildren {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("source not found: %s", fromName),
			Path:    fromName,
		}
	}

	// Find source file
	sourceHandle, exists := fromChildren[fromName]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("source not found: %s", fromName),
			Path:    fromName,
		}
	}

	// Get source attributes
	sourceKey := handleToKey(sourceHandle)
	sourceData, exists := store.files[sourceKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "source file not found",
		}
	}

	// Get destination children map (create if needed)
	toChildren, hasToChildren := store.children[toDirKey]
	if !hasToChildren {
		toChildren = make(map[string]metadata.FileHandle)
		store.children[toDirKey] = toChildren
	}

	// Check if destination exists
	destHandle, destExists := toChildren[toName]
	if destExists {
		// Destination exists - check replacement rules
		destKey := handleToKey(destHandle)
		destData, exists := store.files[destKey]
		if !exists {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "destination file not found",
			}
		}

		// Check type compatibility for replacement
		if sourceData.Attr.Type == metadata.FileTypeDirectory {
			// Moving directory
			if destData.Attr.Type != metadata.FileTypeDirectory {
				return &metadata.StoreError{
					Code:    metadata.ErrNotDirectory,
					Message: "cannot replace non-directory with directory",
					Path:    toName,
				}
			}
			// Check if destination directory is empty
			destChildren, hasDestChildren := store.children[destKey]
			if hasDestChildren && len(destChildren) > 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrNotEmpty,
					Message: "destination directory not empty",
					Path:    toName,
				}
			}
		} else {
			// Moving non-directory
			if destData.Attr.Type == metadata.FileTypeDirectory {
				return &metadata.StoreError{
					Code:    metadata.ErrIsDirectory,
					Message: "cannot replace directory with non-directory",
					Path:    toName,
				}
			}
		}

		// Remove destination (will be replaced)
		// Handle link count for replaced file
		destLinkCount := store.linkCounts[destKey]

		// For directories, the normal link count is 2 ("." and parent entry)
		// For files, normal link count is 1
		// Only keep the file if it has additional hard links beyond the normal count
		var hasOtherLinks bool
		if destData.Attr.Type == metadata.FileTypeDirectory {
			hasOtherLinks = destLinkCount > 2 // More than just "." and parent
		} else {
			hasOtherLinks = destLinkCount > 1 // More than just this directory entry
		}

		if hasOtherLinks {
			// Has other links, just decrement
			store.linkCounts[destKey]--
		} else {
			// Last link (or only link for directories), remove all metadata
			delete(store.files, destKey)
			delete(store.linkCounts, destKey)
			delete(store.parents, destKey)
			if destData.Attr.Type == metadata.FileTypeDirectory {
				delete(store.children, destKey)
				// Decrement destination directory's link count (removing subdirectory)
				if store.linkCounts[toDirKey] > 0 {
					store.linkCounts[toDirKey]--
				}
			}
		}
	}

	// Perform the move
	// Remove from source directory
	delete(fromChildren, fromName)

	// Add to destination directory
	toChildren[toName] = sourceHandle

	// Update parent pointer if moving to different directory
	if fromDirKey != toDirKey {
		store.parents[sourceKey] = toDir

		// If moving a directory, update parent link counts
		if sourceData.Attr.Type == metadata.FileTypeDirectory {
			// Decrement source directory link count (losing ".." reference)
			if store.linkCounts[fromDirKey] > 0 {
				store.linkCounts[fromDirKey]--
			}
			// Increment destination directory link count (gaining ".." reference)
			store.linkCounts[toDirKey]++
		}
	}

	// Update timestamps
	now := time.Now()

	// Update source file ctime (metadata changed - new parent/name)
	sourceData.Attr.Ctime = now

	// Update source directory (contents changed)
	fromDirData.Attr.Mtime = now
	fromDirData.Attr.Ctime = now

	// Update destination directory if different (contents changed)
	if fromDirKey != toDirKey {
		toDirData.Attr.Mtime = now
		toDirData.Attr.Ctime = now
	}

	return nil
}

// ReadSymlink reads the target path of a symbolic link.
//
// This returns the path stored in the symlink without following it or validating
// that the target exists. Also returns the symlink's attributes for cache consistency.
func (store *MemoryMetadataStore) ReadSymlink(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (string, *metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return "", nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Get file data
	key := handleToKey(handle)
	fileData, exists := store.files[key]
	if !exists {
		return "", nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Verify it's a symlink
	if fileData.Attr.Type != metadata.FileTypeSymlink {
		return "", nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "not a symbolic link",
		}
	}

	// Check read permission on the symlink
	granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionRead)
	if err != nil {
		return "", nil, err
	}
	if granted&metadata.PermissionRead == 0 {
		return "", nil, &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no read permission on symlink",
		}
	}

	// Return target and attributes
	return fileData.Attr.LinkTarget, fileData.Attr, nil
}

// ReadDirectory reads one page of directory entries with pagination support.
//
// Pagination uses opaque tokens:
//   - Start with token="" to read from beginning
//   - Use page.NextToken for subsequent pages
//   - page.NextToken="" indicates no more pages
//   - page.HasMore is a convenience flag
//
// The token format is implementation-specific (offset-based in this implementation).
func (store *MemoryMetadataStore) ReadDirectory(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	token string,
	maxBytes uint32,
) (*metadata.ReadDirPage, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Get directory data
	dirKey := handleToKey(dirHandle)
	dirData, exists := store.files[dirKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "directory not found",
		}
	}

	// Verify it's a directory
	if dirData.Attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Check read and execute permissions
	granted, err := store.checkPermissionsLocked(ctx, dirHandle,
		metadata.PermissionRead|metadata.PermissionTraverse)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionRead == 0 || granted&metadata.PermissionTraverse == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no read or execute permission on directory",
		}
	}

	// Parse and validate token before checking directory contents
	// This ensures invalid tokens are rejected even for empty directories
	offset := 0
	if token != "" {
		parsedOffset, err := strconv.Atoi(token)
		if err != nil {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "invalid pagination token",
			}
		}
		offset = parsedOffset
	}

	// Get children
	childrenMap, hasChildren := store.children[dirKey]
	if !hasChildren || len(childrenMap) == 0 {
		// Empty directory
		return &metadata.ReadDirPage{
			Entries:   []metadata.DirEntry{},
			NextToken: "",
			HasMore:   false,
		}, nil
	}

	// Convert children map to sorted slice for stable ordering
	type namedHandle struct {
		name   string
		handle metadata.FileHandle
	}

	allChildren := make([]namedHandle, 0, len(childrenMap))
	for name, handle := range childrenMap {
		allChildren = append(allChildren, namedHandle{name: name, handle: handle})
	}

	// Sort by name for stable ordering
	sort.Slice(allChildren, func(i, j int) bool {
		return allChildren[i].name < allChildren[j].name
	})

	// Check if offset is valid
	if offset >= len(allChildren) {
		// Past the end, return empty
		return &metadata.ReadDirPage{
			Entries:   []metadata.DirEntry{},
			NextToken: "",
			HasMore:   false,
		}, nil
	}

	// Default maxBytes if not specified
	if maxBytes == 0 {
		maxBytes = 8192
	}

	// Estimate bytes per entry (rough estimate)
	// Name + ID (uint64) ≈ len(name) + 8 + overhead
	const estimatedOverheadPerEntry = 50

	// Build entries for this page
	entries := []metadata.DirEntry{}
	currentBytes := uint32(0)
	currentIndex := offset

	for currentIndex < len(allChildren) {
		child := allChildren[currentIndex]

		// Extract file ID from handle
		fileID := extractFileIDFromHandle(child.handle)

		// Estimate entry size
		entrySize := uint32(len(child.name)) + 8 + estimatedOverheadPerEntry

		// Check if adding this entry would exceed maxBytes
		if len(entries) > 0 && currentBytes+entrySize > maxBytes {
			// Would exceed limit, stop here
			break
		}

		// Add entry
		entries = append(entries, metadata.DirEntry{
			ID:   fileID,
			Name: child.name,
		})

		currentBytes += entrySize
		currentIndex++
	}

	// Determine if there are more entries
	hasMore := currentIndex < len(allChildren)
	nextToken := ""
	if hasMore {
		nextToken = strconv.Itoa(currentIndex)
	}

	return &metadata.ReadDirPage{
		Entries:   entries,
		NextToken: nextToken,
		HasMore:   hasMore,
	}, nil
}

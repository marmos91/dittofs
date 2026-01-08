package memory

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// File/Directory Lookup and Attributes
// ============================================================================

// Lookup resolves a name within a directory to a file handle and attributes.
//
// This is the fundamental operation for path resolution, combining directory
// search, permission checking, and attribute retrieval into a single atomic
// operation.
func (store *MemoryMetadataStore) Lookup(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
) (*metadata.File, error) {
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

	// Check execute/traverse permission on directory (search permission)
	granted, err := store.checkPermissionsLocked(ctx, dirHandle, metadata.PermissionTraverse)
	if err != nil {
		return nil, err
	}
	if granted&metadata.PermissionTraverse == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no search permission on directory",
		}
	}

	// Handle special names
	var targetHandle metadata.FileHandle

	switch name {
	case ".":
		// Current directory - return directory itself
		targetHandle = dirHandle

	case "..":
		// Parent directory
		parentHandle, hasParent := store.parents[dirKey]
		if !hasParent {
			// This is a root directory, ".." refers to itself
			targetHandle = dirHandle
		} else {
			targetHandle = parentHandle
		}

	default:
		// Regular name lookup
		childrenMap, hasChildren := store.children[dirKey]
		if !hasChildren {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("name not found: %s", name),
				Path:    name,
			}
		}

		childHandle, exists := childrenMap[name]
		if !exists {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("name not found: %s", name),
				Path:    name,
			}
		}
		targetHandle = childHandle
	}

	// Get target data
	targetKey := handleToKey(targetHandle)
	targetData, exists := store.files[targetKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "target file not found",
		}
	}

	// Return full File information with link count
	return store.buildFileWithNlink(targetHandle, targetData)
}

// GetFile retrieves file attributes by handle.
//
// This is a lightweight operation that only reads metadata without permission
// checking. It's used for operations where permission checking has already been
// performed or is not required (e.g., getting attributes after successful lookup).
//
// For operations requiring permission checking, use Lookup or PrepareRead instead.
//
// Use Cases:
//   - Getting attributes after successful Lookup (permission already checked)
//   - Protocol operations that work on file handles (e.g., GETATTR in NFS)
//   - Internal operations that need file metadata
//   - Cache updates after modifications
//
// Thread Safety:
// This method uses a read lock and is safe for concurrent access.
func (s *MemoryMetadataStore) GetFile(
	ctx context.Context,
	handle metadata.FileHandle,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Check for invalid (empty) handle
	if len(handle) == 0 {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get file data
	key := handleToKey(handle)
	fileData, exists := s.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Return full File information with link count
	return s.buildFileWithNlink(handle, fileData)
}

// GetShareNameForHandle returns the share name for a given file handle.
//
// This works with both path-based and hash-based handles by looking up the
// file metadata which contains the ShareName field.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle (path-based or hash-based)
//
// Returns:
//   - string: Share name the file belongs to
//   - error: ErrNotFound if handle is invalid, context errors
func (s *MemoryMetadataStore) GetShareNameForHandle(
	ctx context.Context,
	handle metadata.FileHandle,
) (string, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Check for invalid (empty) handle
	if len(handle) == 0 {
		return "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get file data
	key := handleToKey(handle)
	fileData, exists := s.files[key]
	if !exists {
		return "", &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	return fileData.ShareName, nil
}

// SetFileAttributes updates file attributes with validation and access control.
//
// Only attributes with non-nil pointers in attrs are modified.
// Other attributes remain unchanged.
func (store *MemoryMetadataStore) SetFileAttributes(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	attrs *metadata.SetAttrs,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Get file data
	key := handleToKey(handle)
	fileData, exists := store.files[key]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	attr := fileData.Attr // Get the actual attributes

	identity := ctx.Identity
	if identity == nil || identity.UID == nil {
		return &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "authentication required to modify attributes",
		}
	}

	uid := *identity.UID
	isOwner := uid == attr.UID
	isRoot := uid == 0

	// Track if any changes were made (for ctime update)
	changed := false

	// Validate and apply mode changes
	if attrs.Mode != nil {
		// Only owner or root can change mode (EPERM per POSIX)
		// This is a privilege check, not a Unix permission check
		if !isOwner && !isRoot {
			return &metadata.StoreError{
				Code:    metadata.ErrPrivilegeRequired,
				Message: "only owner or root can change mode",
			}
		}

		// Validate mode (only lower 12 bits)
		newMode := *attrs.Mode & 0o7777

		// POSIX: If the calling process does not have appropriate privileges, and
		// if the group ID of the file does not match the effective group ID or one
		// of the supplementary group IDs, bit S_ISGID (set-group-ID on execution)
		// in the file's mode shall be cleared upon successful return from chmod().
		// Note: This only applies to non-root users on regular files.
		if !isRoot && attr.Type == metadata.FileTypeRegular {
			// Check if user is a member of the file's group
			isMemberOfFileGroup := false
			if identity.GID != nil && *identity.GID == attr.GID {
				isMemberOfFileGroup = true
			}
			if !isMemberOfFileGroup && identity.HasGID(attr.GID) {
				isMemberOfFileGroup = true
			}

			// Clear setgid bit if user is not a member of the file's group
			if !isMemberOfFileGroup {
				newMode &= ^uint32(0o2000) // Clear S_ISGID
			}
		}

		attr.Mode = newMode
		changed = true
	}

	// Validate and apply UID changes
	if attrs.UID != nil {
		newUID := *attrs.UID
		// Per POSIX chown(2): only root can change UID
		// A non-owner cannot call chown at all, even to the same values
		if !isRoot {
			// Non-root: only owner can attempt chown (for GID changes via chgrp)
			// but UID changes require root privileges
			if newUID != attr.UID {
				return &metadata.StoreError{
					Code:    metadata.ErrPrivilegeRequired,
					Message: "only root can change ownership",
				}
			}
			// Non-root, same UID: check if user is owner
			// A non-owner cannot call chown at all (EPERM), even to same values
			if !isOwner {
				return &metadata.StoreError{
					Code:    metadata.ErrPrivilegeRequired,
					Message: "only owner or root can change ownership",
				}
			}
			// Owner setting UID to same value: allowed (no-op)
		} else {
			// Root can change to any UID
			if newUID != attr.UID {
				attr.UID = newUID
				changed = true
			}
		}
	}

	// Validate and apply GID changes
	if attrs.GID != nil {
		newGID := *attrs.GID
		// Per POSIX chown(2): only owner or root can change GID
		// A non-owner cannot call chown/chgrp at all, even to the same values
		if !isRoot {
			// Non-root: must be owner to call chown/chgrp
			if !isOwner {
				return &metadata.StoreError{
					Code:    metadata.ErrPrivilegeRequired,
					Message: "only owner or root can change group",
				}
			}

			// Owner changing GID: must be member of target group (if different)
			if newGID != attr.GID {
				// Check if owner is member of target group
				isMember := false
				if identity.GID != nil && *identity.GID == newGID {
					isMember = true
				}
				if !isMember && slices.Contains(identity.GIDs, newGID) {
					isMember = true
				}

				if !isMember {
					return &metadata.StoreError{
						Code:    metadata.ErrPrivilegeRequired,
						Message: "owner must be member of target group",
					}
				}

				attr.GID = newGID
				changed = true
			}
			// Owner setting GID to same value: allowed (no-op)
		} else {
			// Root can change to any group
			if newGID != attr.GID {
				attr.GID = newGID
				changed = true
			}
		}
	}

	// POSIX: Clear setuid/setgid bits when non-root user performs chown
	// This is a security measure to prevent privilege escalation.
	// Per POSIX, ANY chown operation by a non-privileged user should clear
	// these bits, even if the ownership values don't actually change.
	// This applies to all file types except directories (and symlinks have fixed mode).
	// Directories retain setgid for inheritance semantics.
	if !isRoot && (attrs.UID != nil || attrs.GID != nil) {
		if attr.Type != metadata.FileTypeDirectory && attr.Type != metadata.FileTypeSymlink {
			attr.Mode &= ^uint32(0o6000) // Clear both setuid (04000) and setgid (02000)
			changed = true
		}
	}

	// Validate and apply size changes
	if attrs.Size != nil {
		// Cannot change size of directories or special files
		if attr.Type != metadata.FileTypeRegular {
			return &metadata.StoreError{
				Code:    metadata.ErrInvalidArgument,
				Message: "cannot change size of non-regular file",
			}
		}

		// Check write permission
		granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission",
			}
		}

		// Update size and mtime
		attr.Size = *attrs.Size
		attr.Mtime = time.Now()
		changed = true
	}

	// Apply atime changes
	// Per POSIX utimensat(2):
	// - UTIME_NOW (AtimeNow=true): owner, root, OR write permission
	// - Arbitrary time (AtimeNow=false): owner OR root ONLY
	if attrs.Atime != nil {
		if !isOwner && !isRoot {
			if attrs.AtimeNow {
				// UTIME_NOW: write permission is sufficient
				granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
				if err != nil {
					return err
				}
				if granted&metadata.PermissionWrite == 0 {
					return &metadata.StoreError{
						Code:    metadata.ErrAccessDenied,
						Message: "no permission to change atime",
					}
				}
			} else {
				// Arbitrary time: only owner or root can set (EPERM)
				return &metadata.StoreError{
					Code:    metadata.ErrPrivilegeRequired,
					Message: "only owner can set arbitrary timestamps",
				}
			}
		}

		attr.Atime = *attrs.Atime
		changed = true
	}

	// Apply mtime changes
	// Per POSIX utimensat(2):
	// - UTIME_NOW (MtimeNow=true): owner, root, OR write permission
	// - Arbitrary time (MtimeNow=false): owner OR root ONLY
	if attrs.Mtime != nil {
		if !isOwner && !isRoot {
			if attrs.MtimeNow {
				// UTIME_NOW: write permission is sufficient
				granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
				if err != nil {
					return err
				}
				if granted&metadata.PermissionWrite == 0 {
					return &metadata.StoreError{
						Code:    metadata.ErrAccessDenied,
						Message: "no permission to change mtime",
					}
				}
			} else {
				// Arbitrary time: only owner or root can set (EPERM)
				return &metadata.StoreError{
					Code:    metadata.ErrPrivilegeRequired,
					Message: "only owner can set arbitrary timestamps",
				}
			}
		}

		attr.Mtime = *attrs.Mtime
		changed = true
	}

	// Apply CreationTime changes (SMB/Windows only)
	if attrs.CreationTime != nil {
		// Check write permission or ownership
		if !isOwner && !isRoot {
			granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrAccessDenied,
					Message: "no permission to change creation time",
				}
			}
		}

		attr.CreationTime = *attrs.CreationTime
		changed = true
	}

	// Apply Hidden attribute changes (SMB/Windows only)
	// Hidden can be set by owner or anyone with write permission
	if attrs.Hidden != nil {
		if !isOwner && !isRoot {
			granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrAccessDenied,
					Message: "no permission to change hidden attribute",
				}
			}
		}

		attr.Hidden = *attrs.Hidden
		changed = true
	}

	// Update ctime if any changes were made
	if changed {
		attr.Ctime = time.Now()
	}

	return nil
}

// ============================================================================
// File/Directory Creation
// ============================================================================

// Create creates a new file or directory.
//
// The type is determined by attr.Type (must be FileTypeRegular or FileTypeDirectory).
func (store *MemoryMetadataStore) Create(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate type
	if err := metadata.ValidateCreateType(attr.Type); err != nil {
		return nil, err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify parent exists and is a directory
	parentKey := handleToKey(parentHandle)
	parentData, exists := store.files[parentKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent directory not found",
		}
	}
	if parentData.Attr.Type != metadata.FileTypeDirectory {
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
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on parent directory",
		}
	}

	// Check if name already exists
	childrenMap, hasChildren := store.children[parentKey]
	if hasChildren {
		if _, exists := childrenMap[name]; exists {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		}
	}

	// Check storage limits
	if store.maxFiles > 0 && uint64(len(store.files)) >= store.maxFiles {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNoSpace,
			Message: "maximum file count reached",
		}
	}

	// Build full path and generate deterministic handle
	// Build full path for handle generation.
	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations receive (parent_handle, name) pairs. Deeply nested directories
	// are valid as long as each name component is <= NAME_MAX.
	fullPath := store.buildFullPath(parentHandle, name)
	handle := store.generateFileHandle(parentData.ShareName, fullPath)

	// Apply defaults for mode, UID/GID, timestamps, and size
	metadata.ApplyCreateDefaults(attr, ctx, "")

	// Complete file attributes
	newAttr := &metadata.FileAttr{
		Type:         attr.Type,
		Mode:         attr.Mode,
		UID:          attr.UID,
		GID:          attr.GID,
		Size:         attr.Size,
		Atime:        attr.Atime,
		Mtime:        attr.Mtime,
		Ctime:        attr.Ctime,
		CreationTime: attr.CreationTime,
		LinkTarget:   "",
	}

	// Type-specific initialization
	if attr.Type == metadata.FileTypeRegular {
		// Generate ContentID for regular files
		// Format: shareName/path/to/file (e.g., "export/docs/report.pdf")
		// This mirrors the filesystem structure in S3 for easy inspection and recovery
		contentID := buildContentID(parentData.ShareName, fullPath)
		newAttr.ContentID = metadata.ContentID(contentID)
	} else {
		// Directories don't have content
		newAttr.ContentID = ""
	}

	// Store file with ShareName inherited from parent
	key := handleToKey(handle)
	store.files[key] = &fileData{
		Attr:      newAttr,
		ShareName: parentData.ShareName, // Inherit from parent
	}

	// Set link count
	if attr.Type == metadata.FileTypeDirectory {
		// Directories start at 2 ("." and parent entry)
		store.linkCounts[key] = 2
		newAttr.Nlink = 2
		// Initialize empty children map
		store.children[key] = make(map[string]metadata.FileHandle)
		// Increment parent's link count (new subdirectory adds ".." reference to parent)
		store.linkCounts[parentKey]++
		parentData.Attr.Nlink = store.linkCounts[parentKey]
	} else {
		// Regular files start at 1
		store.linkCounts[key] = 1
		newAttr.Nlink = 1
	}

	// Add to parent's children
	if !hasChildren {
		store.children[parentKey] = make(map[string]metadata.FileHandle)
	}
	store.children[parentKey][name] = handle

	// Invalidate parent directory cache
	store.invalidateDirCache(parentHandle)

	// Set parent relationship
	store.parents[key] = parentHandle

	// Update parent timestamps
	parentData.Attr.Mtime = attr.Mtime
	parentData.Attr.Ctime = attr.Ctime

	// Decode handle to get ID
	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to decode newly created handle",
		}
	}

	// Return full File information
	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      fullPath,
		FileAttr:  *newAttr,
	}, nil
}

// CreateSymlink creates a symbolic link pointing to a target path.
func (store *MemoryMetadataStore) CreateSymlink(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	target string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Validate target
	if err := metadata.ValidateSymlinkTarget(target); err != nil {
		return nil, err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify parent exists and is a directory
	parentKey := handleToKey(parentHandle)
	parentData, exists := store.files[parentKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent directory not found",
		}
	}
	if parentData.Attr.Type != metadata.FileTypeDirectory {
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
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on parent directory",
		}
	}

	// Check if name already exists
	childrenMap, hasChildren := store.children[parentKey]
	if hasChildren {
		if _, exists := childrenMap[name]; exists {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		}
	}

	// Check storage limits
	if store.maxFiles > 0 && uint64(len(store.files)) >= store.maxFiles {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNoSpace,
			Message: "maximum file count reached",
		}
	}

	// Build full path and generate deterministic handle
	// Build full path for handle generation.
	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations receive (parent_handle, name) pairs. Deeply nested directories
	// are valid as long as each name component is <= NAME_MAX.
	fullPath := store.buildFullPath(parentHandle, name)
	handle := store.generateFileHandle(parentData.ShareName, fullPath)

	// Set symlink type and apply defaults
	attr.Type = metadata.FileTypeSymlink
	metadata.ApplyCreateDefaults(attr, ctx, target)

	// Create symlink attributes
	newAttr := &metadata.FileAttr{
		Type:         metadata.FileTypeSymlink,
		Mode:         attr.Mode,
		UID:          attr.UID,
		GID:          attr.GID,
		Size:         attr.Size,
		Atime:        attr.Atime,
		Mtime:        attr.Mtime,
		Ctime:        attr.Ctime,
		CreationTime: attr.CreationTime,
		LinkTarget:   target,
		ContentID:    "", // Symlinks don't have content
	}

	// Store symlink with ShareName inherited from parent
	key := handleToKey(handle)
	store.files[key] = &fileData{
		Attr:      newAttr,
		ShareName: parentData.ShareName, // Inherit from parent
	}
	store.linkCounts[key] = 1
	newAttr.Nlink = 1

	// Add to parent's children
	if !hasChildren {
		store.children[parentKey] = make(map[string]metadata.FileHandle)
	}
	store.children[parentKey][name] = handle

	// Invalidate parent directory cache
	store.invalidateDirCache(parentHandle)

	// Set parent relationship
	store.parents[key] = parentHandle

	// Update parent timestamps
	parentData.Attr.Mtime = attr.Mtime
	parentData.Attr.Ctime = attr.Ctime

	// Decode handle to get ID
	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to decode newly created handle",
		}
	}

	// Return full File information
	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      fullPath,
		FileAttr:  *newAttr,
	}, nil
}

// CreateSpecialFile creates a special file (device, socket, or FIFO).
func (store *MemoryMetadataStore) CreateSpecialFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	fileType metadata.FileType,
	attr *metadata.FileAttr,
	deviceMajor, deviceMinor uint32,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate file type
	if err := metadata.ValidateSpecialFileType(fileType); err != nil {
		return nil, err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Check if user is root (required for device files)
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		if err := metadata.RequiresRoot(ctx); err != nil {
			return nil, err
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify parent exists and is a directory
	parentKey := handleToKey(parentHandle)
	parentData, exists := store.files[parentKey]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "parent directory not found",
		}
	}
	if parentData.Attr.Type != metadata.FileTypeDirectory {
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
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on parent directory",
		}
	}

	// Check if name already exists
	childrenMap, hasChildren := store.children[parentKey]
	if hasChildren {
		if _, exists := childrenMap[name]; exists {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		}
	}

	// Check storage limits
	if store.maxFiles > 0 && uint64(len(store.files)) >= store.maxFiles {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNoSpace,
			Message: "maximum file count reached",
		}
	}

	// Build full path and generate deterministic handle
	// Build full path for handle generation.
	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations receive (parent_handle, name) pairs. Deeply nested directories
	// are valid as long as each name component is <= NAME_MAX.
	fullPath := store.buildFullPath(parentHandle, name)
	handle := store.generateFileHandle(parentData.ShareName, fullPath)

	// Set special file type and apply defaults
	attr.Type = fileType
	metadata.ApplyCreateDefaults(attr, ctx, "")

	// Create special file attributes
	newAttr := &metadata.FileAttr{
		Type:         fileType,
		Mode:         attr.Mode,
		UID:          attr.UID,
		GID:          attr.GID,
		Size:         attr.Size,
		Atime:        attr.Atime,
		Mtime:        attr.Mtime,
		Ctime:        attr.Ctime,
		CreationTime: attr.CreationTime,
		LinkTarget:   "",
		ContentID:    "", // Special files don't have content
	}

	// Set device numbers for block and character devices
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		newAttr.Rdev = metadata.MakeRdev(deviceMajor, deviceMinor)
	}

	// Store file with ShareName inherited from parent
	key := handleToKey(handle)
	store.files[key] = &fileData{
		Attr:      newAttr,
		ShareName: parentData.ShareName, // Inherit from parent
	}
	store.linkCounts[key] = 1
	newAttr.Nlink = 1

	// Store device numbers for block and character devices
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		store.deviceNumbers[key] = &deviceNumber{
			Major: deviceMajor,
			Minor: deviceMinor,
		}
	}

	// Add to parent's children
	if !hasChildren {
		store.children[parentKey] = make(map[string]metadata.FileHandle)
	}
	store.children[parentKey][name] = handle

	// Invalidate parent directory cache
	store.invalidateDirCache(parentHandle)

	// Set parent relationship
	store.parents[key] = parentHandle

	// Update parent timestamps
	parentData.Attr.Mtime = attr.Mtime
	parentData.Attr.Ctime = attr.Ctime

	// Decode handle to get ID
	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to decode newly created handle",
		}
	}

	// Return full File information
	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      fullPath,
		FileAttr:  *newAttr,
	}, nil
}

// CreateHardLink creates a hard link to an existing file.
func (store *MemoryMetadataStore) CreateHardLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	targetHandle metadata.FileHandle,
) error {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return err
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify directory exists and is a directory
	dirKey := handleToKey(dirHandle)
	dirData, exists := store.files[dirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "directory not found",
		}
	}
	if dirData.Attr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Verify target exists
	targetKey := handleToKey(targetHandle)
	targetData, exists := store.files[targetKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "target file not found",
		}
	}

	// Cannot hard link directories
	if targetData.Attr.Type == metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot create hard link to directory",
		}
	}

	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations receive (parent_handle, name) pairs.

	// Check write permission on directory
	granted, err := store.checkPermissionsLocked(ctx, dirHandle, metadata.PermissionWrite)
	if err != nil {
		return err
	}
	if granted&metadata.PermissionWrite == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrAccessDenied,
			Message: "no write permission on directory",
		}
	}

	// Check if name already exists
	childrenMap, hasChildren := store.children[dirKey]
	if hasChildren {
		if _, exists := childrenMap[name]; exists {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		}
	}

	// Check link count limit
	currentLinks := store.linkCounts[targetKey]
	if currentLinks >= store.capabilities.MaxHardLinkCount {
		return &metadata.StoreError{
			Code:    metadata.ErrNotSupported,
			Message: "maximum hard link count reached",
		}
	}

	// Add to directory's children
	if !hasChildren {
		store.children[dirKey] = make(map[string]metadata.FileHandle)
	}
	store.children[dirKey][name] = targetHandle

	// Invalidate directory cache
	store.invalidateDirCache(dirHandle)

	// Increment link count
	store.linkCounts[targetKey]++
	targetData.Attr.Nlink = store.linkCounts[targetKey]

	// Update timestamps
	now := time.Now()
	targetData.Attr.Ctime = now // Target file's metadata changed
	dirData.Attr.Mtime = now    // Directory contents changed
	dirData.Attr.Ctime = now

	return nil
}

// ============================================================================
// Helper Methods
// ============================================================================

// checkPermissionsLocked is an internal helper that checks permissions without
// acquiring the lock (caller must hold lock).
func (store *MemoryMetadataStore) checkPermissionsLocked(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	requested metadata.Permission,
) (metadata.Permission, error) {
	// Get file data
	key := handleToKey(handle)
	fileData, exists := store.files[key]
	if !exists {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	attr := fileData.Attr
	identity := ctx.Identity

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions
		granted := metadata.CheckOtherPermissions(attr.Mode, requested)

		// Apply read-only share restriction for anonymous users
		if shareData, exists := store.shares[fileData.ShareName]; exists {
			if shareData.Share.Options.ReadOnly {
				granted &= ^(metadata.PermissionWrite | metadata.PermissionDelete)
			}
		}

		return granted, nil
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass: UID 0 gets all permissions (even on read-only shares for admin purposes)
	if uid == 0 {
		return requested, nil
	}

	// Determine which permission bits apply
	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid != nil && (*gid == attr.GID || identity.HasGID(attr.GID)) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	// Map Unix permission bits to Permission flags
	granted := metadata.CalculatePermissionsFromBits(permBits)

	// Owner gets additional privileges
	if uid == attr.UID {
		granted |= metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
	}

	// Apply read-only share restriction for non-root users
	if shareData, exists := store.shares[fileData.ShareName]; exists {
		if shareData.Share.Options.ReadOnly {
			granted &= ^(metadata.PermissionWrite | metadata.PermissionDelete)
		}
	}

	return granted & requested, nil
}

// GetFileByContentID retrieves file metadata by its content identifier.
//
// This scans all files to find one matching the given ContentID.
// Note: This is O(n) and may be slow for large filesystems.
func (store *MemoryMetadataStore) GetFileByContentID(
	ctx context.Context,
	contentID metadata.ContentID,
) (*metadata.File, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Scan all files for matching ContentID
	for _, fileData := range store.files {
		if fileData.Attr.ContentID == contentID {
			// Return File with just the attributes we need
			// ID and Path aren't needed by the flusher (only Size is used)
			return &metadata.File{
				ShareName: fileData.ShareName,
				FileAttr:  *fileData.Attr,
			}, nil
		}
	}

	return nil, &metadata.StoreError{
		Code:    metadata.ErrNotFound,
		Message: fmt.Sprintf("no file found with content ID: %s", contentID),
	}
}

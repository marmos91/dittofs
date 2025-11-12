package memory

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
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
) (metadata.FileHandle, *metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, nil, err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()

	// Get directory attributes
	dirKey := handleToKey(dirHandle)
	dirAttr, exists := store.files[dirKey]
	if !exists {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "directory not found",
		}
	}

	// Verify it's a directory
	if dirAttr.Type != metadata.FileTypeDirectory {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Check execute/traverse permission on directory (search permission)
	granted, err := store.checkPermissionsLocked(ctx, dirHandle, metadata.PermissionTraverse)
	if err != nil {
		return nil, nil, err
	}
	if granted&metadata.PermissionTraverse == 0 {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
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
			return nil, nil, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("name not found: %s", name),
				Path:    name,
			}
		}

		childHandle, exists := childrenMap[name]
		if !exists {
			return nil, nil, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: fmt.Sprintf("name not found: %s", name),
				Path:    name,
			}
		}
		targetHandle = childHandle
	}

	// Get target attributes
	targetKey := handleToKey(targetHandle)
	targetAttr, exists := store.files[targetKey]
	if !exists {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "target file not found",
		}
	}

	// Return handle and attributes
	return targetHandle, targetAttr, nil
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
) (*metadata.FileAttr, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Get file attributes
	key := handleToKey(handle)
	attr, exists := s.files[key]
	if !exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Return attributes
	return attr, nil
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

	// Get file attributes
	key := handleToKey(handle)
	attr, exists := store.files[key]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	identity := ctx.Identity
	if identity == nil || identity.UID == nil {
		return &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
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
		// Only owner or root can change mode
		if !isOwner && !isRoot {
			return &metadata.StoreError{
				Code:    metadata.ErrPermissionDenied,
				Message: "only owner or root can change mode",
			}
		}

		// Validate mode (only lower 12 bits)
		newMode := *attrs.Mode & 0o7777
		attr.Mode = newMode
		changed = true
	}

	// Validate and apply UID changes
	if attrs.UID != nil {
		// Only root can change ownership
		if !isRoot {
			return &metadata.StoreError{
				Code:    metadata.ErrPermissionDenied,
				Message: "only root can change ownership",
			}
		}

		attr.UID = *attrs.UID
		changed = true
	}

	// Validate and apply GID changes
	if attrs.GID != nil {
		// Root can change to any group
		// Owner can change only if member of target group
		if !isRoot {
			if !isOwner {
				return &metadata.StoreError{
					Code:    metadata.ErrPermissionDenied,
					Message: "only owner or root can change group",
				}
			}

			// Check if owner is member of target group
			targetGID := *attrs.GID
			isMember := false
			if identity.GID != nil && *identity.GID == targetGID {
				isMember = true
			}
			if !isMember && slices.Contains(identity.GIDs, targetGID) {
				isMember = true
			}

			if !isMember {
				return &metadata.StoreError{
					Code:    metadata.ErrPermissionDenied,
					Message: "owner must be member of target group",
				}
			}
		}

		attr.GID = *attrs.GID
		changed = true
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
				Code:    metadata.ErrPermissionDenied,
				Message: "no write permission",
			}
		}

		// Update size and mtime
		attr.Size = *attrs.Size
		attr.Mtime = time.Now()
		changed = true
	}

	// Apply atime changes
	if attrs.Atime != nil {
		// Check write permission or ownership
		if !isOwner && !isRoot {
			granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrPermissionDenied,
					Message: "no permission to change atime",
				}
			}
		}

		attr.Atime = *attrs.Atime
		changed = true
	}

	// Apply mtime changes
	if attrs.Mtime != nil {
		// Check write permission or ownership
		if !isOwner && !isRoot {
			granted, err := store.checkPermissionsLocked(ctx, handle, metadata.PermissionWrite)
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrPermissionDenied,
					Message: "no permission to change mtime",
				}
			}
		}

		attr.Mtime = *attrs.Mtime
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
) (metadata.FileHandle, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate type
	if attr.Type != metadata.FileTypeRegular && attr.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "Create only supports regular files and directories",
		}
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

	// Generate handle and complete attributes
	handle := store.generateFileHandle()
	now := time.Now()

	// Set defaults if not provided
	if attr.Mode == 0 {
		if attr.Type == metadata.FileTypeDirectory {
			attr.Mode = 0755
		} else {
			attr.Mode = 0644
		}
	}

	// Use authenticated user's credentials if not provided
	if ctx.Identity != nil && ctx.Identity.UID != nil {
		if attr.UID == 0 {
			attr.UID = *ctx.Identity.UID
		}
		if attr.GID == 0 && ctx.Identity.GID != nil {
			attr.GID = *ctx.Identity.GID
		}
	}

	// Complete file attributes
	newAttr := &metadata.FileAttr{
		Type:       attr.Type,
		Mode:       attr.Mode & 0o7777,
		UID:        attr.UID,
		GID:        attr.GID,
		Size:       0,
		Atime:      now,
		Mtime:      now,
		Ctime:      now,
		LinkTarget: "",
	}

	// Type-specific initialization
	if attr.Type == metadata.FileTypeRegular {
		// Generate ContentID for regular files
		newAttr.ContentID = metadata.ContentID(handleToKey(store.generateFileHandle()))
	} else {
		// Directories don't have content
		newAttr.ContentID = ""
	}

	// Store file
	key := handleToKey(handle)
	store.files[key] = newAttr

	// Set link count
	if attr.Type == metadata.FileTypeDirectory {
		// Directories start at 2 ("." and parent entry)
		store.linkCounts[key] = 2
		// Initialize empty children map
		store.children[key] = make(map[string]metadata.FileHandle)
	} else {
		// Regular files start at 1
		store.linkCounts[key] = 1
	}

	// Add to parent's children
	if !hasChildren {
		store.children[parentKey] = make(map[string]metadata.FileHandle)
	}
	store.children[parentKey][name] = handle

	// Set parent relationship
	store.parents[key] = parentHandle

	// Update parent timestamps
	parentAttr.Mtime = now
	parentAttr.Ctime = now

	return handle, nil
}

// CreateSymlink creates a symbolic link pointing to a target path.
func (store *MemoryMetadataStore) CreateSymlink(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	target string,
	attr *metadata.FileAttr,
) (metadata.FileHandle, error) {
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

	// Validate target
	if target == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "symlink target cannot be empty",
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

	// Generate handle and complete attributes
	handle := store.generateFileHandle()
	now := time.Now()

	// Set defaults if not provided
	mode := attr.Mode
	if mode == 0 {
		mode = 0777 // Symlinks typically have 0777
	}

	// Use authenticated user's credentials if not provided
	uid := attr.UID
	gid := attr.GID
	if ctx.Identity != nil && ctx.Identity.UID != nil {
		if uid == 0 {
			uid = *ctx.Identity.UID
		}
		if gid == 0 && ctx.Identity.GID != nil {
			gid = *ctx.Identity.GID
		}
	}

	// Create symlink attributes
	newAttr := &metadata.FileAttr{
		Type:       metadata.FileTypeSymlink,
		Mode:       mode & 0o7777,
		UID:        uid,
		GID:        gid,
		Size:       uint64(len(target)), // Size is length of target path
		Atime:      now,
		Mtime:      now,
		Ctime:      now,
		LinkTarget: target,
		ContentID:  "", // Symlinks don't have content
	}

	// Store symlink
	key := handleToKey(handle)
	store.files[key] = newAttr
	store.linkCounts[key] = 1

	// Add to parent's children
	if !hasChildren {
		store.children[parentKey] = make(map[string]metadata.FileHandle)
	}
	store.children[parentKey][name] = handle

	// Set parent relationship
	store.parents[key] = parentHandle

	// Update parent timestamps
	parentAttr.Mtime = now
	parentAttr.Ctime = now

	return handle, nil
}

// CreateSpecialFile creates a special file (device, socket, or FIFO).
func (store *MemoryMetadataStore) CreateSpecialFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	fileType metadata.FileType,
	attr *metadata.FileAttr,
	deviceMajor, deviceMinor uint32,
) (metadata.FileHandle, error) {
	// Check context before acquiring lock
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate file type
	switch fileType {
	case metadata.FileTypeBlockDevice, metadata.FileTypeCharDevice,
		metadata.FileTypeSocket, metadata.FileTypeFIFO:
		// Valid special file types
	default:
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: fmt.Sprintf("invalid special file type: %d", fileType),
		}
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

	// Check if user is root (required for device files)
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		if ctx.Identity == nil || ctx.Identity.UID == nil || *ctx.Identity.UID != 0 {
			return nil, &metadata.StoreError{
				Code:    metadata.ErrPermissionDenied,
				Message: "only root can create device files",
			}
		}
	}

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

	// Generate handle and complete attributes
	handle := store.generateFileHandle()
	now := time.Now()

	// Set defaults if not provided
	mode := attr.Mode
	if mode == 0 {
		mode = 0644
	}

	// Use authenticated user's credentials if not provided
	uid := attr.UID
	gid := attr.GID
	if ctx.Identity != nil && ctx.Identity.UID != nil {
		if uid == 0 {
			uid = *ctx.Identity.UID
		}
		if gid == 0 && ctx.Identity.GID != nil {
			gid = *ctx.Identity.GID
		}
	}

	// Create special file attributes
	// Note: DeviceMajor and DeviceMinor are not in FileAttr struct
	// In a real implementation, you might need to store these separately
	// or extend FileAttr. For now, we'll just create the file without storing device numbers.
	newAttr := &metadata.FileAttr{
		Type:       fileType,
		Mode:       mode & 0o7777,
		UID:        uid,
		GID:        gid,
		Size:       0, // Special files have no size
		Atime:      now,
		Mtime:      now,
		Ctime:      now,
		LinkTarget: "",
		ContentID:  "", // Special files don't have content
	}

	// TODO: Store deviceMajor and deviceMinor somewhere
	// This could be in a separate map or in Extended attributes
	// For now, they are accepted but not stored

	// Store file
	key := handleToKey(handle)
	store.files[key] = newAttr
	store.linkCounts[key] = 1

	// Add to parent's children
	if !hasChildren {
		store.children[parentKey] = make(map[string]metadata.FileHandle)
	}
	store.children[parentKey][name] = handle

	// Set parent relationship
	store.parents[key] = parentHandle

	// Update parent timestamps
	parentAttr.Mtime = now
	parentAttr.Ctime = now

	return handle, nil
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
	if name == "" || name == "." || name == ".." {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid name",
			Path:    name,
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify directory exists and is a directory
	dirKey := handleToKey(dirHandle)
	dirAttr, exists := store.files[dirKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "directory not found",
		}
	}
	if dirAttr.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Verify target exists
	targetKey := handleToKey(targetHandle)
	targetAttr, exists := store.files[targetKey]
	if !exists {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "target file not found",
		}
	}

	// Cannot hard link directories
	if targetAttr.Type == metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot create hard link to directory",
		}
	}

	// Check write permission on directory
	granted, err := store.checkPermissionsLocked(ctx, dirHandle, metadata.PermissionWrite)
	if err != nil {
		return err
	}
	if granted&metadata.PermissionWrite == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrPermissionDenied,
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

	// Increment link count
	store.linkCounts[targetKey]++

	// Update timestamps
	now := time.Now()
	targetAttr.Ctime = now // Target file's metadata changed
	dirAttr.Mtime = now    // Directory contents changed
	dirAttr.Ctime = now

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
	// Get file attributes
	key := handleToKey(handle)
	attr, exists := store.files[key]
	if !exists {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	identity := ctx.Identity

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions
		otherBits := attr.Mode & 0x7
		var granted metadata.Permission

		if otherBits&0x4 != 0 { // Read
			granted |= metadata.PermissionRead | metadata.PermissionListDirectory
		}
		if otherBits&0x2 != 0 { // Write
			granted |= metadata.PermissionWrite | metadata.PermissionDelete
		}
		if otherBits&0x1 != 0 { // Execute
			granted |= metadata.PermissionExecute | metadata.PermissionTraverse
		}

		return granted & requested, nil
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass
	if uid == 0 {
		return requested, nil
	}

	// Determine which permission bits apply
	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid != nil && (*gid == attr.GID || containsGID(identity.GIDs, attr.GID)) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	// Map Unix permission bits to Permission flags
	var granted metadata.Permission

	if permBits&0x4 != 0 { // Read
		granted |= metadata.PermissionRead | metadata.PermissionListDirectory
	}
	if permBits&0x2 != 0 { // Write
		granted |= metadata.PermissionWrite | metadata.PermissionDelete
	}
	if permBits&0x1 != 0 { // Execute
		granted |= metadata.PermissionExecute | metadata.PermissionTraverse
	}

	// Owner gets additional privileges
	if uid == attr.UID {
		granted |= metadata.PermissionChangePermissions | metadata.PermissionChangeOwnership
	}

	return granted & requested, nil
}

package metadata

import (
	"time"
)

// ============================================================================
// Shared File Operations
// ============================================================================
//
// These functions provide centralized business logic for file operations.
// They use the CRUD methods to access data, keeping the business logic
// separate from storage concerns.
//
// All operations follow the pattern:
//  1. Validate inputs
//  2. Check permissions
//  3. Perform the operation using CRUD methods
//  4. Update timestamps
//
// Error Handling:
// - Input validation errors: ErrInvalidArgument
// - Permission errors: ErrAccessDenied
// - Type errors: ErrIsDirectory, ErrNotDirectory
// - Not found errors: ErrNotFound

// RemoveFileOp removes a file from its parent directory.
//
// This is the centralized implementation of file removal that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Sticky bit enforcement
//   - Hard link management (decrement or set nlink=0)
//   - Parent timestamp updates
//
// Important: This method does NOT delete the file's content data.
// The returned File includes ContentID for caller to coordinate content deletion.
// ContentID is empty if other hard links still reference the content.
//
// POSIX Compliance:
//   - When last link is removed, nlink is set to 0 (not deleted)
//   - This allows fstat() on open file descriptors to return nlink=0
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name of file to remove
//
// Returns:
//   - *File: Removed file's metadata (ContentID empty if other links exist)
//   - error: Various errors for validation, permission, not found, etc.
func RemoveFileOp(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
) (*File, error) {
	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetEntry(ctx.Context, parentHandle)
	if err != nil {
		return nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := CheckWritePermission(store, ctx, parentHandle); err != nil {
		return nil, err
	}

	// Get child handle
	fileHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetEntry(ctx.Context, fileHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's not a directory
	if file.Type == FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot remove directory with RemoveFile, use RemoveDirectory",
			Path:    name,
		}
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &file.FileAttr); err != nil {
		return nil, err
	}

	// Get current link count
	linkCount, err := store.GetLinkCount(ctx.Context, fileHandle)
	if err != nil {
		// If we can't get link count, assume 1
		linkCount = 1
	}

	now := time.Now()

	// Prepare return value
	returnFile := &File{
		ID:        file.ID,
		ShareName: file.ShareName,
		Path:      file.Path,
		FileAttr:  file.FileAttr,
	}

	// Handle link count
	if linkCount > 1 {
		// File has other hard links, just decrement count
		// Empty ContentID signals caller NOT to delete content
		returnFile.ContentID = ""
		returnFile.Nlink = linkCount - 1
		returnFile.Ctime = now

		// Update file's link count and ctime
		if err := store.SetLinkCount(ctx.Context, fileHandle, linkCount-1); err != nil {
			return nil, err
		}

		// Update file's ctime
		file.Ctime = now
		if err := store.PutEntry(ctx.Context, file); err != nil {
			return nil, err
		}
	} else {
		// Last link - set nlink=0 but keep metadata for POSIX compliance
		returnFile.Nlink = 0
		returnFile.Ctime = now

		// Set link count to 0
		if err := store.SetLinkCount(ctx.Context, fileHandle, 0); err != nil {
			return nil, err
		}

		// Update file's ctime and nlink
		file.Ctime = now
		file.Nlink = 0
		if err := store.PutEntry(ctx.Context, file); err != nil {
			return nil, err
		}

		// Note: We don't delete the entry - it stays with nlink=0 for POSIX compliance
		// (fstat on open fd should return nlink=0, not ESTALE)
	}

	// Remove from parent's children
	if err := store.DeleteChild(ctx.Context, parentHandle, name); err != nil {
		return nil, err
	}

	// Update parent timestamps
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutEntry(ctx.Context, parent); err != nil {
		return nil, err
	}

	return returnFile, nil
}

// RemoveDirectoryOp removes an empty directory from its parent.
//
// This is the centralized implementation of directory removal that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Sticky bit enforcement
//   - Empty check (directory must have no children)
//   - Parent link count update (removing ".." reference)
//   - Parent timestamp updates
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name of directory to remove
//
// Returns:
//   - error: Various errors for validation, permission, not empty, etc.
func RemoveDirectoryOp(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
) error {
	// Validate name
	if err := ValidateName(name); err != nil {
		return err
	}

	// Get parent entry
	parent, err := store.GetEntry(ctx.Context, parentHandle)
	if err != nil {
		return err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := CheckWritePermission(store, ctx, parentHandle); err != nil {
		return err
	}

	// Get child handle
	dirHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return err
	}

	// Get directory entry
	dir, err := store.GetEntry(ctx.Context, dirHandle)
	if err != nil {
		return err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    name,
		}
	}

	// Check sticky bit restriction
	if err := CheckStickyBitRestriction(ctx, &parent.FileAttr, &dir.FileAttr); err != nil {
		return err
	}

	// Check if directory is empty by trying to list children
	// We use a minimal page to check if any children exist
	page, err := store.ReadDirectory(ctx, dirHandle, "", 1)
	if err == nil && len(page.Entries) > 0 {
		return &StoreError{
			Code:    ErrNotEmpty,
			Message: "directory not empty",
			Path:    name,
		}
	}

	// Remove directory entry
	if err := store.DeleteEntry(ctx.Context, dirHandle); err != nil {
		return err
	}

	// Remove from parent's children
	if err := store.DeleteChild(ctx.Context, parentHandle, name); err != nil {
		return err
	}

	// Update parent's link count (removing ".." reference)
	parentLinkCount, err := store.GetLinkCount(ctx.Context, parentHandle)
	if err == nil && parentLinkCount > 0 {
		if err := store.SetLinkCount(ctx.Context, parentHandle, parentLinkCount-1); err != nil {
			// Non-fatal, continue
		}
	}

	// Update parent timestamps
	now := time.Now()
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutEntry(ctx.Context, parent); err != nil {
		return err
	}

	return nil
}

// LookupOp resolves a name within a directory to a file handle and attributes.
//
// This is the centralized implementation of name lookup that all stores
// should delegate to. It handles:
//   - Special names: "." (current dir), ".." (parent dir)
//   - Permission checking (execute on directory for search)
//   - Name resolution in directory
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - dirHandle: Directory to search in
//   - name: Name to resolve (can be ".", "..", or regular name)
//
// Returns:
//   - *File: Resolved file's complete metadata
//   - error: ErrNotFound, ErrNotDirectory, ErrAccessDenied, etc.
func LookupOp(
	store MetadataStore,
	ctx *AuthContext,
	dirHandle FileHandle,
	name string,
) (*File, error) {
	// Get directory entry
	dir, err := store.GetEntry(ctx.Context, dirHandle)
	if err != nil {
		return nil, err
	}

	// Verify it's a directory
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
			Path:    dir.Path,
		}
	}

	// Check execute/search permission on directory
	if err := CheckExecutePermission(store, ctx, dirHandle); err != nil {
		return nil, err
	}

	// Handle special names
	if name == "." {
		return dir, nil
	}

	if name == ".." {
		parentHandle, err := store.GetParent(ctx.Context, dirHandle)
		if err != nil {
			// No parent means this is root, return self
			return dir, nil
		}
		return store.GetEntry(ctx.Context, parentHandle)
	}

	// Regular name lookup
	childHandle, err := store.GetChild(ctx.Context, dirHandle, name)
	if err != nil {
		return nil, err
	}

	return store.GetEntry(ctx.Context, childHandle)
}

// CreateFileOp creates a new regular file in a directory.
//
// This is the centralized implementation of file creation that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Name collision detection
//   - Default attribute application
//   - Parent timestamp updates
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name for the new file
//   - attr: Initial attributes (Mode, UID, GID)
//
// Returns:
//   - *File: Created file's complete metadata
//   - error: ErrAlreadyExists, ErrAccessDenied, etc.
func CreateFileOp(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
) (*File, error) {
	return createEntryOp(store, ctx, parentHandle, name, attr, FileTypeRegular, "", 0, 0)
}

// CreateDirectoryOp creates a new directory in a parent directory.
//
// This is the centralized implementation of directory creation that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Name collision detection
//   - Default attribute application
//   - Parent link count update (new ".." reference)
//   - Parent timestamp updates
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name for the new directory
//   - attr: Initial attributes (Mode, UID, GID)
//
// Returns:
//   - *File: Created directory's complete metadata
//   - error: ErrAlreadyExists, ErrAccessDenied, etc.
func CreateDirectoryOp(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
) (*File, error) {
	return createEntryOp(store, ctx, parentHandle, name, attr, FileTypeDirectory, "", 0, 0)
}

// CreateSymlinkOp creates a new symbolic link in a directory.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - parentHandle: Handle of parent directory
//   - name: Name for the new symlink
//   - target: Target path the symlink points to
//   - attr: Initial attributes (Mode, UID, GID)
//
// Returns:
//   - *File: Created symlink's complete metadata
//   - error: ErrAlreadyExists, ErrAccessDenied, etc.
func CreateSymlinkOp(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	target string,
	attr *FileAttr,
) (*File, error) {
	// Validate symlink target
	if err := ValidateSymlinkTarget(target); err != nil {
		return nil, err
	}

	return createEntryOp(store, ctx, parentHandle, name, attr, FileTypeSymlink, target, 0, 0)
}

// createEntryOp is the internal implementation for creating files, directories, and symlinks.
func createEntryOp(
	store MetadataStore,
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
	fileType FileType,
	linkTarget string,
	deviceMajor, deviceMinor uint32,
) (*File, error) {
	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetEntry(ctx.Context, parentHandle)
	if err != nil {
		return nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := CheckWritePermission(store, ctx, parentHandle); err != nil {
		return nil, err
	}

	// Check if name already exists
	_, err = store.GetChild(ctx.Context, parentHandle, name)
	if err == nil {
		return nil, &StoreError{
			Code:    ErrAlreadyExists,
			Message: "file already exists",
			Path:    name,
		}
	}
	// If error is not ErrNotFound, it's a real error
	if !IsNotFoundError(err) {
		return nil, err
	}

	// Generate new handle
	newHandle, err := store.GenerateHandle(ctx.Context, parent.ShareName, buildPath(parent.Path, name))
	if err != nil {
		return nil, err
	}

	// Decode handle to get ID
	_, id, err := DecodeFileHandle(newHandle)
	if err != nil {
		return nil, err
	}

	// Prepare attributes
	newAttr := *attr
	newAttr.Type = fileType
	newAttr.LinkTarget = linkTarget
	ApplyCreateDefaults(&newAttr, ctx, linkTarget)
	ApplyOwnerDefaults(&newAttr, ctx)

	// Set content ID for regular files
	if fileType == FileTypeRegular {
		newAttr.ContentID = ContentID(buildContentID(parent.ShareName, buildPath(parent.Path, name)))
	}

	// Create the file entry
	newFile := &File{
		ID:        id,
		ShareName: parent.ShareName,
		Path:      buildPath(parent.Path, name),
		FileAttr:  newAttr,
	}
	newFile.Nlink = GetInitialLinkCount(fileType)

	// Store the entry
	if err := store.PutEntry(ctx.Context, newFile); err != nil {
		return nil, err
	}

	// Set parent reference
	if err := store.SetParent(ctx.Context, newHandle, parentHandle); err != nil {
		// Cleanup on failure
		_ = store.DeleteEntry(ctx.Context, newHandle)
		return nil, err
	}

	// Add to parent's children
	if err := store.SetChild(ctx.Context, parentHandle, name, newHandle); err != nil {
		// Cleanup on failure
		_ = store.DeleteEntry(ctx.Context, newHandle)
		return nil, err
	}

	// For directories, increment parent's link count (new ".." reference)
	if fileType == FileTypeDirectory {
		parentLinkCount, err := store.GetLinkCount(ctx.Context, parentHandle)
		if err == nil {
			_ = store.SetLinkCount(ctx.Context, parentHandle, parentLinkCount+1)
		}
	}

	// Update parent timestamps
	now := time.Now()
	parent.Mtime = now
	parent.Ctime = now
	if err := store.PutEntry(ctx.Context, parent); err != nil {
		// Non-fatal, file was created
	}

	return newFile, nil
}

// buildPath constructs a full path from parent path and child name.
func buildPath(parentPath, childName string) string {
	if parentPath == "/" {
		return "/" + childName
	}
	return parentPath + "/" + childName
}

// buildContentID constructs a content ID from share name and path.
func buildContentID(shareName, path string) string {
	// Remove leading "/" from path and combine with share name
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(shareName) > 0 && shareName[0] == '/' {
		shareName = shareName[1:]
	}
	return shareName + "/" + path
}

// MoveOp moves or renames a file or directory atomically.
//
// This is the centralized implementation of move/rename that all stores
// should delegate to. It handles:
//   - Input validation
//   - Permission checking (write on both directories)
//   - Sticky bit enforcement
//   - Type compatibility (file over file, dir over empty dir)
//   - Atomic replacement of destination
//   - Timestamp updates
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - fromDir: Source directory handle
//   - fromName: Source name
//   - toDir: Destination directory handle
//   - toName: Destination name
//
// Returns:
//   - error: Various errors for validation, permission, type mismatch, etc.
func MoveOp(
	store MetadataStore,
	ctx *AuthContext,
	fromDir FileHandle,
	fromName string,
	toDir FileHandle,
	toName string,
) error {
	// Validate names
	if err := ValidateName(fromName); err != nil {
		return err
	}
	if err := ValidateName(toName); err != nil {
		return err
	}

	// Get source directory
	srcDir, err := store.GetEntry(ctx.Context, fromDir)
	if err != nil {
		return err
	}
	if srcDir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "source parent is not a directory",
		}
	}

	// Get destination directory
	dstDir, err := store.GetEntry(ctx.Context, toDir)
	if err != nil {
		return err
	}
	if dstDir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "destination parent is not a directory",
		}
	}

	// Check write permission on both directories
	if err := CheckWritePermission(store, ctx, fromDir); err != nil {
		return err
	}
	if err := CheckWritePermission(store, ctx, toDir); err != nil {
		return err
	}

	// Get source file
	srcHandle, err := store.GetChild(ctx.Context, fromDir, fromName)
	if err != nil {
		return err
	}
	srcFile, err := store.GetEntry(ctx.Context, srcHandle)
	if err != nil {
		return err
	}

	// Check sticky bit on source directory
	if err := CheckStickyBitRestriction(ctx, &srcDir.FileAttr, &srcFile.FileAttr); err != nil {
		return err
	}

	// Check if destination exists
	dstHandle, err := store.GetChild(ctx.Context, toDir, toName)
	if err == nil {
		// Destination exists - check compatibility
		dstFile, err := store.GetEntry(ctx.Context, dstHandle)
		if err != nil {
			return err
		}

		// Check sticky bit on destination directory
		if err := CheckStickyBitRestriction(ctx, &dstDir.FileAttr, &dstFile.FileAttr); err != nil {
			return err
		}

		// Type compatibility checks
		if srcFile.Type == FileTypeDirectory {
			if dstFile.Type != FileTypeDirectory {
				return &StoreError{
					Code:    ErrNotDirectory,
					Message: "cannot overwrite non-directory with directory",
				}
			}
			// Check if destination directory is empty
			page, err := store.ReadDirectory(ctx, dstHandle, "", 1)
			if err == nil && len(page.Entries) > 0 {
				return &StoreError{
					Code:    ErrNotEmpty,
					Message: "destination directory not empty",
				}
			}
		} else {
			if dstFile.Type == FileTypeDirectory {
				return &StoreError{
					Code:    ErrIsDirectory,
					Message: "cannot overwrite directory with non-directory",
				}
			}
		}

		// Remove destination
		if dstFile.Type == FileTypeDirectory {
			if err := store.DeleteEntry(ctx.Context, dstHandle); err != nil {
				return err
			}
		} else {
			// For files, decrement link count or set to 0
			linkCount, _ := store.GetLinkCount(ctx.Context, dstHandle)
			if linkCount <= 1 {
				_ = store.SetLinkCount(ctx.Context, dstHandle, 0)
			} else {
				_ = store.SetLinkCount(ctx.Context, dstHandle, linkCount-1)
			}
		}

		// Remove destination from children
		if err := store.DeleteChild(ctx.Context, toDir, toName); err != nil {
			return err
		}
	} else if !IsNotFoundError(err) {
		return err
	}

	// Remove source from old parent
	if err := store.DeleteChild(ctx.Context, fromDir, fromName); err != nil {
		return err
	}

	// Add source to new parent
	if err := store.SetChild(ctx.Context, toDir, toName, srcHandle); err != nil {
		return err
	}

	// Update parent reference if directories are different
	if string(fromDir) != string(toDir) {
		if err := store.SetParent(ctx.Context, srcHandle, toDir); err != nil {
			// Non-fatal
		}

		// Update link counts for directory moves
		if srcFile.Type == FileTypeDirectory {
			// Decrement source parent's link count
			srcLinkCount, _ := store.GetLinkCount(ctx.Context, fromDir)
			if srcLinkCount > 0 {
				_ = store.SetLinkCount(ctx.Context, fromDir, srcLinkCount-1)
			}
			// Increment destination parent's link count
			dstLinkCount, _ := store.GetLinkCount(ctx.Context, toDir)
			_ = store.SetLinkCount(ctx.Context, toDir, dstLinkCount+1)
		}
	}

	// Update timestamps
	now := time.Now()
	srcFile.Ctime = now
	if err := store.PutEntry(ctx.Context, srcFile); err != nil {
		// Non-fatal
	}

	srcDir.Mtime = now
	srcDir.Ctime = now
	if err := store.PutEntry(ctx.Context, srcDir); err != nil {
		// Non-fatal
	}

	if string(fromDir) != string(toDir) {
		dstDir.Mtime = now
		dstDir.Ctime = now
		if err := store.PutEntry(ctx.Context, dstDir); err != nil {
			// Non-fatal
		}
	}

	return nil
}

// MarkFileAsOrphaned sets a file's link count to 0, marking it as orphaned.
//
// This function is used by NFS handlers for "silly rename" behavior. When an
// NFS client deletes a file that's still open, the server renames it to a
// `.nfsXXXX` temporary name. The file is marked as orphaned (nlink=0) so that:
//   - fstat() on open file descriptors correctly returns nlink=0
//   - The file can be garbage collected when all file descriptors are closed
//
// This is an NFS-specific behavior and should only be called from NFS handlers.
// POSIX filesystems don't have silly rename - they just set nlink=0 on unlink.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Context for the operation
//   - handle: Handle of the file to mark as orphaned
//
// Returns:
//   - error: ErrNotFound if file doesn't exist, ErrIsDirectory if it's a directory
func MarkFileAsOrphaned(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
) error {
	// Get file entry
	file, err := store.GetEntry(ctx.Context, handle)
	if err != nil {
		return err
	}

	// Only mark regular files as orphaned (directories don't have silly rename)
	if file.Type == FileTypeDirectory {
		return nil
	}

	// Set link count to 0
	if err := store.SetLinkCount(ctx.Context, handle, 0); err != nil {
		return err
	}

	// Update file's nlink and ctime
	now := time.Now()
	file.Nlink = 0
	file.Ctime = now
	return store.PutEntry(ctx.Context, file)
}

// CreateHardLinkOp creates a hard link to an existing file.
//
// This is the centralized implementation of hard link creation that all stores
// should delegate to.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - dirHandle: Directory where the link will be created
//   - name: Name for the new link
//   - targetHandle: File to link to
//
// Returns:
//   - error: ErrIsDirectory if target is a directory, ErrAlreadyExists, etc.
func CreateHardLinkOp(
	store MetadataStore,
	ctx *AuthContext,
	dirHandle FileHandle,
	name string,
	targetHandle FileHandle,
) error {
	// Validate name
	if err := ValidateName(name); err != nil {
		return err
	}

	// Get directory entry
	dir, err := store.GetEntry(ctx.Context, dirHandle)
	if err != nil {
		return err
	}
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Check write permission on directory
	if err := CheckWritePermission(store, ctx, dirHandle); err != nil {
		return err
	}

	// Get target file
	target, err := store.GetEntry(ctx.Context, targetHandle)
	if err != nil {
		return err
	}

	// Cannot hard link directories
	if target.Type == FileTypeDirectory {
		return &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot create hard link to directory",
		}
	}

	// Check if name already exists
	_, err = store.GetChild(ctx.Context, dirHandle, name)
	if err == nil {
		return &StoreError{
			Code:    ErrAlreadyExists,
			Message: "file already exists",
			Path:    name,
		}
	}
	if !IsNotFoundError(err) {
		return err
	}

	// Add to directory's children
	if err := store.SetChild(ctx.Context, dirHandle, name, targetHandle); err != nil {
		return err
	}

	// Increment target's link count
	linkCount, _ := store.GetLinkCount(ctx.Context, targetHandle)
	if err := store.SetLinkCount(ctx.Context, targetHandle, linkCount+1); err != nil {
		// Cleanup
		_ = store.DeleteChild(ctx.Context, dirHandle, name)
		return err
	}

	// Update timestamps
	now := time.Now()
	target.Ctime = now
	_ = store.PutEntry(ctx.Context, target)

	dir.Mtime = now
	dir.Ctime = now
	_ = store.PutEntry(ctx.Context, dir)

	return nil
}

// ============================================================================
// File I/O Operations
// ============================================================================

// PrepareWriteOp validates a write operation and returns a write intent.
//
// This is the centralized implementation of write preparation that all stores
// should delegate to. It handles:
//   - File type validation (must be regular file)
//   - Permission checking (write permission)
//   - Building WriteOperation with pre-operation attributes
//
// The method does NOT modify any metadata. Metadata changes are applied by
// CommitWriteOp after the content write succeeds.
//
// Two-Phase Write Pattern:
//  1. PrepareWriteOp - validates and creates intent
//  2. ContentStore.WriteAt - writes actual content
//  3. CommitWriteOp - updates metadata (size, mtime, ctime)
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - handle: File handle to write to
//   - newSize: New file size after write (offset + data length)
//
// Returns:
//   - *WriteOperation: Intent containing ContentID and pre-write attributes
//   - error: ErrNotFound, ErrAccessDenied, ErrIsDirectory, etc.
func PrepareWriteOp(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
	newSize uint64,
) (*WriteOperation, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetEntry(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	// Verify it's a regular file
	if file.Type != FileTypeRegular {
		if file.Type == FileTypeDirectory {
			return nil, &StoreError{
				Code:    ErrIsDirectory,
				Message: "cannot write to directory",
				Path:    file.Path,
			}
		}
		return nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "cannot write to non-regular file",
			Path:    file.Path,
		}
	}

	// Check write permission
	// Owner can always write to their own files (even if mode is 0444)
	// This matches POSIX semantics where permissions are checked at open() time.
	isOwner := ctx.Identity != nil && ctx.Identity.UID != nil && *ctx.Identity.UID == file.UID

	if !isOwner {
		// Non-owner: check permissions using normal Unix permission bits
		if err := CheckWritePermission(store, ctx, handle); err != nil {
			return nil, err
		}
	}

	// Make a copy of current attributes for PreWriteAttr
	preWriteAttr := CopyFileAttr(&file.FileAttr)

	// Create write operation
	writeOp := &WriteOperation{
		Handle:       handle,
		NewSize:      newSize,
		NewMtime:     time.Now(),
		ContentID:    file.ContentID,
		PreWriteAttr: preWriteAttr,
	}

	return writeOp, nil
}

// CommitWriteOp applies metadata changes after a successful content write.
//
// This is the centralized implementation of write commit that all stores
// should delegate to. It handles:
//   - File size update (max of current and new size)
//   - Timestamp updates (mtime, ctime)
//   - POSIX: clearing setuid/setgid bits for non-root users
//
// Should be called after ContentStore.WriteAt succeeds.
//
// If this fails after content was written, the file is in an inconsistent
// state (content newer than metadata). This can be detected by consistency
// checkers.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - intent: The write intent from PrepareWriteOp
//
// Returns:
//   - *File: Updated file with new attributes
//   - error: ErrNotFound if file was deleted, etc.
func CommitWriteOp(
	store MetadataStore,
	ctx *AuthContext,
	intent *WriteOperation,
) (*File, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get current file state
	file, err := store.GetEntry(ctx.Context, intent.Handle)
	if err != nil {
		return nil, err
	}

	// Verify it's still a regular file
	if file.Type != FileTypeRegular {
		return nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "file type changed after prepare",
			Path:    file.Path,
		}
	}

	// Apply metadata changes
	now := time.Now()

	// Use max(current_size, new_size) to handle concurrent writes completing out of order
	// This prevents a write at an earlier offset from shrinking the file
	if intent.NewSize > file.Size {
		file.Size = intent.NewSize
	}
	file.Mtime = now
	file.Ctime = now

	// POSIX: Clear setuid/setgid bits when a non-root user writes to a file
	// This is a security measure to prevent privilege escalation.
	identity := ctx.Identity
	if identity != nil && identity.UID != nil && *identity.UID != 0 {
		file.Mode &= ^uint32(0o6000) // Clear both setuid (04000) and setgid (02000)
	}

	// Store updated file
	if err := store.PutEntry(ctx.Context, file); err != nil {
		return nil, err
	}

	return file, nil
}

// PrepareReadOp validates a read operation and returns file metadata.
//
// This is the centralized implementation of read preparation that all stores
// should delegate to. It handles:
//   - File type validation (must be regular file)
//   - Permission checking (read permission)
//   - Returning metadata including ContentID for content store
//
// The method does NOT perform actual data reading. The protocol handler
// coordinates between metadata and content stores.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - handle: File handle to read from
//
// Returns:
//   - *ReadMetadata: Contains file attributes including ContentID
//   - error: ErrNotFound, ErrAccessDenied, ErrIsDirectory, etc.
func PrepareReadOp(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
) (*ReadMetadata, error) {
	// Check context
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetEntry(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	// Verify it's a regular file
	if file.Type != FileTypeRegular {
		if file.Type == FileTypeDirectory {
			return nil, &StoreError{
				Code:    ErrIsDirectory,
				Message: "cannot read directory",
				Path:    file.Path,
			}
		}
		return nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "cannot read non-regular file",
			Path:    file.Path,
		}
	}

	// Check read permission
	if err := CheckReadPermission(store, ctx, handle); err != nil {
		return nil, err
	}

	// Return read metadata with a copy of attributes
	attrCopy := file.FileAttr
	return &ReadMetadata{
		Attr: &attrCopy,
	}, nil
}

// ReadSymlinkOp reads the target path of a symbolic link.
//
// This is the centralized implementation of symlink reading that all stores
// should delegate to.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - handle: Symlink handle
//
// Returns:
//   - string: Target path
//   - *File: Symlink's file metadata
//   - error: ErrInvalidArgument if not a symlink, etc.
func ReadSymlinkOp(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
) (string, *File, error) {
	// Get file entry
	file, err := store.GetEntry(ctx.Context, handle)
	if err != nil {
		return "", nil, err
	}

	// Verify it's a symlink
	if file.Type != FileTypeSymlink {
		return "", nil, &StoreError{
			Code:    ErrInvalidArgument,
			Message: "not a symbolic link",
			Path:    file.Path,
		}
	}

	return file.LinkTarget, file, nil
}

// SetFileAttributesOp updates file attributes with validation and access control.
//
// This is the centralized implementation of attribute setting that all stores
// should delegate to. It handles:
//   - Permission checking (owner or root)
//   - Attribute validation
//   - Timestamp updates
//
// Only attributes with non-nil pointers in attrs are modified.
//
// Parameters:
//   - store: MetadataStore for CRUD operations
//   - ctx: Authentication context
//   - handle: File handle
//   - attrs: Attributes to set (only non-nil fields are updated)
//
// Returns:
//   - error: ErrAccessDenied, ErrNotFound, etc.
func SetFileAttributesOp(
	store MetadataStore,
	ctx *AuthContext,
	handle FileHandle,
	attrs *SetAttrs,
) error {
	// Get file entry
	file, err := store.GetEntry(ctx.Context, handle)
	if err != nil {
		return err
	}

	// Check permissions: only owner or root can change attributes
	identity := ctx.Identity
	isOwner := identity != nil && identity.UID != nil && *identity.UID == file.UID
	isRoot := identity != nil && identity.UID != nil && *identity.UID == 0

	if !isOwner && !isRoot {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: "permission denied",
			Path:    file.Path,
		}
	}

	now := time.Now()
	modified := false

	// Apply requested changes
	if attrs.Mode != nil {
		file.Mode = *attrs.Mode
		modified = true
	}

	if attrs.UID != nil {
		// Only root can change owner
		if !isRoot {
			return &StoreError{
				Code:    ErrAccessDenied,
				Message: "only root can change owner",
				Path:    file.Path,
			}
		}
		file.UID = *attrs.UID
		modified = true
	}

	if attrs.GID != nil {
		// Root can change to any group
		// Owner can change to their own supplementary groups
		if !isRoot {
			// Check if user is member of target group
			canChangeGID := false
			if identity.GID != nil && *identity.GID == *attrs.GID {
				canChangeGID = true
			}
			if !canChangeGID && identity.HasGID(*attrs.GID) {
				canChangeGID = true
			}
			if !canChangeGID {
				return &StoreError{
					Code:    ErrAccessDenied,
					Message: "not a member of target group",
					Path:    file.Path,
				}
			}
		}
		file.GID = *attrs.GID
		modified = true
	}

	if attrs.Size != nil {
		// Size change requires write permission
		if err := CheckWritePermission(store, ctx, handle); err != nil {
			return err
		}
		file.Size = *attrs.Size
		modified = true
	}

	if attrs.Atime != nil {
		file.Atime = *attrs.Atime
		modified = true
	}

	if attrs.Mtime != nil {
		file.Mtime = *attrs.Mtime
		modified = true
	}

	// Always update ctime when attributes change
	if modified {
		file.Ctime = now
		return store.PutEntry(ctx.Context, file)
	}

	return nil
}

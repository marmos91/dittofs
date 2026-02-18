package metadata

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// File Operations (MetadataService methods)
// ============================================================================

// RemoveFile removes a file from its parent directory.
//
// This handles:
//   - Input validation
//   - Permission checking (write on parent)
//   - Sticky bit enforcement
//   - Hard link management (decrement or set nlink=0)
//   - Parent timestamp updates
//
// Important: This method does NOT delete the file's content data.
// The returned File includes PayloadID for caller to coordinate content deletion.
// PayloadID is empty if other hard links still reference the content.
//
// POSIX Compliance:
//   - When last link is removed, nlink is set to 0 (not deleted)
//   - This allows fstat() on open file descriptors to return nlink=0
func (s *MetadataService) RemoveFile(ctx *AuthContext, parentHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
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
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
		return nil, err
	}

	// Get child handle
	fileHandle, err := store.GetChild(ctx.Context, parentHandle, name)
	if err != nil {
		return nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, fileHandle)
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

	// Execute all write operations in a single transaction for better performance.
	err = store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Handle link count
		if linkCount > 1 {
			// File has other hard links, just decrement count
			// Empty PayloadID signals caller NOT to delete content
			returnFile.PayloadID = ""
			returnFile.Nlink = linkCount - 1
			returnFile.Ctime = now

			// Update file's link count and ctime
			if err := tx.SetLinkCount(ctx.Context, fileHandle, linkCount-1); err != nil {
				return err
			}

			// Update file's ctime
			file.Ctime = now
			if err := tx.PutFile(ctx.Context, file); err != nil {
				return err
			}
		} else {
			// Last link - set nlink=0 but keep metadata for POSIX compliance
			returnFile.Nlink = 0
			returnFile.Ctime = now

			// Set link count to 0
			if err := tx.SetLinkCount(ctx.Context, fileHandle, 0); err != nil {
				return err
			}

			// Update file's ctime and nlink
			file.Ctime = now
			file.Nlink = 0
			if err := tx.PutFile(ctx.Context, file); err != nil {
				return err
			}
		}

		// Remove from parent's children
		if err := tx.DeleteChild(ctx.Context, parentHandle, name); err != nil {
			return err
		}

		// Update parent timestamps
		parent.Mtime = now
		parent.Ctime = now
		return tx.PutFile(ctx.Context, parent)
	})

	if err != nil {
		return nil, err
	}

	return returnFile, nil
}

// Lookup resolves a name within a directory to a file handle and attributes.
//
// This handles:
//   - Special names: "." (current dir), ".." (parent dir)
//   - Permission checking (execute on directory for search)
//   - Name resolution in directory
func (s *MetadataService) Lookup(ctx *AuthContext, dirHandle FileHandle, name string) (*File, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
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
	if err := s.checkExecutePermission(ctx, dirHandle); err != nil {
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
		return store.GetFile(ctx.Context, parentHandle)
	}

	// Regular name lookup
	childHandle, err := store.GetChild(ctx.Context, dirHandle, name)
	if err != nil {
		return nil, err
	}

	return store.GetFile(ctx.Context, childHandle)
}

// CreateFile creates a new regular file in a directory.
func (s *MetadataService) CreateFile(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, error) {
	return s.createEntry(ctx, parentHandle, name, attr, FileTypeRegular, "", 0, 0)
}

// CreateSymlink creates a new symbolic link in a directory.
func (s *MetadataService) CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (*File, error) {
	// Validate symlink target
	if err := ValidateSymlinkTarget(target); err != nil {
		return nil, err
	}

	return s.createEntry(ctx, parentHandle, name, attr, FileTypeSymlink, target, 0, 0)
}

// CreateSpecialFile creates a special file (device, socket, or FIFO).
func (s *MetadataService) CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, deviceMajor, deviceMinor uint32) (*File, error) {
	// Validate special file type
	if err := ValidateSpecialFileType(fileType); err != nil {
		return nil, err
	}

	// Check if user is root (required for device files)
	if fileType == FileTypeBlockDevice || fileType == FileTypeCharDevice {
		if err := RequiresRoot(ctx); err != nil {
			return nil, err
		}
	}

	return s.createEntry(ctx, parentHandle, name, attr, fileType, "", deviceMajor, deviceMinor)
}

// CreateHardLink creates a hard link to an existing file.
func (s *MetadataService) CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) error {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return err
	}
	if dir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Validate full path length (POSIX PATH_MAX compliance)
	fullPath := buildPath(dir.Path, name)
	if err := ValidatePath(fullPath); err != nil {
		return err
	}

	// Check write permission on directory
	if err := s.checkWritePermission(ctx, dirHandle); err != nil {
		return err
	}

	// Get target file
	target, err := store.GetFile(ctx.Context, targetHandle)
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

	// Execute all write operations in a single transaction for better performance.
	return store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Add to directory's children
		if err := tx.SetChild(ctx.Context, dirHandle, name, targetHandle); err != nil {
			return err
		}

		// Increment target's link count
		linkCount, _ := tx.GetLinkCount(ctx.Context, targetHandle)
		if err := tx.SetLinkCount(ctx.Context, targetHandle, linkCount+1); err != nil {
			return err
		}

		// Update timestamps
		now := time.Now()
		target.Ctime = now
		if err := tx.PutFile(ctx.Context, target); err != nil {
			return err
		}

		dir.Mtime = now
		dir.Ctime = now
		return tx.PutFile(ctx.Context, dir)
	})
}

// ReadSymlink reads the target path of a symbolic link.
func (s *MetadataService) ReadSymlink(ctx *AuthContext, handle FileHandle) (string, *File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return "", nil, err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
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

// SetFileAttributes updates file attributes with validation and access control.
//
// Only attributes with non-nil pointers in attrs are modified.
func (s *MetadataService) SetFileAttributes(ctx *AuthContext, handle FileHandle, attrs *SetAttrs) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return err
	}

	// Check permissions based on what's being changed
	identity := ctx.Identity
	isOwner := identity != nil && identity.UID != nil && *identity.UID == file.UID
	isRoot := identity != nil && identity.UID != nil && *identity.UID == 0

	// POSIX: For utimensat() with UTIME_NOW, write permission is sufficient.
	// Check if we're ONLY setting times to "now" (no other attribute changes).
	onlySettingTimesToNow := attrs.Mode == nil && attrs.UID == nil &&
		attrs.GID == nil && attrs.Size == nil &&
		(attrs.AtimeNow || attrs.MtimeNow)

	if onlySettingTimesToNow && !isOwner && !isRoot {
		// Check write permission instead of ownership
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return &StoreError{
				Code:    ErrPermissionDenied,
				Message: "operation not permitted",
				Path:    file.Path,
			}
		}
		// Write permission granted, allow timestamp update
	} else if !isOwner && !isRoot {
		return &StoreError{
			Code:    ErrPermissionDenied,
			Message: "operation not permitted",
			Path:    file.Path,
		}
	}

	now := time.Now()
	modified := false

	// Apply requested changes
	if attrs.Mode != nil {
		newMode := *attrs.Mode

		// POSIX: Non-root users cannot set SUID/SGID bits arbitrarily
		// - SUID (04000) can only be set by owner or root
		// - SGID (02000) can only be set by owner who is member of file's group, or root
		if !isRoot {
			// Strip SUID bit if caller doesn't own the file
			if newMode&0o4000 != 0 && !isOwner {
				newMode &= ^uint32(0o4000)
			}
			// Strip SGID bit if caller is not a member of the file's group
			if newMode&0o2000 != 0 {
				// For SGID, caller must be owner AND member of file's group
				if !isOwner || !identity.HasGID(file.GID) {
					newMode &= ^uint32(0o2000)
				}
			}
		}

		file.Mode = newMode

		// RFC 7530 Section 6.4.1: chmod adjusts OWNER@/GROUP@/EVERYONE@ ACEs
		// to match the new mode bits when an ACL is present.
		if file.ACL != nil {
			file.ACL = acl.AdjustACLForMode(file.ACL, newMode)
		}

		modified = true
	}

	// Track if ownership changed (for SUID/SGID clearing)
	ownershipChanged := false

	if attrs.UID != nil {
		// Only root can change owner to a different UID
		// Owner can set UID to their own UID (no-op for chown(file, same_uid, new_gid))
		logger.Debug("SetFileAttributes: UID change requested",
			"handle", fmt.Sprintf("%x", handle),
			"file_id", file.ID,
			"file_path", file.Path,
			"old_uid", file.UID,
			"new_uid", *attrs.UID,
			"is_root", isRoot)
		if *attrs.UID != file.UID && !isRoot {
			logger.Debug("SetFileAttributes: UID change DENIED (not root)",
				"handle", fmt.Sprintf("%x", handle),
				"file_id", file.ID,
				"file_path", file.Path,
				"old_uid", file.UID,
				"new_uid", *attrs.UID)
			return &StoreError{
				Code:    ErrPermissionDenied,
				Message: "only root can change owner",
				Path:    file.Path,
			}
		}
		if *attrs.UID != file.UID {
			logger.Debug("SetFileAttributes: UID changed",
				"handle", fmt.Sprintf("%x", handle),
				"file_id", file.ID,
				"file_path", file.Path,
				"old_uid", file.UID,
				"new_uid", *attrs.UID)
			file.UID = *attrs.UID
			modified = true
			ownershipChanged = true
		}
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
					Code:    ErrPermissionDenied,
					Message: "not a member of target group",
					Path:    file.Path,
				}
			}
		}
		if *attrs.GID != file.GID {
			file.GID = *attrs.GID
			modified = true
			ownershipChanged = true
		}
	}

	// POSIX: Clear SUID/SGID bits when ownership changes on non-directory files
	// This is a security measure to prevent privilege escalation.
	// For directories, SGID has different meaning (inherit group) and should NOT be cleared.
	// For symlinks, permissions aren't used (target permissions matter), so we skip them.
	// Note: This clears SUID/SGID regardless of who does the chown (including root),
	// matching Linux kernel behavior.
	if ownershipChanged && file.Type != FileTypeDirectory && file.Type != FileTypeSymlink {
		// Clear SUID (04000) and SGID (02000) bits
		file.Mode &= ^uint32(0o6000)
	}

	if attrs.Size != nil {
		// Size change requires write permission
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return err
		}
		file.Size = *attrs.Size
		modified = true

		// POSIX: Clear SUID/SGID bits on truncate for non-root users (like write)
		if file.Type == FileTypeRegular && !isRoot {
			file.Mode &= ^uint32(0o6000)
		}
	}

	if attrs.Atime != nil {
		file.Atime = *attrs.Atime
		modified = true
	}

	if attrs.Mtime != nil {
		logger.Info("SetFileAttributes: applying mtime change",
			"old_mtime", file.Mtime.Unix(),
			"new_mtime", attrs.Mtime.Unix(),
			"path", file.Path)
		file.Mtime = *attrs.Mtime
		modified = true
	}

	// Handle ACL setting
	if attrs.ACL != nil {
		if err := acl.ValidateACL(attrs.ACL); err != nil {
			return &StoreError{
				Code:    ErrInvalidArgument,
				Message: fmt.Sprintf("invalid ACL: %v", err),
				Path:    file.Path,
			}
		}
		file.ACL = attrs.ACL
		modified = true
	}

	// Always update ctime when attributes change
	if modified {
		file.Ctime = now
		if err := store.PutFile(ctx.Context, file); err != nil {
			return err
		}

		// Invalidate cached file in pending writes to ensure subsequent
		// writes use fresh attributes (e.g., mode changes for SUID/SGID clearing)
		s.pendingWrites.InvalidateCache(handle)
	}

	return nil
}

// Move moves or renames a file or directory atomically.
func (s *MetadataService) Move(ctx *AuthContext, fromDir FileHandle, fromName string, toDir FileHandle, toName string) error {
	store, err := s.storeForHandle(fromDir)
	if err != nil {
		return err
	}

	// Validate names
	if err := ValidateName(fromName); err != nil {
		return err
	}
	if err := ValidateName(toName); err != nil {
		return err
	}

	// Same directory and same name - no-op (POSIX rename semantics)
	if string(fromDir) == string(toDir) && fromName == toName {
		return nil
	}

	// Get source directory
	srcDir, err := store.GetFile(ctx.Context, fromDir)
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
	dstDir, err := store.GetFile(ctx.Context, toDir)
	if err != nil {
		return err
	}
	if dstDir.Type != FileTypeDirectory {
		return &StoreError{
			Code:    ErrNotDirectory,
			Message: "destination parent is not a directory",
		}
	}

	// Validate destination path length (POSIX PATH_MAX compliance)
	destPath := buildPath(dstDir.Path, toName)
	if err := ValidatePath(destPath); err != nil {
		return err
	}

	// Check write permission on both directories
	if err := s.checkWritePermission(ctx, fromDir); err != nil {
		return err
	}
	if err := s.checkWritePermission(ctx, toDir); err != nil {
		return err
	}

	// Get source file
	srcHandle, err := store.GetChild(ctx.Context, fromDir, fromName)
	if err != nil {
		return err
	}
	srcFile, err := store.GetFile(ctx.Context, srcHandle)
	if err != nil {
		return err
	}

	// Debug: Log Move parameters before sticky bit check
	callerUID := "nil"
	if ctx.Identity != nil && ctx.Identity.UID != nil {
		callerUID = fmt.Sprintf("%d", *ctx.Identity.UID)
	}
	logger.Debug("Move: before sticky bit check",
		"src_dir_handle", fmt.Sprintf("%x", fromDir),
		"src_dir_id", srcDir.ID,
		"src_dir_uid", srcDir.UID,
		"src_dir_mode", fmt.Sprintf("%04o", srcDir.Mode),
		"src_file_handle", fmt.Sprintf("%x", srcHandle),
		"src_file_id", srcFile.ID,
		"src_file_uid", srcFile.UID,
		"caller_uid", callerUID,
		"from_name", fromName,
		"to_name", toName)

	// Check sticky bit on source directory
	if err := CheckStickyBitRestriction(ctx, &srcDir.FileAttr, &srcFile.FileAttr); err != nil {
		return err
	}

	// POSIX: When moving a directory to a different parent from a sticky directory,
	// the caller must own the directory being moved (not just the sticky directory).
	// This is because the ".." link inside the moved directory must be updated,
	// which requires ownership of the directory being moved.
	// See rename(2) man page: "If oldpath refers to a directory, then ... if the
	// sticky bit is set on the directory containing oldpath ... the process must
	// own the file being renamed."
	if srcFile.Type == FileTypeDirectory && string(fromDir) != string(toDir) && srcDir.Mode&ModeSticky != 0 {
		callerUID := ^uint32(0) // Invalid UID
		if ctx.Identity != nil && ctx.Identity.UID != nil {
			callerUID = *ctx.Identity.UID
		}
		// Root can always move directories
		if callerUID != 0 && srcFile.UID != callerUID {
			logger.Debug("Move: cross-directory move denied by sticky bit",
				"reason", "caller does not own directory being moved",
				"src_file_uid", srcFile.UID,
				"caller_uid", callerUID)
			return &StoreError{
				Code:    ErrAccessDenied,
				Message: "sticky bit set: cannot move directory you don't own to different parent",
			}
		}
	}

	// Check if destination exists and gather info before transaction
	var dstHandle FileHandle
	var dstFile *File
	dstHandle, err = store.GetChild(ctx.Context, toDir, toName)
	if err == nil {
		// Destination exists - check compatibility
		dstFile, err = store.GetFile(ctx.Context, dstHandle)
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
			entries, _, err := store.ListChildren(ctx.Context, dstHandle, "", 1)
			if err == nil && len(entries) > 0 {
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
	} else if !IsNotFoundError(err) {
		return err
	}

	// Execute all write operations in a single transaction for better performance.
	return store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Handle destination removal if it exists
		if dstFile != nil {
			// Remove destination
			if dstFile.Type == FileTypeDirectory {
				if err := tx.DeleteFile(ctx.Context, dstHandle); err != nil {
					return err
				}
			} else {
				// For files, decrement link count or set to 0
				// POSIX: ctime must be updated when link count changes
				linkCount, _ := tx.GetLinkCount(ctx.Context, dstHandle)
				now := time.Now()
				if linkCount <= 1 {
					_ = tx.SetLinkCount(ctx.Context, dstHandle, 0)
				} else {
					_ = tx.SetLinkCount(ctx.Context, dstHandle, linkCount-1)
				}
				// Update ctime on the file being unlinked (affects remaining hard links)
				dstFile.Ctime = now
				_ = tx.PutFile(ctx.Context, dstFile)
			}

			// Remove destination from children
			if err := tx.DeleteChild(ctx.Context, toDir, toName); err != nil {
				return err
			}
		}

		// Remove source from old parent
		if err := tx.DeleteChild(ctx.Context, fromDir, fromName); err != nil {
			return err
		}

		// Add source to new parent
		if err := tx.SetChild(ctx.Context, toDir, toName, srcHandle); err != nil {
			return err
		}

		// Update parent reference if directories are different
		if string(fromDir) != string(toDir) {
			// Non-fatal error, ignore
			_ = tx.SetParent(ctx.Context, srcHandle, toDir)

			// Update link counts for directory moves
			if srcFile.Type == FileTypeDirectory {
				// Decrement source parent's link count
				srcLinkCount, _ := tx.GetLinkCount(ctx.Context, fromDir)
				if srcLinkCount > 0 {
					_ = tx.SetLinkCount(ctx.Context, fromDir, srcLinkCount-1)
				}
				// Increment destination parent's link count
				dstLinkCount, _ := tx.GetLinkCount(ctx.Context, toDir)
				_ = tx.SetLinkCount(ctx.Context, toDir, dstLinkCount+1)
			}
		}

		// Update timestamps (non-fatal errors, ignore)
		now := time.Now()
		srcFile.Ctime = now
		_ = tx.PutFile(ctx.Context, srcFile)

		srcDir.Mtime = now
		srcDir.Ctime = now
		_ = tx.PutFile(ctx.Context, srcDir)

		if string(fromDir) != string(toDir) {
			dstDir.Mtime = now
			dstDir.Ctime = now
			// Non-fatal error, ignore
			_ = tx.PutFile(ctx.Context, dstDir)
		}

		return nil
	})
}

// MarkFileAsOrphaned sets a file's link count to 0, marking it as orphaned.
//
// This is used by NFS handlers for "silly rename" behavior.
func (s *MetadataService) MarkFileAsOrphaned(ctx *AuthContext, handle FileHandle) error {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return err
	}

	// Get file entry
	file, err := store.GetFile(ctx.Context, handle)
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
	return store.PutFile(ctx.Context, file)
}

// createEntry is the internal implementation for creating files, directories, symlinks, and special files.
func (s *MetadataService) createEntry(
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
	fileType FileType,
	linkTarget string,
	deviceMajor, deviceMinor uint32,
) (*File, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
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

	// Validate full path length (POSIX PATH_MAX compliance)
	fullPath := buildPath(parent.Path, name)
	if err := ValidatePath(fullPath); err != nil {
		return nil, err
	}

	// Check write permission on parent
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
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
	newHandle, err := store.GenerateHandle(ctx.Context, parent.ShareName, fullPath)
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

	// POSIX SGID inheritance:
	// When parent directory has SGID bit set:
	// 1. New entries inherit parent's GID (not the creating user's primary GID)
	// 2. New directories also get SGID bit set (to propagate the behavior)
	// 3. New regular files do NOT get SGID bit set
	parentHasSGID := parent.Mode&0o2000 != 0
	if parentHasSGID {
		// Inherit GID from parent directory
		newAttr.GID = parent.GID

		// For directories, also inherit SGID bit to propagate the behavior
		if fileType == FileTypeDirectory {
			newAttr.Mode |= 0o2000
		} else {
			// For regular files and other types, ensure SGID is NOT set
			// (it may have been set in the input mode, which would be incorrect)
			newAttr.Mode &= ^uint32(0o2000)
		}
	}

	// POSIX: Validate SUID/SGID bits for non-root users
	// Even during file creation, non-root users cannot arbitrarily set these bits
	identity := ctx.Identity
	isRoot := identity != nil && identity.UID != nil && *identity.UID == 0
	if !isRoot {
		// SUID (04000): Only root can set on new files (owner will be the caller anyway)
		// But we still strip it for non-root to be safe
		if newAttr.Mode&0o4000 != 0 {
			newAttr.Mode &= ^uint32(0o4000)
		}

		// SGID (02000): For regular files, non-root can only set if member of file's group
		// For directories, SGID is allowed (inherited above or explicitly requested)
		if fileType != FileTypeDirectory && newAttr.Mode&0o2000 != 0 {
			if !identity.HasGID(newAttr.GID) {
				newAttr.Mode &= ^uint32(0o2000)
			}
		}
	}

	// Set content ID for regular files
	if fileType == FileTypeRegular {
		newAttr.PayloadID = PayloadID(buildPayloadID(parent.ShareName, fullPath))
	}

	// Set device numbers for block/char devices
	if fileType == FileTypeBlockDevice || fileType == FileTypeCharDevice {
		newAttr.Rdev = MakeRdev(deviceMajor, deviceMinor)
	}

	// Create the file entry
	newFile := &File{
		ID:        id,
		ShareName: parent.ShareName,
		Path:      fullPath,
		FileAttr:  newAttr,
	}
	newFile.Nlink = GetInitialLinkCount(fileType)

	// Inherit ACL from parent if parent has one
	if parent.ACL != nil {
		isDir := fileType == FileTypeDirectory
		inherited := acl.ComputeInheritedACL(parent.ACL, isDir)
		newFile.ACL = inherited
	}

	// Execute all write operations in a single transaction for better performance.
	// This reduces PostgreSQL round-trips from 6+ to 2 (BEGIN + COMMIT).
	err = store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Store the entry
		if err := tx.PutFile(ctx.Context, newFile); err != nil {
			return err
		}

		// Initialize link count in the store (required for hard link management)
		if err := tx.SetLinkCount(ctx.Context, newHandle, newFile.Nlink); err != nil {
			return err
		}

		// Set parent reference
		if err := tx.SetParent(ctx.Context, newHandle, parentHandle); err != nil {
			return err
		}

		// Add to parent's children
		if err := tx.SetChild(ctx.Context, parentHandle, name, newHandle); err != nil {
			return err
		}

		// For directories, increment parent's link count (new ".." reference)
		if fileType == FileTypeDirectory {
			parentLinkCount, err := tx.GetLinkCount(ctx.Context, parentHandle)
			if err == nil {
				if err := tx.SetLinkCount(ctx.Context, parentHandle, parentLinkCount+1); err != nil {
					return err
				}
			}
		}

		// Update parent timestamps
		now := time.Now()
		parent.Mtime = now
		parent.Ctime = now
		return tx.PutFile(ctx.Context, parent)
	})

	if err != nil {
		return nil, err
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

// buildPayloadID constructs a content ID from share name and path.
func buildPayloadID(shareName, path string) string {
	// Remove leading "/" from path and combine with share name
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(shareName) > 0 && shareName[0] == '/' {
		shareName = shareName[1:]
	}
	return shareName + "/" + path
}

// File represents a file's complete identity and attributes.
type File struct {
	// ID is a unique identifier for this file.
	ID uuid.UUID `json:"id"`

	// ShareName is the share this file belongs to (e.g., "/export").
	ShareName string `json:"share_name"`

	// Path is the full path within the share (e.g., "/documents/report.pdf").
	Path string `json:"path"`

	// FileAttr is embedded for convenient access to attributes.
	FileAttr
}

// FileAttr contains the complete metadata for a file or directory.
type FileAttr struct {
	// Type is the file type (regular, directory, symlink, etc.)
	Type FileType `json:"type"`

	// Mode contains permission bits (0o7777 max)
	Mode uint32 `json:"mode"`

	// UID is the owner user ID
	UID uint32 `json:"uid"`

	// GID is the owner group ID
	GID uint32 `json:"gid"`

	// Nlink is the number of hard links referencing this file.
	Nlink uint32 `json:"nlink"`

	// Size is the file size in bytes
	Size uint64 `json:"size"`

	// Atime is the last access time
	Atime time.Time `json:"atime"`

	// Mtime is the last modification time (content changes)
	Mtime time.Time `json:"mtime"`

	// Ctime is the last change time (metadata changes)
	Ctime time.Time `json:"ctime"`

	// CreationTime is the file creation time (birth time).
	CreationTime time.Time `json:"creation_time"`

	// PayloadID is the identifier for retrieving file content.
	// This is the legacy path-based content identifier (e.g., "{shareName}/{path}").
	// When deduplication is enabled, ObjectID is the primary identifier.
	PayloadID PayloadID `json:"content_id"`

	// ObjectID is the content-addressed identifier for the file's content.
	// This is the SHA-256 hash of the file's content (or Merkle root of chunk hashes).
	// Used for deduplication: files with the same ObjectID share the same content.
	// Zero value indicates the object is not finalized or deduplication is disabled.
	ObjectID ContentHash `json:"object_id,omitempty"`

	// COWSourcePayloadID is the source PayloadID for copy-on-write semantics.
	// When a hard-linked file with finalized content is written to, it gets a new
	// PayloadID and this field tracks where to lazily copy unmodified blocks from.
	// Empty means no COW source (normal file or blocks already copied).
	COWSourcePayloadID PayloadID `json:"cow_source,omitempty"`

	// LinkTarget is the target path for symbolic links
	LinkTarget string `json:"link_target,omitempty"`

	// Rdev contains device major and minor numbers for device files.
	Rdev uint64 `json:"rdev,omitempty"`

	// Hidden indicates if the file should be hidden from directory listings.
	Hidden bool `json:"hidden,omitempty"`

	// ACL is the NFSv4 Access Control List for this file.
	// nil means no ACL is set -- use classic Unix permission check.
	// Non-nil with empty ACEs means an explicit empty ACL (denies all access).
	ACL *acl.ACL `json:"acl,omitempty"`

	// IdempotencyToken for detecting duplicate creation requests.
	IdempotencyToken uint64 `json:"idempotency_token,omitempty"`
}

// SetAttrs specifies which attributes to update in a SetFileAttributes call.
type SetAttrs struct {
	Mode         *uint32
	UID          *uint32
	GID          *uint32
	Size         *uint64
	Atime        *time.Time
	Mtime        *time.Time
	AtimeNow     bool
	MtimeNow     bool
	CreationTime *time.Time
	Hidden       *bool

	// ACL sets the NFSv4 ACL on the file.
	// When non-nil, the ACL is validated (canonical ordering, max ACEs) before applying.
	ACL *acl.ACL
}

// FileType represents the type of a filesystem object.
type FileType int

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
	FileTypeBlockDevice
	FileTypeCharDevice
	FileTypeSocket
	FileTypeFIFO
)

// PayloadID is an identifier for retrieving file content from the content repository.
type PayloadID string

// ============================================================================
// Device Number Helpers
// ============================================================================

// MakeRdev encodes major and minor device numbers into a single Rdev value.
func MakeRdev(major, minor uint32) uint64 {
	return (uint64(major) << 20) | uint64(minor&0xFFFFF)
}

// RdevMajor extracts the major device number from an Rdev value.
func RdevMajor(rdev uint64) uint32 {
	return uint32(rdev >> 20)
}

// RdevMinor extracts the minor device number from an Rdev value.
func RdevMinor(rdev uint64) uint32 {
	return uint32(rdev & 0xFFFFF)
}

// GetInitialLinkCount returns the initial link count for a new file.
func GetInitialLinkCount(fileType FileType) uint32 {
	if fileType == FileTypeDirectory {
		return 2 // . and parent entry
	}
	return 1
}

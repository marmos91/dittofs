package metadata

import (
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

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

	noOwnershipAttrs := attrs.Mode == nil && attrs.UID == nil && attrs.GID == nil

	// POSIX: For utimensat() with UTIME_NOW, write permission is sufficient.
	onlySettingTimesToNow := noOwnershipAttrs && attrs.Size == nil &&
		(attrs.AtimeNow || attrs.MtimeNow)

	// POSIX: truncate() requires write access, not ownership.
	onlySettingSize := noOwnershipAttrs && attrs.Size != nil &&
		!attrs.AtimeNow && !attrs.MtimeNow

	// POSIX: When a non-owner writes to a file with SUID/SGID bits set, those
	// bits must be cleared. The Linux NFS client implements this via
	// file_remove_privs() which sends SETATTR(mode = current & ~06000) before
	// the WRITE. We must allow this SETATTR even from non-owners who have write
	// permission, as long as the ONLY mode change is clearing SUID/SGID bits.
	onlyClearingSuidSgid := false
	if attrs.Mode != nil && attrs.UID == nil && attrs.GID == nil && attrs.Size == nil {
		clearedMode := file.Mode & ^uint32(0o6000)
		if *attrs.Mode == clearedMode && file.Mode&0o6000 != 0 {
			onlyClearingSuidSgid = true
		}
	}

	// Both timestamp-now and truncate-only operations allow write permission
	// as an alternative to ownership (POSIX semantics).
	writePermSufficient := onlySettingTimesToNow || onlySettingSize || onlyClearingSuidSgid

	if writePermSufficient && !isOwner && !isRoot {
		if err := s.checkWritePermission(ctx, handle); err != nil {
			return err
		}
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
		if *attrs.UID != file.UID && !isRoot {
			return &StoreError{
				Code:    ErrPermissionDenied,
				Message: "only root can change owner",
				Path:    file.Path,
			}
		}
		if *attrs.UID != file.UID {
			logger.Debug("SetFileAttributes: UID changed",
				"path", file.Path,
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
			isPrimaryGroup := identity.GID != nil && *identity.GID == *attrs.GID
			if !isPrimaryGroup && !identity.HasGID(*attrs.GID) {
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

		// POSIX: truncate updates mtime and ctime when size changes
		// The server must do this even if the client doesn't send TIME_MODIFY_SET,
		// because POSIX requires it and NFS clients may rely on server-side updates.
		file.Mtime = now
		file.Ctime = now

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

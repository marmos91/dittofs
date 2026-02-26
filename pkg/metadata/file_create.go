package metadata

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// File Creation Operations (MetadataService methods)
// ============================================================================

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

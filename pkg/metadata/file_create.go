package metadata

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// CreateFile creates a new regular file in a directory. The returned DirWcc
// carries the parent's pre/post attributes captured atomically with the create
// (H9).
func (s *Service) CreateFile(ctx *AuthContext, parentHandle FileHandle, name string, attr *FileAttr) (*File, *DirWcc, error) {
	file, wcc, err := s.createEntry(ctx, parentHandle, name, attr, FileTypeRegular, "", 0, 0)
	if err != nil {
		return nil, nil, err
	}
	s.notifyDirChange(shareNameForHandle(parentHandle), parentHandle, lock.DirChangeAddEntry, ctx)
	return file, wcc, nil
}

// CreateSymlink creates a new symbolic link in a directory. The returned DirWcc
// carries the parent's pre/post attributes captured atomically (H9).
func (s *Service) CreateSymlink(ctx *AuthContext, parentHandle FileHandle, name string, target string, attr *FileAttr) (*File, *DirWcc, error) {
	// Validate symlink target
	if err := ValidateSymlinkTarget(target); err != nil {
		return nil, nil, err
	}

	file, wcc, err := s.createEntry(ctx, parentHandle, name, attr, FileTypeSymlink, target, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	s.notifyDirChange(shareNameForHandle(parentHandle), parentHandle, lock.DirChangeAddEntry, ctx)
	return file, wcc, nil
}

// CreateSpecialFile creates a special file (device, socket, or FIFO). The
// returned DirWcc carries the parent's pre/post attributes captured
// atomically (H9).
func (s *Service) CreateSpecialFile(ctx *AuthContext, parentHandle FileHandle, name string, fileType FileType, attr *FileAttr, deviceMajor, deviceMinor uint32) (*File, *DirWcc, error) {
	// Validate special file type
	if err := ValidateSpecialFileType(fileType); err != nil {
		return nil, nil, err
	}

	// Check if user is root (required for device files)
	if fileType == FileTypeBlockDevice || fileType == FileTypeCharDevice {
		if err := RequiresRoot(ctx); err != nil {
			return nil, nil, err
		}
	}

	file, wcc, err := s.createEntry(ctx, parentHandle, name, attr, fileType, "", deviceMajor, deviceMinor)
	if err != nil {
		return nil, nil, err
	}
	s.notifyDirChange(shareNameForHandle(parentHandle), parentHandle, lock.DirChangeAddEntry, ctx)
	return file, wcc, nil
}

// CreateHardLink creates a hard link to an existing file. The returned DirWcc
// carries the link directory's pre/post attributes captured atomically with the
// mutation (H9).
func (s *Service) CreateHardLink(ctx *AuthContext, dirHandle FileHandle, name string, targetHandle FileHandle) (*DirWcc, error) {
	store, err := s.storeForHandle(dirHandle)
	if err != nil {
		return nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	// Get directory entry
	dir, err := store.GetFile(ctx.Context, dirHandle)
	if err != nil {
		return nil, err
	}
	if dir.Type != FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "not a directory",
		}
	}

	// Validate full path length (POSIX PATH_MAX compliance)
	fullPath := buildPath(dir.Path, name)
	if err := ValidatePath(fullPath); err != nil {
		return nil, err
	}

	// Check write permission on directory
	if err := s.checkWritePermission(ctx, dirHandle); err != nil {
		return nil, err
	}

	// Get target file
	target, err := store.GetFile(ctx.Context, targetHandle)
	if err != nil {
		return nil, err
	}

	// Cannot hard link directories
	if target.Type == FileTypeDirectory {
		return nil, &StoreError{
			Code:    ErrIsDirectory,
			Message: "cannot create hard link to directory",
		}
	}

	// Check if name already exists
	_, err = store.GetChild(ctx.Context, dirHandle, name)
	if err == nil {
		return nil, &StoreError{
			Code:    ErrAlreadyExists,
			Message: "file already exists",
			Path:    name,
		}
	}
	if !IsNotFoundError(err) {
		return nil, err
	}

	// wcc brackets the link directory attributes around the mutation, both
	// captured inside the transaction below (H9).
	wcc := &DirWcc{}

	// Execute all write operations in a single transaction for better performance.
	err = store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// Re-read the directory inside the transaction so the pre-op snapshot and
		// the timestamp mutation derive from the same committed state.
		if txDir, dErr := tx.GetFile(ctx.Context, dirHandle); dErr == nil && txDir != nil {
			dir = txDir
		}
		wcc.Before = CopyFileAttr(&dir.FileAttr)

		// Re-check existence inside the transaction to close the TOCTOU race
		// between the outer GetChild and this SetChild (same pattern as
		// createEntry).
		if _, innerErr := tx.GetChild(ctx.Context, dirHandle, name); innerErr == nil {
			return &StoreError{
				Code:    ErrAlreadyExists,
				Message: "file already exists",
				Path:    name,
			}
		}

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
		wcc.After = CopyFileAttr(&dir.FileAttr)
		return tx.PutFile(ctx.Context, dir)
	})
	if err != nil {
		return nil, err
	}

	s.notifyDirChange(shareNameForHandle(dirHandle), dirHandle, lock.DirChangeAddEntry, ctx)
	return wcc, nil
}

// createEntry is the internal implementation for creating files, directories, symlinks, and special files.
func (s *Service) createEntry(
	ctx *AuthContext,
	parentHandle FileHandle,
	name string,
	attr *FileAttr,
	fileType FileType,
	linkTarget string,
	deviceMajor, deviceMinor uint32,
) (*File, *DirWcc, error) {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return nil, nil, err
	}

	// Validate name
	if err := ValidateName(name); err != nil {
		return nil, nil, err
	}

	// Get parent entry
	parent, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return nil, nil, err
	}

	// Verify parent is a directory
	if parent.Type != FileTypeDirectory {
		return nil, nil, &StoreError{
			Code:    ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Validate full path length (POSIX PATH_MAX compliance)
	fullPath := buildPath(parent.Path, name)
	if err := ValidatePath(fullPath); err != nil {
		return nil, nil, err
	}

	// Check create permission on parent. Use the precise NFSv4 mask for the
	// child being created (ADD_FILE vs ADD_SUBDIRECTORY) so a DACL that
	// denies only one variant does not also block the other — required by
	// MS-FSA 2.1.5.1.1 and smbtorture smb2.create.mkdir-visible.
	if err := s.CheckParentCreateAccess(ctx, parentHandle, fileType == FileTypeDirectory); err != nil {
		return nil, nil, err
	}

	// Check if name already exists
	_, err = store.GetChild(ctx.Context, parentHandle, name)
	if err == nil {
		return nil, nil, &StoreError{
			Code:    ErrAlreadyExists,
			Message: "file already exists",
			Path:    name,
		}
	}
	// If error is not ErrNotFound, it's a real error
	if !IsNotFoundError(err) {
		return nil, nil, err
	}

	// Generate new handle
	newHandle, err := store.GenerateHandle(ctx.Context, parent.ShareName, fullPath)
	if err != nil {
		return nil, nil, err
	}

	// Decode handle to get ID
	_, id, err := DecodeFileHandle(newHandle)
	if err != nil {
		return nil, nil, err
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
		// SUID (04000): Only root can set on new files
		newAttr.Mode &= ^uint32(0o4000)

		// SGID (02000): For regular files, non-root can only set if member of file's group.
		// For directories, SGID is allowed (inherited above or explicitly requested).
		if fileType != FileTypeDirectory && newAttr.Mode&0o2000 != 0 && !identity.HasGID(newAttr.GID) {
			newAttr.Mode &= ^uint32(0o2000)
		}
	}

	// Set content ID for regular files
	if fileType == FileTypeRegular {
		newAttr.PayloadID = PayloadID(buildPayloadID(parent.ShareName, id))
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

	// Per-identity inode (file-count) quota enforcement. Only regular files
	// contribute to usage counters, so only their creation is gated here.
	// Charged to the new file's owner (newAttr.UID/GID). Runs before the
	// transaction so a rejected create never touches the store. Returns
	// ErrQuotaExceeded (-> DQUOT / STATUS_QUOTA_EXCEEDED).
	if fileType == FileTypeRegular {
		if err := s.checkIdentityQuotas(parent.ShareName, store, newAttr.UID, newAttr.GID, int64(newAttr.Size), 1); err != nil {
			return nil, nil, err
		}
	}

	// Inherit ACL from parent if parent has one. CREATOR_OWNER / CREATOR_GROUP
	// placeholders are substituted with the requester's frozen identity per
	// MS-DTYP §2.5.3.4. For anonymous creates (no UID/GID on the identity)
	// we pass a zero Creator, which substitutes "0@localdomain" — acceptable
	// for the rare anonymous-into-CREATOR_OWNER-DACL case.
	if parent.ACL != nil {
		isDir := fileType == FileTypeDirectory
		var creator acl.Creator
		if ctx != nil && ctx.Identity != nil {
			if ctx.Identity.UID != nil {
				creator.UID = *ctx.Identity.UID
			}
			if ctx.Identity.GID != nil {
				creator.GID = *ctx.Identity.GID
			}
			if ctx.Identity.SID != nil {
				creator.SID = *ctx.Identity.SID
			}
		}
		inherited := acl.ComputeInheritedACL(parent.ACL, isDir, creator)
		newFile.ACL = inherited
	}

	// wcc brackets the parent directory attributes around the mutation, both
	// captured inside the transaction below so they atomically describe the
	// state immediately before and after the create (H9).
	wcc := &DirWcc{}

	// Overlay any coalesced (not-yet-persisted) parent-directory timestamps onto
	// the pre-op snapshot so WCC.Before reflects what readers currently observe
	// (#1573). Captured before the transaction: the create no longer reads or
	// writes the parent inode, so there is nothing to re-snapshot inside it.
	s.mergeDirTimes(parentHandle, &parent.FileAttr)
	wcc.Before = CopyFileAttr(&parent.FileAttr)
	now := time.Now()

	// Execute the child-creating writes in a single transaction. Crucially this
	// transaction does NOT read or write the parent inode: the parent mtime bump
	// used to make every concurrent same-dir create read+write one shared key,
	// which BadgerDB SSI aborts as a conflict, serializing them on retry-backoff
	// (#1573). Touching only the new child's disjoint keys lets group-commit
	// batch concurrent creates; the parent timestamp is coalesced below.
	err = store.WithTransaction(ctx.Context, func(tx Transaction) error {
		// TOCTOU guard: re-check inside transaction.
		if _, innerErr := tx.GetChild(ctx.Context, parentHandle, name); innerErr == nil {
			return &StoreError{
				Code:    ErrAlreadyExists,
				Message: "file already exists",
				Path:    name,
			}
		}

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

		// For directories, increment parent's link count (new ".." reference).
		// This still reads+writes the parent link-count key, so concurrent
		// mkdirs in one directory serialize on it — acceptable, mkdir is rare
		// relative to file create, which is fully disjoint above.
		if fileType == FileTypeDirectory {
			parentLinkCount, err := tx.GetLinkCount(ctx.Context, parentHandle)
			if err == nil {
				if err := tx.SetLinkCount(ctx.Context, parentHandle, parentLinkCount+1); err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	// Coalesce the parent directory timestamp bump. Per MS-FSA 2.1.4.4 a
	// directory modified by adding an entry updates Mtime, Ctime and Atime.
	s.recordDirTimes(ctx.Context, parentHandle, now)
	parent.Mtime = now
	parent.Ctime = now
	parent.Atime = now
	wcc.After = CopyFileAttr(&parent.FileAttr)

	return newFile, wcc, nil
}

package badger

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Lookup resolves a name within a directory to a file handle and attributes.
//
// This is the fundamental operation for path resolution, combining directory
// search, permission checking, and attribute retrieval into a single atomic
// operation using a BadgerDB read transaction.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - dirHandle: Directory to search in
//   - name: Name to resolve (including "." and "..")
//
// Returns:
//   - *File: Complete file information (ID, ShareName, Path, and all attributes)
//   - error: ErrNotFound, ErrNotDirectory, ErrAccessDenied, or context errors
func (s *BadgerMetadataStore) Lookup(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Decode directory handle to get UUID
	_, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	var targetFile *metadata.File

	err = s.db.View(func(txn *badger.Txn) error {
		// Get directory data
		item, err := txn.Get(keyFile(dirID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get directory: %w", err)
		}

		var dirFile *metadata.File
		err = item.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			dirFile = f
			return nil
		})
		if err != nil {
			return err
		}

		// Verify it's a directory
		if dirFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
			}
		}

		// Check execute/traverse permission on directory (search permission)
		granted, err := s.CheckPermissions(ctx, dirHandle, metadata.PermissionTraverse)
		if err != nil {
			return err
		}
		if granted&metadata.PermissionTraverse == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no search permission on directory",
			}
		}

		// Handle special names
		switch name {
		case ".":
			// Current directory - return directory itself
			targetFile = dirFile

		case "..":
			// Parent directory
			parentItem, err := txn.Get(keyParent(dirID))
			if err == badger.ErrKeyNotFound {
				// This is a root directory, ".." refers to itself
				targetFile = dirFile
			} else if err != nil {
				return fmt.Errorf("failed to get parent: %w", err)
			} else {
				var parentID uuid.UUID
				err = parentItem.Value(func(val []byte) error {
					parentID, err = uuid.FromBytes(val)
					return err
				})
				if err != nil {
					return err
				}

				// Get parent file
				parentFileItem, err := txn.Get(keyFile(parentID))
				if err != nil {
					return fmt.Errorf("failed to get parent file: %w", err)
				}
				err = parentFileItem.Value(func(val []byte) error {
					parentFile, err := decodeFile(val)
					if err != nil {
						return err
					}
					targetFile = parentFile
					return nil
				})
				if err != nil {
					return err
				}
			}

		default:
			// Regular name lookup
			childItem, err := txn.Get(keyChild(dirID, name))
			if err == badger.ErrKeyNotFound {
				return &metadata.StoreError{
					Code:    metadata.ErrNotFound,
					Message: fmt.Sprintf("name not found: %s", name),
					Path:    name,
				}
			}
			if err != nil {
				return fmt.Errorf("failed to lookup child: %w", err)
			}

			var childID uuid.UUID
			err = childItem.Value(func(val []byte) error {
				childID, err = uuid.FromBytes(val)
				return err
			})
			if err != nil {
				return err
			}

			// Get child file
			childFileItem, err := txn.Get(keyFile(childID))
			if err == badger.ErrKeyNotFound {
				return &metadata.StoreError{
					Code:    metadata.ErrNotFound,
					Message: "target file not found",
				}
			}
			if err != nil {
				return fmt.Errorf("failed to get child file: %w", err)
			}

			err = childFileItem.Value(func(val []byte) error {
				childFile, err := decodeFile(val)
				if err != nil {
					return err
				}
				targetFile = childFile
				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return targetFile, nil
}

// GetFile retrieves complete file information by handle.
//
// This is a lightweight operation that only reads metadata without permission
// checking using a BadgerDB read transaction.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: The file handle to query
//
// Returns:
//   - *File: Complete file information (ID, ShareName, Path, and all attributes)
//   - error: ErrNotFound, ErrInvalidHandle, or context errors
func (s *BadgerMetadataStore) GetFile(
	ctx context.Context,
	handle metadata.FileHandle,
) (*metadata.File, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	var file *metadata.File

	err = s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(keyFile(fileID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		return item.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			file = f
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	return file, nil
}

// GetShareNameForHandle returns the share name for a given file handle.
//
// This works with UUID-based handles by decoding the handle format.
//
// Parameters:
//   - ctx: Context for cancellation
//   - handle: File handle (UUID-based)
//
// Returns:
//   - string: Share name the file belongs to
//   - error: ErrNotFound if handle is invalid, context errors
func (s *BadgerMetadataStore) GetShareNameForHandle(
	ctx context.Context,
	handle metadata.FileHandle,
) (string, error) {
	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Decode handle to extract share name
	shareName, _, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	return shareName, nil
}

// SetFileAttributes updates file attributes with validation and access control.
//
// This implements selective attribute updates based on the Set* flags in attrs,
// using a BadgerDB write transaction for atomicity.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - handle: The file handle to update
//   - attrs: Attributes to set (only non-nil fields are modified)
//
// Returns:
//   - error: ErrNotFound, ErrAccessDenied, ErrPermissionDenied, etc.
func (s *BadgerMetadataStore) SetFileAttributes(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	attrs *metadata.SetAttrs,
) error {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Decode handle to get UUID
	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Lock file to serialize concurrent attribute modifications
	mu := s.lockFile(fileID.String())
	defer s.unlockFile(fileID.String(), mu)

	return s.db.Update(func(txn *badger.Txn) error {
		// Get file data
		item, err := txn.Get(keyFile(fileID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get file: %w", err)
		}

		var file *metadata.File
		err = item.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			file = f
			return nil
		})
		if err != nil {
			return err
		}

		identity := ctx.Identity
		if identity == nil || identity.UID == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "authentication required to modify attributes",
			}
		}

		uid := *identity.UID
		isOwner := uid == file.UID
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
			if !isRoot && file.Type == metadata.FileTypeRegular {
				// Check if user is a member of the file's group
				isMemberOfFileGroup := false
				if identity.GID != nil && *identity.GID == file.GID {
					isMemberOfFileGroup = true
				}
				if !isMemberOfFileGroup && identity.HasGID(file.GID) {
					isMemberOfFileGroup = true
				}

				// Clear setgid bit if user is not a member of the file's group
				if !isMemberOfFileGroup {
					newMode &= ^uint32(0o2000) // Clear S_ISGID
				}
			}

			file.Mode = newMode
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
				if newUID != file.UID {
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
				if newUID != file.UID {
					file.UID = newUID
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
				if newGID != file.GID {
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

					file.GID = newGID
					changed = true
				}
				// Owner setting GID to same value: allowed (no-op)
			} else {
				// Root can change to any group
				if newGID != file.GID {
					file.GID = newGID
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
			if file.Type != metadata.FileTypeDirectory && file.Type != metadata.FileTypeSymlink {
				file.Mode &= ^uint32(0o6000) // Clear both setuid (04000) and setgid (02000)
				changed = true
			}
		}

		// Validate and apply size changes
		if attrs.Size != nil {
			if file.Type != metadata.FileTypeRegular {
				return &metadata.StoreError{
					Code:    metadata.ErrInvalidArgument,
					Message: "cannot change size of non-regular file",
				}
			}

			// Check write permission
			granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
			if err != nil {
				return err
			}
			if granted&metadata.PermissionWrite == 0 {
				return &metadata.StoreError{
					Code:    metadata.ErrAccessDenied,
					Message: "no write permission",
				}
			}

			file.Size = *attrs.Size
			file.Mtime = time.Now()
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
					granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
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

			file.Atime = *attrs.Atime
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
					granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
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

			file.Mtime = *attrs.Mtime
			changed = true
		}

		// Apply CreationTime changes (SMB/Windows only)
		if attrs.CreationTime != nil {
			if !isOwner && !isRoot {
				granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
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

			file.CreationTime = *attrs.CreationTime
			changed = true
		}

		// Apply Hidden attribute changes (SMB/Windows only)
		// Hidden can be set by owner or anyone with write permission
		if attrs.Hidden != nil {
			if !isOwner && !isRoot {
				granted, err := s.CheckPermissions(ctx, handle, metadata.PermissionWrite)
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

			file.Hidden = *attrs.Hidden
			changed = true
		}

		// Update ctime if any changes were made
		if changed {
			file.Ctime = time.Now()

			// Store updated file data
			fileBytes, err := encodeFile(file)
			if err != nil {
				return err
			}
			if err := txn.Set(keyFile(fileID), fileBytes); err != nil {
				return fmt.Errorf("failed to update file data: %w", err)
			}
		}

		return nil
	})
}

// Create creates a new file or directory.
//
// The type is determined by attr.Type (must be FileTypeRegular or FileTypeDirectory).
// This uses a BadgerDB write transaction to ensure atomicity of all related operations.
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - parentHandle: Handle of the parent directory
//   - name: Name for the new file/directory
//   - attr: Attributes including Type (must be Regular or Directory)
//
// Returns:
//   - *File: Complete file information for the newly created file/directory
//   - error: ErrInvalidArgument, ErrAccessDenied, ErrAlreadyExists, or other errors
func (s *BadgerMetadataStore) Create(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Check context cancellation
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

	// Decode parent handle
	shareName, parentID, err := metadata.DecodeFileHandle(parentHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Lock parent directory to serialize concurrent creates in the same directory
	mu := s.lockDir(parentID.String())
	defer s.unlockDir(parentID.String(), mu)

	var newFile *metadata.File

	err = s.db.Update(func(txn *badger.Txn) error {
		// Verify parent exists and is a directory
		item, err := txn.Get(keyFile(parentID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "parent directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get parent: %w", err)
		}

		var parentFile *metadata.File
		err = item.Value(func(val []byte) error {
			pf, err := decodeFile(val)
			if err != nil {
				return err
			}
			parentFile = pf
			return nil
		})
		if err != nil {
			return err
		}

		if parentFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "parent is not a directory",
			}
		}

		// Check write permission on parent
		granted, err := s.CheckPermissions(ctx, parentHandle, metadata.PermissionWrite)
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on parent directory",
			}
		}

		// Check if name already exists
		_, err = txn.Get(keyChild(parentID, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Generate UUID for new file
		newID := uuid.New()

		// Build full path
		fullPath := buildFullPath(parentFile.Path, name)

		// Validate path length (POSIX PATH_MAX = 4096)
		if err := metadata.ValidatePath(fullPath); err != nil {
			return err
		}

		// Apply defaults for mode, UID/GID, timestamps, and size
		metadata.ApplyCreateDefaults(attr, ctx, "")

		// Create file
		newFile = &metadata.File{
			ID:        newID,
			ShareName: shareName,
			Path:      fullPath,
			FileAttr: metadata.FileAttr{
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
			},
		}

		// Type-specific initialization
		if attr.Type == metadata.FileTypeRegular {
			// Generate ContentID for regular files
			contentID := buildContentID(shareName, fullPath)
			newFile.ContentID = metadata.ContentID(contentID)

			// Log ContentID generation for debugging
			logger.Debug("Generated ContentID", "name", name, "path", fullPath, "content_id", contentID)
		} else {
			newFile.ContentID = ""
		}

		// Set link count (store in both File.Nlink and separate key for consistency)
		var linkCount uint32
		if attr.Type == metadata.FileTypeDirectory {
			linkCount = 2 // "." and parent entry
			// Increment parent's link count (new subdirectory adds ".." reference to parent)
			parentFile.Nlink++
		} else {
			linkCount = 1
		}
		newFile.Nlink = linkCount

		// Store file
		fileBytes, err := encodeFile(newFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(newID), fileBytes); err != nil {
			return fmt.Errorf("failed to store file: %w", err)
		}

		// Also store link count separately for efficient updates
		if err := txn.Set(keyLinkCount(newID), encodeUint32(linkCount)); err != nil {
			return fmt.Errorf("failed to store link count: %w", err)
		}

		// Update parent link count if we created a directory
		if attr.Type == metadata.FileTypeDirectory {
			if err := txn.Set(keyLinkCount(parentID), encodeUint32(parentFile.Nlink)); err != nil {
				return fmt.Errorf("failed to update parent link count: %w", err)
			}
		}

		// Add to parent's children (store UUID bytes)
		if err := txn.Set(keyChild(parentID, name), newID[:]); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Set parent relationship (store UUID bytes)
		if err := txn.Set(keyParent(newID), parentID[:]); err != nil {
			return fmt.Errorf("failed to set parent: %w", err)
		}

		// Update parent timestamps
		parentFile.Mtime = attr.Mtime
		parentFile.Ctime = attr.Ctime
		parentBytes, err := encodeFile(parentFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(parentID), parentBytes); err != nil {
			return fmt.Errorf("failed to update parent: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return newFile, nil
}

// CreateHardLink creates a hard link to an existing file.
//
// TODO: Hard links are 90% implemented but CreateHardLink is not yet exposed.
// Note: Move/Rename IS fully implemented (see Move() function).
//
// Current state:
// - ✅ Link counts are tracked internally (keyLinkCount)
// - ✅ Link counts initialized properly (1 for files, 2 for directories)
// - ✅ Link counts decremented on file removal (see remove.go)
// - ✅ Files only deleted when link count reaches 0
// - ✅ Capabilities report SupportsHardLinks: true
// - ❌ CreateHardLink not implemented (this function)
// - ❌ Link counts not exposed in FileAttr struct (always reported as 1 to NFS clients)
//
// To complete hard link support:
// 1. Implement this function to create additional child entries pointing to same file UUID
// 2. Increment link count when creating hard link
// 3. Add Nlink field to FileAttr (see pkg/store/metadata/file.go:89)
// 4. Read and return link counts in GetFileAttributes and similar operations
// 5. Update NFS protocol layer to use FileAttr.Nlink instead of hardcoding 1
//
// For now, this returns ErrNotSupported to indicate the feature is not yet fully exposed.
func (s *BadgerMetadataStore) CreateHardLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	targetHandle metadata.FileHandle,
) error {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return err
	}

	// Decode directory handle
	_, dirID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	// Decode target handle
	_, targetID, err := metadata.DecodeFileHandle(targetHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid target handle",
		}
	}

	// Lock directory to serialize concurrent hard link operations in the same directory
	mu := s.lockDir(dirID.String())
	defer s.unlockDir(dirID.String(), mu)

	return s.db.Update(func(txn *badger.Txn) error {
		// Verify directory exists and is a directory
		item, err := txn.Get(keyFile(dirID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get directory: %w", err)
		}

		var dirFile *metadata.File
		err = item.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			dirFile = f
			return nil
		})
		if err != nil {
			return err
		}

		if dirFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "not a directory",
			}
		}

		// Verify target exists
		targetItem, err := txn.Get(keyFile(targetID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "target file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get target file: %w", err)
		}

		var targetFile *metadata.File
		err = targetItem.Value(func(val []byte) error {
			f, err := decodeFile(val)
			if err != nil {
				return err
			}
			targetFile = f
			return nil
		})
		if err != nil {
			return err
		}

		// Cannot hard link directories
		if targetFile.Type == metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrIsDirectory,
				Message: "cannot create hard link to directory",
			}
		}

		// Validate path length (POSIX PATH_MAX = 4096)
		linkPath := buildFullPath(dirFile.Path, name)
		if err := metadata.ValidatePath(linkPath); err != nil {
			return err
		}

		// Check write permission on directory
		granted, err := s.CheckPermissions(ctx, dirHandle, metadata.PermissionWrite)
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
		_, err = txn.Get(keyChild(dirID, name))
		if err == nil {
			return &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: fmt.Sprintf("name already exists: %s", name),
				Path:    name,
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check child existence: %w", err)
		}

		// Get current link count
		var currentLinks uint32
		linkItem, err := txn.Get(keyLinkCount(targetID))
		if err == badger.ErrKeyNotFound {
			// If link count doesn't exist, initialize to 1
			currentLinks = 1
		} else if err != nil {
			return fmt.Errorf("failed to get link count: %w", err)
		} else {
			err = linkItem.Value(func(val []byte) error {
				currentLinks, err = decodeUint32(val)
				return err
			})
			if err != nil {
				return err
			}
		}

		// Check link count limit
		if currentLinks >= s.capabilities.MaxHardLinkCount {
			return &metadata.StoreError{
				Code:    metadata.ErrNotSupported,
				Message: "maximum hard link count reached",
			}
		}

		// Add to directory's children (point to the same target UUID)
		if err := txn.Set(keyChild(dirID, name), targetID[:]); err != nil {
			return fmt.Errorf("failed to add child: %w", err)
		}

		// Increment link count
		newLinkCount := currentLinks + 1
		if err := txn.Set(keyLinkCount(targetID), encodeUint32(newLinkCount)); err != nil {
			return fmt.Errorf("failed to update link count: %w", err)
		}

		// Update timestamps
		now := time.Now()

		// Target file's metadata changed (ctime and Nlink)
		targetFile.Ctime = now
		targetFile.Nlink = newLinkCount
		targetBytes, err := encodeFile(targetFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(targetID), targetBytes); err != nil {
			return fmt.Errorf("failed to update target file: %w", err)
		}

		// Directory contents changed (mtime and ctime)
		dirFile.Mtime = now
		dirFile.Ctime = now
		dirBytes, err := encodeFile(dirFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(dirID), dirBytes); err != nil {
			return fmt.Errorf("failed to update directory: %w", err)
		}

		return nil
	})
}

// Move moves or renames a file or directory atomically.
//
// This operation performs a full atomic move/rename within a BadgerDB transaction:
// - Validates both source and destination directory handles
// - Checks write permissions on both directories
// - Handles replacement rules:
//   - File can replace file
//   - Directory can only replace empty directory
//   - Directory cannot replace file and vice versa
//
// - Updates parent-child relationships in BadgerDB
// - Updates timestamps (mtime/ctime) on source and destination directories
// - Updates ctime on the moved file/directory
//
// Thread Safety: Safe for concurrent use.
//
// Parameters:
//   - ctx: Authentication context for permission checking
//   - fromDir: Source directory handle
//   - fromName: Current name
//   - toDir: Destination directory handle
//   - toName: New name
//
// Returns:
//   - error: ErrAccessDenied, ErrNotFound, ErrNotEmpty, ErrIsDirectory, etc.
func (s *BadgerMetadataStore) Move(
	ctx *metadata.AuthContext,
	fromDir metadata.FileHandle,
	fromName string,
	toDir metadata.FileHandle,
	toName string,
) error {
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	// Validate names
	if err := metadata.ValidateName(fromName); err != nil {
		return err
	}
	if err := metadata.ValidateName(toName); err != nil {
		return err
	}

	// Decode handles
	_, fromDirID, err := metadata.DecodeFileHandle(fromDir)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid source directory handle",
		}
	}
	_, toDirID, err := metadata.DecodeFileHandle(toDir)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid destination directory handle",
		}
	}

	// Lock both directories in consistent order to prevent deadlock
	locks := s.lockDirsOrdered(fromDirID.String(), toDirID.String())
	defer s.unlockDirs(locks)

	// Perform the move in a transaction
	err = s.db.Update(func(txn *badger.Txn) error {
		// 1. Get and verify source directory
		fromDirItem, err := txn.Get(keyFile(fromDirID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "source directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get source directory: %w", err)
		}

		var fromDirFile *metadata.File
		err = fromDirItem.Value(func(val []byte) error {
			fd, err := decodeFile(val)
			if err != nil {
				return err
			}
			fromDirFile = fd
			return nil
		})
		if err != nil {
			return err
		}

		if fromDirFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "source parent is not a directory",
			}
		}

		// Check write permission on source directory
		granted, err := s.CheckPermissions(ctx, fromDir, metadata.PermissionWrite)
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on source directory",
			}
		}

		// 2. Get and verify destination directory
		toDirItem, err := txn.Get(keyFile(toDirID))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "destination directory not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get destination directory: %w", err)
		}

		var toDirFile *metadata.File
		err = toDirItem.Value(func(val []byte) error {
			fd, err := decodeFile(val)
			if err != nil {
				return err
			}
			toDirFile = fd
			return nil
		})
		if err != nil {
			return err
		}

		if toDirFile.Type != metadata.FileTypeDirectory {
			return &metadata.StoreError{
				Code:    metadata.ErrNotDirectory,
				Message: "destination parent is not a directory",
			}
		}

		// Validate destination path length (POSIX PATH_MAX = 4096)
		destPath := buildFullPath(toDirFile.Path, toName)
		if err := metadata.ValidatePath(destPath); err != nil {
			return err
		}

		// Check write permission on destination directory
		granted, err = s.CheckPermissions(ctx, toDir, metadata.PermissionWrite)
		if err != nil {
			return err
		}
		if granted&metadata.PermissionWrite == 0 {
			return &metadata.StoreError{
				Code:    metadata.ErrAccessDenied,
				Message: "no write permission on destination directory",
			}
		}

		// 3. Get source file
		sourceChildItem, err := txn.Get(keyChild(fromDirID, fromName))
		if err == badger.ErrKeyNotFound {
			return &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "source file not found",
			}
		}
		if err != nil {
			return fmt.Errorf("failed to get source child: %w", err)
		}

		var sourceID uuid.UUID
		err = sourceChildItem.Value(func(val []byte) error {
			id, err := uuid.FromBytes(val)
			if err != nil {
				return fmt.Errorf("failed to decode child UUID: %w", err)
			}
			sourceID = id
			return nil
		})
		if err != nil {
			return err
		}

		// Get source file metadata
		sourceFileItem, err := txn.Get(keyFile(sourceID))
		if err != nil {
			return fmt.Errorf("failed to get source file: %w", err)
		}

		var sourceFile *metadata.File
		err = sourceFileItem.Value(func(val []byte) error {
			fd, err := decodeFile(val)
			if err != nil {
				return err
			}
			sourceFile = fd
			return nil
		})
		if err != nil {
			return err
		}

		// Check sticky bit restriction on source directory
		// When the directory has the sticky bit set, only certain users can rename/delete
		if err := metadata.CheckStickyBitRestriction(ctx, &fromDirFile.FileAttr, &sourceFile.FileAttr); err != nil {
			return err
		}

		// POSIX: When moving a directory to a different parent, updating the ".."
		// entry requires write permission to the source directory itself. Only the
		// directory owner or root can perform this operation. This is separate from
		// the sticky bit check above.
		if sourceFile.Type == metadata.FileTypeDirectory && fromDirID != toDirID {
			callerUID := ^uint32(0) // Default to invalid
			if ctx.Identity != nil && ctx.Identity.UID != nil {
				callerUID = *ctx.Identity.UID
			}

			// Root (UID 0) can always move directories
			// Otherwise, caller must own the source directory
			if callerUID != 0 && callerUID != sourceFile.UID {
				return &metadata.StoreError{
					Code:    metadata.ErrAccessDenied,
					Message: "cannot move directory to different parent: not owner",
				}
			}
		}

		// 4. Check if destination exists
		var destID uuid.UUID
		var destFile *metadata.File
		destChildItem, err := txn.Get(keyChild(toDirID, toName))
		if err == nil {
			// Destination exists - check replacement rules
			err = destChildItem.Value(func(val []byte) error {
				id, err := uuid.FromBytes(val)
				if err != nil {
					return fmt.Errorf("failed to decode dest UUID: %w", err)
				}
				destID = id
				return nil
			})
			if err != nil {
				return err
			}

			destFileItem, err := txn.Get(keyFile(destID))
			if err != nil {
				return fmt.Errorf("failed to get destination file: %w", err)
			}

			err = destFileItem.Value(func(val []byte) error {
				fd, err := decodeFile(val)
				if err != nil {
					return err
				}
				destFile = fd
				return nil
			})
			if err != nil {
				return err
			}

			// Apply replacement rules
			if sourceFile.Type == metadata.FileTypeDirectory {
				// Moving directory: destination must not exist or be empty directory
				if destFile.Type != metadata.FileTypeDirectory {
					return &metadata.StoreError{
						Code:    metadata.ErrAlreadyExists,
						Message: "cannot replace non-directory with directory",
					}
				}

				// Check if destination directory is empty
				prefix := keyChildPrefix(destID)
				opts := badger.DefaultIteratorOptions
				opts.PrefetchValues = false
				opts.Prefix = prefix
				it := txn.NewIterator(opts)
				defer it.Close()

				it.Rewind()
				if it.ValidForPrefix(prefix) {
					return &metadata.StoreError{
						Code:    metadata.ErrNotEmpty,
						Message: "destination directory not empty",
					}
				}
			} else {
				// Moving non-directory: destination must not be a directory
				if destFile.Type == metadata.FileTypeDirectory {
					return &metadata.StoreError{
						Code:    metadata.ErrIsDirectory,
						Message: "cannot replace directory with non-directory",
					}
				}
			}

			// Handle destination file replacement
			// First, get the link count to check if it has other hard links
			destLinkItem, err := txn.Get(keyLinkCount(destID))
			if err != nil && err != badger.ErrKeyNotFound {
				return fmt.Errorf("failed to get destination link count: %w", err)
			}

			var destLinkCount uint32 = 1 // Default if not found
			if err == nil {
				err = destLinkItem.Value(func(val []byte) error {
					lc, decErr := decodeUint32(val)
					if decErr != nil {
						return decErr
					}
					destLinkCount = lc
					return nil
				})
				if err != nil {
					return err
				}
			}

			// Remove the directory entry for the destination
			if err := txn.Delete(keyChild(toDirID, toName)); err != nil {
				return fmt.Errorf("failed to delete destination child entry: %w", err)
			}

			// Handle based on link count
			if destLinkCount > 1 {
				// File has other hard links - just decrement count, don't delete
				destLinkCount--
				if err := txn.Set(keyLinkCount(destID), encodeUint32(destLinkCount)); err != nil {
					return fmt.Errorf("failed to update destination link count: %w", err)
				}
				// Update the file's Nlink and Ctime
				destFile.Nlink = destLinkCount
				destFile.Ctime = time.Now()
				destBytes, err := encodeFile(destFile)
				if err != nil {
					return err
				}
				if err := txn.Set(keyFile(destID), destBytes); err != nil {
					return fmt.Errorf("failed to update destination file: %w", err)
				}
			} else {
				// Last link - delete the file metadata
				if err := txn.Delete(keyFile(destID)); err != nil {
					return fmt.Errorf("failed to delete destination file: %w", err)
				}
				if err := txn.Delete(keyLinkCount(destID)); err != nil && err != badger.ErrKeyNotFound {
					return fmt.Errorf("failed to delete destination link count: %w", err)
				}
			}
		} else if err != badger.ErrKeyNotFound {
			return fmt.Errorf("failed to check destination: %w", err)
		}

		// 5. Perform the move
		// Remove from source directory
		if err := txn.Delete(keyChild(fromDirID, fromName)); err != nil {
			return fmt.Errorf("failed to delete source child entry: %w", err)
		}

		// Add to destination directory
		sourceIDBytes, err := sourceID.MarshalBinary()
		if err != nil {
			return fmt.Errorf("failed to marshal source ID: %w", err)
		}
		if err := txn.Set(keyChild(toDirID, toName), sourceIDBytes); err != nil {
			return fmt.Errorf("failed to set destination child entry: %w", err)
		}

		// 6. Update timestamps and link counts
		now := time.Now()

		// If moving a directory to a different parent, update parent link counts
		// (the ".." entry in the moved directory changes its target)
		if sourceFile.Type == metadata.FileTypeDirectory && fromDirID != toDirID {
			// Decrement source directory's link count (losing ".." reference)
			fromLinkItem, err := txn.Get(keyLinkCount(fromDirID))
			if err == nil {
				var fromLinkCount uint32
				err = fromLinkItem.Value(func(val []byte) error {
					lc, decErr := decodeUint32(val)
					if decErr != nil {
						return decErr
					}
					fromLinkCount = lc
					return nil
				})
				if err == nil && fromLinkCount > 0 {
					fromLinkCount--
					if err := txn.Set(keyLinkCount(fromDirID), encodeUint32(fromLinkCount)); err != nil {
						return fmt.Errorf("failed to update source directory link count: %w", err)
					}
					fromDirFile.Nlink = fromLinkCount
				}
			}

			// Increment destination directory's link count (gaining ".." reference)
			toLinkItem, err := txn.Get(keyLinkCount(toDirID))
			var toLinkCount uint32 = 2 // Default for directory
			if err == nil {
				err = toLinkItem.Value(func(val []byte) error {
					lc, decErr := decodeUint32(val)
					if decErr != nil {
						return decErr
					}
					toLinkCount = lc
					return nil
				})
			}
			if err == nil || err == badger.ErrKeyNotFound {
				toLinkCount++
				if err := txn.Set(keyLinkCount(toDirID), encodeUint32(toLinkCount)); err != nil {
					return fmt.Errorf("failed to update destination directory link count: %w", err)
				}
				toDirFile.Nlink = toLinkCount
			}
		}

		// Update source directory (lost a child)
		fromDirFile.Ctime = now
		fromDirFile.Mtime = now
		fromDirBytes, err := encodeFile(fromDirFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(fromDirID), fromDirBytes); err != nil {
			return fmt.Errorf("failed to update source directory: %w", err)
		}

		// Update destination directory (gained a child)
		toDirFile.Ctime = now
		toDirFile.Mtime = now
		toDirBytes, err := encodeFile(toDirFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(toDirID), toDirBytes); err != nil {
			return fmt.Errorf("failed to update destination directory: %w", err)
		}

		// Detect NFS "silly rename" pattern: RENAME to ".nfs*"
		// When the NFS client renames a file to .nfs* instead of unlinking it
		// (because the file is still open), we should set nlink=0 to match
		// POSIX semantics where fstat() on an unlinked open file returns nlink=0.
		if strings.HasPrefix(toName, ".nfs") && sourceFile.Type != metadata.FileTypeDirectory {
			sourceFile.Nlink = 0
			if err := txn.Set(keyLinkCount(sourceID), encodeUint32(0)); err != nil {
				return fmt.Errorf("failed to update link count for silly rename: %w", err)
			}
		}

		// Update moved file's ctime
		sourceFile.Ctime = now
		sourceBytes, err := encodeFile(sourceFile)
		if err != nil {
			return err
		}
		if err := txn.Set(keyFile(sourceID), sourceBytes); err != nil {
			return fmt.Errorf("failed to update moved file: %w", err)
		}

		return nil
	})

	return err
}

// GetFileByContentID retrieves file metadata by its content identifier.
//
// This scans all files to find one matching the given ContentID.
// Note: This is O(n) and may be slow for large filesystems.
func (store *BadgerMetadataStore) GetFileByContentID(
	ctx context.Context,
	contentID metadata.ContentID,
) (*metadata.File, error) {
	// Check context before starting
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result *metadata.File

	err := store.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(prefixFile)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			// Check context periodically
			if err := ctx.Err(); err != nil {
				return err
			}

			item := it.Item()
			err := item.Value(func(val []byte) error {
				file, err := decodeFile(val)
				if err != nil {
					return nil // Skip corrupted entries
				}

				if file.ContentID == contentID {
					// Found matching file - return it directly
					// (decodeFile returns *metadata.File with all fields populated)
					result = file
					return errFound // Signal we found it
				}
				return nil
			})

			if err == errFound {
				return nil // Exit iterator
			}
			if err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("no file found with content ID: %s", contentID),
		}
	}

	return result, nil
}

// errFound is used to signal iterator completion when we find a match
var errFound = fmt.Errorf("found")
